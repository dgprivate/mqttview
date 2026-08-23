package auth

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"

	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/secrets"
)

// attributeStatement builds the shape crewjam/saml hands back after it has
// verified a response, so the mapping can be tested without standing up an
// identity provider.
func assertionWith(nameID string, attrs map[string]string) *saml.Assertion {
	a := &saml.Assertion{}
	if nameID != "" {
		a.Subject = &saml.Subject{NameID: &saml.NameID{Value: nameID}}
	}
	stmt := saml.AttributeStatement{}
	for name, value := range attrs {
		stmt.Attributes = append(stmt.Attributes, saml.Attribute{
			Name:   name,
			Values: []saml.AttributeValue{{Value: value}},
		})
	}
	a.AttributeStatements = []saml.AttributeStatement{stmt}
	return a
}

func TestSAMLIdentityReadsTheCommonAttributeNames(t *testing.T) {
	tests := []struct {
		name      string
		nameID    string
		attrs     map[string]string
		wantEmail string
		wantName  string
	}{
		{
			name:   "keycloak and authentik use the LDAP OIDs",
			nameID: "f:1234:dean",
			attrs: map[string]string{
				"urn:oid:0.9.2342.19200300.100.1.3": "dean@example.com",
				"urn:oid:2.16.840.1.113730.3.1.241": "Dean G",
			},
			wantEmail: "dean@example.com",
			wantName:  "Dean G",
		},
		{
			name:   "entra id and adfs use the claims URIs",
			nameID: "abc-123",
			attrs: map[string]string{
				"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": "dean@example.com",
				"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name":         "Dean G",
			},
			wantEmail: "dean@example.com",
			wantName:  "Dean G",
		},
		{
			name:      "a plain email attribute",
			nameID:    "abc-123",
			attrs:     map[string]string{"email": "dean@example.com"},
			wantEmail: "dean@example.com",
		},
		{
			name:      "no attributes, but the NameID is an address",
			nameID:    "dean@example.com",
			wantEmail: "dean@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject, email, name := samlIdentity(assertionWith(tt.nameID, tt.attrs), config.SAMLProviderConfig{})

			if email != tt.wantEmail {
				t.Errorf("email = %q, want %q", email, tt.wantEmail)
			}
			if tt.wantName != "" && name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			// The subject is what an account is linked by, so it must never be
			// empty when there was anything at all to link on.
			if subject == "" {
				t.Error("no subject was derived")
			}
			if tt.nameID != "" && subject != tt.nameID {
				t.Errorf("subject = %q, want the NameID %q", subject, tt.nameID)
			}
		})
	}
}

func TestSAMLIdentityPrefersTheConfiguredAttribute(t *testing.T) {
	attrs := map[string]string{
		"email":                   "wrong@example.com",
		"http://company/upn":      "right@example.com",
		"http://company/fullname": "Dean G",
	}
	cfg := config.SAMLProviderConfig{
		EmailAttribute: "http://company/upn",
		NameAttribute:  "http://company/fullname",
	}

	_, email, name := samlIdentity(assertionWith("abc", attrs), cfg)
	if email != "right@example.com" {
		t.Errorf("email = %q, want the configured attribute", email)
	}
	if name != "Dean G" {
		t.Errorf("name = %q, want the configured attribute", name)
	}
}

// A configured attribute that the identity provider does not send must yield
// nothing, rather than quietly falling back to one it does send. Silently
// reading a different attribute is how an account gets linked to the wrong
// person.
func TestSAMLIdentityDoesNotFallBackFromAConfiguredAttribute(t *testing.T) {
	attrs := map[string]string{"email": "someone.else@example.com"}
	cfg := config.SAMLProviderConfig{EmailAttribute: "http://company/upn"}

	_, email, _ := samlIdentity(assertionWith("abc", attrs), cfg)
	if email != "" {
		t.Errorf("email = %q, want empty when the configured attribute is absent", email)
	}
}

func TestSAMLIdentityHandlesAnEmptyAssertion(t *testing.T) {
	subject, email, name := samlIdentity(&saml.Assertion{}, config.SAMLProviderConfig{})
	if subject != "" || email != "" || name != "" {
		t.Errorf("an empty assertion produced (%q, %q, %q)", subject, email, name)
	}
}

