// Package auth handles who may use mqttview: local username+password
// accounts, optional OIDC single sign-on, cookie sessions and role checks.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/secrets"
	"github.com/mqttview/mqttview/internal/store"
)

// Cookie names. The CSRF cookie is deliberately readable by JavaScript: the
// SPA echoes it back in a header, which is what proves same-origin.
const (
	SessionCookie = "mqttview_session"
	CSRFCookie    = "mqttview_csrf"
	CSRFHeader    = "X-CSRF-Token"
	oidcCookie    = "mqttview_oidc"
)

// ErrInvalidCredentials is returned for any failed login, regardless of
// whether the account exists, so the API cannot be used to enumerate users.
var ErrInvalidCredentials = errors.New("invalid email or password")

// ErrUnauthorized means no valid session was presented.
var ErrUnauthorized = errors.New("not authenticated")

// ErrForbidden means the session lacks the required role.
var ErrForbidden = errors.New("insufficient permissions")

// ErrLocalLoginDisabled is returned when password login is switched off.
var ErrLocalLoginDisabled = errors.New("password login is disabled on this server")

type contextKey int

const userContextKey contextKey = iota

// Service issues and validates sessions.
type Service struct {
	store   *store.Store
	cfg     config.AuthConfig
	baseURL string
	secure  bool
	box     *secrets.Box
	log     *slog.Logger

	limiter *attemptLimiter

	mu        sync.RWMutex
	providers map[string]*ssoProvider
}

// New builds the auth service. OIDC providers are resolved lazily on first
// use so that an unreachable issuer cannot block startup.
func New(st *store.Store, cfg config.Config, box *secrets.Box, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:     st,
		cfg:       cfg.Auth,
		baseURL:   cfg.BaseURL,
		secure:    strings.HasPrefix(strings.ToLower(cfg.BaseURL), "https://"),
		box:       box,
		log:       log,
		limiter:   newAttemptLimiter(10, 15*time.Minute),
		providers: map[string]*ssoProvider{},
	}
}

// Config exposes the auth configuration, e.g. for the login page.
func (s *Service) Config() config.AuthConfig { return s.cfg }

// Login verifies local credentials and returns the user.
func (s *Service) Login(ctx context.Context, email, password, clientIP string) (store.User, error) {
	if !s.cfg.AllowLocal {
		return store.User{}, ErrLocalLoginDisabled
	}
	key := strings.ToLower(strings.TrimSpace(email)) + "|" + clientIP
	if !s.limiter.allow(key) {
		return store.User{}, errors.New("too many failed attempts, try again later")
	}

	u, err := s.store.GetUserByEmail(email)
	if err != nil {
		// Spend roughly the same time as a real verification so that a
		// missing account is not detectable by response latency.
		_, _ = VerifyPassword(dummyHash, password)
		return store.User{}, ErrInvalidCredentials
	}
	if u.Disabled || u.PasswordHash == "" {
		_, _ = VerifyPassword(dummyHash, password)
		return store.User{}, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(u.PasswordHash, password)
	if err != nil {
		s.log.Warn("stored password hash is unusable", "user", u.ID, "error", err)
		return store.User{}, ErrInvalidCredentials
	}
	if !ok {
		return store.User{}, ErrInvalidCredentials
	}

	s.limiter.reset(key)
	if err := s.store.TouchLogin(u.ID); err != nil {
		s.log.Warn("recording login time failed", "user", u.ID, "error", err)
	}
	return u, nil
}

// IssueSession creates a session and writes the session and CSRF cookies.
func (s *Service) IssueSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	token, err := randomToken(32)
	if err != nil {
		return err
	}
	ttl := time.Duration(s.cfg.SessionTTLHours) * time.Hour
	expires := time.Now().Add(ttl)

	if err := s.store.CreateSession(store.Session{
		ID:        hashToken(token),
		UserID:    u.ID,
		ExpiresAt: expires,
		UserAgent: truncate(r.UserAgent(), 255),
		IP:        clientIP(r),
	}); err != nil {
		return err
	}

	csrf, err := randomToken(32)
	if err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    csrf,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: false, // read by the SPA and echoed in CSRFHeader
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Logout deletes the session and clears the cookies.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(hashToken(c.Value)); err != nil {
			s.log.Warn("deleting session failed", "error", err)
		}
	}
	s.clearCookie(w, SessionCookie, true)
	s.clearCookie(w, CSRFCookie, false)
	return nil
}

