package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/xml"
	"html"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"

	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/store"
)

// The whole SAML exchange against a real identity provider: crewjam/saml's
// own, signing with a real key. The tests in saml_test.go prove that rubbish
// is refused; these prove that a genuine assertion is accepted, and that one
// which is genuine in every respect but one is not. Without the first half,
// a flow that refused everything would pass every test.

// testSAMLIDP is an identity provider that signs in whoever session says.
type testSAMLIDP struct {
	idp     *saml.IdentityProvider
	srv     *httptest.Server
	svc     *Service
	session *saml.Session
}

func newSAMLKeypair(t *testing.T) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test identity provider"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

// newSAMLFlow stands up the identity provider and a service configured to
// trust it, the way an operator would: by pointing metadata_url at it.
func newSAMLFlow(t *testing.T, mutate ...func(*config.Config)) (*testSAMLIDP, *store.Store) {
	t.Helper()

	key, cert := newSAMLKeypair(t)
	f := &testSAMLIDP{
		session: &saml.Session{
			ID: "idp-session", CreateTime: time.Now(), ExpireTime: time.Now().Add(time.Hour),
			NameID: "person-1", UserEmail: "person@example.com", UserCommonName: "Person",
		},
	}
	f.idp = &saml.IdentityProvider{
		Key:                     key,
		Certificate:             cert,
		Logger:                  log.New(io.Discard, "", 0),
		ServiceProviderProvider: f,
		SessionProvider:         f,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metadata", f.idp.ServeMetadata)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	metadataURL, _ := url.Parse(f.srv.URL + "/metadata")
	ssoURL, _ := url.Parse(f.srv.URL + "/sso")
	f.idp.MetadataURL = *metadataURL
	f.idp.SSOURL = *ssoURL

	svc, db := newTestService(t, append([]func(*config.Config){func(c *config.Config) {
		c.Auth.AllowSignup = true
		c.Auth.SAMLProviders = map[string]config.SAMLProviderConfig{
			"work": {Enabled: true, MetadataURL: metadataURL.String()},
		}
	}}, mutate...)...)
	f.svc = svc
	return f, db
}

// GetServiceProvider is the identity provider's side of the registration an
// operator does by hand: it trusts exactly the metadata mqttview publishes.
func (f *testSAMLIDP) GetServiceProvider(r *http.Request, _ string) (*saml.EntityDescriptor, error) {
	raw, err := f.svc.SAMLMetadata(r.Context(), "work")
	if err != nil {
		return nil, err
	}
	var d saml.EntityDescriptor
	if err := xml.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (f *testSAMLIDP) GetSession(_ http.ResponseWriter, _ *http.Request, _ *saml.IdpAuthnRequest) *saml.Session {
	return f.session
}

var samlResponseField = regexp.MustCompile(`name="SAMLResponse" value="([^"]*)"`)

// start runs the first leg — mqttview's redirect, and the identity provider
// answering it — and returns the state cookie plus the response the browser
// would post back.
func (f *testSAMLIDP) start(t *testing.T, next string) (*http.Cookie, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	redirect, err := f.svc.StartSAML(rec,
		httptest.NewRequest(http.MethodGet, "/api/auth/saml/work/start", nil), "work", next)
	if err != nil {
		t.Fatalf("StartSAML: %v", err)
	}
	var state *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == samlCookie {
			state = c
		}
	}
	if state == nil {
		t.Fatal("StartSAML set no state cookie")
	}

	idpRec := httptest.NewRecorder()
	f.idp.ServeSSO(idpRec, httptest.NewRequest(http.MethodGet, redirect, nil))
	m := samlResponseField.FindStringSubmatch(idpRec.Body.String())
	if m == nil {
		t.Fatalf("the identity provider did not answer with a response: %d %s",
			idpRec.Code, idpRec.Body.String())
	}
	return state, html.UnescapeString(m[1])
}

// complete posts a response to the assertion consumer service as the browser
// would, and reports the outcome and any session cookie that was issued.
func (f *testSAMLIDP) complete(state *http.Cookie, response string) (store.User, string, *http.Cookie, error) {
	form := url.Values{"SAMLResponse": {response}}
	r := httptest.NewRequest(http.MethodPost, "https://mqttview.example.com/api/auth/saml/work/acs",
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(state)

	rec := httptest.NewRecorder()
	user, next, err := f.svc.CompleteSAML(rec, r, "work")
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			session = c
		}
	}
	return user, next, session, err
}

