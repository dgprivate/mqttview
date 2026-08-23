package mqttc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// v5Client speaks MQTT 5.0 via eclipse/paho.golang's autopaho wrapper, which
// supplies the reconnect loop.
type v5Client struct {
	spec   ConnectionSpec
	events Events
	cfg    autopaho.ClientConfig

	mu     sync.Mutex
	subs   map[string]Subscription
	cm     *autopaho.ConnectionManager
	cancel context.CancelFunc
	// connErr is the last failure autopaho reported from its retry loop.
	// AwaitConnection only ever returns the caller's context error, so without
	// this the real reason never leaves the library.
	connErr error
	// failed is closed on the first connect failure, letting Connect answer
	// immediately instead of waiting out its timeout.
	failed chan struct{}
}

func (c *v5Client) setConnErr(err error) {
	c.mu.Lock()
	c.connErr = err
	if err != nil {
		select {
		case <-c.failed: // already closed
		default:
			close(c.failed)
		}
	}
	c.mu.Unlock()
}

func (c *v5Client) lastConnErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connErr
}

func newV5Client(spec ConnectionSpec, tlsCfg *tls.Config, events Events) (*v5Client, error) {
	u, err := url.Parse(spec.URL)
	if err != nil {
		return nil, fmt.Errorf("mqttc: parse url: %w", err)
	}

	c := &v5Client{
		spec:   spec,
		events: events,
		subs:   make(map[string]Subscription, len(spec.Subscriptions)),
		failed: make(chan struct{}),
	}
	for _, s := range spec.Subscriptions {
		c.subs[s.Filter] = s
	}

	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		TlsCfg:                        tlsCfg,
		KeepAlive:                     uint16(spec.KeepAlive),
		CleanStartOnInitialConnection: spec.CleanStart,
		SessionExpiryInterval:         spec.SessionExpiry,
		ConnectTimeout:                time.Duration(spec.ConnectTimeout) * time.Second,
		ReconnectBackoff:              autopaho.NewExponentialBackoff(time.Second, time.Minute, 5*time.Second, 2),
		ConnectUsername:               spec.Username,
		ConnectPassword:               []byte(spec.Password),
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connack *paho.Connack) {
			// Subscriptions must be re-sent unless the broker restored the
			// session; re-sending is harmless when it did.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.subscribeVia(ctx, cm, c.snapshotSubs()); err != nil {
					c.events.fail(err)
				}
			}()
			c.setConnErr(nil)
			c.events.up(connack.SessionPresent)
		},
		OnConnectionDown: func() bool {
			c.events.down(nil)
			return true // keep retrying
		},
		OnConnectError: func(err error) {
			c.setConnErr(err)
			c.events.fail(explainConnectError(spec, err))
		},
		ClientConfig: paho.ClientConfig{
			ClientID: spec.ClientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					c.events.message(convertV5(spec.ID, pr.Packet))
					return true, nil
				},
			},
			OnClientError: func(err error) {
				c.events.fail(err)
			},
			OnServerDisconnect: func(d *paho.Disconnect) {
				c.events.down(fmt.Errorf("server sent DISCONNECT (reason code %d)", d.ReasonCode))
			},
		},
	}
	if spec.Will != nil {
		cfg.WillMessage = &paho.WillMessage{
			Topic:   spec.Will.Topic,
			Payload: []byte(spec.Will.Payload),
			QoS:     spec.Will.QoS,
			Retain:  spec.Will.Retain,
		}
		if spec.Will.DelayInterval > 0 {
			delay := spec.Will.DelayInterval
			cfg.WillProperties = &paho.WillProperties{WillDelayInterval: &delay}
		}
	}
	c.cfg = cfg
	return c, nil
}

func (c *v5Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.cm != nil {
		cm := c.cm
		c.mu.Unlock()
		return cm.AwaitConnection(ctx)
	}
	// autopaho ties the reconnect loop's lifetime to this context, so it must
	// outlive the request that triggered the connect.
	runCtx, cancel := context.WithCancel(context.Background())
	cm, err := autopaho.NewConnection(runCtx, c.cfg)
	if err != nil {
		cancel()
		c.mu.Unlock()
		return err
	}
	c.cm, c.cancel = cm, cancel
	failed := c.failed
	c.mu.Unlock()

	awaited := make(chan error, 1)
	go func() { awaited <- cm.AwaitConnection(ctx) }()

	select {
	case err := <-awaited:
		if err != nil {
			// Prefer whatever autopaho's retry loop actually hit; the context
			// error only says we stopped waiting, not why nothing came back.
			if last := c.lastConnErr(); last != nil {
				return last
			}
			return err
		}
		return nil
	case <-failed:
		// autopaho would keep retrying a broker that rejects us, leaving the
		// caller to wait out the whole timeout for an answer we already have.
		if last := c.lastConnErr(); last != nil {
			return last
		}
		return errors.New("mqttc: connection attempt failed")
	}
}

