package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsUsable(t *testing.T) {
	cfg := Default()

	if cfg.Addr == "" || cfg.BaseURL == "" || cfg.DataDir == "" {
		t.Fatalf("the defaults leave something empty: %+v", cfg)
	}
	// A default that would not start is not a default.
	if err := cfg.validate(); err != nil {
		t.Fatalf("the default config does not validate: %v", err)
	}
	if !cfg.Auth.AllowLocal {
		t.Error("local login is off by default, so nobody could sign in")
	}
	if cfg.Auth.RequireTwoFactor {
		t.Error("two-factor is required by default, which would lock out a first run")
	}
	if cfg.Auth.TrustProxyHeaders {
		t.Error("proxy headers are trusted by default, which defeats the rate limit")
	}
}

func TestLoadFallsBackToDefaultsWhenTheFileIsAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("a missing config file should not be an error: %v", err)
	}
	if cfg.Addr != Default().Addr {
		t.Errorf("addr = %q, want the default", cfg.Addr)
	}
}

func TestLoadReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mqttview.yaml")
	body := `
addr: 127.0.0.1:9999
base_url: https://mqtt.example.com/
data_dir: /var/lib/mqttview
auth:
  session_ttl_hours: 24
  allow_local: true
  require_two_factor: true
  two_factor_issuer: Example
  trust_proxy_headers: true
  saml_providers:
    work:
      enabled: true
      display_name: Work
      metadata_url: https://idp.example.com/metadata
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	// The trailing slash is trimmed, because every URL is built by appending
	// to this and a double slash breaks an OAuth redirect match.
	if cfg.BaseURL != "https://mqtt.example.com" {
		t.Errorf("base_url = %q, want the trailing slash trimmed", cfg.BaseURL)
	}
	if !cfg.Auth.RequireTwoFactor || cfg.Auth.TwoFactorIssuer != "Example" {
		t.Errorf("two-factor settings not read: %+v", cfg.Auth)
	}
	if !cfg.Auth.TrustProxyHeaders {
		t.Error("trust_proxy_headers not read")
	}
	if p, ok := cfg.Auth.SAMLProviders["work"]; !ok || !p.Enabled {
		t.Errorf("saml provider not read: %+v", cfg.Auth.SAMLProviders)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("addr: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("malformed YAML was accepted")
	}
}

func TestEnvironmentOverlaysTheFile(t *testing.T) {
	t.Setenv("MQTTVIEW_ADDR", "0.0.0.0:1234")
	t.Setenv("MQTTVIEW_BASE_URL", "https://env.example.com")
	t.Setenv("MQTTVIEW_DATA_DIR", "/env/data")
	t.Setenv("MQTTVIEW_SECRET_KEY", "envkey")
	t.Setenv("MQTTVIEW_REQUIRE_TWO_FACTOR", "true")
	t.Setenv("MQTTVIEW_TWO_FACTOR_ISSUER", "EnvIssuer")
	t.Setenv("MQTTVIEW_TRUST_PROXY_HEADERS", "1")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "0.0.0.0:1234" || cfg.BaseURL != "https://env.example.com" {
		t.Errorf("env did not override: %+v", cfg)
	}
	if cfg.DataDir != "/env/data" || cfg.SecretKey != "envkey" {
		t.Errorf("env did not override: %+v", cfg)
	}
	if !cfg.Auth.RequireTwoFactor || cfg.Auth.TwoFactorIssuer != "EnvIssuer" {
		t.Errorf("two-factor env not applied: %+v", cfg.Auth)
	}
	if !cfg.Auth.TrustProxyHeaders {
		t.Error("trust_proxy_headers env not applied")
	}
}

func TestOIDCSecretsComeFromTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mqttview.yaml")
	body := `
auth:
  providers:
    my-idp:
      enabled: true
      issuer: https://idp.example.com
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The point of the pattern is that a client secret never has to be written
	// into a file that might be committed.
	t.Setenv("MQTTVIEW_OIDC_MY_IDP_CLIENT_ID", "id-from-env")
	t.Setenv("MQTTVIEW_OIDC_MY_IDP_CLIENT_SECRET", "secret-from-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Auth.Providers["my-idp"]
	if p.ClientID != "id-from-env" || p.ClientSecret != "secret-from-env" {
		t.Fatalf("provider secrets not taken from the environment: %+v", p)
	}
}

