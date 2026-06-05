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
}

type ServerConfig struct {
	Port          int    `yaml:"port"`
	APIKey        string `yaml:"api_key"`
	CertFile      string `yaml:"cert_file"`
	KeyFile       string `yaml:"key_file"`
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"`
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
