package mqttc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound is returned for an unknown connection ID.
var ErrNotFound = errors.New("mqttc: connection not found")

// Observer receives live updates for every connection. Callbacks run on the
// MQTT client's goroutine and must not block.
type Observer struct {
	OnMessage func(Message)
	OnStatus  func(Status)
}

// Manager owns every configured broker connection and the live state derived
// from it.
type Manager struct {
	log *slog.Logger

	mu    sync.RWMutex
	conns map[string]*Conn

	obsMu   sync.RWMutex
	obs     map[int]Observer
	nextObs int
}

// NewManager returns an empty manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		log:   log,
		conns: make(map[string]*Conn),
		obs:   make(map[int]Observer),
	}
}

// AddObserver registers callbacks and returns a function that removes them.
func (m *Manager) AddObserver(o Observer) func() {
	m.obsMu.Lock()
	id := m.nextObs
	m.nextObs++
	m.obs[id] = o
	m.obsMu.Unlock()

	return func() {
		m.obsMu.Lock()
		delete(m.obs, id)
		m.obsMu.Unlock()
	}
}

func (m *Manager) emitMessage(msg Message) {
	m.obsMu.RLock()
	defer m.obsMu.RUnlock()
	for _, o := range m.obs {
		if o.OnMessage != nil {
			o.OnMessage(msg)
		}
	}
}

func (m *Manager) emitStatus(st Status) {
	m.obsMu.RLock()
	defer m.obsMu.RUnlock()
	for _, o := range m.obs {
		if o.OnStatus != nil {
			o.OnStatus(st)
		}
	}
}

// Upsert registers or replaces a connection definition. An already-running
// connection is torn down and restarted only when the new spec would change
// the session; cosmetic edits such as renaming leave it untouched.
func (m *Manager) Upsert(ctx context.Context, spec ConnectionSpec) (*Conn, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	existing, ok := m.conns[spec.ID]
	if !ok {
		c := newConn(m, spec)
		m.conns[spec.ID] = c
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	wasRunning := existing.Status().State != StateDisconnected
	restart := wasRunning && existing.needsRestart(spec)
	if restart {
		if err := existing.disconnect(ctx); err != nil {
			m.log.Warn("disconnect before restart failed", "connection", spec.ID, "error", err)
		}
	}
	existing.setSpec(spec)
	if restart {
		if err := existing.connect(ctx); err != nil {
			return existing, err
		}
	} else if wasRunning {
		// Subscriptions can change without a reconnect.
		if err := existing.syncSubscriptions(ctx); err != nil {
			return existing, err
		}
	}
	return existing, nil
}

// Remove disconnects and forgets a connection.
func (m *Manager) Remove(ctx context.Context, id string) error {
	m.mu.Lock()
	c, ok := m.conns[id]
	delete(m.conns, id)
	m.mu.Unlock()

	if !ok {
		return ErrNotFound
	}
	return c.disconnect(ctx)
}

// Get returns a connection by ID.
func (m *Manager) Get(id string) (*Conn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.conns[id]
	return c, ok
}

// List returns every connection, ordered by name.
func (m *Manager) List() []*Conn {
	m.mu.RLock()
	out := make([]*Conn, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, c)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Spec().Name < out[j].Spec().Name })
	return out
}

// Connect starts a connection by ID.
func (m *Manager) Connect(ctx context.Context, id string) error {
	c, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	return c.connect(ctx)
}

// Disconnect stops a connection by ID.
func (m *Manager) Disconnect(ctx context.Context, id string) error {
	c, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	return c.disconnect(ctx)
}

// Publish sends a message on a connection.
func (m *Manager) Publish(ctx context.Context, id string, req PublishRequest) error {
	c, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	return c.Publish(ctx, req)
}

// StartAutoConnect connects every connection flagged AutoConnect. Failures are
// logged rather than returned: one unreachable broker must not stop startup.
func (m *Manager) StartAutoConnect(ctx context.Context) {
	for _, c := range m.List() {
		spec := c.Spec()
		if !spec.AutoConnect {
			continue
		}
		go func(c *Conn, name string) {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.connect(cctx); err != nil {
				m.log.Warn("auto-connect failed", "connection", name, "error", err)
			}
		}(c, spec.Name)
	}
}

// Shutdown disconnects everything.
func (m *Manager) Shutdown(ctx context.Context) {
	for _, c := range m.List() {
		if err := c.disconnect(ctx); err != nil {
			m.log.Warn("disconnect during shutdown failed", "connection", c.Spec().ID, "error", err)
		}
	}
}

// Conn is one broker connection plus its derived live state.
type Conn struct {
	mgr *Manager

	mu     sync.RWMutex
	spec   ConnectionSpec
	client Client
	status Status

	tree    *Tree
	history *History

	// ephemeral holds subscriptions owned by plugins. They are re-applied on
	// every connect but never written to the stored connection definition.
	ephemeral map[string]Subscription

	seq      atomic.Uint64
	received atomic.Uint64
	sent     atomic.Uint64
}