func TestSAMLProviderInfosListsOnlyEnabledOnes(t *testing.T) {
	s := &Service{cfg: config.AuthConfig{
		SAMLProviders: map[string]config.SAMLProviderConfig{
			"work":    {Enabled: true, DisplayName: "Work SSO"},
			"old":     {Enabled: false, DisplayName: "Retired"},
			"unnamed": {Enabled: true},
		},
	}}

	infos := s.SAMLProviderInfos()
	if len(infos) != 2 {
		t.Fatalf("got %d providers, want 2: %+v", len(infos), infos)
	}
	// Sorted by ID, so the login page does not shuffle between reloads.
	if infos[0].ID != "unnamed" && infos[1].ID != "unnamed" {
		t.Errorf("the unnamed provider is missing: %+v", infos)
	}
	for _, i := range infos {
		if i.DisplayName == "" {
			t.Errorf("provider %q has no display name to put on a button", i.ID)
		}
		if i.ID == "old" {
			t.Error("a disabled provider was listed")
		}
	}
}

func TestSAMLMetadataDescribesThisServiceProvider(t *testing.T) {
	s := newSAMLTestService(t)

	metadata, err := s.SAMLMetadata(t.Context(), "work")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	doc := string(metadata)

	for _, want := range []string{
		"EntityDescriptor",
		"SPSSODescriptor",
		"AssertionConsumerService",
		"https://mqttview.example.com/api/auth/saml/work/acs",
		"X509Certificate",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("metadata is missing %q", want)
		}
	}
	// The private key must never appear in a document handed to an identity
	// provider.
	if strings.Contains(doc, "PRIVATE KEY") {
		t.Fatal("the metadata contains a private key")
	}
}

func TestSAMLKeypairIsGeneratedOnceAndKeptPrivate(t *testing.T) {
	s := newSAMLTestService(t)

	first, err := s.spKeypair()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.spKeypair()
	if err != nil {
		t.Fatal(err)
	}
	// A new keypair on every call would invalidate the metadata every identity
	// provider was configured with.
	if first != second {
		t.Fatal("a second call produced a different keypair")
	}

	// The private half is a credential and sits in the data directory beside
	// the encryption key; it must not be readable by anyone else on the host.
	info, err := os.Stat(filepath.Join(s.dataDir, samlSPKeyFile))
	if err != nil {
		t.Fatalf("the SAML key was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s is mode %o, want no group or world access", samlSPKeyFile, perm)
	}
}

// newSAMLTestService builds a Service with one SAML provider whose identity
// provider metadata is a file on disk, so no network is involved.
func newSAMLTestService(t *testing.T) *Service {
	t.Helper()

	dir := t.TempDir()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}

	metadataPath := filepath.Join(dir, "idp.xml")
	if err := os.WriteFile(metadataPath, []byte(testIDPMetadata), 0o600); err != nil {
		t.Fatal(err)
	}

	return &Service{
		cfg: config.AuthConfig{
			SAMLProviders: map[string]config.SAMLProviderConfig{
				"work": {Enabled: true, DisplayName: "Work SSO", MetadataFile: metadataPath},
			},
		},
		baseURL: "https://mqttview.example.com",
		secure:  true,
		box:     box,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		dataDir: dir,
	}
}

// testIDPMetadata is a minimal but well-formed identity provider descriptor.
const testIDPMetadata = `<?xml version="1.0"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com/metadata">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"
                         Location="https://idp.example.com/sso"/>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
                         Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`

// The redirect out to the identity provider, and everything that can go wrong
// on the way back. A SAML flow that fails open is an authentication bypass, so
// every one of these has to be a refusal.

