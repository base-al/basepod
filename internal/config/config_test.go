package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("missing file must yield defaults, got %v", err)
	}
	if cfg.Listen != "127.0.0.1:3080" || cfg.HTTPPort != 80 || cfg.HTTPSPort != 443 {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.DataDir == "" {
		t.Fatal("DataDir default empty")
	}
}

func TestLoadFileAndEnvOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("root_domain: apps.example.com\nhttp_port: 8080\n"), 0o600)
	t.Setenv("BASEPOD_LISTEN", "127.0.0.1:9999")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootDomain != "apps.example.com" || cfg.HTTPPort != 8080 {
		t.Fatalf("yaml not applied: %+v", cfg)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("env override not applied: %+v", cfg)
	}
}
