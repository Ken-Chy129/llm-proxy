package config

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/pricing"
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
	Routing     RoutingConfig     `yaml:"routing"`
	Pricing     PricingConfig     `yaml:"pricing"`

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
	ProjectID string        `yaml:"project_id"`
	Region    string        `yaml:"region"`
	Models    []ModelConfig `yaml:"models"`
}

// ModelConfig is one served model. Model is the upstream id to call; empty
// means "same as Name", which is the common case — writing the name twice is
// noise, and omitempty keeps it out of the file the dashboard rewrites.
type ModelConfig struct {
	Name  string `yaml:"name"`
	Model string `yaml:"model,omitempty"`
}

type ClaudeOAuthConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Models   []string `yaml:"models"`
	TokenDir string   `yaml:"token_dir"`
}

type CodexConfig struct {
	Enabled bool     `yaml:"enabled"`
	Models  []string `yaml:"models"`
}

// KimiConfig intentionally stores only the name of an environment variable,
// never the API key itself. This keeps config.yaml and dashboard saves free of
// upstream credentials.
type KimiConfig struct {
	Enabled   bool          `yaml:"enabled"`
	BaseURL   string        `yaml:"base_url"`
	APIKeyEnv string        `yaml:"api_key_env"`
	APIFormat string        `yaml:"api_format"`
	Models    []ModelConfig `yaml:"models"`
}

// RelayConfig describes an Anthropic-compatible upstream authenticated with a
// static token read from the environment. Keeping only the environment variable
// name here prevents relay credentials from being persisted by dashboard saves.
type RelayConfig struct {
	Enabled      bool          `yaml:"enabled"`
	BaseURL      string        `yaml:"base_url"`
	AuthTokenEnv string        `yaml:"auth_token_env"`
	Models       []ModelConfig `yaml:"models"`
}

// AnyGenConfig keeps the sk-ag credential outside config.yaml. Models are a
// startup fallback only: a configured backend replaces them with the zero-cost
// model list returned by AnyGen's OpenAI-compatible /models endpoint.
type AnyGenConfig struct {
	Enabled   bool     `yaml:"enabled"`
	BaseURL   string   `yaml:"base_url"`
	APIKeyEnv string   `yaml:"api_key_env"`
	Models    []string `yaml:"models"`
}

type RoutingConfig struct {
	// BackendPriority resolves models exposed by multiple backends. Earlier
	// entries win; disabled and protocol-incompatible entries are skipped.
	BackendPriority []string `yaml:"backend_priority"`
}

var DefaultBackendPriority = []string{
	"claude",
	"codex",
	"vertex",
	"kimi",
	"anygen",
	"relay",
}

var knownBackends = map[string]bool{
	"claude": true,
	"codex":  true,
	"vertex": true,
	"kimi":   true,
	"anygen": true,
	"relay":  true,
}

// NormalizeBackendPriority validates an external priority list and appends
// omitted known backends in the stable default order. Partial configuration is
// therefore safe and forward-compatible with the existing config files.
func NormalizeBackendPriority(in []string) ([]string, error) {
	out := make([]string, 0, len(DefaultBackendPriority))
	seen := make(map[string]bool, len(DefaultBackendPriority))
	for _, backend := range in {
		backend = strings.TrimSpace(backend)
		if backend == "" {
			continue
		}
		if !knownBackends[backend] {
			return nil, fmt.Errorf("unknown backend %q", backend)
		}
		if seen[backend] {
			return nil, fmt.Errorf("duplicate backend %q", backend)
		}
		seen[backend] = true
		out = append(out, backend)
	}
	for _, backend := range DefaultBackendPriority {
		if !seen[backend] {
			out = append(out, backend)
		}
	}
	return out, nil
}

// PricingConfig overrides or extends the built-in per-model price table used to
// cost requests. Prices are USD per 1M tokens.
//
// Two reasons this exists: published rates change and a rebuild is a silly way
// to track them, and a model this proxy serves may not be in the built-in table
// at all (a private endpoint, a subscription seat, a renamed alias). A model
// with no price anywhere is recorded as *unknown* cost rather than $0 — write
// an all-zeros entry here to say "this one really is free".
type PricingConfig struct {
	Models []pricing.Price `yaml:"models"`
}

// ModelAliases returns the alias → upstream-model mapping for every backend that
// has one (Vertex, Kimi and Relay; OAuth backends pass names through unchanged).
// Pricing uses it so a freely-named alias still resolves to its model's price.
func (c *Config) ModelAliases() map[string]string {
	out := make(map[string]string, len(c.Vertex.Models)+len(c.Kimi.Models)+len(c.Relay.Models))
	for _, list := range [][]ModelConfig{c.Vertex.Models, c.Kimi.Models, c.Relay.Models} {
		for _, m := range list {
			if m.Name != "" && m.Model != "" {
				out[m.Name] = m.Model
			}
		}
	}
	return out
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

func (c *Config) BackendPriority() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.Routing.BackendPriority...)
}

func (c *Config) SetBackendPriority(priority []string) error {
	normalized, err := NormalizeBackendPriority(priority)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.Routing.BackendPriority = normalized
	c.mu.Unlock()
	return nil
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
	priority, err := NormalizeBackendPriority(cfg.Routing.BackendPriority)
	if err != nil {
		return nil, fmt.Errorf("routing.backend_priority: %w", err)
	}
	cfg.Routing.BackendPriority = priority
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
