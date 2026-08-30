package config

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Vertex      VertexConfig      `yaml:"vertex"`
	ClaudeOAuth ClaudeOAuthConfig `yaml:"claude_oauth"`
	Codex       CodexConfig       `yaml:"codex"`
	Kimi        KimiConfig        `yaml:"kimi"`
	Relay       RelayConfig       `yaml:"relay"`
	AnyGen      AnyGenConfig      `yaml:"anygen"`
	// Series holds the default provider order per model family, and Models the
	// per-model overrides. Together they replace the old per-provider model
	// lists: a model is named once, and the providers that can serve it are an
	// ordered chain rather than a global priority table.
	Series SeriesConfig `yaml:"series"`
	Models []ModelRoute `yaml:"models"`

	// mu guards only the ServerConfig fields the dashboard can rewrite while the
	// server is serving: Port, AdminUser, AdminPassword, TrayToken. Everything
	// else is written once by Load and read-only afterwards, so it needs no
	// synchronisation.
	//
	// Read and write those four through the accessors below, never directly —
	// a bare `cfg.Server.AdminPassword` read in a request handler races with a
	// concurrent config save (`go test -race` will say so).
	//
	// Locking is per-field, not per-save: two dashboard saves racing each other
	// interleave field-by-field rather than one winning wholesale. That is
	// acceptable for a single-admin console and much cheaper than threading a
	// transaction through the whole update path.
	mu sync.RWMutex
}

type ServerConfig struct {
	Port          int    `yaml:"port"`
	CertFile      string `yaml:"cert_file"`
	KeyFile       string `yaml:"key_file"`
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"`
	// TrayToken authenticates the desktop widget's polls of /api/tray.
	//
	// It is deliberately NOT one of the managed API keys from the Keys page:
	// those live on the traffic plane (they can spend quota and carry daily
	// limits), while /api/tray is admin-plane data — account emails, quota
	// headroom, per-key usage. One credential per plane keeps "can read my
	// dashboard" from silently implying "can spend my quota".
	//
	// Empty means the widget cannot authenticate at all; see server.TrayAuth.
	TrayToken string `yaml:"tray_token"`
	// AccountStrategy selects how a provider's accounts are picked per request:
	// "weekly_expiry" (default) — quota-aware: prefer the usable account whose
	// weekly window resets soonest, so perishable weekly budget is burned first;
	// "round_robin" — the legacy blind rotation.
	AccountStrategy string `yaml:"account_strategy"`
}

type VertexConfig struct {
	ProjectID string `yaml:"project_id"`
	Region    string `yaml:"region"`
	Enabled   bool   `yaml:"enabled"`
}

// ModelConfig is one model as a provider must be told about it: the published
// name, plus the id to use on the wire when the upstream insists on its own.
// Empty Model means "same as Name", which is the common case.
//
// It is no longer parsed from the config file — provider model lists are
// derived from the routing table — but it remains how a provider is configured
// at runtime.
type ModelConfig struct {
	Name  string
	Model string
}

type ClaudeOAuthConfig struct {
	Enabled  bool   `yaml:"enabled"`
	TokenDir string `yaml:"token_dir"`
}

type CodexConfig struct {
	Enabled bool `yaml:"enabled"`
}

// KimiConfig intentionally stores only the name of an environment variable,
// never the API key itself. This keeps config.yaml and dashboard saves free of
// upstream credentials.
type KimiConfig struct {
	Enabled   bool   `yaml:"enabled"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIFormat string `yaml:"api_format"`
}

// RelayConfig describes an Anthropic-compatible upstream authenticated with a
// static token read from the environment. Keeping only the environment variable
// name here prevents relay credentials from being persisted by dashboard saves.
type RelayConfig struct {
	Enabled      bool   `yaml:"enabled"`
	BaseURL      string `yaml:"base_url"`
	AuthTokenEnv string `yaml:"auth_token_env"`
}

// AnyGenConfig keeps the sk-ag credential outside config.yaml. Its catalog is
// discovered at runtime from the OpenAI-compatible /models endpoint.
type AnyGenConfig struct {
	Enabled   bool   `yaml:"enabled"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// KnownProviders is every provider this proxy can route to. A provider is a
// place a request can be sent; what it is allowed to serve is decided entirely
// by the model routes below.
var KnownProviders = []string{"claude_oauth", "codex", "vertex", "kimi", "relay", "anygen"}

var knownProviders = func() map[string]bool {
	m := make(map[string]bool, len(KnownProviders))
	for _, p := range KnownProviders {
		m[p] = true
	}
	return m
}()

// ProviderRef is one link in a model's provider chain: where to send the
// request, and — only when the upstream insists on a different id than the name
// we publish — what to call the model on the wire.
//
// Upstream is a connection detail, not an identity: it never reaches clients,
// pricing, or stats, all of which see the published model name.
type ProviderRef struct {
	Provider string `yaml:"provider" json:"provider"`
	Upstream string `yaml:"upstream,omitempty" json:"upstream,omitempty"`
}

// UnmarshalYAML accepts both forms a chain entry can take:
//
//	providers: [claude_oauth, relay]        # no rename
//	providers: [{vertex: claude-haiku-4-5-20251001}]
//
// The bare-string form is the common case and the map form is only reached for
// a genuine rename, which keeps the rename visually rare in the file — it is a
// quirk of one upstream, not a property of the model.
func (p *ProviderRef) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var name string
		if err := value.Decode(&name); err != nil {
			return err
		}
		p.Provider, p.Upstream = strings.TrimSpace(name), ""
		return nil
	case yaml.MappingNode:
		// Long form ({provider: vertex, upstream: x}) and the shorthand
		// ({vertex: x}) are told apart by the presence of a "provider" key.
		var long struct {
			Provider string `yaml:"provider"`
			Upstream string `yaml:"upstream"`
		}
		if err := value.Decode(&long); err == nil && strings.TrimSpace(long.Provider) != "" {
			p.Provider = strings.TrimSpace(long.Provider)
			p.Upstream = strings.TrimSpace(long.Upstream)
			return nil
		}
		var short map[string]string
		if err := value.Decode(&short); err != nil {
			return err
		}
		if len(short) != 1 {
			return fmt.Errorf("a provider entry must name exactly one provider, got %d keys", len(short))
		}
		for name, upstream := range short {
			p.Provider = strings.TrimSpace(name)
			p.Upstream = strings.TrimSpace(upstream)
		}
		return nil
	default:
		return fmt.Errorf("a provider entry must be a name or a single-key mapping")
	}
}

