package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/store"
)

// ProviderInfo is the public description of an SSO provider, safe to show on
// the login page.
type ProviderInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// ssoProvider is a lazily-resolved OIDC provider.
type ssoProvider struct {
	id       string
	cfg      config.ProviderConfig
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// ProviderInfos lists the enabled SSO providers.
func (s *Service) ProviderInfos() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(s.cfg.Providers))
	for id, p := range s.cfg.Providers {
		if !p.Enabled {
			continue
		}
		name := p.DisplayName
		if name == "" {
			name = strings.ToUpper(id[:1]) + id[1:]
		}
		out = append(out, ProviderInfo{ID: id, DisplayName: name})
	}
	return out
}

// provider resolves and caches a provider's OIDC discovery document.
func (s *Service) provider(ctx context.Context, id string) (*ssoProvider, error) {
	s.mu.RLock()
	p, ok := s.providers[id]
	s.mu.RUnlock()
	if ok {
		return p, nil
	}

	pc, ok := s.cfg.Providers[id]
	if !ok || !pc.Enabled {
		return nil, fmt.Errorf("auth: SSO provider %q is not enabled", id)
	}

	// Discovery is a network call, so it is bounded and done outside the lock.
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(dctx, pc.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC discovery for %q failed: %w", id, err)
	}

	scopes := pc.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}

	sp := &ssoProvider{
		id:       id,
		cfg:      pc,
		verifier: provider.Verifier(&oidc.Config{ClientID: pc.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     pc.ClientID,
			ClientSecret: pc.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  s.baseURL + "/api/auth/sso/" + id + "/callback",
			Scopes:       scopes,
		},
	}

	s.mu.Lock()
	s.providers[id] = sp
	s.mu.Unlock()
	return sp, nil
}

// oidcState is the short-lived value carried in a cookie across the redirect
// to the provider. Keeping it client-side means SSO works with several server
// replicas behind a load balancer without shared storage.
type oidcState struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

