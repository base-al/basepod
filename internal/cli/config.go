// Package cli implements the basepod command-line client: a config/context
// store (this file), an HTTP client for BasePod's REST API (client.go), a
// deploy-context tar/gzip packer (tar.go), a minimal SSE line reader
// (sse.go), and the cobra command tree itself (commands.go).
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Context is one saved server the CLI can talk to: its base URL and the
// session token obtained from `basepod login`.
type Context struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// Config is the on-disk shape of the CLI's config file: a set of named
// contexts (server + token) and which one is active by default.
type Config struct {
	Contexts map[string]Context `yaml:"contexts"`
	Current  string             `yaml:"current"`
}

// ErrNotLoggedIn is returned by CurrentContext (and surfaces from every
// command that needs one) when the config has no current context — i.e.
// the user has never run `basepod login`, or named a --context that
// doesn't exist.
var ErrNotLoggedIn = errors.New("not logged in — run `basepod login <url>`")

// configPathEnv is the environment variable that overrides the CLI config
// file location, checked by ConfigPath before falling back to the default
// per-user path. Tests set this (via t.Setenv) to isolate the config file
// they read and write from the real user's ~/.config/basepod/cli.yaml.
const configPathEnv = "BASEPOD_CLI_CONFIG"

// ConfigPath resolves the CLI config file's location: the BASEPOD_CLI_CONFIG
// environment variable if set, otherwise ~/.config/basepod/cli.yaml (via
// os.UserConfigDir(), matching internal/config.DefaultPath's convention for
// the server's own config file).
func ConfigPath() string {
	if v := os.Getenv(configPathEnv); v != "" {
		return v
	}
	base, _ := os.UserConfigDir()
	return filepath.Join(base, "basepod", "cli.yaml")
}

// LoadConfig reads the CLI config from ConfigPath(). A missing file is not
// an error — it yields an empty Config (no contexts, no current), which
// every command that needs a context turns into ErrNotLoggedIn.
func LoadConfig() (*Config, error) {
	return LoadConfigFrom(ConfigPath())
}

// LoadConfigFrom reads the CLI config from an explicit path, bypassing
// ConfigPath's env-var resolution. Exported mainly so tests that already
// have a path in hand (rather than going through the env var) can load it
// directly.
func LoadConfigFrom(path string) (*Config, error) {
	cfg := &Config{Contexts: map[string]Context{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("cli: parse config %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// No config yet — defaults above stand.
	default:
		return nil, fmt.Errorf("cli: read config %s: %w", path, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	return cfg, nil
}

// SaveConfig writes cfg to ConfigPath(), creating its parent directory if
// needed, with file mode 0600 — the config holds a bearer session token,
// so it must not be world- or group-readable.
func SaveConfig(cfg *Config) error {
	return SaveConfigTo(ConfigPath(), cfg)
}

// SaveConfigTo writes cfg to an explicit path, bypassing ConfigPath's
// env-var resolution — the save-side counterpart to LoadConfigFrom.
//
// os.WriteFile's mode argument only takes effect when it creates the file;
// it is silently ignored by the umask-independent "file already exists"
// path, so re-saving over a pre-existing cli.yaml that somehow ended up
// world/group-readable (a stray `umask 022`, a config seeded by another
// tool) would otherwise leave the bearer token exposed at whatever mode
// the file already had. An explicit os.Chmod after every write closes that
// gap regardless of whether the file was just created or already existed.
func SaveConfigTo(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cli: create config dir: %w", err)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("cli: marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("cli: write config %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("cli: chmod config %s: %w", path, err)
	}
	return nil
}

// CurrentContext returns the named context (falling back to cfg.Current
// when name is ""), or ErrNotLoggedIn if there's no such context — either
// because none is configured yet, or an explicit --context named one that
// doesn't exist.
func (cfg *Config) CurrentContext(name string) (Context, string, error) {
	if name == "" {
		name = cfg.Current
	}
	if name == "" {
		return Context{}, "", ErrNotLoggedIn
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return Context{}, "", ErrNotLoggedIn
	}
	return ctx, name, nil
}
