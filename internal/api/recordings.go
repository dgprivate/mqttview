package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/store"
)

type recordedView struct {
	store.RecordedMessage
	payloadView
}

// handleRecordings reads back what a connection recorded.
func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	q := store.RecordedQuery{
		ConnectionID: c.Spec().ID,
		Topic:        r.URL.Query().Get("topic"),
		Limit:        intParam(r, "limit", 500),
	}
	var err error
	if q.Since, err = timeParam(r, "since"); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.Until, err = timeParam(r, "until"); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	messages, err := s.db.Recorded(q)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]recordedView, len(messages))
	for i, m := range messages {
		out[i] = recordedView{RecordedMessage: m, payloadView: encodePayload(m.Payload)}
	}

	resp := map[string]any{"messages": out, "recording": c.Spec().RecordToDisk}
	stats, err := s.db.RecordingStats(c.Spec().ID)
	if err == nil {
		resp["stats"] = stats
	}
	// What the recorder could not keep. A recording with a hole nobody is
	// told about is worse than no recording: it gets read as evidence that
	// nothing happened.
	if s.recorder != nil {
		resp["recorder"] = s.recorder.Stats()
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// handleDeleteRecordings forgets everything a connection recorded.
func (s *Server) handleDeleteRecordings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	stats, _ := s.db.RecordingStats(c.Spec().ID)
	if err := s.db.DeleteRecorded(c.Spec().ID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	user, _ := auth.UserFrom(r.Context())
	s.log.Warn("deleted recordings", "user", user.Email, "connection", c.Spec().Name, "rows", stats.Rows)
	if err := s.db.Audit(store.AuditEntry{
		UserID:   user.ID,
		Username: user.Email,
		Action:   "delete recordings",
		Target:   c.Spec().Name,
		Detail:   byteCount(int(stats.Bytes)) + " over " + itoaInt64(stats.Rows) + " messages",
	}, 0); err != nil {
		s.log.Error("the recordings were deleted but the audit entry was not written", "error", err)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "deleted": stats.Rows})
}

// timeParam reads an RFC 3339 timestamp, and refuses one it cannot read rather
// than silently treating the window as unbounded — which would answer a
// question about Tuesday with everything ever recorded.
func timeParam(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%s must be an RFC 3339 timestamp such as 2026-09-10T14:00:00Z", name)
	}
	return t, nil
}

// itoaInt64 renders a row count for a log line.
func itoaInt64(n int64) string { return strconv.FormatInt(n, 10) }
