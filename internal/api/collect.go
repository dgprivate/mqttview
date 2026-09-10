package api

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/mqttc"
)

// Bounds on a collection window. The ceiling is not arbitrary: a caller that
// wants ten minutes of traffic wants a recording, not a request held open, and
// the topic log is the thing that answers that.
const (
	defaultCollectSeconds = 10
	maxCollectSeconds     = 120
	defaultCollectLimit   = 500
	maxCollectLimit       = 5000
)

// collector accumulates matching messages for the length of one window.
//
// It is bounded, and it counts what it could not keep rather than quietly
// keeping the first N. A caller that asked for a busy filter and got exactly
// its limit back has no way to tell a complete answer from a truncated one
// unless it is told.
type collector struct {
	connectionID string
	filter       string
	limit        int

	mu       sync.Mutex
	messages []mqttc.Message
	dropped  int
}

func (c *collector) add(m mqttc.Message) {
	if m.ConnectionID != c.connectionID || !mqttc.MatchFilter(c.filter, m.Topic) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) >= c.limit {
		c.dropped++
		return
	}
	// The manager hands the same slice to every observer, so this keeps its
	// own copy rather than a window onto a buffer that will be reused.
	m.Payload = append([]byte(nil), m.Payload...)
	c.messages = append(c.messages, m)
}

func (c *collector) result() ([]mqttc.Message, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.messages, c.dropped
}

// handleCollect watches a connection for a fixed window and returns everything
// that arrived on it.
//
// This exists for callers that hold no broker credentials — an agent over MCP,
// a script, a person with curl. mqttview already holds the connection, the
// certificates and the password, so answering "show me what this broker does
// for the next ten seconds" needs none of them to be handed out.
//
// The subscription is ephemeral and lasts the window: asking to watch
// something must not quietly rewrite the connection's stored subscriptions,
// and it must not leave a filter subscribed after the request that wanted it
// has gone.
func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	seconds, err := strictInt(r, "seconds", defaultCollectSeconds, 1, maxCollectSeconds)
	if err != nil {
		httpx.WriteErrorf(w, http.StatusBadRequest,
			"%v; for a window longer than %d seconds, read the topic history instead", err, maxCollectSeconds)
		return
	}

	limit, err := strictInt(r, "limit", defaultCollectLimit, 1, maxCollectLimit)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	filter := r.URL.Query().Get("filter")
	if filter == "" {
		filter = "#"
	}
	if err := mqttc.ValidateFilter(filter); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if c.Status().State != mqttc.StateConnected {
		httpx.WriteError(w, http.StatusConflict,
			"the connection is not connected, so nothing would arrive during the window")
		return
	}

	col := &collector{connectionID: c.Spec().ID, filter: filter, limit: limit}

	// Observe first, subscribe second. The other order loses whatever arrives
	// between the broker accepting the subscription and the observer existing,
	// which on a retained topic is the very first thing that comes back.
	stop := s.mqtt.AddObserver(mqttc.Observer{OnMessage: col.add})
	defer stop()

	subscribed, err := s.holdFilter(r, c, filter)
	if err != nil {
		httpx.WriteErrorf(w, http.StatusBadGateway, "subscribing to %q failed: %v", filter, err)
		return
	}
	if subscribed {
		defer s.releaseFilter(c, filter)
	}

	started := time.Now()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-r.Context().Done():
		// The caller gave up or the window was cut short. What was collected
		// so far is still an honest answer for the time it covers.
	}

	messages, dropped := col.result()
	out := make([]historyEntry, len(messages))
	topics := make(map[string]int, len(messages))
	for i, m := range messages {
		out[i] = toHistoryEntry(m)
		topics[m.Topic]++
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"filter":   filter,
		"started":  started,
		"ended":    time.Now(),
		"seconds":  time.Since(started).Seconds(),
		"messages": out,
		"topics":   topics,
		// Reported rather than implied: a caller that got exactly its limit
		// back cannot otherwise tell a complete answer from a truncated one.
		"dropped":   dropped,
		"truncated": dropped > 0,
	})
}

// holdFilter adds an ephemeral subscription for the duration of a request,
// unless the connection already carries one that covers it.
//
// Windows overlap: two agents watching the same filter at once is the normal
// case, not a rare one, so the leases are counted. Without that the first to
// finish unsubscribes the filter out from under the second, which then reports
// an honest and completely wrong "nothing happened".
func (s *Server) holdFilter(r *http.Request, c *mqttc.Conn, filter string) (bool, error) {
	for _, existing := range c.Spec().Subscriptions {
		if existing.Filter == filter {
			return false, nil
		}
	}

	key := c.Spec().ID + "\x00" + filter
	s.leaseMu.Lock()
	s.leases[key]++
	first := s.leases[key] == 1
	s.leaseMu.Unlock()

	if !first {
		return true, nil
	}

	ctx, cancel := opCtx(r, 20*time.Second)
	defer cancel()

	if err := c.SubscribeEphemeral(ctx, []mqttc.Subscription{{Filter: filter, QoS: 0}}); err != nil {
		s.leaseMu.Lock()
		s.leases[key]--
		s.leaseMu.Unlock()
		return false, err
	}
	return true, nil
}

// releaseFilter drops a lease, and the subscription with it when it was the
// last one.
func (s *Server) releaseFilter(c *mqttc.Conn, filter string) {
	key := c.Spec().ID + "\x00" + filter
	s.leaseMu.Lock()
	s.leases[key]--
	last := s.leases[key] <= 0
	if last {
		delete(s.leases, key)
	}
	s.leaseMu.Unlock()

	if !last {
		return
	}

	// A fresh context: the request's own is already cancelled by the time this
	// runs, and unsubscribing is not something to skip because the caller
	// hung up.
	ctx, cancel := opCtxBackground(20 * time.Second)
	defer cancel()

	if err := c.UnsubscribeEphemeral(ctx, []string{filter}); err != nil {
		s.log.Warn("dropping a collection's subscription failed",
			"connection", c.Spec().ID, "filter", filter, "error", err)
	}
}

// strictInt reads a bounded integer parameter.
//
// Unlike intParam it refuses a value it cannot honour rather than substituting
// the default. An agent that asks for a window of nought seconds and is given
// ten has been overruled without being told, and will report the ten seconds
// of traffic as though it had asked for them.
func strictInt(r *http.Request, name string, def, min, max int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, not %q", name, raw)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%s must be between %d and %d, not %d", name, min, max, v)
	}
	return v, nil
}