// MarshalYAML writes the shortest form that round-trips, so a config saved from
// the dashboard stays as readable as a hand-written one.
func (p ProviderRef) MarshalYAML() (any, error) {
	if p.Upstream == "" {
		return p.Provider, nil
	}
	return map[string]string{p.Provider: p.Upstream}, nil
}

// ModelRoute is one model as clients see it: a published name and the ordered
// providers that can serve it. Earlier providers are tried first, and the chain
// doubles as the failover order — a provider that is disabled, unconfigured, or
// out of quota hands the request to the next one.
type ModelRoute struct {
	Name      string        `yaml:"name" json:"name"`
	Providers []ProviderRef `yaml:"providers,omitempty" json:"providers,omitempty"`
}

// SeriesConfig maps a model family to the provider order its models get when
// they do not name one themselves.
type SeriesConfig map[string][]ProviderRef

// SeriesOf classifies a model name into a family. The prefixes are deliberately
// coarse: a series only supplies a default ordering, and anything it gets wrong
// is fixed by naming providers on the model itself.
func SeriesOf(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "claude-"):
		return "claude"
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "gpt"
	case strings.HasPrefix(m, "gemini-"):
		return "gemini"
	case strings.HasPrefix(m, "kimi-"):
		return "kimi"
	case strings.HasPrefix(m, "deepseek-"):
		return "deepseek"
	default:
		return "other"
	}
}

