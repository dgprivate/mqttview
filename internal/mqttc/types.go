// Package mqttc provides a single interface over every MQTT protocol version
// mqttview supports (3.1, 3.1.1 and 5.0) across every transport (TCP, TLS,
// WebSocket, secure WebSocket and Unix sockets), plus the live state — topic
// tree, retained values and recent message history — that the UI renders.
package mqttc

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Version is the MQTT protocol level as it appears on the wire: 3 for MQTT
// 3.1, 4 for MQTT 3.1.1 and 5 for MQTT 5.0.
type Version int

const (
	V31  Version = 3
	V311 Version = 4
	V5   Version = 5
)

// ParseVersion accepts both the wire level ("4") and the human name ("3.1.1").
func ParseVersion(s string) (Version, error) {
	switch strings.TrimSpace(s) {
	case "3", "3.1", "v3", "v3.1":
		return V31, nil
	case "4", "3.1.1", "v4", "v3.1.1", "":
		return V311, nil
	case "5", "5.0", "v5", "v5.0":
		return V5, nil
	}
	return 0, fmt.Errorf("mqttc: unknown MQTT version %q", s)
}

func (v Version) String() string {
	switch v {
	case V31:
		return "3.1"
	case V311:
		return "3.1.1"
	case V5:
		return "5.0"
	}
	return fmt.Sprintf("unknown(%d)", int(v))
}

// Valid reports whether v is a protocol version we can actually speak.
func (v Version) Valid() bool { return v == V31 || v == V311 || v == V5 }

// Subscription is one topic filter the connection should hold open. The
// NoLocal, RetainAsPublished and RetainHandling options only exist in MQTT 5
// and are silently ignored on 3.x connections.
type Subscription struct {
	Filter            string `json:"filter"`
	QoS               byte   `json:"qos"`
	NoLocal           bool   `json:"noLocal,omitempty"`
	RetainAsPublished bool   `json:"retainAsPublished,omitempty"`
	RetainHandling    byte   `json:"retainHandling,omitempty"`
}

// TLSSpec carries the material needed to build a *tls.Config for a broker.
// PEM blocks are stored inline rather than as file paths so that mqttview
// stays deployable as a single binary with a single data directory.
type TLSSpec struct {
	// InsecureSkipVerify disables certificate verification. Exposed because
	// self-signed brokers are extremely common on home networks, but the UI
	// marks any connection using it as insecure.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// ServerName overrides SNI / hostname verification.
	ServerName string `json:"serverName,omitempty"`
	// CAPEM is a PEM bundle of extra roots to trust. Empty means system roots.
	CAPEM string `json:"caPem,omitempty"`
	// ClientCertPEM and ClientKeyPEM enable mutual TLS.
	ClientCertPEM string `json:"clientCertPem,omitempty"`
	ClientKeyPEM  string `json:"clientKeyPem,omitempty"`
	// MinVersion is "1.2" or "1.3". Empty defaults to 1.2.
	MinVersion string `json:"minVersion,omitempty"`
	// ALPN protocols, needed by some cloud brokers (e.g. "mqtt", "x-amzn-mqtt-ca").
	ALPN []string `json:"alpn,omitempty"`
}

// Will is the MQTT Last Will and Testament.
type Will struct {
	Topic   string `json:"topic"`
	Payload string `json:"payload"`
	QoS     byte   `json:"qos"`
	Retain  bool   `json:"retain"`
	// DelayInterval is the MQTT 5 will delay in seconds; ignored on 3.x.
	DelayInterval uint32 `json:"delayInterval,omitempty"`
}

// ConnectionSpec fully describes one broker connection.
type ConnectionSpec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// URL is scheme://host[:port][/path]. Supported schemes: mqtt, tcp, mqtts,
	// ssl, tls, ws, wss, unix.
	URL      string  `json:"url"`
	Version  Version `json:"version"`
	ClientID string  `json:"clientId"`
	Username string  `json:"username,omitempty"`
	Password string  `json:"-"`

	KeepAlive  int  `json:"keepAlive"`
	CleanStart bool `json:"cleanStart"`
	// SessionExpiry in seconds (MQTT 5 only).
	SessionExpiry uint32 `json:"sessionExpiry,omitempty"`
	// ConnectTimeout in seconds; 0 uses a 10s default.
	ConnectTimeout int `json:"connectTimeout,omitempty"`

	TLS  TLSSpec `json:"tls"`
	Will *Will   `json:"will,omitempty"`

	Subscriptions []Subscription `json:"subscriptions"`
	AutoConnect   bool           `json:"autoConnect"`

	// HistorySize caps the per-connection message ring buffer. 0 uses the
	// package default.
	HistorySize int `json:"historySize,omitempty"`
}