func (s *Service) clearCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: httpOnly,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Authenticate resolves the session cookie on a request.
func (s *Service) Authenticate(r *http.Request) (store.User, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return store.User{}, ErrUnauthorized
	}
	_, u, err := s.store.SessionUser(hashToken(c.Value))
	if err != nil {
		return store.User{}, ErrUnauthorized
	}
	return u, nil
}

// Middleware rejects unauthenticated requests and puts the user in the
// request context.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Authenticate(r)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// RequireRole returns middleware enforcing a minimum role.
func (s *Service) RequireRole(min store.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := UserFrom(r.Context())
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "not authenticated")
				return
			}
			if !u.Role.AtLeast(min) {
				httpx.WriteError(w, http.StatusForbidden, fmt.Sprintf("this action requires the %s role", min))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRF enforces the double-submit cookie pattern on state-changing requests.
// Read-only verbs pass through untouched.
func (s *Service) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(CSRFCookie)
		header := r.Header.Get(CSRFHeader)
		if err != nil || cookie.Value == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
			httpx.WriteError(w, http.StatusForbidden, "CSRF token missing or invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithUser stores a user in a context.
func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userContextKey, u)
}

// UserFrom retrieves the authenticated user from a context.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userContextKey).(store.User)
	return u, ok
}

// BootstrapAdmin creates the initial admin account when the database has no
// users. It returns the generated password when one had to be invented, so
// the operator can be told it exactly once on stdout.
func (s *Service) BootstrapAdmin(email, password string) (created bool, generated string, err error) {
	n, err := s.store.CountUsers()
	if err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "", nil
	}
	if email == "" {
		email = "admin@localhost"
	}
	if password == "" {
		password, err = randomToken(12)
		if err != nil {
			return false, "", err
		}
		generated = password
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, "", err
	}
	if _, err := s.store.CreateUser(store.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         "Administrator",
		PasswordHash: hash,
		Role:         store.RoleAdmin,
		Provider:     "local",
	}); err != nil {
		return false, "", err
	}
	return true, generated, nil
}

// PurgeSessions is called periodically to drop expired rows.
func (s *Service) PurgeSessions() {
	if n, err := s.store.PurgeExpiredSessions(); err != nil {
		s.log.Warn("purging expired sessions failed", "error", err)
	} else if n > 0 {
		s.log.Debug("purged expired sessions", "count", n)
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is what gets stored: possession of the database alone does not
// yield a usable session cookie.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func clientIP(r *http.Request) string {
	// Behind a reverse proxy the first XFF entry is the closest thing to the
	// real client. It is advisory only: used for rate limiting and display.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// dummyHash is a real argon2id hash of a random value, used to keep failed
// logins as expensive as successful ones.
var dummyHash = func() string {
	h, err := HashPassword("mqttview-timing-equalizer-value")
	if err != nil {
		return ""
	}
	return h
}()

// attemptLimiter is a small fixed-window limiter for login attempts.
type attemptLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	entries map[string]*attemptEntry
}

type attemptEntry struct {
	count int
	start time.Time
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, entries: map[string]*attemptEntry{}}
}

func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	// Opportunistically drop stale entries so the map cannot grow without
	// bound under a distributed guessing attempt.
	for k, e := range l.entries {
		if now.Sub(e.start) > l.window {
			delete(l.entries, k)
		}
	}

	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) > l.window {
		l.entries[key] = &attemptEntry{count: 1, start: now}
		return true
	}
	e.count++
	return e.count <= l.max
}

func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}
