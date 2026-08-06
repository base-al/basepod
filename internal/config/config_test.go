package config

import (
	"os"
	"path/filepath"
	"strings"
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

// TestValidateListenLoopbackForms proves every accepted loopback spelling
// (audit finding M5's "127.0.0.0/8, ::1, or localhost") passes without
// BASEPOD_ALLOW_PUBLIC_LISTEN set.
func TestValidateListenLoopbackForms(t *testing.T) {
	cases := []string{
		"127.0.0.1:3080",
		"127.0.0.1:0",
		"127.5.6.7:3080", // still 127.0.0.0/8
		"localhost:3080",
		"LOCALHOST:3080", // case-insensitive
		"[::1]:3080",
	}
	for _, listen := range cases {
		t.Run(listen, func(t *testing.T) {
			if err := validateListen(listen); err != nil {
				t.Errorf("validateListen(%q) = %v, want nil", listen, err)
			}
		})
	}
}

// TestValidateListenRejectsPublicWithoutEnv proves a non-loopback listen
// address is refused by default — the core of audit finding M5 — with an
// error naming the dashboard's TLS-terminated route as the supported
// public path.
func TestValidateListenRejectsPublicWithoutEnv(t *testing.T) {
	cases := []string{
		"0.0.0.0:3080",
		":3080",         // no host at all — binds every interface
		"10.0.0.5:3080", // a real LAN address
		"[::]:3080",     // all IPv6 interfaces
		"example.com:3080",
	}
	for _, listen := range cases {
		t.Run(listen, func(t *testing.T) {
			err := validateListen(listen)
			if err == nil {
				t.Fatalf("validateListen(%q) = nil, want an error", listen)
			}
			if !strings.Contains(err.Error(), "dashboard") {
				t.Errorf("validateListen(%q) error = %v, want it to name the dashboard route", listen, err)
			}
		})
	}
}

// TestValidateListenAllowsPublicWithEnv proves BASEPOD_ALLOW_PUBLIC_LISTEN=1
// is the documented escape hatch, and that unrelated/unset values don't
// accidentally also open it.
func TestValidateListenAllowsPublicWithEnv(t *testing.T) {
	t.Setenv(allowPublicListenEnv, "1")
	if err := validateListen("0.0.0.0:3080"); err != nil {
		t.Errorf("validateListen with %s=1 = %v, want nil", allowPublicListenEnv, err)
	}
}

func TestValidateListenEnvMustBeExactlyOne(t *testing.T) {
	for _, v := range []string{"true", "yes", "0", ""} {
		t.Setenv(allowPublicListenEnv, v)
		if err := validateListen("0.0.0.0:3080"); err == nil {
			t.Errorf("validateListen with %s=%q = nil, want an error (only exactly \"1\" should bypass the guard)", allowPublicListenEnv, v)
		}
	}
}

// TestLoadRejectsPublicListen proves the guard is actually wired into
// Load (not just validateListen in isolation): a config file that sets a
// public listen address makes Load fail.
func TestLoadRejectsPublicListen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("listen: 0.0.0.0:3080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("Load with a public listen address = nil error, want a rejection")
	}

	t.Setenv(allowPublicListenEnv, "1")
	if _, err := Load(p); err != nil {
		t.Fatalf("Load with a public listen address and %s=1 = %v, want nil", allowPublicListenEnv, err)
	}
}