func (c *v5Client) Subscribe(ctx context.Context, subs []Subscription) error {
	if len(subs) == 0 {
		return nil
	}
	c.mu.Lock()
	for _, s := range subs {
		c.subs[s.Filter] = s
	}
	cm := c.cm
	c.mu.Unlock()

	if cm == nil {
		return nil // applied on connect
	}
	return c.subscribeVia(ctx, cm, subs)
}

func (c *v5Client) Unsubscribe(ctx context.Context, filters []string) error {
	if len(filters) == 0 {
		return nil
	}
	c.mu.Lock()
	for _, f := range filters {
		delete(c.subs, f)
	}
	cm := c.cm
	c.mu.Unlock()

	if cm == nil {
		return nil
	}
	_, err := cm.Unsubscribe(ctx, &paho.Unsubscribe{Topics: filters})
	return err
}

func (c *v5Client) Publish(ctx context.Context, req PublishRequest) error {
	c.mu.Lock()
	cm := c.cm
	c.mu.Unlock()
	if cm == nil {
		return errors.New("mqttc: not connected")
	}

	p := &paho.Publish{
		Topic:   req.Topic,
		QoS:     req.QoS,
		Retain:  req.Retain,
		Payload: req.Payload,
	}
	if req.Props != nil {
		props := &paho.PublishProperties{
			ContentType:     req.Props.ContentType,
			ResponseTopic:   req.Props.ResponseTopic,
			CorrelationData: req.Props.CorrelationData,
			PayloadFormat:   req.Props.PayloadFormat,
			MessageExpiry:   req.Props.MessageExpiry,
		}
		for k, v := range req.Props.User {
			props.User = append(props.User, paho.UserProperty{Key: k, Value: v})
		}
		p.Properties = props
	}
	_, err := cm.Publish(ctx, p)
	return err
}

func (c *v5Client) Disconnect(ctx context.Context) error {
	c.mu.Lock()
	cm, cancel := c.cm, c.cancel
	c.cm, c.cancel = nil, nil
	c.mu.Unlock()

	if cm == nil {
		return nil
	}
	err := cm.Disconnect(ctx)
	if cancel != nil {
		cancel()
	}

	// Wait for autopaho to actually finish. Disconnect only asks: its retry
	// loop can still be part-way through dialling, and returning here would
	// mean Shutdown reports the connection closed while a socket is still
	// being opened. On SIGTERM that is a connection left half-open behind a
	// process that has already exited.
	select {
	case <-cm.Done():
	case <-ctx.Done():
		return errors.Join(err, fmt.Errorf("mqttc: waiting for the connection to close: %w", ctx.Err()))
	}
	return err
}

func (c *v5Client) snapshotSubs() []Subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Subscription, 0, len(c.subs))
	for _, s := range c.subs {
		out = append(out, s)
	}
	return out
}

func (c *v5Client) subscribeVia(ctx context.Context, cm *autopaho.ConnectionManager, subs []Subscription) error {
	if len(subs) == 0 {
		return nil
	}
	opts := make([]paho.SubscribeOptions, 0, len(subs))
	for _, s := range subs {
		opts = append(opts, paho.SubscribeOptions{
			Topic:             s.Filter,
			QoS:               s.QoS,
			NoLocal:           s.NoLocal,
			RetainAsPublished: s.RetainAsPublished,
			RetainHandling:    s.RetainHandling,
		})
	}
	_, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: opts})
	return err
}

func convertV5(connID string, p *paho.Publish) Message {
	m := Message{
		ConnectionID: connID,
		Topic:        p.Topic,
		Payload:      p.Payload,
		QoS:          p.QoS,
		Retain:       p.Retain,
		ReceivedAt:   time.Now(),
	}
	if p.Properties != nil {
		props := &MessageProps{
			ContentType:     p.Properties.ContentType,
			ResponseTopic:   p.Properties.ResponseTopic,
			CorrelationData: p.Properties.CorrelationData,
			PayloadFormat:   p.Properties.PayloadFormat,
			MessageExpiry:   p.Properties.MessageExpiry,
		}
		if len(p.Properties.User) > 0 {
			props.User = make(map[string]string, len(p.Properties.User))
			for _, up := range p.Properties.User {
				props.User[up.Key] = up.Value
			}
		}
		m.Props = props
	}
	return m
}