func TestStartSAMLRedirectsAndRemembersTheRequest(t *testing.T) {
	s := newSAMLTestService(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/auth/saml/work/start", nil)

	redirect, err := s.StartSAML(w, r, "work", "/connections")
	if err != nil {
		t.Fatalf("StartSAML: %v", err)
	}
	if !strings.HasPrefix(redirect, "https://idp.example.com/sso?") {
		t.Errorf("redirect = %q, want the identity provider's SSO endpoint", redirect)
	}
	if !strings.Contains(redirect, "SAMLRequest=") {
		t.Error("the redirect carries no SAMLRequest")
	}

	cookies := w.Result().Cookies()
	var state *http.Cookie
	for _, c := range cookies {
		if c.Name == samlCookie {
			state = c
		}
	}
	if state == nil {
		t.Fatalf("no state cookie was set: %+v", cookies)
	}
	// SameSite=None, because the assertion comes back as a cross-site POST and
	// a Lax cookie would simply not be sent. None requires Secure.
	if state.SameSite != http.SameSiteNoneMode || !state.Secure {
		t.Errorf("state cookie = %+v", state)
	}
	if !state.HttpOnly {
		t.Error("the state cookie is readable from JavaScript")
	}
}

func TestStartSAMLOnAProviderThatIsNotConfigured(t *testing.T) {
	s := newSAMLTestService(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/auth/saml/nope/start", nil)

	if _, err := s.StartSAML(w, r, "nope", ""); err == nil {
		t.Fatal("an unknown provider produced a redirect")
	}
	if _, err := s.SAMLMetadata(t.Context(), "nope"); err == nil {
		t.Error("an unknown provider produced metadata")
	}
}

func TestAnAssertionWithoutValidStateIsRefused(t *testing.T) {
	s := newSAMLTestService(t)

	post := func(cookie *http.Cookie) error {
		form := url.Values{"SAMLResponse": {"not-an-assertion"}}
		r := httptest.NewRequest(http.MethodPost, "/api/auth/saml/work/acs",
			strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		_, _, err := s.CompleteSAML(httptest.NewRecorder(), r, "work")
		return err
	}

	// No cookie at all: the browser that started the login is not this one.
	if err := post(nil); err == nil {
		t.Error("an assertion with no login state was accepted")
	}
	// A cookie that is not ours: the sealed value will not open.
	if err := post(&http.Cookie{Name: samlCookie, Value: "bm90LW91cnM"}); err == nil {
		t.Error("a forged state cookie was accepted")
	}
	// Our own encoding, but expired: replaying yesterday's login must not work.
	if err := post(sealedState(t, s, samlState{
		Provider: "work", RequestID: "id-1", Expires: time.Now().Add(-time.Minute).Unix(),
	})); err == nil {
		t.Error("an expired login state was accepted")
	}
	// Valid state, but issued for a different provider.
	if err := post(sealedState(t, s, samlState{
		Provider: "other", RequestID: "id-1", Expires: time.Now().Add(time.Minute).Unix(),
	})); err == nil {
		t.Error("state from another provider was accepted")
	}
	// Valid state, and the response is still not a signed assertion.
	if err := post(sealedState(t, s, samlState{
		Provider: "work", RequestID: "id-1", Expires: time.Now().Add(time.Minute).Unix(),
	})); err == nil {
		t.Error("an unsigned response was accepted")
	}
}

func TestIdentityProviderMetadataThatCannotBeUsed(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	s := newSAMLTestService(t)

	for name, cfg := range map[string]config.SAMLProviderConfig{
		"a file that is not there": {MetadataFile: filepath.Join(dir, "absent.xml")},
		"a file that is not XML":   {MetadataFile: write("junk.xml", "this is not xml")},
		"metadata describing no identity provider": {MetadataFile: write("empty.xml",
			`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"/>`)},
		"neither a URL nor a file": {},
	} {
		if _, err := s.fetchIDPMetadata(t.Context(), cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMetadataWrappedInAnEntitiesDescriptorIsAccepted(t *testing.T) {
	// ADFS and Shibboleth publish a federation document wrapping the entity,
	// which is well-formed SAML and has to work.
	dir := t.TempDir()
	path := filepath.Join(dir, "entities.xml")
	body := `<?xml version="1.0"?>
<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata">` +
		strings.TrimPrefix(testIDPMetadata, `<?xml version="1.0"?>`) +
		`</EntitiesDescriptor>`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newSAMLTestService(t)
	descriptor, err := s.fetchIDPMetadata(t.Context(), config.SAMLProviderConfig{MetadataFile: path})
	if err != nil {
		t.Fatalf("a wrapped descriptor was refused: %v", err)
	}
	if descriptor.EntityID != "https://idp.example.com/metadata" {
		t.Errorf("entity ID = %q", descriptor.EntityID)
	}
}

func TestMetadataFetchedOverHTTP(t *testing.T) {
	s := newSAMLTestService(t)

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testIDPMetadata))
	}))
	defer ok.Close()

	if _, err := s.fetchIDPMetadata(t.Context(),
		config.SAMLProviderConfig{MetadataURL: ok.URL}); err != nil {
		t.Errorf("metadata over HTTP was refused: %v", err)
	}

	// A provider that answers with an error page must not be treated as one
	// that published metadata.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bad.Close()

	if _, err := s.fetchIDPMetadata(t.Context(),
		config.SAMLProviderConfig{MetadataURL: bad.URL}); err == nil {
		t.Error("a 404 was accepted as metadata")
	}
	if _, err := s.fetchIDPMetadata(t.Context(),
		config.SAMLProviderConfig{MetadataURL: "http://127.0.0.1:1/metadata"}); err == nil {
		t.Error("an unreachable metadata URL was accepted")
	}
}

// sealedState builds the login-state cookie the SAML flow sets, so a test can
// present one the service will accept as its own.
func sealedState(t *testing.T, s *Service, st samlState) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.box.Seal(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{
		Name:  samlCookie,
		Value: base64.RawURLEncoding.EncodeToString([]byte(sealed)),
	}
}
