package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/store"
)

// fakeIDP is a minimal OIDC provider: discovery, a JWKS and a token endpoint
// that mints a real signed ID token. Everything the verifier checks is checked
// against a genuine signature, so a test cannot pass by accident.
type fakeIDP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	keyID  string
	claims map[string]any
	// nonce is captured from the authorisation request and echoed into the
	// token, which is what the verifier insists on.
	nonce string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{key: key, keyID: "test-key", claims: map[string]any{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/authorize",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: idp.keyID, Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"access_token": "at", "token_type": "Bearer", "id_token": idp.idToken(t),
		})
	})

	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *fakeIDP) idToken(t *testing.T) string {
	t.Helper()

	claims := map[string]any{
		"iss":   i.srv.URL,
		"aud":   "client-id",
		"sub":   "subject-1",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": i.nonce,
	}
	for k, v := range i.claims {
		claims[k] = v
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.keyID))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := josejwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func withProvider(idp *fakeIDP) func(*config.Config) {
	return func(c *config.Config) {
		c.Auth.AllowSignup = true
		c.Auth.Providers = map[string]config.ProviderConfig{
			"test": {
				Enabled: true, DisplayName: "Test IdP",
				Issuer: idp.srv.URL, ClientID: "client-id", ClientSecret: "client-secret",
			},
		}
	}
}

// startSSO drives the first leg and returns the state cookie plus the query
// values the provider was asked to redirect back with.
func startSSO(t *testing.T, svc *Service, next string) (*http.Cookie, url.Values) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/test/start", nil)

	redirect, err := svc.StartSSO(rec, req, "test", next)
	if err != nil {
		t.Fatalf("StartSSO: %v", err)
	}
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("StartSSO set no state cookie")
	}
	return cookies[0], u.Query()
}

func TestStartSSOAsksForWhatItWillCheck(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))

	_, q := startSSO(t, svc, "/connections")

	if q.Get("client_id") != "client-id" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Error("state or nonce is missing")
	}
	// PKCE: without a challenge, an intercepted code could be redeemed by
	// somebody else.
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("PKCE challenge = %q %q", q.Get("code_challenge"), q.Get("code_challenge_method"))
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("scope = %q", q.Get("scope"))
	}
}

// completeSSO replays the callback with whatever query the test wants.
func completeSSO(t *testing.T, svc *Service, cookie *http.Cookie, q url.Values) (store.User, string, error) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/test/callback?"+q.Encode(), nil)
	req.AddCookie(cookie)
	return svc.CompleteSSO(httptest.NewRecorder(), req, "test")
}

func TestCompleteSSOSignsInAVerifiedIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	svc, db := newTestService(t, withProvider(idp))

	cookie, q := startSSO(t, svc, "/connections")
	idp.nonce = q.Get("nonce")
	idp.claims = map[string]any{
		"email": "person@example.com", "email_verified": true, "name": "Person",
	}

	user, next, err := completeSSO(t, svc, cookie, url.Values{
		"state": {q.Get("state")}, "code": {"auth-code"},
	})
	if err != nil {
		t.Fatalf("CompleteSSO: %v", err)
	}
	if user.Email != "person@example.com" || user.Provider != "test" {
		t.Fatalf("user = %+v", user)
	}
	if next != "/connections" {
		t.Errorf("next = %q, want the sanitised original", next)
	}
	if _, err := db.GetUserByProviderSubject("test", "subject-1"); err != nil {
		t.Errorf("the identity was not linked: %v", err)
	}
}

func TestCompleteSSORefusesAnUnverifiedEmail(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))

	cookie, q := startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")
	// A provider that lets a user set an arbitrary unverified address could
	// otherwise impersonate any account.
	idp.claims = map[string]any{"email": "victim@example.com", "email_verified": false}

	_, _, err := completeSSO(t, svc, cookie, url.Values{"state": {q.Get("state")}, "code": {"c"}})
	if err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("error = %v", err)
	}

	// A missing claim is treated the same way as false.
	cookie, q = startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")
	idp.claims = map[string]any{"email": "victim@example.com"}
	if _, _, err := completeSSO(t, svc, cookie, url.Values{"state": {q.Get("state")}, "code": {"c"}}); err == nil {
		t.Fatal("a missing email_verified was accepted")
	}
}

