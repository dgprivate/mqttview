package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/logbuf"
	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/store"
)

// clearRetainedRequest names one topic. One, not a filter: clearing a retained
// message destroys state that other clients depend on and cannot be undone,
// and a wildcard turns a single mistake into a hundred of them. Somebody who
// really means to clear a subtree can say so a topic at a time, and will have
// seen each one named.
type clearRetainedRequest struct {
	Topic string `json:"topic"`
}

// handleClearRetained removes a broker's retained message for one topic.
//
// The mechanism is in the protocol rather than in an API: publishing a
// zero-length payload with the retain flag set tells the broker to forget what
// it was holding. That means this is an ordinary publish as far as the broker
// is concerned, and it is destructive in the way a publish is not — the value
// is gone for every client that connects afterwards, and mqttview cannot put
// it back.
//
// So it needs the operator role, it is written to the audit log with the
// account that did it, and the UI names the topic before it will send it.
func (s *Server) handleClearRetained(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	var req clearRetainedRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// ValidateTopic rejects wildcards, which is the check that matters here:
	// a retain-clear published to "home/#" is not a bulk delete, it is an
	// error, and some brokers would take it as a literal topic name.
	if err := mqttc.ValidateTopic(req.Topic); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Reported, not enforced. The tree only knows what this connection has
	// seen, and a broker may be holding a retained message on a topic nobody
	// here has subscribed to; refusing on that basis would make the feature
	// useless exactly when it is needed.
	value, known := c.Tree().Value(req.Topic)
	hadRetained := known && value.Retain

	ctx, cancel := opCtx(r, 20*time.Second)
	defer cancel()

	// A zero-length payload, retained. The empty slice is the whole message.
	if err := c.Publish(ctx, mqttc.PublishRequest{
		Topic:   req.Topic,
		Payload: []byte{},
		QoS:     1,
		Retain:  true,
	}); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	user, _ := auth.UserFrom(r.Context())
	s.log.Warn("cleared a retained message",
		"user", user.Email, "connection", c.Spec().Name, "topic", req.Topic,
		"hadRetainedValue", hadRetained)

	detail := "no retained value was known to mqttview"
	if hadRetained {
		detail = "cleared a retained value of " + byteCount(value.Size)
	}
	if err := s.db.Audit(store.AuditEntry{
		UserID:   user.ID,
		Username: user.Email,
		Action:   "clear retained",
		Target:   c.Spec().Name + " " + req.Topic,
		Detail:   detail,
	}, 0); err != nil {
		// The broker has already forgotten the value. Reporting a failure now
		// would say nothing happened, which is the opposite of the truth.
		s.log.Error("the retained message was cleared but the audit entry was not written",
			"user", user.Email, "topic", req.Topic, "error", err)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"topic":            req.Topic,
		"hadRetainedValue": hadRetained,
	})
}

// handleAuditLog returns what has been done. Admin only: it names accounts and
// what they did, which is not a viewer's business.
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	entries, err := s.db.AuditLog(intParam(r, "limit", 100))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	httpx.WriteJSON(w, http.StatusOK, entries)
}

// byteCount renders a size for a human reading a log line.
func byteCount(n int) string {
	switch {
	case n == 1:
		return "1 byte"
	case n < 1024:
		return strconv.Itoa(n) + " bytes"
	default:
		return strconv.Itoa(n/1024) + " KiB"
	}
}

// handleLogs returns the recent log records.
//
// Admin only. Log lines name accounts, topics and broker hostnames, and the
// point of the view is to see what went wrong — which is exactly the material
// that should not be readable by everyone with a session.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable,
			"this build keeps no log buffer, so there is nothing to show")
		return
	}

	level, ok := logbuf.ParseLevel(r.URL.Query().Get("level"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "level must be debug, info, warn or error")
		return
	}

	var since uint64
	if raw := r.URL.Query().Get("since"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "since must be a sequence number from an earlier read")
			return
		}
		since = v
	}

	records := s.logs.Records(level, since, intParam(r, "limit", 500))
	if records == nil {
		records = []logbuf.Record{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"records": records,
		// The newest sequence number whether or not anything matched the
		// filter, so a poll that saw nothing still moves forward instead of
		// re-reading the same records at the next level change.
		"seq": s.logs.Seq(),
	})
}
