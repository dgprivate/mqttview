// Package api wires mqttview's HTTP surface: the REST API, the WebSocket
// endpoint and the embedded single-page frontend.
package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/hub"
	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/plugin"
	"github.com/dgprivate/mqttview/internal/store"
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
	r.Use(s.securityHeaders)

	// In Home Assistant mode the Supervisor has already decided who this is,
	// so the session middleware is replaced wholesale and the local sign-in
	// routes below are never mounted. Keeping them would mean a login form
	// nobody has a password for, and an account nobody can recover.
	authenticate := s.auth.Middleware
	csrf := s.auth.CSRF
	if s.auth.IngressMode() {
		authenticate = s.auth.IngressMiddleware
		csrf = s.auth.IngressCSRF
	}

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", s.handleHealth)

		// Public auth endpoints. The login form needs to know which methods
		// are available before anyone is authenticated.
		r.Route("/auth", func(r chi.Router) {
			r.Get("/config", s.handleAuthConfig)

			if s.auth.IngressMode() {
				r.Group(func(r chi.Router) {
					r.Use(authenticate, csrf)
					r.Get("/me", s.handleMe)
				})
				return
			}

			// Login carries no session yet, so there is no CSRF token to
			// check; the rate limiter in the auth service is what protects it.
			r.Post("/login", s.handleLogin)
			r.Get("/sso/{provider}/start", s.handleSSOStart)
			r.Get("/sso/{provider}/callback", s.handleSSOCallback)

			// SAML. The assertion arrives as a cross-site POST from the
			// identity provider, so the ACS route cannot carry a CSRF token;
			// the signed assertion and the one-shot request ID are what make
			// it safe, and it lives outside the CSRF group for that reason.
			r.Get("/saml/{provider}/metadata", s.handleSAMLMetadata)
			r.Get("/saml/{provider}/start", s.handleSAMLStart)
			r.Post("/saml/{provider}/acs", s.handleSAMLACS)

			r.Group(func(r chi.Router) {
				r.Use(authenticate, csrf)
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
			r.Use(authenticate)

			// The WebSocket handshake is a GET, so CSRF does not apply; the
			// origin check inside the hub is what guards it.
			r.Get("/ws", s.handleWS)

			r.Group(func(r chi.Router) {
				r.Use(csrf)
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
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	frameAncestors, xFrameOptions := s.frameRules()

	csp := "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"font-src 'self' data:; " +
		"connect-src 'self' ws: wss:; " +
		"frame-ancestors " + frameAncestors + "; " +
		"base-uri 'self'; " +
		"form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", csp)
		// X-Frame-Options has no syntax for a list of origins, so it is set
		// only when it can say the same thing as the CSP. Where it cannot,
		// frame-ancestors is the one that counts and every browser mqttview
		// supports honours it.
		if xFrameOptions != "" {
			h.Set("X-Frame-Options", xFrameOptions)
		}
		next.ServeHTTP(w, r)
	})
}

// frameRules decides who may put mqttview in an iframe.
//
// Refusing everybody is right for a standalone install: a page that frames it
// can trick somebody into clicking a button they cannot see. It is exactly
// wrong for Home Assistant, where the panel *is* an iframe — served from Home
// Assistant's own origin under ingress, hence 'self'. Anything else has to be
// named by the operator, because it means trusting that site with the UI.
func (s *Server) frameRules() (csp, xFrameOptions string) {
	if len(s.cfg.FrameAncestors) > 0 {
		return strings.Join(s.cfg.FrameAncestors, " "), ""
	}
	if s.auth.IngressMode() {
		return "'self'", "SAMEORIGIN"
	}
	return "'none'", "DENY"
}

// OriginPatterns derives the allowed WebSocket origins from the configured
// base URL, so a browser on another site cannot open a socket with the user's
// cookies.
//
// In Home Assistant mode the origin is Home Assistant's, and mqttview has no
// way to know what that is: the same install is reached at a .local name, at a
// LAN address and through Nabu Casa, and all three are legitimate. Any pattern
// is returned instead, which is safe here only because every ingress request
// has already been proved to come from the Supervisor before it reaches this
// point — a browser on another site cannot get a request there at all.
func OriginPatterns(cfg config.Config) []string {
	if cfg.Auth.Mode == config.ModeIngress {
		return []string{"*"}
	}
	u, err := url.Parse(cfg.BaseURL)
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
