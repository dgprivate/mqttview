package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewjam/saml"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/store"
)

// SAML 2.0 support, alongside the OIDC in oidc.go.
//
// The two prove identity differently and agree on everything afterwards, so
// this file ends by handing off to resolveFederatedUser, which is the same
// account-provisioning policy OIDC uses.
//
// XML signature verification is not hand-written: crewjam/saml wraps
// goxmldsig, and signature handling on canonicalised XML is a place where a
// subtle mistake is a silent authentication bypass rather than a visible bug.

// samlCookie carries the in-flight request ID between the redirect and the
// assertion coming back.
const samlCookie = "mqttview_saml"

// samlSPKeyFile and samlSPCertFile hold the service provider's own keypair,
// generated on first use and kept in the data directory beside secret.key.
const (
	samlSPKeyFile  = "saml-sp.key"
	samlSPCertFile = "saml-sp.crt"
)

// samlProvider is a lazily-resolved SAML identity provider.
type samlProvider struct {
	id  string
	cfg config.SAMLProviderConfig
	sp  *saml.ServiceProvider
}

// samlState is what the cookie holds between the two legs of a login.
type samlState struct {
	Provider  string `json:"p"`
	RequestID string `json:"r"`
	Next      string `json:"n"`
	Expires   int64  `json:"e"`
}

// samlCache memoises resolved providers and the SP keypair, both of which cost
// a network fetch or a key generation to build.
type samlCache struct {
	mu        sync.Mutex
	providers map[string]*samlProvider
	keypair   *tls.Certificate
}