// cleanChain validates one provider chain. Duplicates are rejected rather than
// deduplicated: a chain listing the same provider twice means the author
// believed the second entry would do something, and silently dropping it would
// hide the mistake.
//
// published is the model name the chain belongs to, or "" for a series default.
// It is only used to recognise a non-rename: the dashboard prefills the upstream
// box with the model name, so most saves arrive with upstream == name.
func cleanChain(in []ProviderRef, context, published string) ([]ProviderRef, error) {
	out := make([]ProviderRef, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, ref := range in {
		name := strings.TrimSpace(ref.Provider)
		if name == "" {
			continue
		}
		if !knownProviders[name] {
			return nil, fmt.Errorf("%s: unknown provider %q", context, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s: duplicate provider %q", context, name)
		}
		seen[name] = true
		upstream := strings.TrimSpace(ref.Upstream)
		// An upstream equal to the published name is the default; storing it
		// would just be the name written twice.
		if upstream == published {
			upstream = ""
		}
		out = append(out, ProviderRef{Provider: name, Upstream: upstream})
	}
	return out, nil
}

// NormalizeRoutes validates the model table and fills each model's chain from
// its series default when it declares none.
func NormalizeRoutes(models []ModelRoute, series SeriesConfig) ([]ModelRoute, error) {
	defaults := make(SeriesConfig, len(series))
	for name, chain := range series {
		cleaned, err := cleanChain(chain, "series "+name, "")
		if err != nil {
			return nil, err
		}
		defaults[strings.TrimSpace(name)] = cleaned
	}

	out := make([]ModelRoute, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, route := range models {
		name := strings.TrimSpace(route.Name)
		if name == "" {
			return nil, fmt.Errorf("models: a route needs a model name")
		}
		if seen[name] {
			return nil, fmt.Errorf("models: duplicate model %q", name)
		}
		seen[name] = true
		chain, err := cleanChain(route.Providers, "model "+name, name)
		if err != nil {
			return nil, err
		}
		if len(chain) == 0 {
			// Copied, not referenced: a later edit to one model's chain from the
			// dashboard must not mutate every other model in the same series.
			chain = append([]ProviderRef(nil), defaults[SeriesOf(name)]...)
		}
		if len(chain) == 0 {
			return nil, fmt.Errorf("model %q has no providers and its series (%s) has no default", name, SeriesOf(name))
		}
		out = append(out, ModelRoute{Name: name, Providers: chain})
	}
	return out, nil
}

// Routes returns a copy of the current model table.
func (c *Config) Routes() []ModelRoute {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneRoutes(c.Models)
}

func cloneRoutes(in []ModelRoute) []ModelRoute {
	out := make([]ModelRoute, len(in))
	for i, route := range in {
		out[i] = ModelRoute{
			Name:      route.Name,
			Providers: append([]ProviderRef(nil), route.Providers...),
		}
	}
	return out
}

// SetRoutes replaces the model table after validating it.
func (c *Config) SetRoutes(models []ModelRoute) error {
	c.mu.RLock()
	series := c.Series
	c.mu.RUnlock()
	normalized, err := NormalizeRoutes(models, series)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.Models = normalized
	c.mu.Unlock()
	return nil
}

// SeriesDefaults returns a copy of the per-series provider defaults.
func (c *Config) SeriesDefaults() SeriesConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(SeriesConfig, len(c.Series))
	for name, chain := range c.Series {
		out[name] = append([]ProviderRef(nil), chain...)
	}
	return out
}

// SetSeries replaces the per-series defaults. Existing models keep the chain
// they already resolved to: a series default only applies to a model that
// declares no providers of its own, and NormalizeRoutes has already filled
// those in.
func (c *Config) SetSeries(series SeriesConfig) {
	out := make(SeriesConfig, len(series))
	for name, chain := range series {
		out[name] = append([]ProviderRef(nil), chain...)
	}
	c.mu.Lock()
	c.Series = out
	c.mu.Unlock()
}

// ProviderEnabled reports whether a provider is switched on in the config file.
// This is static configuration; the runtime pause switch lives in the token
// store and is checked separately by the router.
func (c *Config) ProviderEnabled(provider string) bool {
	switch provider {
	case "claude_oauth":
		return c.ClaudeOAuth.Enabled
	case "codex":
		return c.Codex.Enabled
	case "vertex":
		return c.Vertex.Enabled
	case "kimi":
		return c.Kimi.Enabled
	case "relay":
		return c.Relay.Enabled
	case "anygen":
		return c.AnyGen.Enabled
	default:
		return false
	}
}

// Environment fallbacks for the dashboard credentials. Containerised deploys
// inject secrets as env vars instead of editing a mounted YAML, and shipping
// config.example.yaml with these commented out is exactly how an install ends up
// with no credentials at all.
const (
	EnvAdminUser     = "LLM_PROXY_ADMIN_USER"
	EnvAdminPassword = "LLM_PROXY_ADMIN_PASSWORD"
)

// AdminConfigured reports whether dashboard login is usable at all. Both halves
// must be set: a blank half would otherwise be matched by a blank form field.
func (c *Config) AdminConfigured() bool {
	user, password := c.AdminCreds()
	return user != "" && password != ""
}

// AdminCreds returns the dashboard login credentials.
func (c *Config) AdminCreds() (user, password string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Server.AdminUser, c.Server.AdminPassword
}

// SetAdminCreds updates the dashboard login credentials, ignoring empty values.
// "" means "keep current" here because the dashboard's password box is never
// prefilled — an empty box means the admin did not touch it.
func (c *Config) SetAdminCreds(user, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if user != "" {
		c.Server.AdminUser = user
	}
	if password != "" {
		c.Server.AdminPassword = password
	}
}

// TrayToken returns the desktop widget's credential ("" = widget locked out).
func (c *Config) TrayToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Server.TrayToken
}

// SetTrayToken stores the value verbatim, including "" — unlike the admin
// password, the dashboard prefills this box with the current token, so a
// cleared box is a deliberate "revoke it". Callers that mean "leave unchanged"
// must not call this at all.
func (c *Config) SetTrayToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Server.TrayToken = token
}

// Port returns the configured listen port. The running listener keeps its
// original port until restart; this is the value a later restart will use.
func (c *Config) Port() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Server.Port
}

func (c *Config) SetPort(port int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Server.Port = port
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	routes, err := NormalizeRoutes(cfg.Models, cfg.Series)
	if err != nil {
		return nil, err
	}
	cfg.Models = routes
	// The config file wins when both are present — it is the more explicit of the
	// two, and silently preferring an inherited env var would make a checked-in
	// value lie about what the server is actually using.
	if cfg.Server.AdminUser == "" {
		cfg.Server.AdminUser = os.Getenv(EnvAdminUser)
	}
	if cfg.Server.AdminPassword == "" {
		cfg.Server.AdminPassword = os.Getenv(EnvAdminPassword)
	}
	return &cfg, nil
}

// Save writes cfg back to path as YAML. The write is atomic (temp file + rename)
// so a crash mid-write never leaves a truncated config. Note: this re-marshals
// the whole struct, so any comments in the original file are lost.
//
// Marshalling reads every field, including the runtime-mutable ones, so it takes
// the read lock. Callers must not already hold it.
func Save(path string, cfg *Config) error {
	cfg.mu.RLock()
	data, err := yaml.Marshal(cfg)
	cfg.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
