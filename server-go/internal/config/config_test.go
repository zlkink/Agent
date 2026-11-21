package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A fresh account has no ~/.prometheus/config.json; both reading and saving a
// gateway must work without one.
func TestMissingConfigIsEmptyAndCreatable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("PROMETHEUS_HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Gateways) != 0 {
		t.Fatalf("gateways = %d, want 0", len(cfg.Gateways))
	}
	gws, err := UpsertGateway(map[string]any{
		"id": "test", "name": "Test", "format": "anthropic-messages", "baseUrl": "https://example.test",
	})
	if err != nil {
		t.Fatalf("UpsertGateway: %v", err)
	}
	if len(gws) != 1 || gws[0].ID != "test" {
		t.Fatalf("gateways = %#v", gws)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Fatalf("config.json was not written: %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if len(cfg.Gateways) != 1 || cfg.Gateways[0].BaseURL != "https://example.test" {
		t.Fatalf("gateways = %#v", cfg.Gateways)
	}
}