func TestCompleteSSOChecksTheStateAndTheNonce(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))

	cookie, q := startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")
	idp.claims = map[string]any{"email": "person@example.com", "email_verified": true}

	// A state that does not match the cookie is a cross-site login attempt.
	if _, _, err := completeSSO(t, svc, cookie, url.Values{
		"state": {"somebody-elses-state"}, "code": {"c"},
	}); err == nil {
		t.Error("a mismatched state was accepted")
	}

	// A token minted for a different login has the wrong nonce.
	cookie, q = startSSO(t, svc, "")
	idp.nonce = "a-different-nonce"
	if _, _, err := completeSSO(t, svc, cookie, url.Values{
		"state": {q.Get("state")}, "code": {"c"},
	}); err == nil {
		t.Error("a token with the wrong nonce was accepted")
	}
}

func TestCompleteSSOReportsWhatWentWrong(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))

	cookie, q := startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")

	// The provider refusing is a normal outcome and must read as one.
	_, _, err := completeSSO(t, svc, cookie, url.Values{
		"state": {q.Get("state")}, "error": {"access_denied"}, "error_description": {"user said no"},
	})
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %v", err)
	}

	cookie, q = startSSO(t, svc, "")
	if _, _, err := completeSSO(t, svc, cookie, url.Values{"state": {q.Get("state")}}); err == nil {
		t.Error("a callback with no code was accepted")
	}

	// No cookie at all: nothing ties the callback to a login this server began.
	req := httptest.NewRequest(http.MethodGet, "/cb?state=x&code=y", nil)
	if _, _, err := svc.CompleteSSO(httptest.NewRecorder(), req, "test"); err == nil {
		t.Error("a callback with no state cookie was accepted")
	}
}

func TestSSOHonoursTheDomainAllowList(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, func(c *config.Config) {
		withProvider(idp)(c)
		p := c.Auth.Providers["test"]
		p.AllowedDomains = []string{"example.com"}
		c.Auth.Providers["test"] = p
	})

	cookie, q := startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")
	idp.claims = map[string]any{"email": "person@elsewhere.net", "email_verified": true}

	_, _, err := completeSSO(t, svc, cookie, url.Values{"state": {q.Get("state")}, "code": {"c"}})
	if err == nil || !strings.Contains(err.Error(), "elsewhere.net") {
		t.Fatalf("error = %v", err)
	}
}

func TestStartSSOForAnUnknownProvider(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.StartSSO(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil), "nope", ""); err == nil {
		t.Fatal("an unknown provider started a login")
	}
}

func TestStartSAMLProducesARedirectAndState(t *testing.T) {
	svc := newSAMLTestService(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/saml/work/start", nil)

	redirect, err := svc.StartSAML(rec, req, "work", "/connections")
	if err != nil {
		t.Fatalf("StartSAML: %v", err)
	}
	if !strings.HasPrefix(redirect, "https://idp.example.com/sso") {
		t.Errorf("redirect = %q", redirect)
	}
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("SAMLRequest") == "" {
		t.Error("no SAMLRequest in the redirect")
	}

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no state cookie was set")
	}
	c := cookies[0]
	// The assertion comes back as a cross-site POST, so Lax would drop this.
	if c.SameSite != http.SameSiteNoneMode || !c.HttpOnly {
		t.Errorf("cookie = %+v", c)
	}
}

