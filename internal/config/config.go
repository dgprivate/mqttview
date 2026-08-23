// Package config loads mqttview server configuration from a YAML file and
// environment variables. Environment always wins over the file so that
// container deployments can override secrets without rewriting config.
package config

import (
	"errors"
	"fmt"
	"net"
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

	// FrameAncestors names the origins allowed to put mqttview in an iframe,
	// as CSP source expressions. Empty refuses everybody, which is right for a
	// standalone install and wrong for Home Assistant — ingress mode allows
	// its own origin without needing this set.
	//
	// Set it when embedding a standalone mqttview in another site's panel, and
	// understand that it means trusting that site not to overlay the UI.
	FrameAncestors []string `yaml:"frame_ancestors"`

	TLS  TLSConfig  `yaml:"tls"`
	Auth AuthConfig `yaml:"auth"`

	// Connections are broker connections to create on first start if they are
	// not there yet, so an install can arrive already pointed at a broker
	// instead of at an empty page.
	//
	// Seeding only ever adds. A connection that already exists by name is left
	// exactly as it is, because whoever edited it in the UI meant it.
	Connections []ConnectionSeed `yaml:"connections"`

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

// Mode selects who decides whether somebody may use mqttview.
type Mode string

const (
	// ModeStandalone is mqttview's own sign-in: passwords, OIDC, SAML.
	ModeStandalone Mode = "standalone"
	// ModeIngress hands that decision to Home Assistant. mqttview is reached
	// only through the Supervisor's ingress proxy, which has already
	// authenticated the person and tells us who they are in a header.
	ModeIngress Mode = "ingress"
)

// AuthConfig controls how humans log in to mqttview.
type AuthConfig struct {
	// Mode is "standalone" (the default) or "ingress". In ingress mode every
	// local sign-in route is switched off, because a login form behind an
	// already-authenticated proxy is a second password to lose rather than a
	// second lock.
	Mode Mode `yaml:"mode"`
	// Ingress configures the Home Assistant mode. It is ignored otherwise.
	Ingress IngressConfig `yaml:"ingress"`
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

// IngressConfig is the Home Assistant mode.
//
// The security of this whole mode rests on one thing: that a request really
// did come through the Supervisor. Anyone who can reach the port directly can
// otherwise send the identity headers themselves and be whoever they like, so
// TrustedProxies is checked before a header is read, and an empty list is
// refused rather than treated as "trust everyone".
type IngressConfig struct {
	// TrustedProxies are the addresses ingress requests arrive from, as IPs or
	// CIDRs. The Supervisor's own address is 172.30.32.2, which is the
	// default.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// DefaultRole is what a Home Assistant user gets on first sight. Home
	// Assistant does not tell an add-on whether the person is one of its
	// administrators, so mqttview cannot copy that and this is the honest
	// alternative: everybody who can open the panel gets the same role, and
	// AdminUsers names the exceptions.
	DefaultRole string `yaml:"default_role"`
	// AdminUsers are Home Assistant user IDs or usernames that get the admin
	// role in mqttview.
	AdminUsers []string `yaml:"admin_users"`
	// FallbackUser is the name to use when the Supervisor sends no identity
	// headers at all, which older Supervisor versions do not. Without it those
	// versions cannot use ingress; with it, everyone shares one account, which
	// is why it is opt-in rather than the default.
	FallbackUser string `yaml:"fallback_user"`
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

// ConnectionSeed is a broker connection declared in configuration.
//
// It is deliberately a small subset of what the UI can set: enough to reach a
// broker and see something, not a second way to express every option. Anything
// beyond this is a click away once the connection exists.
type ConnectionSeed struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Version is "3.1", "3.1.1" or "5"; empty means 3.1.1.
	Version string `yaml:"version"`
	// Subscribe are topic filters to subscribe to. Empty means "#", because a
	// connection that shows nothing is not obviously working.
	Subscribe []string `yaml:"subscribe"`
	// AutoConnect defaults to true: a connection somebody put in a config file
	// is one they want up.
	AutoConnect *bool `yaml:"auto_connect"`
	// InsecureSkipVerify accepts any TLS certificate. For a home broker with a
	// self-signed one; it is a real hole and is named so it reads like one.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// Wanted reports whether this connection should be dialled on boot.
func (c ConnectionSeed) Wanted() bool {
	return c.AutoConnect == nil || *c.AutoConnect
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
			Mode:            ModeStandalone,
			SessionTTLHours: 24 * 7,
			AllowLocal:      true,
			AllowSignup:     false,
			Providers:       map[string]ProviderConfig{},
			SAMLProviders:   map[string]SAMLProviderConfig{},
			Ingress: IngressConfig{
				// The Supervisor's address on the internal Home Assistant
				// network. Nothing else reaches an add-on's ingress port.
				TrustedProxies: []string{"172.30.32.2"},
				DefaultRole:    "operator",
			},
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
	if cfg.Auth.Mode == "" {
		cfg.Auth.Mode = ModeStandalone
	}
	if cfg.Auth.Mode == ModeIngress {
		if cfg.Auth.Ingress.DefaultRole == "" {
			cfg.Auth.Ingress.DefaultRole = "operator"
		}
		if cfg.Auth.Ingress.TrustedProxies == nil {
			cfg.Auth.Ingress.TrustedProxies = []string{"172.30.32.2"}
		}
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
	if v, ok := boolEnv("MQTTVIEW_REQUIRE_TWO_FACTOR"); ok {
		cfg.Auth.RequireTwoFactor = v
	}
	if v := os.Getenv("MQTTVIEW_TWO_FACTOR_ISSUER"); v != "" {
		cfg.Auth.TwoFactorIssuer = v
	}
	if v, ok := boolEnv("MQTTVIEW_TRUST_PROXY_HEADERS"); ok {
		cfg.Auth.TrustProxyHeaders = v
	}
	if v := os.Getenv("MQTTVIEW_AUTH_MODE"); v != "" {
		cfg.Auth.Mode = Mode(strings.ToLower(v))
	}
	if v := os.Getenv("MQTTVIEW_INGRESS_TRUSTED_PROXIES"); v != "" {
		cfg.Auth.Ingress.TrustedProxies = splitList(v)
	}
	if v := os.Getenv("MQTTVIEW_INGRESS_DEFAULT_ROLE"); v != "" {
		cfg.Auth.Ingress.DefaultRole = strings.ToLower(v)
	}
	if v := os.Getenv("MQTTVIEW_INGRESS_ADMIN_USERS"); v != "" {
		cfg.Auth.Ingress.AdminUsers = splitList(v)
	}
	if v := os.Getenv("MQTTVIEW_INGRESS_FALLBACK_USER"); v != "" {
		cfg.Auth.Ingress.FallbackUser = v
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

// splitList reads a comma-separated environment value. The add-on's run script
// joins list options this way, because an environment variable cannot hold a
// list any other way.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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
	switch c.Auth.Mode {
	case ModeStandalone:
	case ModeIngress:
		return c.validateIngress()
	default:
		return fmt.Errorf("auth.mode %q is not one of %q or %q", c.Auth.Mode, ModeStandalone, ModeIngress)
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

// validateIngress refuses a Home Assistant configuration that would let
// anybody in. Every check here is the difference between "Home Assistant says
// who this is" and "whoever reached the port says who they are".
func (c Config) validateIngress() error {
	if len(c.Auth.Ingress.TrustedProxies) == 0 {
		return errors.New("auth.ingress.trusted_proxies must not be empty: with no trusted proxy, " +
			"anyone who can reach the port can send the identity headers themselves")
	}
	for _, p := range c.Auth.Ingress.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err == nil {
			continue
		}
		if net.ParseIP(p) == nil {
			return fmt.Errorf("auth.ingress.trusted_proxies: %q is not an IP address or CIDR", p)
		}
	}
	switch c.Auth.Ingress.DefaultRole {
	case "viewer", "operator", "admin":
	default:
		return fmt.Errorf("auth.ingress.default_role %q is not viewer, operator or admin",
			c.Auth.Ingress.DefaultRole)
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