// Normalize fills in defaults and rejects specs we cannot honour. It is called
// on every save and before every connect, so the rest of the package can
// assume a sane spec.
func (s *ConnectionSpec) Normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return errors.New("mqttc: name is required")
	}
	s.URL = strings.TrimSpace(s.URL)
	if s.URL == "" {
		return errors.New("mqttc: url is required")
	}
	if !strings.Contains(s.URL, "://") {
		s.URL = "mqtt://" + s.URL
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("mqttc: parse url: %w", err)
	}
	scheme, err := normalizeScheme(u.Scheme)
	if err != nil {
		return err
	}
	u.Scheme = scheme
	if u.Host == "" && scheme != "unix" {
		return errors.New("mqttc: url has no host")
	}
	if u.Port() == "" && scheme != "unix" {
		u.Host = u.Host + ":" + defaultPort(scheme)
	}
	// Credentials belong in Username/Password, not in the URL, so they are
	// never logged as part of the endpoint.
	if u.User != nil {
		if s.Username == "" {
			s.Username = u.User.Username()
		}
		if pw, ok := u.User.Password(); ok && s.Password == "" {
			s.Password = pw
		}
		u.User = nil
	}
	s.URL = u.String()

	if s.Version == 0 {
		s.Version = V311
	}
	if !s.Version.Valid() {
		return fmt.Errorf("mqttc: unsupported MQTT version %d", int(s.Version))
	}
	if s.Version == V5 && scheme == "unix" {
		return errors.New("mqttc: unix sockets are only supported on MQTT 3.x")
	}
	if s.ClientID == "" {
		s.ClientID = "mqttview-" + shortID()
	}
	if s.KeepAlive <= 0 {
		s.KeepAlive = 60
	}
	if s.KeepAlive > 65535 {
		return errors.New("mqttc: keepAlive must be <= 65535 seconds")
	}
	if s.ConnectTimeout <= 0 {
		s.ConnectTimeout = 10
	}

	seen := make(map[string]struct{}, len(s.Subscriptions))
	subs := s.Subscriptions[:0]
	for _, sub := range s.Subscriptions {
		sub.Filter = strings.TrimSpace(sub.Filter)
		if sub.Filter == "" {
			continue
		}
		if err := ValidateFilter(sub.Filter); err != nil {
			return err
		}
		if sub.QoS > 2 {
			return fmt.Errorf("mqttc: subscription %q: qos must be 0, 1 or 2", sub.Filter)
		}
		if sub.RetainHandling > 2 {
			return fmt.Errorf("mqttc: subscription %q: retainHandling must be 0, 1 or 2", sub.Filter)
		}
		if _, dup := seen[sub.Filter]; dup {
			continue
		}
		seen[sub.Filter] = struct{}{}
		subs = append(subs, sub)
	}
	s.Subscriptions = subs

	if s.Will != nil {
		if err := ValidateTopic(s.Will.Topic); err != nil {
			return fmt.Errorf("mqttc: will: %w", err)
		}
		if s.Will.QoS > 2 {
			return errors.New("mqttc: will: qos must be 0, 1 or 2")
		}
	}
	if s.TLS.MinVersion != "" && s.TLS.MinVersion != "1.2" && s.TLS.MinVersion != "1.3" {
		return errors.New("mqttc: tls.minVersion must be 1.2 or 1.3")
	}
	if (s.TLS.ClientCertPEM == "") != (s.TLS.ClientKeyPEM == "") {
		return errors.New("mqttc: client certificate and key must be provided together")
	}
	return nil
}

// UsesTLS reports whether the spec's scheme is an encrypted transport.
func (s ConnectionSpec) UsesTLS() bool {
	scheme := s.scheme()
	return scheme == "ssl" || scheme == "wss"
}

func (s ConnectionSpec) scheme() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return u.Scheme
}

// normalizeScheme collapses the many spellings brokers use into the four
// canonical schemes both client libraries understand, plus unix.
func normalizeScheme(scheme string) (string, error) {
	switch strings.ToLower(scheme) {
	case "", "mqtt", "tcp":
		return "tcp", nil
	case "mqtts", "ssl", "tls", "tcps", "mqtt+ssl":
		return "ssl", nil
	case "ws":
		return "ws", nil
	case "wss":
		return "wss", nil
	case "unix":
		return "unix", nil
	}
	return "", fmt.Errorf("mqttc: unsupported scheme %q", scheme)
}

func defaultPort(scheme string) string {
	switch scheme {
	case "ssl":
		return "8883"
	case "ws":
		return "8083"
	case "wss":
		return "8084"
	default:
		return "1883"
	}
}

// Message is a PUBLISH received from or sent to a broker.
type Message struct {
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Payload      []byte    `json:"payload"`
	QoS          byte      `json:"qos"`
	Retain       bool      `json:"retain"`
	Duplicate    bool      `json:"duplicate,omitempty"`
	ReceivedAt   time.Time `json:"receivedAt"`
	// Seq is a per-connection monotonic counter, used by the UI to detect
	// gaps after a reconnect and to key list rows.
	Seq uint64 `json:"seq"`
	// Props are MQTT 5 publish properties; nil on 3.x.
	Props *MessageProps `json:"props,omitempty"`
}

// MessageProps holds the MQTT 5 properties the UI displays.
type MessageProps struct {
	ContentType     string            `json:"contentType,omitempty"`
	ResponseTopic   string            `json:"responseTopic,omitempty"`
	CorrelationData []byte            `json:"correlationData,omitempty"`
	PayloadFormat   *byte             `json:"payloadFormat,omitempty"`
	MessageExpiry   *uint32           `json:"messageExpiry,omitempty"`
	User            map[string]string `json:"user,omitempty"`
}

// PublishRequest is an outbound publish.
type PublishRequest struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
	Props   *MessageProps
}

// State is the lifecycle state of a connection.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateError        State = "error"
)

// Status is a point-in-time snapshot of a connection, safe to serialise to
// the browser.
type Status struct {
	ConnectionID string     `json:"connectionId"`
	State        State      `json:"state"`
	Since        time.Time  `json:"since"`
	ConnectedAt  *time.Time `json:"connectedAt,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	Attempts     int        `json:"attempts"`
	// Received and Sent are lifetime message counters.
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
	// SessionPresent reflects the broker's CONNACK flag.
	SessionPresent bool `json:"sessionPresent"`
	// Version is the negotiated protocol version, echoed for the UI badge.
	Version string `json:"version"`
}
