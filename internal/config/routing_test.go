package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadDefaultsBackendPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := []string{"claude", "codex", "vertex", "kimi", "anygen", "relay"}
	if got := cfg.BackendPriority(); !slices.Equal(got, want) {
		t.Fatalf("BackendPriority() = %v, want %v", got, want)
	}
}

func TestLoadAppendsMissingBackendsToConfiguredPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("routing:\n  backend_priority: [claude, anygen, relay]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := []string{"claude", "anygen", "relay", "codex", "vertex", "kimi"}
	if got := cfg.BackendPriority(); !slices.Equal(got, want) {
		t.Fatalf("BackendPriority() = %v, want %v", got, want)
	}
}

func TestLoadRejectsInvalidBackendPriority(t *testing.T) {
	for name, yaml := range map[string]string{
		"duplicate": "routing:\n  backend_priority: [claude, claude]\n",
		"unknown":   "routing:\n  backend_priority: [claude, mystery]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load() succeeded, want validation error")
			}
		})
	}
}
