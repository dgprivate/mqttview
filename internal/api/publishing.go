package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/store"
)

// payloadView carries a stored payload to the browser the same way the topic
// history does: text when it is valid UTF-8, base64 when it is not. A payload
// worth sending again is quite often not text.
type payloadView struct {
	Payload string `json:"payload"`
	Base64  bool   `json:"payloadBase64,omitempty"`
}

func encodePayload(raw []byte) payloadView {
	if utf8.Valid(raw) {
		return payloadView{Payload: string(raw)}
	}
	return payloadView{Payload: base64.StdEncoding.EncodeToString(raw), Base64: true}
}

// decodePayload is the reverse, and is where a malformed base64 body is
// refused rather than being published as its own literal text.
func decodePayload(text string, isBase64 bool) ([]byte, error) {
	if !isBase64 {
		return []byte(text), nil
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, errors.New("payload is not valid base64")
	}
	return raw, nil
}

type publishHistoryView struct {
	store.PublishRecord
	payloadView
}

// handlePublishHistory lists what has been sent on this connection.
func (s *Server) handlePublishHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	records, err := s.db.PublishHistory(c.Spec().ID, r.URL.Query().Get("q"), intParam(r, "limit", 100))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]publishHistoryView, len(records))
	for i, rec := range records {
		out[i] = publishHistoryView{PublishRecord: rec, payloadView: encodePayload(rec.Payload)}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// handleClearPublishHistory forgets what this connection has sent.
func (s *Server) handleClearPublishHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	if err := s.db.ClearPublishHistory(c.Spec().ID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRepublish sends a previous message again, exactly as it was sent.
//
// The payload comes from the stored bytes rather than from anything the
// browser sends back, so "send that again" means the same message and not
// whatever survived a round trip through a text field.
func (s *Server) handleRepublish(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	id, err := strconv.ParseInt(chi.URLParam(r, "publishID"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "the publish id must be a number")
		return
	}

	rec, err := s.db.GetPublish(c.Spec().ID, id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "no such entry in this connection's publish history")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.publishAndRecord(w, r, c, mqttc.PublishRequest{
		Topic:   rec.Topic,
		Payload: rec.Payload,
		QoS:     rec.QoS,
		Retain:  rec.Retain,
	}, "republish")
}

type savedMessageRequest struct {
	Folder         string `json:"folder"`
	Name           string `json:"name"`
	Topic          string `json:"topic"`
	Payload        string `json:"payload"`
	PayloadEncoded bool   `json:"payloadBase64"`
	QoS            byte   `json:"qos"`
	Retain         bool   `json:"retain"`
	SortOrder      int    `json:"sortOrder"`
	// Global keeps the message out of any one connection's scope, so a
	// collection built against staging can be used against production.
	Global bool `json:"global"`
}

type savedMessageView struct {
	store.SavedMessage
	payloadView
}

// handleListSaved returns the collection that applies to this connection: the
// messages saved against it, and the ones saved against no connection at all.
func (s *Server) handleListSaved(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	messages, err := s.db.SavedMessages(c.Spec().ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]savedMessageView, len(messages))
	for i, m := range messages {
		out[i] = savedMessageView{SavedMessage: m, payloadView: encodePayload(m.Payload)}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSaved(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	var req savedMessageRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := decodePayload(req.Payload, req.PayloadEncoded)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := mqttc.ValidateTopic(req.Topic); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	user, _ := auth.UserFrom(r.Context())
	msg := store.SavedMessage{
		ID:        uuid.NewString(),
		Folder:    req.Folder,
		Name:      req.Name,
		Topic:     req.Topic,
		Payload:   payload,
		QoS:       req.QoS,
		Retain:    req.Retain,
		SortOrder: req.SortOrder,
		CreatedBy: user.ID,
	}
	if !req.Global {
		msg.ConnectionID = c.Spec().ID
	}

	if err := s.db.SaveMessage(msg); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated,
		savedMessageView{SavedMessage: msg, payloadView: encodePayload(payload)})
}

func (s *Server) handleUpdateSaved(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.conn(w, r); !ok {
		return
	}

	existing, ok := s.savedMessage(w, r)
	if !ok {
		return
	}

	var req savedMessageRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := decodePayload(req.Payload, req.PayloadEncoded)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := mqttc.ValidateTopic(req.Topic); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing.Folder = req.Folder
	existing.Name = req.Name
	existing.Topic = req.Topic
	existing.Payload = payload
	existing.QoS = req.QoS
	existing.Retain = req.Retain
	existing.SortOrder = req.SortOrder

	if err := s.db.SaveMessage(existing); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK,
		savedMessageView{SavedMessage: existing, payloadView: encodePayload(payload)})
}

func (s *Server) handleDeleteSaved(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.conn(w, r); !ok {
		return
	}
	if _, ok := s.savedMessage(w, r); !ok {
		return
	}

	if err := s.db.DeleteSavedMessage(chi.URLParam(r, "savedID")); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePublishSaved sends a saved message on this connection.
func (s *Server) handlePublishSaved(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	msg, ok := s.savedMessage(w, r)
	if !ok {
		return
	}

	s.publishAndRecord(w, r, c, mqttc.PublishRequest{
		Topic:   msg.Topic,
		Payload: msg.Payload,
		QoS:     msg.QoS,
		Retain:  msg.Retain,
	}, "publish saved")
}

// savedMessage loads the saved message named in the URL and refuses one that
// belongs to a different connection — an id from another broker's collection
// is not reachable by guessing it here.
func (s *Server) savedMessage(w http.ResponseWriter, r *http.Request) (store.SavedMessage, bool) {
	connID := chi.URLParam(r, "id")

	msg, err := s.db.GetSavedMessage(chi.URLParam(r, "savedID"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "no such saved message")
		return store.SavedMessage{}, false
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return store.SavedMessage{}, false
	}
	if msg.ConnectionID != "" && msg.ConnectionID != connID {
		httpx.WriteError(w, http.StatusNotFound, "no such saved message")
		return store.SavedMessage{}, false
	}
	return msg, true
}

// publishAndRecord is the one path everything that sends a message goes
// through, so that the history and the log cannot record one thing while a
// different message goes out.
func (s *Server) publishAndRecord(w http.ResponseWriter, r *http.Request, c *mqttc.Conn, req mqttc.PublishRequest, why string) {
	ctx, cancel := opCtx(r, 20*time.Second)
	defer cancel()

	if err := c.Publish(ctx, req); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	user, _ := auth.UserFrom(r.Context())
	s.log.Info(why, "user", user.Email, "connection", c.Spec().Name,
		"topic", req.Topic, "qos", req.QoS, "retain", req.Retain, "bytes", len(req.Payload))

	// The history is a convenience. Failing to write it is worth a line in the
	// log, but the message has already gone: reporting an error now would say
	// the publish failed when it did not.
	if err := s.db.RecordPublish(store.PublishRecord{
		ConnectionID: c.Spec().ID,
		UserID:       user.ID,
		Username:     user.Email,
		Topic:        req.Topic,
		Payload:      req.Payload,
		QoS:          req.QoS,
		Retain:       req.Retain,
	}, 0); err != nil {
		s.log.Warn("recording the publish failed; the message was still sent",
			"connection", c.Spec().ID, "topic", req.Topic, "error", err)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
