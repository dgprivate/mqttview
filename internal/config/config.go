// Package config loads mqttview server configuration from a YAML file and
// environment variables. Environment always wins over the file so that
// container deployments can override secrets without rewriting config.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the fully resolved server configuration.
type Config struct {
	// Addr is the listen address, e.g. "127.0.0.1:8114".
	Addr string `yaml:"addr"`
	// BaseURL is the externally reachable origin, used to build OAuth
	// redirect URIs and to validate WebSocket origins.
	BaseURL string `yaml:"base_url"`
	// DataDir holds the SQLite database and any plugin state.
	DataDir string `yaml:"data_dir"`
	// SecretKey is a 32-byte key (hex or base64) used to encrypt broker
	// credentials at rest. Generated on first run if absent.
	SecretKey string `yaml:"secret_key"`

	TLS  TLSConfig  `yaml:"tls"`
	Auth AuthConfig `yaml:"auth"`

	// Plugins maps a plugin ID to its enablement and settings.
	Plugins map[string]PluginConfig `yaml:"plugins"`
}

// TLSConfig configures HTTPS for the mqttview UI itself. This is independent
// of the TLS used to reach MQTT brokers.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// AuthConfig controls how humans log in to mqttview.
type AuthConfig struct {
	// SessionTTLHours is how long a login session stays valid.
	SessionTTLHours int `yaml:"session_ttl_hours"`
	// RequireTwoFactor makes every local account enrol a second factor before
	// it can do anything. SSO accounts are exempt: they authenticate at the
	// identity provider, which is where their second factor belongs.
	RequireTwoFactor bool `yaml:"require_two_factor"`
	// TwoFactorIssuer is the name an authenticator app shows beside the
	// account. Defaults to "mqttview".
	TwoFactorIssuer string `yaml:"two_factor_issuer"`
	// TrustProxyHeaders makes mqttview believe X-Forwarded-For when working
	// out who a request came from. Off by default, and it must stay off
	// unless a proxy in front strips and rewrites that header: the value keys
	// the sign-in rate limit, and a client that can choose its own address
	// defeats the limit by changing it every attempt.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// AllowLocal enables username+password login. Disable it to force SSO.
	AllowLocal bool `yaml:"allow_local"`
	// AllowSignup lets unknown SSO identities create an account on first
	// login. When false, an admin must pre-create the user.
	AllowSignup bool `yaml:"allow_signup"`
	// Providers are OIDC/OAuth2 single sign-on providers, keyed by ID.
	Providers map[string]ProviderConfig `yaml:"providers"`
	// SAMLProviders are SAML 2.0 identity providers, keyed by ID. The ID
	// namespace is shared with Providers, so the two cannot collide.
	SAMLProviders map[string]SAMLProviderConfig `yaml:"saml_providers"`
}

// ProviderConfig is one SSO provider. Any standards-compliant OIDC issuer
// works; "google" is just an issuer URL like everything else.
type ProviderConfig struct {
	Enabled bool `yaml:"enabled"`
	// DisplayName is what the login button says, e.g. "Google".
	DisplayName string `yaml:"display_name"`
	// Issuer is the OIDC issuer URL used for discovery.
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
	// AllowedDomains restricts login to these email domains. Empty means
	// any domain the provider vouches for is accepted.
	AllowedDomains []string `yaml:"allowed_domains"`
	// AdminEmails are granted the admin role on first login.
	AdminEmails []string `yaml:"admin_emails"`
}

// SAMLProviderConfig is one SAML 2.0 identity provider.
//
// SAML predates OIDC and says nothing about how attributes are named, so the
// email and name attributes are configurable; the defaults cover the URNs that
// Entra ID, Okta, Keycloak and Authentik actually send.
type SAMLProviderConfig struct {
	Enabled bool `yaml:"enabled"`
	// DisplayName is what the login button says, e.g. "Company SSO".
	DisplayName string `yaml:"display_name"`
	// MetadataURL is fetched at startup to learn the IdP's endpoints and
	// signing certificates. Use MetadataFile instead for an air-gapped IdP.
	MetadataURL  string `yaml:"metadata_url"`
	MetadataFile string `yaml:"metadata_file"`
	// EntityID identifies this service provider to the IdP. Empty defaults to
	// the metadata URL, which is the usual convention.
	EntityID string `yaml:"entity_id"`
	// EmailAttribute and NameAttribute name the assertion attributes to read.
	// Empty means try the common ones in turn.
	EmailAttribute string `yaml:"email_attribute"`
	NameAttribute  string `yaml:"name_attribute"`
	// AllowedDomains restricts login to these email domains.
	AllowedDomains []string `yaml:"allowed_domains"`
	// AdminEmails are granted the admin role on first login.
	AdminEmails []string `yaml:"admin_emails"`
}

// PluginConfig is per-plugin enablement plus free-form settings that the
// plugin decodes itself.
type PluginConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Settings map[string]any `yaml:"settings"`
}

// Default returns a config with production-safe defaults applied.
func Default() Config {
	return Config{
		Addr:    "127.0.0.1:8114",
		BaseURL: "http://127.0.0.1:8114",
		DataDir: "./data",
		Auth: AuthConfig{
			SessionTTLHours: 24 * 7,
			AllowLocal:      true,
			AllowSignup:     false,
			Providers:       map[string]ProviderConfig{},
			SAMLProviders:   map[string]SAMLProviderConfig{},
		},
		Plugins: map[string]PluginConfig{},
	}
}

