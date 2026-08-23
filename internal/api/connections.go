package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/store"
)

func (s *Server) mountConnections(r chi.Router) {
	r.Route("/connections", func(r chi.Router) {
		r.Get("/", s.handleListConnections)
		r.With(s.auth.RequireRole(store.RoleAdmin)).Post("/", s.handleCreateConnection)

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleGetConnection)
			r.With(s.auth.RequireRole(store.RoleAdmin)).Put("/", s.handleUpdateConnection)
			r.With(s.auth.RequireRole(store.RoleAdmin)).Delete("/", s.handleDeleteConnection)

			r.With(s.auth.RequireRole(store.RoleOperator)).Post("/connect", s.handleConnect)
			r.With(s.auth.RequireRole(store.RoleOperator)).Post("/disconnect", s.handleDisconnect)
			r.With(s.auth.RequireRole(store.RoleOperator)).Post("/publish", s.handlePublish)
			r.With(s.auth.RequireRole(store.RoleOperator)).Post("/subscribe", s.handleSubscribe)
			r.With(s.auth.RequireRole(store.RoleOperator)).Post("/unsubscribe", s.handleUnsubscribe)
			r.With(s.auth.RequireRole(store.RoleOperator)).Post("/clear", s.handleClearState)

			r.Get("/tree", s.handleTree)
			r.Get("/topic", s.handleTopic)
			r.Get("/messages", s.handleMessages)
			r.Get("/search", s.handleSearch)
		})
	})
}

// connectionView is the client-facing shape of a connection. It never carries
// the broker password or private key: those are write-only from the API's
// point of view.
type connectionView struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	URL            string               `json:"url"`
	Version        string               `json:"version"`
	ClientID       string               `json:"clientId"`
	Username       string               `json:"username"`
	HasPassword    bool                 `json:"hasPassword"`
	KeepAlive      int                  `json:"keepAlive"`
	CleanStart     bool                 `json:"cleanStart"`
	SessionExpiry  uint32               `json:"sessionExpiry"`
	ConnectTimeout int                  `json:"connectTimeout"`
	TLS            tlsView              `json:"tls"`
	Will           *mqttc.Will          `json:"will,omitempty"`
	Subscriptions  []mqttc.Subscription `json:"subscriptions"`
	AutoConnect    bool                 `json:"autoConnect"`
	HistorySize    int                  `json:"historySize"`
	Status         mqttc.Status         `json:"status"`
	Topics         int                  `json:"topics"`
	TreeFull       bool                 `json:"treeFull"`
}

// tlsView mirrors mqttc.TLSSpec but reports only whether secret material is
// present, never the material itself.
type tlsView struct {
	InsecureSkipVerify bool     `json:"insecureSkipVerify"`
	ServerName         string   `json:"serverName,omitempty"`
	MinVersion         string   `json:"minVersion,omitempty"`
	ALPN               []string `json:"alpn,omitempty"`
	HasCA              bool     `json:"hasCa"`
	HasClientCert      bool     `json:"hasClientCert"`
}

func viewOf(c *mqttc.Conn) connectionView {
	spec := c.Spec()
	topics, _, full := c.Tree().Stats()
	return connectionView{
		ID:             spec.ID,
		Name:           spec.Name,
		URL:            spec.URL,
		Version:        spec.Version.String(),
		ClientID:       spec.ClientID,
		Username:       spec.Username,
		HasPassword:    spec.Password != "",
		KeepAlive:      spec.KeepAlive,
		CleanStart:     spec.CleanStart,
		SessionExpiry:  spec.SessionExpiry,
		ConnectTimeout: spec.ConnectTimeout,
		TLS: tlsView{
			InsecureSkipVerify: spec.TLS.InsecureSkipVerify,
			ServerName:         spec.TLS.ServerName,
			MinVersion:         spec.TLS.MinVersion,
			ALPN:               spec.TLS.ALPN,
			HasCA:              spec.TLS.CAPEM != "",
			HasClientCert:      spec.TLS.ClientCertPEM != "",
		},
		Will:          spec.Will,
		Subscriptions: spec.Subscriptions,
		AutoConnect:   spec.AutoConnect,
		HistorySize:   spec.HistorySize,
		Status:        c.Status(),
		Topics:        topics,
		TreeFull:      full,
	}
}

// connectionRequest is the write shape. Secret fields are optional on update:
// omitting them keeps whatever is already stored.
type connectionRequest struct {
	Name          string               `json:"name"`
	URL           string               `json:"url"`
	Version       string               `json:"version"`
	ClientID      string               `json:"clientId"`
	Username      string               `json:"username"`
	Password      *string              `json:"password"`
	KeepAlive     int                  `json:"keepAlive"`
	CleanStart    bool                 `json:"cleanStart"`
	SessionExpiry uint32               `json:"sessionExpiry"`
	TLS           tlsRequest           `json:"tls"`
	Will          *mqttc.Will          `json:"will"`
	Subscriptions []mqttc.Subscription `json:"subscriptions"`
	AutoConnect   bool                 `json:"autoConnect"`
	HistorySize   int                  `json:"historySize"`
}

