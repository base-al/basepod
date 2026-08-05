package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadConfigMissingFileYieldsEmptyDefaults(t *testing.T) {
	cfg, err := LoadConfigFrom(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Current != "" {
		t.Fatalf("Current = %q, want empty", cfg.Current)
	}
	if cfg.Contexts == nil || len(cfg.Contexts) != 0 {
		t.Fatalf("Contexts = %#v, want empty non-nil map", cfg.Contexts)
	}
}

func TestSaveLoadConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cli.yaml")
	cfg := &Config{
		Contexts: map[string]Context{
			"prod": {URL: "https://basepod.example.com", Token: "tok-1"},
			"dev":  {URL: "http://localhost:8080", Token: "tok-2"},
		},
		Current: "prod",
	}
	if err := SaveConfigTo(path, cfg); err != nil {
		t.Fatalf("SaveConfigTo: %v", err)
	}

	got, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if got.Current != "prod" {
		t.Fatalf("Current = %q, want prod", got.Current)
	}
	if len(got.Contexts) != 2 {
		t.Fatalf("Contexts = %#v, want 2 entries", got.Contexts)
	}
	if got.Contexts["prod"] != cfg.Contexts["prod"] {
		t.Fatalf("prod context = %+v, want %+v", got.Contexts["prod"], cfg.Contexts["prod"])
	}
	if got.Contexts["dev"] != cfg.Contexts["dev"] {
		t.Fatalf("dev context = %+v, want %+v", got.Contexts["dev"], cfg.Contexts["dev"])
	}
}

func TestSaveConfigFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits aren't meaningful on windows")
	}
	path := filepath.Join(t.TempDir(), "cli.yaml")
	cfg := &Config{Contexts: map[string]Context{"prod": {URL: "https://x", Token: "t"}}, Current: "prod"}
	if err := SaveConfigTo(path, cfg); err != nil {
		t.Fatalf("SaveConfigTo: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}
}

func TestConfigPathEnvOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-cli.yaml")
	t.Setenv(configPathEnv, custom)
	if got := ConfigPath(); got != custom {
		t.Fatalf("ConfigPath() = %q, want %q", got, custom)
	}

	cfg := &Config{Contexts: map[string]Context{"x": {URL: "u", Token: "t"}}, Current: "x"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("SaveConfig did not write to env-overridden path: %v", err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Current != "x" {
		t.Fatalf("Current = %q, want x", got.Current)
	}
}

func TestCurrentContextNotLoggedIn(t *testing.T) {
	cfg := &Config{Contexts: map[string]Context{}}
	if _, _, err := cfg.CurrentContext(""); err != ErrNotLoggedIn {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}

	cfg2 := &Config{Contexts: map[string]Context{"a": {URL: "u"}}, Current: "a"}
	if _, _, err := cfg2.CurrentContext("missing"); err != ErrNotLoggedIn {
		t.Fatalf("err = %v, want ErrNotLoggedIn for unknown --context name", err)
	}
}

func TestCurrentContextResolvesNameOverCurrent(t *testing.T) {
	cfg := &Config{
		Contexts: map[string]Context{
			"prod": {URL: "https://prod", Token: "p"},
			"dev":  {URL: "http://dev", Token: "d"},
		},
		Current: "prod",
	}
	ctx, name, err := cfg.CurrentContext("dev")
	if err != nil {
		t.Fatalf("CurrentContext(dev): %v", err)
	}
	if name != "dev" || ctx.URL != "http://dev" {
		t.Fatalf("got (%q, %+v), want (dev, http://dev)", name, ctx)
	}

	ctx, name, err = cfg.CurrentContext("")
	if err != nil {
		t.Fatalf("CurrentContext(\"\"): %v", err)
	}
	if name != "prod" || ctx.URL != "https://prod" {
		t.Fatalf("got (%q, %+v), want (prod, https://prod)", name, ctx)
	}
}