// Load reads the YAML file at path (missing file is not an error), then
// applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(raw, &cfg); err != nil {
				return cfg, fmt.Errorf("parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// Running without a config file is a supported mode.
		default:
			return cfg, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if cfg.Auth.Providers == nil {
		cfg.Auth.Providers = map[string]ProviderConfig{}
	}
	if cfg.Auth.SAMLProviders == nil {
		cfg.Auth.SAMLProviders = map[string]SAMLProviderConfig{}
	}
	if cfg.Plugins == nil {
		cfg.Plugins = map[string]PluginConfig{}
	}
	if cfg.Auth.SessionTTLHours <= 0 {
		cfg.Auth.SessionTTLHours = 24 * 7
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	return cfg, cfg.validate()
}

// applyEnv overlays MQTTVIEW_* environment variables. Provider credentials
// use the pattern MQTTVIEW_OIDC_<ID>_CLIENT_ID so secrets can stay out of the
// config file entirely.
func applyEnv(cfg *Config) {
	if v := os.Getenv("MQTTVIEW_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("MQTTVIEW_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("MQTTVIEW_REQUIRE_TWO_FACTOR"); v != "" {
		cfg.Auth.RequireTwoFactor = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v := os.Getenv("MQTTVIEW_TWO_FACTOR_ISSUER"); v != "" {
		cfg.Auth.TwoFactorIssuer = v
	}
	if v := os.Getenv("MQTTVIEW_TRUST_PROXY_HEADERS"); v != "" {
		cfg.Auth.TrustProxyHeaders = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v := os.Getenv("MQTTVIEW_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("MQTTVIEW_SECRET_KEY"); v != "" {
		cfg.SecretKey = v
	}
	if v := os.Getenv("MQTTVIEW_TLS_CERT"); v != "" {
		cfg.TLS.CertFile = v
		cfg.TLS.Enabled = true
	}
	if v := os.Getenv("MQTTVIEW_TLS_KEY"); v != "" {
		cfg.TLS.KeyFile = v
	}
	if v, ok := boolEnv("MQTTVIEW_ALLOW_LOCAL"); ok {
		cfg.Auth.AllowLocal = v
	}
	if v, ok := boolEnv("MQTTVIEW_ALLOW_SIGNUP"); ok {
		cfg.Auth.AllowSignup = v
	}

	for id, p := range cfg.Auth.Providers {
		prefix := "MQTTVIEW_OIDC_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
		if v := os.Getenv(prefix + "_CLIENT_ID"); v != "" {
			p.ClientID = v
		}
		if v := os.Getenv(prefix + "_CLIENT_SECRET"); v != "" {
			p.ClientSecret = v
		}
		if v := os.Getenv(prefix + "_ISSUER"); v != "" {
			p.Issuer = v
		}
		if v, ok := boolEnv(prefix + "_ENABLED"); ok {
			p.Enabled = v
		}
		cfg.Auth.Providers[id] = p
	}
}

func boolEnv(key string) (bool, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return false, false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return v, true
}

func (c Config) validate() error {
	if c.Addr == "" {
		return errors.New("addr must not be empty")
	}
	if c.TLS.Enabled && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return errors.New("tls.enabled requires tls.cert_file and tls.key_file")
	}
	if !c.Auth.AllowLocal && !c.hasEnabledProvider() {
		return errors.New("auth.allow_local is false and no SSO provider is enabled: nobody could log in")
	}
	for id, p := range c.Auth.SAMLProviders {
		if _, clash := c.Auth.Providers[id]; clash {
			return fmt.Errorf("auth.saml_providers.%s: an OIDC provider already uses that id", id)
		}
		if !p.Enabled {
			continue
		}
		if p.MetadataURL == "" && p.MetadataFile == "" {
			return fmt.Errorf("auth.saml_providers.%s: metadata_url or metadata_file is required", id)
		}
	}
	for id, p := range c.Auth.Providers {
		if !p.Enabled {
			continue
		}
		if p.Issuer == "" {
			return fmt.Errorf("auth.providers.%s: issuer is required", id)
		}
		if p.ClientID == "" || p.ClientSecret == "" {
			return fmt.Errorf("auth.providers.%s: client_id and client_secret are required", id)
		}
	}
	return nil
}

func (c Config) hasEnabledProvider() bool {
	for _, p := range c.Auth.Providers {
		if p.Enabled {
			return true
		}
	}
	for _, p := range c.Auth.SAMLProviders {
		if p.Enabled {
			return true
		}
	}
	return false
}

// SAMLMetadataURL is where this service publishes its SP metadata, and the
// entity ID it uses by default.
func (c Config) SAMLMetadataURL(providerID string) string {
	return c.BaseURL + "/api/auth/saml/" + providerID + "/metadata"
}

// SAMLACSURL is where the identity provider posts its assertion.
func (c Config) SAMLACSURL(providerID string) string {
	return c.BaseURL + "/api/auth/saml/" + providerID + "/acs"
}

// RedirectURI is the OAuth2 callback URL for a provider.
func (c Config) RedirectURI(providerID string) string {
	return c.BaseURL + "/api/auth/sso/" + providerID + "/callback"
}
