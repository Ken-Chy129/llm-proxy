package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestMagpieConfigRoundTrip(t *testing.T) {
	cfg, err := loadYAML(t, `
magpie:
  enabled: true
  base_url: https://axon.ponte.sh/v1
  api_key_env: MAGPIE_API_KEY
  models: [claude/claude-opus-5-5]
models:
  - name: claude-opus-5-5
    providers:
      - magpie: claude/claude-opus-5-5
      - relay
`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProviderEnabled("magpie") || len(cfg.ProviderModels("magpie")) != 1 {
		t.Fatal("Magpie connection/catalog missing")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Routes(), reloaded.Routes()) ||
		!reflect.DeepEqual(cfg.Magpie, reloaded.Magpie) {
		t.Fatal("Magpie connection or upstream rename lost on save")
	}
}
