package auth

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewjam/saml"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/secrets"
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