func newConn(m *Manager, spec ConnectionSpec) *Conn {
	return &Conn{
		mgr:       m,
		spec:      spec,
		tree:      NewTree(),
		history:   NewHistory(spec.HistorySize),
		ephemeral: map[string]Subscription{},
		status: Status{
			ConnectionID: spec.ID,
			State:        StateDisconnected,
			Since:        time.Now(),
			Version:      spec.Version.String(),
		},
	}
}

// Spec returns a copy of the connection definition. The password is included,
// so callers must not serialise it directly to a client.
func (c *Conn) Spec() ConnectionSpec {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.spec
}

// Tree exposes the live topic hierarchy.
func (c *Conn) Tree() *Tree { return c.tree }

// History exposes the recent-message ring.
func (c *Conn) History() *History { return c.history }

// Status returns the current connection status.
func (c *Conn) Status() Status {
	c.mu.RLock()
	st := c.status
	c.mu.RUnlock()
	st.Received = c.received.Load()
	st.Sent = c.sent.Load()
	return st
}

func (c *Conn) setSpec(spec ConnectionSpec) {
	c.mu.Lock()
	c.spec = spec
	c.status.Version = spec.Version.String()
	c.mu.Unlock()
}

// needsRestart reports whether moving to next requires a new MQTT session.
func (c *Conn) needsRestart(next ConnectionSpec) bool {
	cur := c.Spec()
	return cur.URL != next.URL ||
		cur.Version != next.Version ||
		cur.ClientID != next.ClientID ||
		cur.Username != next.Username ||
		cur.Password != next.Password ||
		cur.KeepAlive != next.KeepAlive ||
		cur.CleanStart != next.CleanStart ||
		cur.SessionExpiry != next.SessionExpiry ||
		tlsChanged(cur.TLS, next.TLS) ||
		willChanged(cur.Will, next.Will)
}

// tlsChanged compares TLS settings field by field; TLSSpec is not comparable
// with == because of the ALPN slice.
func tlsChanged(a, b TLSSpec) bool {
	if a.InsecureSkipVerify != b.InsecureSkipVerify ||
		a.ServerName != b.ServerName ||
		a.CAPEM != b.CAPEM ||
		a.ClientCertPEM != b.ClientCertPEM ||
		a.ClientKeyPEM != b.ClientKeyPEM ||
		a.MinVersion != b.MinVersion ||
		len(a.ALPN) != len(b.ALPN) {
		return true
	}
	return slices.Compare(a.ALPN, b.ALPN) != 0
}

func willChanged(a, b *Will) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil || b == nil:
		return true
	default:
		return *a != *b
	}
}

func (c *Conn) connect(ctx context.Context) error {
	c.mu.Lock()
	if c.client != nil {
		c.mu.Unlock()
		return nil // already running; reconnects are the client's job
	}
	spec := c.spec
	client, err := NewClient(spec, c.events())
	if err != nil {
		c.mu.Unlock()
		c.setState(StateError, err)
		return err
	}
	c.client = client
	c.status.Attempts++
	c.mu.Unlock()

	c.setState(StateConnecting, nil)

	if err := client.Connect(ctx); err != nil {
		c.mu.Lock()
		c.client = nil
		c.mu.Unlock()
		_ = client.Disconnect(context.Background())
		c.setState(StateError, err)
		return fmt.Errorf("connect to %s: %w", spec.URL, err)
	}

	c.mu.RLock()
	subs := make([]Subscription, 0, len(spec.Subscriptions)+len(c.ephemeral))
	subs = append(subs, spec.Subscriptions...)
	for _, s := range c.ephemeral {
		subs = append(subs, s)
	}
	c.mu.RUnlock()

	if err := client.Subscribe(ctx, subs); err != nil {
		c.mgr.log.Warn("initial subscribe failed", "connection", spec.ID, "error", err)
	}
	return nil
}

// SubscribeEphemeral adds subscriptions that belong to a plugin. They survive
// reconnects but are never persisted with the connection definition, so
// disabling a plugin leaves no trace in the user's configuration.
func (c *Conn) SubscribeEphemeral(ctx context.Context, subs []Subscription) error {
	for _, s := range subs {
		if err := ValidateFilter(s.Filter); err != nil {
			return err
		}
	}

	c.mu.Lock()
	for _, s := range subs {
		c.ephemeral[s.Filter] = s
	}
	client := c.client
	c.mu.Unlock()

	if client == nil {
		return nil
	}
	return client.Subscribe(ctx, subs)
}