func TestCompleteSAMLNeedsItsStateCookie(t *testing.T) {
	svc := newSAMLTestService(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/saml/work/acs", nil)
	if _, _, err := svc.CompleteSAML(httptest.NewRecorder(), req, "work"); err == nil {
		t.Fatal("an assertion with no state cookie was accepted")
	}

	// A cookie that is not ours does not decrypt.
	req.AddCookie(&http.Cookie{Name: samlCookie, Value: base64.RawURLEncoding.EncodeToString([]byte("rubbish"))})
	if _, _, err := svc.CompleteSAML(httptest.NewRecorder(), req, "work"); err == nil {
		t.Fatal("a forged state cookie was accepted")
	}
}

func TestCompleteSAMLRefusesAnExpiredOrMismatchedState(t *testing.T) {
	svc := newSAMLTestService(t)

	seal := func(st samlState) *http.Cookie {
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := svc.box.Seal(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: samlCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(sealed))}
	}

	expired := seal(samlState{Provider: "work", RequestID: "r", Expires: time.Now().Add(-time.Minute).Unix()})
	req := httptest.NewRequest(http.MethodPost, "/acs", nil)
	req.AddCookie(expired)
	if _, _, err := svc.CompleteSAML(httptest.NewRecorder(), req, "work"); err == nil ||
		!strings.Contains(err.Error(), "too long") {
		t.Errorf("expired state gave %v", err)
	}

	other := seal(samlState{Provider: "other", RequestID: "r", Expires: time.Now().Add(time.Minute).Unix()})
	req = httptest.NewRequest(http.MethodPost, "/acs", nil)
	req.AddCookie(other)
	if _, _, err := svc.CompleteSAML(httptest.NewRecorder(), req, "work"); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Errorf("a state for another provider gave %v", err)
	}
}

func TestSAMLMetadataCanBeFetchedOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(testIDPMetadata))
	}))
	defer srv.Close()

	svc := newSAMLTestService(t)
	svc.cfg.SAMLProviders["remote"] = config.SAMLProviderConfig{
		Enabled: true, MetadataURL: srv.URL,
	}

	if _, err := svc.SAMLMetadata(t.Context(), "remote"); err != nil {
		t.Fatalf("fetching metadata over HTTP failed: %v", err)
	}
}

func TestSAMLMetadataFailuresAreReported(t *testing.T) {
	svc := newSAMLTestService(t)

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()

	rubbish := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not XML"))
	}))
	defer rubbish.Close()

	missingFile := filepath.Join(t.TempDir(), "absent.xml")

	for name, cfg := range map[string]config.SAMLProviderConfig{
		"http error": {Enabled: true, MetadataURL: notFound.URL},
		"not xml":    {Enabled: true, MetadataURL: rubbish.URL},
		"no file":    {Enabled: true, MetadataFile: missingFile},
		"no source":  {Enabled: true},
	} {
		svc.cfg.SAMLProviders["broken"] = cfg
		// Each build is fresh, or a cached provider would hide the failure.
		svc.saml.providers = nil
		if _, err := svc.SAMLMetadata(t.Context(), "broken"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMetadataWithNoIdentityProviderIsRefused(t *testing.T) {
	svc := newSAMLTestService(t)

	path := filepath.Join(t.TempDir(), "sp-only.xml")
	body := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x">
	  <SPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"/>
	</EntityDescriptor>`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	svc.cfg.SAMLProviders["sp-only"] = config.SAMLProviderConfig{Enabled: true, MetadataFile: path}
	svc.saml.providers = nil
	if _, err := svc.SAMLMetadata(t.Context(), "sp-only"); err == nil ||
		!strings.Contains(err.Error(), "no identity provider") {
		t.Fatalf("error = %v", err)
	}
}

func TestReadAllLimitedStopsAtTheCap(t *testing.T) {
	got, err := readAllLimited(strings.NewReader(strings.Repeat("x", 100)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("read %d bytes, want the limit of 10", len(got))
	}
}

func TestPKCEChallengeMatchesTheVerifier(t *testing.T) {
	// Not a property of this package, but the thing StartSSO relies on: the
	// challenge sent is the SHA-256 of a verifier only this server knows.
	verifier := "a-verifier-value-that-is-long-enough-for-pkce"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if len(challenge) == 0 || strings.ContainsAny(challenge, "+/=") {
		t.Fatalf("challenge = %q, want url-safe base64 with no padding", challenge)
	}
	if big.NewInt(int64(len(challenge))).Cmp(big.NewInt(43)) != 0 {
		t.Errorf("challenge length = %d, want 43", len(challenge))
	}
}