type tlsRequest struct {
	InsecureSkipVerify bool     `json:"insecureSkipVerify"`
	ServerName         string   `json:"serverName"`
	MinVersion         string   `json:"minVersion"`
	ALPN               []string `json:"alpn"`
	CAPEM              *string  `json:"caPem"`
	ClientCertPEM      *string  `json:"clientCertPem"`
	ClientKeyPEM       *string  `json:"clientKeyPem"`
}

// toSpec builds a spec from the request, carrying forward secrets from prev
// when the client omitted them.
func (req connectionRequest) toSpec(id string, prev *mqttc.ConnectionSpec) (mqttc.ConnectionSpec, error) {
	version, err := mqttc.ParseVersion(req.Version)
	if err != nil {
		return mqttc.ConnectionSpec{}, err
	}

	spec := mqttc.ConnectionSpec{
		ID:            id,
		Name:          req.Name,
		URL:           req.URL,
		Version:       version,
		ClientID:      req.ClientID,
		Username:      req.Username,
		KeepAlive:     req.KeepAlive,
		CleanStart:    req.CleanStart,
		SessionExpiry: req.SessionExpiry,
		Will:          req.Will,
		Subscriptions: req.Subscriptions,
		AutoConnect:   req.AutoConnect,
		HistorySize:   req.HistorySize,
		TLS: mqttc.TLSSpec{
			InsecureSkipVerify: req.TLS.InsecureSkipVerify,
			ServerName:         req.TLS.ServerName,
			MinVersion:         req.TLS.MinVersion,
			ALPN:               req.TLS.ALPN,
		},
	}

	carry := func(supplied *string, previous string) string {
		if supplied != nil {
			return *supplied
		}
		return previous
	}
	if prev != nil {
		spec.Password = carry(req.Password, prev.Password)
		spec.TLS.CAPEM = carry(req.TLS.CAPEM, prev.TLS.CAPEM)
		spec.TLS.ClientCertPEM = carry(req.TLS.ClientCertPEM, prev.TLS.ClientCertPEM)
		spec.TLS.ClientKeyPEM = carry(req.TLS.ClientKeyPEM, prev.TLS.ClientKeyPEM)
	} else {
		spec.Password = carry(req.Password, "")
		spec.TLS.CAPEM = carry(req.TLS.CAPEM, "")
		spec.TLS.ClientCertPEM = carry(req.TLS.ClientCertPEM, "")
		spec.TLS.ClientKeyPEM = carry(req.TLS.ClientKeyPEM, "")
	}

	if err := spec.Normalize(); err != nil {
		return mqttc.ConnectionSpec{}, err
	}
	return spec, nil
}

func (s *Server) handleListConnections(w http.ResponseWriter, _ *http.Request) {
	conns := s.mqtt.List()
	out := make([]connectionView, 0, len(conns))
	for _, c := range conns {
		out = append(out, viewOf(c))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// conn resolves the {id} URL parameter, writing a 404 when it is unknown.
func (s *Server) conn(w http.ResponseWriter, r *http.Request) (*mqttc.Conn, bool) {
	id := chi.URLParam(r, "id")
	c, ok := s.mqtt.Get(id)
	if !ok {
		httpx.WriteErrorf(w, http.StatusNotFound, "no connection with id %q", id)
		return nil, false
	}
	return c, true
}

func (s *Server) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewOf(c))
}

func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireRole(w, r, store.RoleAdmin)
	if !ok {
		return
	}

	var req connectionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	spec, err := req.toSpec(uuid.NewString(), nil)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.db.SaveConnection(store.ConnectionRecord{Spec: spec, CreatedBy: user.ID}); err != nil {
		s.log.Error("saving connection failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not save the connection")
		return
	}

	ctx, cancel := opCtx(r, 30*time.Second)
	defer cancel()

	c, err := s.mqtt.Upsert(ctx, spec)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if spec.AutoConnect {
		go s.connectInBackground(spec.ID)
	}
	httpx.WriteJSON(w, http.StatusCreated, viewOf(c))
}