func TestASignedSAMLAssertionSignsThePersonIn(t *testing.T) {
	f, db := newSAMLFlow(t)
	f.session.UserEmail = "Person@Example.com"

	state, response := f.start(t, "/connections")
	user, next, session, err := f.complete(state, response)
	if err != nil {
		t.Fatalf("a correctly signed assertion was refused: %v", err)
	}
	if user.Email != "person@example.com" || user.Provider != "work" || user.Name != "Person" {
		t.Errorf("user = %+v", user)
	}
	if next != "/connections" {
		t.Errorf("next = %q, want where the login started", next)
	}
	if _, err := db.GetUserByProviderSubject("work", "person-1"); err != nil {
		t.Errorf("the account is not linked to the NameID: %v", err)
	}

	// The consequence that matters: the cookie it set is a working session.
	if session == nil {
		t.Fatal("no session cookie was issued")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.AddCookie(session)
	if got, err := f.svc.Authenticate(r); err != nil || got.ID != user.ID {
		t.Errorf("the issued session does not authenticate: %+v %v", got, err)
	}
}

func TestASAMLLoginWithNoDestinationLandsOnTheHomePage(t *testing.T) {
	f, _ := newSAMLFlow(t)

	state, response := f.start(t, "")
	_, next, _, err := f.complete(state, response)
	if err != nil {
		t.Fatal(err)
	}
	if next != "/" {
		t.Errorf("next = %q, want /", next)
	}
}

func TestASAMLAssertionSignedByAnotherKeyIsRefused(t *testing.T) {
	f, db := newSAMLFlow(t)

	// mqttview learns the genuine key from the metadata on the first leg...
	state, _ := f.start(t, "")

	// ...and the response comes back signed by somebody else, carrying an
	// identity of their choosing.
	f.idp.Key, f.idp.Certificate = newSAMLKeypair(t)
	f.session.UserEmail = "admin@example.com"
	_, forged := f.start(t, "")

	_, _, session, err := f.complete(state, forged)
	if err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("an assertion signed by an unknown key was not refused at verification: %v", err)
	}
	if session != nil {
		t.Error("a session was issued for a forged assertion")
	}
	if _, err := db.GetUserByEmail("admin@example.com"); err == nil {
		t.Error("a forged assertion created an account")
	}
}

func TestASAMLResponseCannotBeReplayedIntoAnotherLogin(t *testing.T) {
	f, _ := newSAMLFlow(t)

	_, first := f.start(t, "")
	second, own := f.start(t, "")

	// A genuine, correctly signed response, but minted for the first login.
	if _, _, session, err := f.complete(second, first); err == nil || session != nil {
		t.Fatalf("a response for another login was accepted: %v", err)
	}
	// The second login's own response still works, so the refusal above was
	// the request ID and not something else being broken.
	if _, _, _, err := f.complete(second, own); err != nil {
		t.Fatalf("the matching response was refused: %v", err)
	}
}

func TestASAMLAssertionMustNameSomebody(t *testing.T) {
	for name, tc := range map[string]struct {
		nameID, email, want string
	}{
		"no email and a NameID that is not one": {"opaque-id", "", "no email address"},
		"neither a NameID nor an email":         {"", "", "no stable subject"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newSAMLFlow(t)
			f.session.NameID, f.session.UserEmail = tc.nameID, tc.email

			state, response := f.start(t, "")
			_, _, session, err := f.complete(state, response)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one about %q", err, tc.want)
			}
			if session != nil {
				t.Error("a session was issued for an assertion naming nobody")
			}
		})
	}
}

func TestSAMLHonoursTheDomainAllowList(t *testing.T) {
	f, db := newSAMLFlow(t, func(c *config.Config) {
		p := c.Auth.SAMLProviders["work"]
		p.AllowedDomains = []string{"example.com"}
		c.Auth.SAMLProviders["work"] = p
	})
	f.session.UserEmail = "person@elsewhere.net"

	state, response := f.start(t, "")
	if _, _, _, err := f.complete(state, response); err == nil || !strings.Contains(err.Error(), "elsewhere.net") {
		t.Fatalf("error = %v", err)
	}
	if _, err := db.GetUserByEmail("person@elsewhere.net"); err == nil {
		t.Error("an address outside the allow-list got an account")
	}
}

func TestSAMLDoesNotCreateAccountsWhenSignupIsOff(t *testing.T) {
	f, db := newSAMLFlow(t, func(c *config.Config) { c.Auth.AllowSignup = false })

	state, response := f.start(t, "")
	if _, _, session, err := f.complete(state, response); err == nil || session != nil {
		t.Fatalf("an unknown person was signed in with signup off: %v", err)
	}
	if _, err := db.GetUserByEmail("person@example.com"); err == nil {
		t.Error("an account was created with signup off")
	}
}