// StartSSO returns the URL the browser should be redirected to, after storing
// the matching state in a cookie.
func (s *Service) StartSSO(w http.ResponseWriter, r *http.Request, id, next string) (string, error) {
	sp, err := s.provider(r.Context(), id)
	if err != nil {
		return "", err
	}

	state, err := randomToken(24)
	if err != nil {
		return "", err
	}
	nonce, err := randomToken(24)
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()

	payload, err := json.Marshal(oidcState{
		Provider: id,
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		Next:     sanitizeNext(next),
		Expires:  time.Now().Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	// Sealing rather than signing keeps the PKCE verifier confidential even if
	// the cookie leaks into a log or a browser extension.
	sealed, err := s.box.Seal(string(payload))
	if err != nil {
		return "", fmt.Errorf("auth: seal SSO state: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(sealed)),
		Path:     "/api/auth/sso/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})

	return sp.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), nil
}

// CompleteSSO validates the callback and returns the logged-in user plus the
// path the browser should land on.
func (s *Service) CompleteSSO(w http.ResponseWriter, r *http.Request, id string) (store.User, string, error) {
	cookie, err := r.Cookie(oidcCookie)
	if err != nil {
		return store.User{}, "", errors.New("auth: login state cookie is missing or expired")
	}
	// One-shot: clear it whatever happens next.
	http.SetCookie(w, &http.Cookie{
		Name: oidcCookie, Value: "", Path: "/api/auth/sso/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})

	rawSealed, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return store.User{}, "", errors.New("auth: login state cookie is malformed")
	}
	plain, err := s.box.Open(string(rawSealed))
	if err != nil {
		return store.User{}, "", errors.New("auth: login state cookie could not be decrypted")
	}
	var st oidcState
	if err := json.Unmarshal([]byte(plain), &st); err != nil {
		return store.User{}, "", errors.New("auth: login state cookie is malformed")
	}
	if time.Now().Unix() > st.Expires {
		return store.User{}, "", errors.New("auth: login took too long, please try again")
	}
	if st.Provider != id {
		return store.User{}, "", errors.New("auth: login state does not match this provider")
	}
	if q := r.URL.Query().Get("state"); q == "" || q != st.State {
		return store.User{}, "", errors.New("auth: login state mismatch")
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		desc := r.URL.Query().Get("error_description")
		if desc != "" {
			return store.User{}, "", fmt.Errorf("auth: provider rejected the login: %s (%s)", errMsg, desc)
		}
		return store.User{}, "", fmt.Errorf("auth: provider rejected the login: %s", errMsg)
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return store.User{}, "", errors.New("auth: provider returned no authorization code")
	}

	sp, err := s.provider(r.Context(), id)
	if err != nil {
		return store.User{}, "", err
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	token, err := sp.oauth.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return store.User{}, "", fmt.Errorf("auth: exchanging the authorization code failed: %w", err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return store.User{}, "", errors.New("auth: provider returned no ID token")
	}
	idToken, err := sp.verifier.Verify(ctx, rawID)
	if err != nil {
		return store.User{}, "", fmt.Errorf("auth: ID token verification failed: %w", err)
	}
	if idToken.Nonce != st.Nonce {
		return store.User{}, "", errors.New("auth: ID token nonce mismatch")
	}

	var claims struct {
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return store.User{}, "", fmt.Errorf("auth: reading ID token claims failed: %w", err)
	}
	if claims.Subject == "" {
		return store.User{}, "", errors.New("auth: ID token has no subject claim")
	}
	email := store.NormalizeEmail(claims.Email)
	if email == "" {
		return store.User{}, "", errors.New("auth: provider did not return an email address")
	}
	// Treat a missing email_verified as unverified: accepting it would let a
	// provider that allows arbitrary email claims impersonate any account.
	if claims.EmailVerified == nil || !*claims.EmailVerified {
		return store.User{}, "", errors.New("auth: the email address on this account is not verified")
	}
	if err := checkDomain(email, sp.cfg.AllowedDomains); err != nil {
		return store.User{}, "", err
	}

	user, err := s.resolveSSOUser(sp, claims.Subject, email, claims.Name)
	if err != nil {
		return store.User{}, "", err
	}
	if err := s.store.TouchLogin(user.ID); err != nil {
		s.log.Warn("recording login time failed", "user", user.ID, "error", err)
	}
	if err := s.IssueSession(w, r, user); err != nil {
		return store.User{}, "", err
	}

	next := st.Next
	if next == "" {
		next = "/"
	}
	return user, next, nil
}

// resolveSSOUser maps a verified SSO identity to an mqttview account,
// creating or linking one when policy allows.
func (s *Service) resolveSSOUser(sp *ssoProvider, subject, email, name string) (store.User, error) {
	return s.resolveFederatedUser(sp.id, subject, email, name, sp.cfg.AdminEmails)
}

// resolveFederatedUser maps a verified identity from any protocol to an
// mqttview account, creating or linking one when policy allows. OIDC and SAML
// differ in how they prove who somebody is and agree on everything after that,
// so the agreeing part lives here once.
func (s *Service) resolveFederatedUser(providerID, subject, email, name string, adminEmails []string) (store.User, error) {
	if u, err := s.store.GetUserByProviderSubject(providerID, subject); err == nil {
		if u.Disabled {
			return store.User{}, errors.New("auth: this account is disabled")
		}
		return u, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}

	// An existing local account with the same verified address is the same
	// person, so link rather than creating a duplicate.
	if u, err := s.store.GetUserByEmail(email); err == nil {
		if u.Disabled {
			return store.User{}, errors.New("auth: this account is disabled")
		}
		if u.ProviderSubject != "" && u.ProviderSubject != subject {
			return store.User{}, errors.New("auth: this email is already linked to a different identity")
		}
		if err := s.store.LinkProvider(u.ID, providerID, subject); err != nil {
			return store.User{}, err
		}
		u.Provider = providerID
		u.ProviderSubject = subject
		return u, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}

	if !s.cfg.AllowSignup {
		return store.User{}, errors.New("auth: no account exists for this address; ask an administrator to create one")
	}

	role := store.RoleViewer
	for _, admin := range adminEmails {
		if store.NormalizeEmail(admin) == email {
			role = store.RoleAdmin
			break
		}
	}
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}

	return s.store.CreateUser(store.User{
		ID:              uuid.NewString(),
		Email:           email,
		Name:            name,
		Role:            role,
		Provider:        providerID,
		ProviderSubject: subject,
	})
}

func checkDomain(email string, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	_, domain, _ := strings.Cut(email, "@")
	for _, d := range allowed {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return nil
		}
	}
	return fmt.Errorf("auth: the domain %q is not allowed to sign in", domain)
}

// sanitizeNext keeps post-login redirects on this origin: an attacker-supplied
// absolute URL would turn the login endpoint into an open redirect.
func sanitizeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}
