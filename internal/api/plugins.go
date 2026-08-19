package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/store"
)

func (s *Server) mountPlugins(r chi.Router) {
	r.Route("/plugins", func(r chi.Router) {
		// Everyone can see which plugins are on, because that determines
		// which pages the UI shows.
		r.Get("/", s.handleListPlugins)
		r.Get("/{id}", s.handleGetPlugin)

		r.Group(func(r chi.Router) {
			r.Use(s.auth.RequireRole(store.RoleAdmin))
			r.Put("/{id}/enabled", s.handleSetPluginEnabled)
			r.Put("/{id}/settings", s.handleSavePluginSettings)
		})
	})
}

func (s *Server) handleListPlugins(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, s.plugins.List())
}

func (s *Server) handleGetPlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	info, ok := s.plugins.Get(id)
	if !ok {
		httpx.WriteErrorf(w, http.StatusNotFound, "no plugin named %q", id)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

type setEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleSetPluginEnabled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req setEnabledRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := opCtx(r, 30*time.Second)
	defer cancel()

	if err := s.plugins.SetEnabled(ctx, id, req.Enabled); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, _ := s.plugins.Get(id)
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (s *Server) handleSavePluginSettings(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var settings map[string]any
	if err := httpx.DecodeJSON(w, r, &settings); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := opCtx(r, 30*time.Second)
	defer cancel()

	if err := s.plugins.SaveSettings(ctx, id, settings); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, _ := s.plugins.Get(id)
	httpx.WriteJSON(w, http.StatusOK, info)
}
