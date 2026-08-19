package mqttc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// Client is the protocol-agnostic surface the rest of mqttview talks to. Both
// the MQTT 3.x and MQTT 5 implementations reconnect on their own, so callers
// see a connection that heals rather than one they must babysit.
type Client interface {
	// Connect starts the connection. It returns once the first CONNACK has
	// been received, or when ctx expires.
	Connect(ctx context.Context) error
	// Subscribe adds subscriptions to the live session. It is also called
	// automatically after every reconnect.
	Subscribe(ctx context.Context, subs []Subscription) error
	// Unsubscribe drops topic filters.
	Unsubscribe(ctx context.Context, filters []string) error
	// Publish sends a message.
	Publish(ctx context.Context, req PublishRequest) error
	// Disconnect closes the connection cleanly and stops reconnecting.
	Disconnect(ctx context.Context) error
}

// Events are the callbacks a Client invokes. They are called from the client's
// own goroutines and must not block.
type Events struct {
	// Message fires for every received PUBLISH.
	Message func(Message)
	// Up fires on every successful CONNACK, including reconnects.
	Up func(sessionPresent bool)
	// Down fires when an established connection drops.
	Down func(err error)
	// Error fires for connection attempt failures and protocol errors.
	Error func(err error)
}

func (e Events) message(m Message) {
	if e.Message != nil {
		e.Message(m)
	}
}

func (e Events) up(sessionPresent bool) {
	if e.Up != nil {
		e.Up(sessionPresent)
	}
}

func (e Events) down(err error) {
	if e.Down != nil {
		e.Down(err)
	}
}

func (e Events) fail(err error) {
	if e.Error != nil && err != nil {
		e.Error(err)
	}
}

// NewClient builds the right client implementation for the spec's protocol
// version. The spec must already have been normalized.
func NewClient(spec ConnectionSpec, events Events) (Client, error) {
	if !spec.Version.Valid() {
		return nil, fmt.Errorf("mqttc: unsupported MQTT version %d", int(spec.Version))
	}
	tlsCfg, err := buildTLSConfig(spec)
	if err != nil {
		return nil, err
	}
	if spec.Version == V5 {
		return newV5Client(spec, tlsCfg, events)
	}
	return newV3Client(spec, tlsCfg, events)
}

// buildTLSConfig assembles the TLS settings for an encrypted transport, or
// returns nil for plaintext ones.
func buildTLSConfig(spec ConnectionSpec) (*tls.Config, error) {
	if !spec.UsesTLS() {
		return nil, nil
	}
	t := spec.TLS
	cfg := &tls.Config{
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // opt-in, surfaced in the UI
		ServerName:         t.ServerName,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         t.ALPN,
	}
	if t.MinVersion == "1.3" {
		cfg.MinVersion = tls.VersionTLS13
	}
	if t.CAPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(t.CAPEM)) {
			return nil, errors.New("mqttc: tls.caPem contains no usable certificate")
		}
		cfg.RootCAs = pool
	}
	if t.ClientCertPEM != "" {
		cert, err := tls.X509KeyPair([]byte(t.ClientCertPEM), []byte(t.ClientKeyPEM))
		if err != nil {
			return nil, fmt.Errorf("mqttc: client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
