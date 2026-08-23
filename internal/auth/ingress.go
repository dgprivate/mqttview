package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/store"
)

// Home Assistant mode.
//
// When mqttview runs as a Home Assistant add-on it is not reachable from a
// browser at all. The Supervisor's ingress proxy is the only thing that can
// open the port, and it does so only after Home Assistant has authenticated
// the person and decided they may see this panel. mqttview then has a choice:
// ask for a second password of its own, or believe the proxy. Asking again is
// not more secure — it is one more credential to lose — so it believes the
// proxy, and everything below is about making sure it really is the proxy.
//
// The identity headers are the Supervisor's, added when it forwards a request.
// Older Supervisor versions send none, which is why there is a configured
// fallback rather than a failure: an add-on that stops working after a Home
// Assistant downgrade is worse than one that says "everybody is this account".

const (
	// IngressUserIDHeader and friends are what the Supervisor adds to a
	// forwarded ingress request. They are read in order of preference: an ID
	// is stable across a rename, a username is not, and a display name is a
	// label rather than an identity.
	//
	// Exported because they are a contract with something outside this
	// program, and a test that hard-codes the string instead would still pass
	// after somebody changed it here.
	IngressUserIDHeader      = "X-Remote-User-Id"
	IngressUserNameHeader    = "X-Remote-User-Name"
	IngressUserDisplayHeader = "X-Remote-User-Display-Name"

	// IngressPathHeader is the prefix the panel is served under, e.g.
	// /api/hassio_ingress/<token>. The Supervisor strips it before proxying,
	// so mqttview sees ordinary paths and only needs it to build links.
	IngressPathHeader = "X-Ingress-Path"

	// ingressProvider is the provider ID stored against accounts created from
	// a Home Assistant identity, so they are recognisable in the user list and
	// cannot collide with a local or SSO account.
	ingressProvider = "homeassistant"

	// ingressEmailDomain is what a Home Assistant identity is given for an
	// address. Home Assistant has no concept of a user's email, and mqttview
	// keys accounts by address, so one is synthesised. ".local" cannot be
	// registered, so this can never collide with somebody's real address.
	ingressEmailDomain = "homeassistant.local"
)

// IngressMode reports whether Home Assistant decides who may use mqttview.
func (s *Service) IngressMode() bool {
	return s.cfg.Mode == config.ModeIngress
}

// ingressIdentity is who the Supervisor says is making a request.
type ingressIdentity struct {
	// Subject is the stable key an account is linked to.
	Subject string
	// Username is the Home Assistant login name, when it was sent.
	Username string
	// DisplayName is what Home Assistant shows for the person.
	DisplayName string
}

// IngressMiddleware authenticates a request the way Home Assistant mode does:
// by checking where it came from, then reading who the Supervisor says it is.
func (s *Service) IngressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.AuthenticateIngress(r)
		if err != nil {
			// 403 and not 401: there is no credential the caller could supply
			// to change this answer, so prompting for one would be a lie.
			httpx.WriteError(w, http.StatusForbidden, err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// AuthenticateIngress resolves the account behind an ingress request.
func (s *Service) AuthenticateIngress(r *http.Request) (store.User, error) {
	if err := s.checkIngressSource(r); err != nil {
		return store.User{}, err
	}

	id, err := s.ingressIdentityOf(r)
	if err != nil {
		return store.User{}, err
	}
	return s.resolveIngressUser(id)
}

// checkIngressSource is the one check the whole mode rests on. Identity
// headers are worth exactly as much as the guarantee that only the Supervisor
// could have set them.
func (s *Service) checkIngressSource(r *http.Request) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return fmt.Errorf("auth: cannot tell where %q came from", r.RemoteAddr)
	}

	for _, trusted := range s.cfg.Ingress.TrustedProxies {
		if _, network, err := net.ParseCIDR(trusted); err == nil {
			if network.Contains(ip) {
				return nil
			}
			continue
		}
		if parsed := net.ParseIP(trusted); parsed != nil && parsed.Equal(ip) {
			return nil
		}
	}

	// Deliberately specific: an operator who has published the port by mistake
	// needs to see which address was refused, and the log line is the only
	// place that will tell them.
	s.log.Warn("an ingress request did not come from a trusted proxy",
		"remote", ip.String(), "trusted", s.cfg.Ingress.TrustedProxies)
	return errors.New("auth: this request did not come through Home Assistant")
}

func (s *Service) ingressIdentityOf(r *http.Request) (ingressIdentity, error) {
	id := ingressIdentity{
		Subject:     strings.TrimSpace(r.Header.Get(IngressUserIDHeader)),
		Username:    strings.TrimSpace(r.Header.Get(IngressUserNameHeader)),
		DisplayName: strings.TrimSpace(r.Header.Get(IngressUserDisplayHeader)),
	}
	if id.Subject == "" {
		// A username without an ID still identifies somebody, and it is what
		// some Supervisor versions send.
		id.Subject = id.Username
	}
	if id.Subject != "" {
		return id, nil
	}

	if s.cfg.Ingress.FallbackUser != "" {
		return ingressIdentity{
			Subject:     "fallback:" + s.cfg.Ingress.FallbackUser,
			Username:    s.cfg.Ingress.FallbackUser,
			DisplayName: s.cfg.Ingress.FallbackUser,
		}, nil
	}

	return ingressIdentity{}, errors.New(
		"auth: Home Assistant did not say who you are; set ingress.fallback_user " +
			"if your Supervisor is too old to send it")
}