// boolEnv reports both the value and whether it was set at all, so that an
// unset variable leaves a file setting alone rather than forcing it to false.
func TestBoolEnvDistinguishesUnsetFromFalse(t *testing.T) {
	const key = "MQTTVIEW_TEST_BOOL"

	if _, ok := boolEnv(key); ok {
		t.Error("an unset variable reported as present")
	}

	for _, s := range []string{"1", "true", "TRUE", "True", "t"} {
		t.Setenv(key, s)
		v, ok := boolEnv(key)
		if !ok || !v {
			t.Errorf("%q gave (%v, %v), want (true, true)", s, v, ok)
		}
	}
	for _, s := range []string{"0", "false", "FALSE", "f"} {
		t.Setenv(key, s)
		v, ok := boolEnv(key)
		if !ok || v {
			t.Errorf("%q gave (%v, %v), want (false, true)", s, v, ok)
		}
	}
	// Nonsense is treated as unset, so a typo cannot silently turn something
	// off that the file turned on.
	for _, s := range []string{"", "maybe", "yes"} {
		t.Setenv(key, s)
		if _, ok := boolEnv(key); ok {
			t.Errorf("%q reported as present", s)
		}
	}
}

func TestValidateRefusesConfigurationsNobodyCouldSignInTo(t *testing.T) {
	tests := []struct {
		name string
		fix  func(*Config)
		want string
	}{
		{
			name: "no listen address",
			fix:  func(c *Config) { c.Addr = "" },
			want: "addr",
		},
		{
			name: "TLS on with no certificate",
			fix:  func(c *Config) { c.TLS.Enabled = true },
			want: "tls",
		},
		{
			name: "local login off and no provider",
			fix:  func(c *Config) { c.Auth.AllowLocal = false },
			want: "nobody could log in",
		},
		{
			name: "an enabled OIDC provider with no issuer",
			fix: func(c *Config) {
				c.Auth.Providers = map[string]ProviderConfig{"x": {Enabled: true}}
			},
			want: "issuer is required",
		},
		{
			name: "an enabled OIDC provider with no credentials",
			fix: func(c *Config) {
				c.Auth.Providers = map[string]ProviderConfig{
					"x": {Enabled: true, Issuer: "https://idp.example.com"},
				}
			},
			want: "client_id",
		},
		{
			name: "an enabled SAML provider with nowhere to get metadata",
			fix: func(c *Config) {
				c.Auth.SAMLProviders = map[string]SAMLProviderConfig{"x": {Enabled: true}}
			},
			want: "metadata_url",
		},
		{
			name: "a SAML provider whose id collides with an OIDC one",
			fix: func(c *Config) {
				c.Auth.Providers = map[string]ProviderConfig{
					"same": {Enabled: true, Issuer: "https://i", ClientID: "a", ClientSecret: "b"},
				}
				c.Auth.SAMLProviders = map[string]SAMLProviderConfig{
					"same": {Enabled: true, MetadataURL: "https://m"},
				}
			},
			want: "already uses that id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.fix(&cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatal("this configuration should not have validated")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestSSOOnlyIsAValidConfiguration(t *testing.T) {
	cfg := Default()
	cfg.Auth.AllowLocal = false
	cfg.Auth.SAMLProviders = map[string]SAMLProviderConfig{
		"work": {Enabled: true, MetadataURL: "https://idp.example.com/metadata"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("SAML alone should be enough to sign in: %v", err)
	}
}

func TestDerivedURLs(t *testing.T) {
	cfg := Default()
	cfg.BaseURL = "https://mqtt.example.com"

	if got := cfg.RedirectURI("google"); got != "https://mqtt.example.com/api/auth/sso/google/callback" {
		t.Errorf("RedirectURI = %q", got)
	}
	if got := cfg.SAMLMetadataURL("work"); got != "https://mqtt.example.com/api/auth/saml/work/metadata" {
		t.Errorf("SAMLMetadataURL = %q", got)
	}
	if got := cfg.SAMLACSURL("work"); got != "https://mqtt.example.com/api/auth/saml/work/acs" {
		t.Errorf("SAMLACSURL = %q", got)
	}
}

func TestHasEnabledProviderSeesBothProtocols(t *testing.T) {
	cfg := Default()
	if cfg.hasEnabledProvider() {
		t.Error("a default config has no providers")
	}

	cfg.Auth.Providers = map[string]ProviderConfig{"a": {Enabled: false}}
	cfg.Auth.SAMLProviders = map[string]SAMLProviderConfig{"b": {Enabled: false}}
	if cfg.hasEnabledProvider() {
		t.Error("disabled providers should not count")
	}

	cfg.Auth.SAMLProviders["b"] = SAMLProviderConfig{Enabled: true}
	if !cfg.hasEnabledProvider() {
		t.Error("an enabled SAML provider was not seen")
	}
}

// Environment variables are how the container is configured, so each one has
// to actually reach the field it names. A typo'd binding is invisible: the
// server starts and quietly uses the default.

func TestEnvironmentOverridesReachTheirFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MQTTVIEW_ADDR", "127.0.0.1:9999")
	t.Setenv("MQTTVIEW_BASE_URL", "https://example.test/")
	t.Setenv("MQTTVIEW_DATA_DIR", dir)
	t.Setenv("MQTTVIEW_SECRET_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("MQTTVIEW_REQUIRE_TWO_FACTOR", "true")
	t.Setenv("MQTTVIEW_TWO_FACTOR_ISSUER", "Podlipa")
	t.Setenv("MQTTVIEW_TRUST_PROXY_HEADERS", "true")
	t.Setenv("MQTTVIEW_ALLOW_LOCAL", "true")
	t.Setenv("MQTTVIEW_ALLOW_SIGNUP", "true")
	t.Setenv("MQTTVIEW_TLS_CERT", "/tmp/cert.pem")
	t.Setenv("MQTTVIEW_TLS_KEY", "/tmp/key.pem")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != "127.0.0.1:9999" || cfg.DataDir != dir {
		t.Errorf("addr = %q, data dir = %q", cfg.Addr, cfg.DataDir)
	}
	// The trailing slash is removed, because every redirect is built by
	// concatenating a path onto this.
	if cfg.BaseURL != "https://example.test" {
		t.Errorf("base URL = %q", cfg.BaseURL)
	}
	if cfg.SecretKey == "" {
		t.Error("the secret key was not picked up")
	}
	if !cfg.Auth.RequireTwoFactor || cfg.Auth.TwoFactorIssuer != "Podlipa" {
		t.Errorf("two-factor settings = %+v", cfg.Auth)
	}
	if !cfg.Auth.TrustProxyHeaders || !cfg.Auth.AllowLocal || !cfg.Auth.AllowSignup {
		t.Errorf("auth flags = %+v", cfg.Auth)
	}
	// Naming a certificate is how TLS is switched on: an operator who sets one
	// and finds plain HTTP would reasonably call that a bug.
	if !cfg.TLS.Enabled || cfg.TLS.CertFile != "/tmp/cert.pem" || cfg.TLS.KeyFile != "/tmp/key.pem" {
		t.Errorf("TLS = %+v", cfg.TLS)
	}
}

func TestProviderCredentialsCanStayOutOfTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mqttview.yaml")
	// The file names the provider; the secrets come from the environment,
	// which is the only way to keep them out of a mounted config file.
	if err := os.WriteFile(path, []byte(`
base_url: https://example.test
data_dir: `+dir+`
auth:
  allow_local: true
  providers:
    my-idp:
      enabled: false
      issuer: ""
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MQTTVIEW_OIDC_MY_IDP_CLIENT_ID", "client")
	t.Setenv("MQTTVIEW_OIDC_MY_IDP_CLIENT_SECRET", "secret")
	t.Setenv("MQTTVIEW_OIDC_MY_IDP_ISSUER", "https://idp.example.test")
	t.Setenv("MQTTVIEW_OIDC_MY_IDP_ENABLED", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Auth.Providers["my-idp"]
	if !p.Enabled || p.ClientID != "client" || p.ClientSecret != "secret" {
		t.Fatalf("provider = %+v", p)
	}
	if p.Issuer != "https://idp.example.test" {
		t.Errorf("issuer = %q", p.Issuer)
	}
}

func TestAConfigNobodyCouldLogIntoIsRefused(t *testing.T) {
	cfg := Default()
	cfg.Auth.AllowLocal = false

	// No passwords and no SSO is not a lockdown, it is a locked-out server.
	if err := cfg.validate(); err == nil {
		t.Fatal("a configuration with no way in was accepted")
	}

	cfg.Auth.SAMLProviders = map[string]SAMLProviderConfig{
		"idp": {Enabled: true, MetadataURL: "https://idp.example.test/metadata"},
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("SAML alone was refused: %v", err)
	}
}

func TestSSOProvidersAreValidatedBeforeTheServerStarts(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"an enabled OIDC provider with no issuer": func(c *Config) {
			c.Auth.Providers = map[string]ProviderConfig{"a": {Enabled: true}}
		},
		"an enabled OIDC provider with no client": func(c *Config) {
			c.Auth.Providers = map[string]ProviderConfig{
				"a": {Enabled: true, Issuer: "https://idp.example.test"},
			}
		},
		"an enabled SAML provider with no metadata": func(c *Config) {
			c.Auth.SAMLProviders = map[string]SAMLProviderConfig{"a": {Enabled: true}}
		},
		"a SAML provider reusing an OIDC id": func(c *Config) {
			c.Auth.Providers = map[string]ProviderConfig{
				"a": {Enabled: true, Issuer: "https://idp.example.test", ClientID: "c", ClientSecret: "s"},
			}
			c.Auth.SAMLProviders = map[string]SAMLProviderConfig{
				"a": {Enabled: true, MetadataURL: "https://idp.example.test/metadata"},
			}
		},
	} {
		cfg := Default()
		mutate(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// A provider that is switched off is not validated: half-finished
	// configuration is how somebody prepares a migration.
	cfg := Default()
	cfg.Auth.Providers = map[string]ProviderConfig{"a": {Enabled: false}}
	cfg.Auth.SAMLProviders = map[string]SAMLProviderConfig{"b": {Enabled: false}}
	if err := cfg.validate(); err != nil {
		t.Errorf("a disabled, incomplete provider was refused: %v", err)
	}
}