// UnsubscribeEphemeral removes plugin-owned subscriptions. Filters that are
// also part of the persisted definition are left in place.
func (c *Conn) UnsubscribeEphemeral(ctx context.Context, filters []string) error {
	c.mu.Lock()
	keep := make(map[string]struct{}, len(c.spec.Subscriptions))
	for _, s := range c.spec.Subscriptions {
		keep[s.Filter] = struct{}{}
	}
	drop := make([]string, 0, len(filters))
	for _, f := range filters {
		delete(c.ephemeral, f)
		if _, held := keep[f]; !held {
			drop = append(drop, f)
		}
	}
	client := c.client
	c.mu.Unlock()

	if client == nil || len(drop) == 0 {
		return nil
	}
	return client.Unsubscribe(ctx, drop)
}

func (c *Conn) disconnect(ctx context.Context) error {
	c.mu.Lock()
	client := c.client
	c.client = nil
	c.mu.Unlock()

	if client == nil {
		c.setState(StateDisconnected, nil)
		return nil
	}
	err := client.Disconnect(ctx)
	c.setState(StateDisconnected, nil)
	return err
}

// Subscribe adds subscriptions at runtime and persists them on the spec so a
// reconnect keeps them.
func (c *Conn) Subscribe(ctx context.Context, subs []Subscription) error {
	for _, s := range subs {
		if err := ValidateFilter(s.Filter); err != nil {
			return err
		}
		if s.QoS > 2 {
			return fmt.Errorf("subscription %q: qos must be 0, 1 or 2", s.Filter)
		}
	}

	c.mu.Lock()
	for _, s := range subs {
		replaced := false
		for i, existing := range c.spec.Subscriptions {
			if existing.Filter == s.Filter {
				c.spec.Subscriptions[i] = s
				replaced = true
				break
			}
		}
		if !replaced {
			c.spec.Subscriptions = append(c.spec.Subscriptions, s)
		}
	}
	client := c.client
	c.mu.Unlock()

	if client == nil {
		return nil
	}
	return client.Subscribe(ctx, subs)
}

// Unsubscribe removes topic filters.
func (c *Conn) Unsubscribe(ctx context.Context, filters []string) error {
	drop := make(map[string]struct{}, len(filters))
	for _, f := range filters {
		drop[f] = struct{}{}
	}

	c.mu.Lock()
	kept := c.spec.Subscriptions[:0]
	for _, s := range c.spec.Subscriptions {
		if _, ok := drop[s.Filter]; !ok {
			kept = append(kept, s)
		}
	}
	c.spec.Subscriptions = kept
	client := c.client
	c.mu.Unlock()

	if client == nil {
		return nil
	}
	return client.Unsubscribe(ctx, filters)
}

// syncSubscriptions re-applies the spec's subscription list to a live client.
func (c *Conn) syncSubscriptions(ctx context.Context) error {
	c.mu.RLock()
	client := c.client
	subs := append([]Subscription(nil), c.spec.Subscriptions...)
	for _, s := range c.ephemeral {
		subs = append(subs, s)
	}
	c.mu.RUnlock()

	if client == nil {
		return nil
	}
	return client.Subscribe(ctx, subs)
}

// Publish sends a message and echoes it into the local view, so the UI shows
// what was sent even when the broker does not loop it back.
func (c *Conn) Publish(ctx context.Context, req PublishRequest) error {
	if err := ValidateTopic(req.Topic); err != nil {
		return err
	}
	if req.QoS > 2 {
		return errors.New("qos must be 0, 1 or 2")
	}

	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	if client == nil {
		return errors.New("connection is not connected")
	}
	if err := client.Publish(ctx, req); err != nil {
		return err
	}
	c.sent.Add(1)
	return nil
}

// ClearState empties the topic tree and message history.
func (c *Conn) ClearState() {
	c.tree.Clear()
	c.history.Clear()
}

func (c *Conn) events() Events {
	return Events{
		Message: c.handleMessage,
		Up: func(sessionPresent bool) {
			c.mu.Lock()
			c.status.SessionPresent = sessionPresent
			c.mu.Unlock()
			c.setState(StateConnected, nil)
		},
		Down: func(err error) {
			c.setState(StateError, err)
		},
		Error: func(err error) {
			c.mu.Lock()
			if err != nil {
				c.status.LastError = err.Error()
			}
			c.mu.Unlock()
		},
	}
}

func (c *Conn) handleMessage(m Message) {
	m.Seq = c.seq.Add(1)
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now()
	}
	c.received.Add(1)
	c.tree.Record(m)
	c.history.Add(m)
	c.mgr.emitMessage(m)
}

func (c *Conn) setState(state State, err error) {
	c.mu.Lock()
	if c.status.State != state {
		c.status.State = state
		c.status.Since = time.Now()
	}
	switch {
	case state == StateConnected:
		now := time.Now()
		c.status.ConnectedAt = &now
		c.status.LastError = ""
	case err != nil:
		c.status.LastError = err.Error()
	case state == StateDisconnected:
		c.status.ConnectedAt = nil
		c.status.LastError = ""
	}
	c.mu.Unlock()

	c.mgr.emitStatus(c.Status())
}
