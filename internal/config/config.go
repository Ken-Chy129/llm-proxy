package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Vertex      VertexConfig      `yaml:"vertex"`
	ClaudeOAuth ClaudeOAuthConfig `yaml:"claude_oauth"`
	Codex       CodexConfig       `yaml:"codex"`
	Kimi        KimiConfig        `yaml:"kimi"`
}

type ServerConfig struct {
	Port          int    `yaml:"port"`
	CertFile      string `yaml:"cert_file"`
	KeyFile       string `yaml:"key_file"`
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"`
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

type ModelConfig struct {
	Name  string `yaml:"name"`
	Model string `yaml:"model"`
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
	return &cfg, nil
}

// Save writes cfg back to path as YAML. The write is atomic (temp file + rename)
// so a crash mid-write never leaves a truncated config. Note: this re-marshals
// the whole struct, so any comments in the original file are lost.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
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
