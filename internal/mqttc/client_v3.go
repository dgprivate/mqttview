package mqttc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	paho3 "github.com/eclipse/paho.mqtt.golang"
)

// v3Client speaks MQTT 3.1 and 3.1.1 via eclipse/paho.mqtt.golang.
type v3Client struct {
	spec   ConnectionSpec
	events Events
	client paho3.Client

	mu     sync.Mutex
	subs   map[string]Subscription
	closed bool
}

func newV3Client(spec ConnectionSpec, tlsCfg *tls.Config, events Events) (*v3Client, error) {
	c := &v3Client{
		spec:   spec,
		events: events,
		subs:   make(map[string]Subscription, len(spec.Subscriptions)),
	}
	for _, s := range spec.Subscriptions {
		c.subs[s.Filter] = s
	}

	opts := paho3.NewClientOptions()
	opts.AddBroker(spec.URL)
	opts.SetClientID(spec.ClientID)
	opts.SetProtocolVersion(uint(spec.Version))
	opts.SetUsername(spec.Username)
	opts.SetPassword(spec.Password)
	opts.SetKeepAlive(time.Duration(spec.KeepAlive) * time.Second)
	opts.SetCleanSession(spec.CleanStart)
	opts.SetConnectTimeout(time.Duration(spec.ConnectTimeout) * time.Second)
	opts.SetAutoReconnect(true)
	// Deliberately off: with ConnectRetry paho loops on the initial connect
	// internally and never completes the token with an error, so the caller
	// only ever sees its own context deadline instead of "no route to host" or
	// "tls: certificate required". Retrying is the Conn supervisor's job, which
	// gets to report each failure on the way.
	opts.SetConnectRetry(false)
	opts.SetMaxReconnectInterval(60 * time.Second)
	// Messages are fanned out to browsers, which imposes no ordering
	// requirement of its own; letting handlers run concurrently avoids the
	// classic paho deadlock where a slow handler stalls the read loop.
	opts.SetOrderMatters(false)
	if tlsCfg != nil {
		opts.SetTLSConfig(tlsCfg)
	}
	if spec.Will != nil {
		opts.SetBinaryWill(spec.Will.Topic, []byte(spec.Will.Payload), spec.Will.QoS, spec.Will.Retain)
	}

	opts.SetDefaultPublishHandler(func(_ paho3.Client, m paho3.Message) {
		c.events.message(Message{
			ConnectionID: spec.ID,
			Topic:        m.Topic(),
			Payload:      m.Payload(),
			QoS:          m.Qos(),
			Retain:       m.Retained(),
			Duplicate:    m.Duplicate(),
			ReceivedAt:   time.Now(),
		})
	})
	opts.SetOnConnectHandler(func(cl paho3.Client) {
		// paho 3 only restores subscriptions for persistent sessions, so we
		// always replay our own set. Doing it here also covers reconnects.
		//
		// Retried, because one failed attempt leaves a client that is
		// connected and deaf: no error anybody would act on, and nothing ever
		// arrives. The retries run in their own goroutine — this handler is on
		// paho's connection goroutine, and sleeping in it stalls the client.
		if err := c.resubscribe(cl); err != nil {
			c.events.fail(err)
			go c.retryResubscribe(cl)
		}
		// MQTT 3 exposes session-present only on the initial connect token,
		// which Connect reports; reconnects assume a fresh session.
		c.events.up(false)
	})
	opts.SetConnectionLostHandler(func(_ paho3.Client, err error) {
		c.events.down(err)
	})
	// No ReconnectingHandler: paho gives it no error to report, so reporting a
	// synthetic "reconnecting" here only overwrote the real reason the link
	// dropped, which is the one thing the user needs to see.

	c.client = paho3.NewClient(opts)
	return c, nil
}

func (c *v3Client) Connect(ctx context.Context) error {
	tok := c.client.Connect()
	select {
	case <-tok.Done():
		if err := tok.Error(); err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if ct, ok := tok.(*paho3.ConnectToken); ok {
		c.events.up(ct.SessionPresent())
	}
	return nil
}

func (c *v3Client) Subscribe(ctx context.Context, subs []Subscription) error {
	if len(subs) == 0 {
		return nil
	}
	filters := make(map[string]byte, len(subs))
	c.mu.Lock()
	for _, s := range subs {
		c.subs[s.Filter] = s
		filters[s.Filter] = s.QoS
	}
	c.mu.Unlock()

	if !c.client.IsConnected() {
		// Recorded above; the OnConnect handler will apply them.
		return nil
	}
	return waitToken(ctx, c.client.SubscribeMultiple(filters, nil))
}

func (c *v3Client) Unsubscribe(ctx context.Context, filters []string) error {
	if len(filters) == 0 {
		return nil
	}
	c.mu.Lock()
	for _, f := range filters {
		delete(c.subs, f)
	}
	c.mu.Unlock()

	if !c.client.IsConnected() {
		return nil
	}
	return waitToken(ctx, c.client.Unsubscribe(filters...))
}

func (c *v3Client) Publish(ctx context.Context, req PublishRequest) error {
	// MQTT 3 has no publish properties; they are dropped rather than
	// silently changing the message.
	return waitToken(ctx, c.client.Publish(req.Topic, req.QoS, req.Retain, req.Payload))
}

func (c *v3Client) Disconnect(_ context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// 250ms lets in-flight packets drain without making the UI wait.
	c.client.Disconnect(250)
	return nil
}

// retryResubscribe keeps replaying the subscriptions until the broker takes
// them or the client is no longer connected.
func (c *v3Client) retryResubscribe(cl paho3.Client) {
	backoff := time.Second
	for attempt := 2; attempt <= 5; attempt++ {
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if !cl.IsConnected() {
			return // a new connection will run the handler again
		}
		if err := c.resubscribe(cl); err == nil {
			return
		} else {
			c.events.fail(fmt.Errorf("subscribe after connecting (attempt %d): %w", attempt, err))
		}
	}
}

func (c *v3Client) resubscribe(cl paho3.Client) error {
	c.mu.Lock()
	filters := make(map[string]byte, len(c.subs))
	for f, s := range c.subs {
		filters[f] = s.QoS
	}
	c.mu.Unlock()

	if len(filters) == 0 {
		return nil
	}
	tok := cl.SubscribeMultiple(filters, nil)
	if !tok.WaitTimeout(10 * time.Second) {
		return errors.New("mqttc: timed out restoring subscriptions")
	}
	return tok.Error()
}

func waitToken(ctx context.Context, tok paho3.Token) error {
	select {
	case <-tok.Done():
		return tok.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}