// resolveIngressUser finds or creates the mqttview account for a Home
// Assistant identity.
//
// Unlike SSO, this does not honour allow_signup. Home Assistant has already
// decided this person may see the panel, and refusing them here would mean an
// add-on that installs cleanly and then locks its owner out.
func (s *Service) resolveIngressUser(id ingressIdentity) (store.User, error) {
	role := s.ingressRoleFor(id)

	u, err := s.store.GetUserByProviderSubject(ingressProvider, id.Subject)
	switch {
	case err == nil:
		if u.Disabled {
			return store.User{}, errors.New("auth: this account is disabled in mqttview")
		}
		// The role is re-applied on every request, so moving somebody in or
		// out of ingress.admin_users takes effect on their next page load
		// rather than after an account is deleted by hand.
		if u.Role != role {
			u.Role = role
			if err := s.store.UpdateUser(u); err != nil {
				return store.User{}, err
			}
		}
		return u, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.User{}, err
	}

	name := id.DisplayName
	if name == "" {
		name = id.Username
	}
	if name == "" {
		name = "Home Assistant user"
	}

	return s.store.CreateUser(store.User{
		ID:              uuid.NewString(),
		Email:           ingressEmail(id),
		Name:            name,
		Role:            role,
		Provider:        ingressProvider,
		ProviderSubject: id.Subject,
	})
}

func (s *Service) ingressRoleFor(id ingressIdentity) store.Role {
	for _, admin := range s.cfg.Ingress.AdminUsers {
		admin = strings.TrimSpace(admin)
		if admin == "" {
			continue
		}
		if strings.EqualFold(admin, id.Subject) || strings.EqualFold(admin, id.Username) {
			return store.RoleAdmin
		}
	}
	role := store.Role(s.cfg.Ingress.DefaultRole)
	if !store.ValidRole(role) {
		return store.RoleViewer
	}
	return role
}

// ingressEmail builds the address an account is stored under. It is derived
// from the stable subject rather than the username so that renaming somebody
// in Home Assistant does not strand their mqttview account.
func ingressEmail(id ingressIdentity) string {
	local := sanitizeEmailLocal(strings.TrimPrefix(id.Subject, "fallback:"))
	if local == "" {
		local = "user"
	}
	return local + "@" + ingressEmailDomain
}

// sanitizeEmailLocal keeps the characters an address can hold. Home Assistant
// user IDs are hex, but a username can be anything a person typed.
func sanitizeEmailLocal(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// IngressCSRF is the double-submit check, with the cookie issued on the way
// past rather than at sign-in.
//
// There is no sign-in here, so nothing else would ever set the cookie. The
// check is kept because the panel is an iframe on a page mqttview does not
// control, and a token the page had to read from its own cookie is what
// distinguishes our frontend from anything else that guesses the URL.
func (s *Service) IngressCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if _, err := r.Cookie(CSRFCookie); err != nil {
				s.setIngressCSRFCookie(w)
			}
			next.ServeHTTP(w, r)
			return
		}
		s.CSRF(next).ServeHTTP(w, r)
	})
}

// setIngressCSRFCookie issues the token the frontend echoes back.
//
// SameSite=None with Secure, because the panel is an iframe inside Home
// Assistant: a Lax cookie is not sent with a request from a framed document,
// so the check would fail every write. Home Assistant serves the panel over
// whatever scheme it is reached on, so on a plain-HTTP install the browser
// will drop a Secure cookie and writes will be refused — which is the correct
// failure, and the reason the documentation says to use HTTPS.
func (s *Service) setIngressCSRFCookie(w http.ResponseWriter) {
	token, err := randomToken(32)
	if err != nil {
		s.log.Error("generating a CSRF token failed", "error", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int((24 * time.Hour).Seconds()),
		HttpOnly: false, // read by the SPA and echoed in CSRFHeader
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
	})
}

// ingressPathPattern is what an ingress prefix may contain.
//
// An allow-list rather than a list of things to reject. The value ends up
// inside an HTML attribute, and the difference between the two approaches is
// whether a character nobody thought of is refused or passed through: this
// accepts the characters a Home Assistant ingress path is actually built from
// and nothing else. Escaping still happens at the point of use; this is what
// makes the escaping a second line rather than the only one.
var ingressPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+/?$`)

// IngressPath returns the prefix the panel is served under, or "".
//
// The value arrives in a header, and a header is only as trustworthy as the
// hop that set it — so it is read only in ingress mode, where the source has
// already been checked, and it is still constrained to something that can only
// be a path. "//evil.example.com" would otherwise repoint every URL in the
// page at another origin.
func IngressPath(r *http.Request) string {
	raw := r.Header.Get(IngressPathHeader)
	if !ingressPathPattern.MatchString(raw) {
		return ""
	}
	return strings.TrimRight(raw, "/")
}