// SAMLProviderInfos lists the enabled SAML providers for the login page.
func (s *Service) SAMLProviderInfos() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(s.cfg.SAMLProviders))
	for id, p := range s.cfg.SAMLProviders {
		if !p.Enabled {
			continue
		}
		name := p.DisplayName
		if name == "" {
			name = id
		}
		out = append(out, ProviderInfo{ID: id, DisplayName: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// spKeypair returns the service provider's signing keypair, creating it on
// first use.
//
// A self-signed certificate is the right shape here: it is an identifier the
// IdP pins from our metadata, not something a CA vouches for. It is written
// with the same permissions as the encryption key, because it is one.
func (s *Service) spKeypair() (*tls.Certificate, error) {
	s.saml.mu.Lock()
	defer s.saml.mu.Unlock()

	if s.saml.keypair != nil {
		return s.saml.keypair, nil
	}

	keyPath := filepath.Join(s.dataDir, samlSPKeyFile)
	certPath := filepath.Join(s.dataDir, samlSPCertFile)

	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		s.saml.keypair = &pair
		return &pair, nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, fmt.Errorf("auth: generate SAML key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("auth: generate SAML serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mqttview SAML service provider"},
		NotBefore:    time.Now().Add(-time.Hour),
		// Long-lived on purpose: rotating it means re-registering with every
		// identity provider by hand, and its security comes from the private
		// key, not from an expiry date.
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("auth: create SAML certificate: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("auth: write SAML key: %w", err)
	}
	// The certificate is public — it is published in the metadata — but nothing
	// else on the host needs to read the file, so it gets the same treatment as
	// the key beside it.
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, fmt.Errorf("auth: write SAML certificate: %w", err)
	}
	s.log.Info("generated a SAML service provider keypair", "path", certPath)

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("auth: load the SAML keypair just written: %w", err)
	}
	s.saml.keypair = &pair
	return &pair, nil
}

// samlSP builds, or returns the cached, service provider for one identity
// provider.
func (s *Service) samlSP(ctx context.Context, id string) (*samlProvider, error) {
	cfg, ok := s.cfg.SAMLProviders[id]
	if !ok || !cfg.Enabled {
		return nil, fmt.Errorf("auth: no SAML provider named %q", id)
	}

	s.saml.mu.Lock()
	if p, ok := s.saml.providers[id]; ok {
		s.saml.mu.Unlock()
		return p, nil
	}
	s.saml.mu.Unlock()

	pair, err := s.spKeypair()
	if err != nil {
		return nil, err
	}
	idp, err := s.fetchIDPMetadata(ctx, cfg)
	if err != nil {
		return nil, err
	}

	metadataURL, err := url.Parse(s.baseURL + "/api/auth/saml/" + id + "/metadata")
	if err != nil {
		return nil, fmt.Errorf("auth: SAML metadata URL: %w", err)
	}
	acsURL, err := url.Parse(s.baseURL + "/api/auth/saml/" + id + "/acs")
	if err != nil {
		return nil, fmt.Errorf("auth: SAML ACS URL: %w", err)
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("auth: parse the SAML certificate: %w", err)
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("auth: the SAML keypair is not RSA")
	}

	sp := &saml.ServiceProvider{
		Key:         key,
		Certificate: leaf,
		MetadataURL: *metadataURL,
		AcsURL:      *acsURL,
		IDPMetadata: idp,
		EntityID:    cfg.EntityID,
		// Signed assertions are the point. An IdP that returns an unsigned one
		// is not proving anything.
		AuthnNameIDFormat: saml.EmailAddressNameIDFormat,
	}

	p := &samlProvider{id: id, cfg: cfg, sp: sp}
	s.saml.mu.Lock()
	if s.saml.providers == nil {
		s.saml.providers = map[string]*samlProvider{}
	}
	s.saml.providers[id] = p
	s.saml.mu.Unlock()
	return p, nil
}

// fetchIDPMetadata reads the identity provider's metadata from a URL or a file.
func (s *Service) fetchIDPMetadata(ctx context.Context, cfg config.SAMLProviderConfig) (*saml.EntityDescriptor, error) {
	var raw []byte

	switch {
	case cfg.MetadataFile != "":
		b, err := os.ReadFile(cfg.MetadataFile)
		if err != nil {
			return nil, fmt.Errorf("auth: read SAML metadata file: %w", err)
		}
		raw = b

	case cfg.MetadataURL != "":
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.MetadataURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("auth: fetch SAML metadata: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("auth: fetching SAML metadata returned %s", resp.Status)
		}
		// Bounded: metadata is a few kilobytes, and an endpoint that streams
		// forever should not be able to exhaust this process.
		b, err := readAllLimited(resp.Body, 2<<20)
		if err != nil {
			return nil, fmt.Errorf("auth: read SAML metadata: %w", err)
		}
		raw = b

	default:
		return nil, errors.New("auth: the SAML provider has no metadata_url or metadata_file")
	}

	var descriptor saml.EntityDescriptor
	if err := xml.Unmarshal(raw, &descriptor); err != nil {
		// Some providers publish an EntitiesDescriptor wrapping one entity.
		var entities saml.EntitiesDescriptor
		if err2 := xml.Unmarshal(raw, &entities); err2 != nil || len(entities.EntityDescriptors) == 0 {
			return nil, fmt.Errorf("auth: SAML metadata is not readable: %w", err)
		}
		descriptor = entities.EntityDescriptors[0]
	}
	if len(descriptor.IDPSSODescriptors) == 0 {
		return nil, errors.New("auth: the SAML metadata describes no identity provider")
	}
	return &descriptor, nil
}

// SAMLMetadata renders this service provider's metadata, which is what an
// identity provider is configured with.
func (s *Service) SAMLMetadata(ctx context.Context, id string) ([]byte, error) {
	p, err := s.samlSP(ctx, id)
	if err != nil {
		return nil, err
	}
	return xml.MarshalIndent(p.sp.Metadata(), "", "  ")
}

// StartSAML returns the URL to redirect the browser to, having stored the
// request ID it must come back with.
func (s *Service) StartSAML(w http.ResponseWriter, r *http.Request, id, next string) (string, error) {
	p, err := s.samlSP(r.Context(), id)
	if err != nil {
		return "", err
	}

	authn, err := p.sp.MakeAuthenticationRequest(
		p.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding),
		saml.HTTPRedirectBinding,
		saml.HTTPPostBinding,
	)
	if err != nil {
		return "", fmt.Errorf("auth: build the SAML request: %w", err)
	}

	payload, err := json.Marshal(samlState{
		Provider:  id,
		RequestID: authn.ID,
		Next:      sanitizeNext(next),
		Expires:   time.Now().Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	sealed, err := s.box.Seal(string(payload))
	if err != nil {
		return "", fmt.Errorf("auth: seal SAML state: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:   samlCookie,
		Value:  base64.RawURLEncoding.EncodeToString([]byte(sealed)),
		Path:   "/api/auth/saml/",
		MaxAge: 600,
		// Lax would drop the cookie: the assertion arrives as a cross-site
		// POST from the identity provider. None requires Secure, which is why
		// SAML needs HTTPS in a way the OIDC redirect flow does not.
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteNoneMode,
	})

	redirect, err := authn.Redirect("", p.sp)
	if err != nil {
		return "", fmt.Errorf("auth: build the SAML redirect: %w", err)
	}
	return redirect.String(), nil
}

// CompleteSAML validates a posted assertion and signs the user in.
func (s *Service) CompleteSAML(w http.ResponseWriter, r *http.Request, id string) (store.User, string, error) {
	cookie, err := r.Cookie(samlCookie)
	if err != nil {
		return store.User{}, "", errors.New("auth: login state cookie is missing or expired")
	}
	// One-shot, whatever happens next.
	http.SetCookie(w, &http.Cookie{
		Name: samlCookie, Value: "", Path: "/api/auth/saml/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteNoneMode,
	})

	rawSealed, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return store.User{}, "", errors.New("auth: login state cookie is malformed")
	}
	plain, err := s.box.Open(string(rawSealed))
	if err != nil {
		return store.User{}, "", errors.New("auth: login state cookie could not be decrypted")
	}
	var st samlState
	if err := json.Unmarshal([]byte(plain), &st); err != nil {
		return store.User{}, "", errors.New("auth: login state cookie is malformed")
	}
	if time.Now().Unix() > st.Expires {
		return store.User{}, "", errors.New("auth: login took too long, please try again")
	}
	if st.Provider != id {
		return store.User{}, "", errors.New("auth: login state does not match this provider")
	}

	p, err := s.samlSP(r.Context(), id)
	if err != nil {
		return store.User{}, "", err
	}

	// ParseResponse is what verifies the signature, the audience, the
	// conditions and the timestamps. Passing exactly one expected request ID
	// is what stops an assertion minted for a different login being replayed
	// into this one.
	assertion, err := p.sp.ParseResponse(r, []string{st.RequestID})
	if err != nil {
		var ive *saml.InvalidResponseError
		if errors.As(err, &ive) {
			s.log.Warn("SAML assertion rejected", "provider", id, "reason", ive.PrivateErr)
		}
		return store.User{}, "", errors.New("auth: the identity provider's response could not be verified")
	}

	subject, email, name := samlIdentity(assertion, p.cfg)
	if subject == "" {
		return store.User{}, "", errors.New("auth: the assertion carries no stable subject")
	}
	email = store.NormalizeEmail(email)
	if email == "" {
		return store.User{}, "", errors.New("auth: the assertion carries no email address")
	}
	if err := checkDomain(email, p.cfg.AllowedDomains); err != nil {
		return store.User{}, "", err
	}

	user, err := s.resolveFederatedUser(id, subject, email, name, p.cfg.AdminEmails)
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

// emailAttributeNames are the attribute names identity providers actually use
// for an email address, most specific first.
var emailAttributeNames = []string{
	"urn:oid:0.9.2342.19200300.100.1.3",                                  // LDAP mail, what Keycloak and Authentik send
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress", // Entra ID, ADFS
	"email", "mail", "emailAddress", "Email", "urn:oasis:names:tc:SAML:attribute:subject-id",
}

// nameAttributeNames are the same for a display name.
var nameAttributeNames = []string{
	"urn:oid:2.16.840.1.113730.3.1.241", // LDAP displayName
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
	"displayName", "name", "cn", "givenName",
}

// samlIdentity pulls the subject, email and display name out of an assertion.
//
// The subject is the NameID when there is one, because that is what the IdP
// promises is stable. Falling back to the email address is a deliberate second
// choice: it is stable enough to link an account by, and an IdP that sends no
// NameID gives nothing better.
func samlIdentity(a *saml.Assertion, cfg config.SAMLProviderConfig) (subject, email, name string) {
	attrs := map[string]string{}
	for _, stmt := range a.AttributeStatements {
		for _, attr := range stmt.Attributes {
			for _, v := range attr.Values {
				if v.Value == "" {
					continue
				}
				if attr.Name != "" {
					attrs[attr.Name] = v.Value
				}
				if attr.FriendlyName != "" {
					attrs[attr.FriendlyName] = v.Value
				}
				break // first non-empty value wins
			}
		}
	}

	email = pickAttribute(attrs, cfg.EmailAttribute, emailAttributeNames)
	name = pickAttribute(attrs, cfg.NameAttribute, nameAttributeNames)

	if a.Subject != nil && a.Subject.NameID != nil {
		subject = strings.TrimSpace(a.Subject.NameID.Value)
	}
	if email == "" && strings.Contains(subject, "@") {
		email = subject
	}
	if subject == "" {
		subject = email
	}
	return subject, email, name
}

// readAllLimited reads at most n bytes, so a metadata endpoint that streams
// forever cannot exhaust this process.
func readAllLimited(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

// pickAttribute prefers the configured name, then the well-known ones.
func pickAttribute(attrs map[string]string, configured string, candidates []string) string {
	if configured != "" {
		return strings.TrimSpace(attrs[configured])
	}
	for _, k := range candidates {
		if v := strings.TrimSpace(attrs[k]); v != "" {
			return v
		}
	}
	return ""
}
