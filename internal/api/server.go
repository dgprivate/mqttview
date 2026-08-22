// Package api wires mqttview's HTTP surface: the REST API, the WebSocket
// endpoint and the embedded single-page frontend.
package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/hub"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/plugin"
	"github.com/mqttview/mqttview/internal/store"
)

// Server holds every dependency the handlers need.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	db      *store.Store
	auth    *auth.Service
	mqtt    *mqttc.Manager
	hub     *hub.Hub
	plugins *plugin.Runtime
	web     fs.FS
	// version is reported by /api/health and shown in the UI footer.
	version string
}

// Options bundles the Server constructor arguments.
type Options struct {
	Config  config.Config
	Log     *slog.Logger
	Store   *store.Store
	Auth    *auth.Service
	MQTT    *mqttc.Manager
	Hub     *hub.Hub
	Plugins *plugin.Runtime
	// Web is the built frontend. Nil disables static serving, which is what
	// happens during `npm run dev` with the Vite proxy in front.
	Web     fs.FS
	Version string
}

// New builds a Server.
func New(o Options) *Server {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:     o.Config,
		log:     log,
		db:      o.Store,
		auth:    o.Auth,
		mqtt:    o.MQTT,
		hub:     o.Hub,
		plugins: o.Plugins,
		web:     o.Web,
		version: o.Version,
	}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// Deliberately not middleware.RealIP: it rewrites RemoteAddr from headers
	// any client can send, whether or not a proxy actually sets them, and the
	// rewritten value is what the sign-in rate limit is keyed on. Forwarded
	// headers are consulted only when the operator has said a proxy is in
	// front — see auth.Service.ClientIP and config.TrustProxyHeaders.
	r.Use(s.requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(securityHeaders)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", s.handleHealth)

		// Public auth endpoints. The login form needs to know which methods
		// are available before anyone is authenticated.
		r.Route("/auth", func(r chi.Router) {
			r.Get("/config", s.handleAuthConfig)
			// Login carries no session yet, so there is no CSRF token to
			// check; the rate limiter in the auth service is what protects it.
			r.Post("/login", s.handleLogin)
			r.Get("/sso/{provider}/start", s.handleSSOStart)
			r.Get("/sso/{provider}/callback", s.handleSSOCallback)

			r.Group(func(r chi.Router) {
				r.Use(s.auth.Middleware, s.auth.CSRF)
				r.Get("/me", s.handleMe)
				r.Post("/logout", s.handleLogout)
				r.Post("/password", s.handleChangePassword)

				// Two-factor, all scoped to the signed-in account. An admin
				// resetting somebody else's goes through /users instead.
				r.Get("/2fa", s.handleTwoFactorStatus)
				r.Post("/2fa/enrol", s.handleTwoFactorEnrol)
				r.Post("/2fa/confirm", s.handleTwoFactorConfirm)
				r.Post("/2fa/disable", s.handleTwoFactorDisable)
				r.Post("/2fa/recovery-codes", s.handleRegenerateRecoveryCodes)
			})
		})

		// Everything below requires a session.
		r.Group(func(r chi.Router) {
			r.Use(s.auth.Middleware)

			// The WebSocket handshake is a GET, so CSRF does not apply; the
			// origin check inside the hub is what guards it.
			r.Get("/ws", s.handleWS)

			r.Group(func(r chi.Router) {
				r.Use(s.auth.CSRF)
				s.mountConnections(r)
				s.mountUsers(r)
				s.mountPlugins(r)

				// Plugin-owned routes. Plugins inherit authentication; they
				// are responsible for their own role checks beyond viewer.
				r.Handle("/p/{pluginID}/*", s.plugins.Handler())
				r.Handle("/p/{pluginID}", s.plugins.Handler())
			})
		})

		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			httpx.WriteError(w, http.StatusNotFound, "no such API endpoint")
		})
	})

	if s.web != nil {
		r.Handle("/*", s.spaHandler())
	}
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"version":     s.version,
		"connections": len(s.mqtt.List()),
		"wsClients":   s.hub.Clients(),
	})
}

// requestLogger logs one line per request at debug level, and warns on 5xx.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration", time.Since(start).Round(time.Millisecond).String(),
		}
		if ww.Status() >= 500 {
			s.log.Error("request failed", attrs...)
		} else {
			s.log.Debug("request", attrs...)
		}
	})
}

// securityHeaders sets the defensive headers that apply to every response.
// The CSP is strict because the SPA ships no inline scripts; 'unsafe-inline'
// on styles is needed for the CSS-in-JS the build emits.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"font-src 'self' data:; "+
				"connect-src 'self' ws: wss:; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// OriginPatterns derives the allowed WebSocket origins from the configured
// base URL, so a browser on another site cannot open a socket with the user's
// cookies.
func OriginPatterns(baseURL string) []string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil
	}
	return []string{u.Host}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.hub.Serve(w, r)
}

// opCtx bounds an MQTT operation, which can otherwise block indefinitely on an
// unreachable broker.
func opCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// opCtxBackground is opCtx for work that outlives the request that started it.
func opCtxBackground(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