func (s *Server) handleUpdateConnection(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	var req connectionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	prev := c.Spec()
	spec, err := req.toSpec(prev.ID, &prev)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	rec, err := s.db.GetConnection(spec.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("loading connection failed", "id", spec.ID, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load the connection")
		return
	}
	rec.Spec = spec
	if err := s.db.SaveConnection(rec); err != nil {
		s.log.Error("saving connection failed", "id", spec.ID, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not save the connection")
		return
	}

	ctx, cancel := opCtx(r, 30*time.Second)
	defer cancel()

	updated, err := s.mqtt.Upsert(ctx, spec)
	if err != nil {
		// The definition is saved; only re-establishing the session failed.
		httpx.WriteErrorf(w, http.StatusBadGateway, "saved, but reconnecting failed: %s", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewOf(updated))
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	ctx, cancel := opCtx(r, 15*time.Second)
	defer cancel()

	if err := s.mqtt.Remove(ctx, id); err != nil && !errors.Is(err, mqttc.ErrNotFound) {
		s.log.Warn("disconnecting during delete failed", "id", id, "error", err)
	}
	if err := s.db.DeleteConnection(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteErrorf(w, http.StatusNotFound, "no connection with id %q", id)
			return
		}
		s.log.Error("deleting connection failed", "id", id, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete the connection")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	ctx, cancel := opCtx(r, 30*time.Second)
	defer cancel()

	if err := s.mqtt.Connect(ctx, c.Spec().ID); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	// A newly connected broker needs any enabled plugin's filters applied.
	s.plugins.ApplyAll(ctx)
	httpx.WriteJSON(w, http.StatusOK, viewOf(c))
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	ctx, cancel := opCtx(r, 15*time.Second)
	defer cancel()

	if err := s.mqtt.Disconnect(ctx, c.Spec().ID); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewOf(c))
}

// publishRequest accepts either a UTF-8 string payload or base64 for binary.
type publishRequest struct {
	Topic          string              `json:"topic"`
	Payload        string              `json:"payload"`
	PayloadEncoded bool                `json:"payloadBase64"`
	QoS            byte                `json:"qos"`
	Retain         bool                `json:"retain"`
	Props          *mqttc.MessageProps `json:"props,omitempty"`
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	var req publishRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	payload := []byte(req.Payload)
	if req.PayloadEncoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Payload)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "payload is not valid base64")
			return
		}
		payload = decoded
	}

	ctx, cancel := opCtx(r, 20*time.Second)
	defer cancel()

	if err := c.Publish(ctx, mqttc.PublishRequest{
		Topic:   req.Topic,
		Payload: payload,
		QoS:     req.QoS,
		Retain:  req.Retain,
		Props:   req.Props,
	}); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	user, _ := auth.UserFrom(r.Context())
	s.log.Info("publish", "user", user.Email, "connection", c.Spec().Name,
		"topic", req.Topic, "qos", req.QoS, "retain", req.Retain, "bytes", len(payload))
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type subscribeRequest struct {
	Subscriptions []mqttc.Subscription `json:"subscriptions"`
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	var req subscribeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := opCtx(r, 20*time.Second)
	defer cancel()

	if err := c.Subscribe(ctx, req.Subscriptions); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.persistSubscriptions(c)
	httpx.WriteJSON(w, http.StatusOK, viewOf(c))
}

type unsubscribeRequest struct {
	Filters []string `json:"filters"`
}

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	var req unsubscribeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := opCtx(r, 20*time.Second)
	defer cancel()

	if err := c.Unsubscribe(ctx, req.Filters); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.persistSubscriptions(c)
	httpx.WriteJSON(w, http.StatusOK, viewOf(c))
}

// persistSubscriptions saves the live subscription list so it survives a
// restart. Failure is logged but not fatal: the live session is already right.
func (s *Server) persistSubscriptions(c *mqttc.Conn) {
	spec := c.Spec()
	rec, err := s.db.GetConnection(spec.ID)
	if err != nil {
		s.log.Warn("persisting subscriptions failed", "connection", spec.ID, "error", err)
		return
	}
	rec.Spec = spec
	if err := s.db.SaveConnection(rec); err != nil {
		s.log.Warn("persisting subscriptions failed", "connection", spec.ID, "error", err)
	}
}

func (s *Server) handleClearState(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	c.ClearState()
	httpx.WriteJSON(w, http.StatusOK, viewOf(c))
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	children, found := c.Tree().Children(prefix)
	if !found {
		httpx.WriteErrorf(w, http.StatusNotFound, "no topics under %q", prefix)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"prefix":   prefix,
		"children": children,
	})
}

func (s *Server) handleTopic(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		httpx.WriteError(w, http.StatusBadRequest, "the topic parameter is required")
		return
	}
	node, found := c.Tree().Node(topic)
	if !found {
		httpx.WriteErrorf(w, http.StatusNotFound, "no data for topic %q", topic)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, node)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	limit := intParam(r, "limit", 200)
	filter := r.URL.Query().Get("filter")
	if filter != "" {
		if err := mqttc.ValidateFilter(filter); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, c.History().Recent(limit, filter))
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	limit := intParam(r, "limit", 200)

	// A query containing wildcards is treated as an MQTT filter, which is
	// what a user typing "home/+/temp" means.
	if q != "" && (containsWildcard(q)) {
		if err := mqttc.ValidateFilter(q); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, c.Tree().Match(q, limit))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c.Tree().Search(q, limit))
}

func (s *Server) connectInBackground(id string) {
	ctx, cancel := opCtxBackground(30 * time.Second)
	defer cancel()
	if err := s.mqtt.Connect(ctx, id); err != nil {
		s.log.Warn("auto-connect after create failed", "connection", id, "error", err)
		return
	}
	s.plugins.ApplyAll(ctx)
}

func containsWildcard(s string) bool {
	for _, r := range s {
		if r == '+' || r == '#' {
			return true
		}
	}
	return false
}

func intParam(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}
