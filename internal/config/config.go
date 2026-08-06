package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen       string `yaml:"listen"`
	RootDomain   string `yaml:"root_domain"`
	DataDir      string `yaml:"data_dir"`
	PodmanSocket string `yaml:"podman_socket"`
	HTTPPort     int    `yaml:"http_port"`
	HTTPSPort    int    `yaml:"https_port"`
	// GitPath overrides internal/gitsource.BinPath's "git" binary
	// resolution (config override, else $BASEPOD_GIT_BIN, else $PATH —
	// see that function's doc comment) — an operator's explicit path,
	// for the rare case git isn't on the control-plane process's $PATH.
	// Empty means "resolve normally"; git being entirely unavailable
	// only disables git push-to-deploy (warn-only at boot), never the
	// image/tarball deploy paths.
	GitPath string `yaml:"git_path"`
}

func DefaultPath() string {
	base, _ := os.UserConfigDir() // ~/Library/Application Support (mac), ~/.config (linux)
	return filepath.Join(base, "basepod", "config.yaml")
}

func defaultDataDir() string {
	base, _ := os.UserConfigDir()
	if home, err := os.UserHomeDir(); err == nil {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "basepod")
		}
		if _, err := os.Stat(filepath.Join(home, ".local", "share")); err == nil {
			return filepath.Join(home, ".local", "share", "basepod")
		}
	}
	return filepath.Join(base, "basepod", "data")
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Listen:    "127.0.0.1:3080",
		DataDir:   defaultDataDir(),
		HTTPPort:  80,
		HTTPSPort: 443,
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	if v := os.Getenv("BASEPOD_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("BASEPOD_ROOT_DOMAIN"); v != "" {
		cfg.RootDomain = v
	}
	if v := os.Getenv("BASEPOD_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("BASEPOD_PODMAN_SOCKET"); v != "" {
		cfg.PodmanSocket = v
	}
	if v := os.Getenv("BASEPOD_GIT_PATH"); v != "" {
		cfg.GitPath = v
	}
	if v := os.Getenv("BASEPOD_HTTP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.HTTPPort = n
		}
	}
	if v := os.Getenv("BASEPOD_HTTPS_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.HTTPSPort = n
		}
	}
	if err := validateListen(cfg.Listen); err != nil {
		return nil, err
	}
	return cfg, nil
}

// allowPublicListenEnv is the escape hatch validateListen honors: an
// operator who has deliberately decided to expose the control-plane API
// itself (not just the dashboard route) directly to a non-loopback
// interface sets this to "1" to bypass the guard.
const allowPublicListenEnv = "BASEPOD_ALLOW_PUBLIC_LISTEN"

// validateListen refuses a non-loopback listen address (audit finding
// M5) unless allowPublicListenEnv is set to "1": binding the control
// plane's raw API listener to a public interface means every request —
// login included — travels in plaintext HTTP with no TLS termination,
// since BasePod's only supported public-facing path is the dashboard
// route (Caddy, TLS-terminated, proxying to the unix-socket listener —
// see internal/server.Run). A hostname other than "localhost" is treated
// as non-loopback (conservatively; a DNS name might resolve to a
// loopback address today and something else tomorrow) rather than
// resolved and checked, so this never depends on network access or DNS
// state at startup.
func validateListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// Not a valid host:port at all — could still be a bare host with
		// no port; treat the whole string as the host for the loopback
		// check rather than failing validation here, since Load isn't the
		// place to diagnose a malformed listen address (the http.Server
		// bind itself will report that clearly enough).
		host = listen
	}
	if isLoopbackHost(host) {
		return nil
	}
	if os.Getenv(allowPublicListenEnv) == "1" {
		return nil
	}
	return fmt.Errorf(
		"config: listen=%q binds a non-loopback address, which would expose the control-plane API directly in plaintext HTTP; "+
			"the supported way to reach BasePod remotely is the TLS-terminated dashboard route (see the README's Remote access section), "+
			"not this listener — set %s=1 if you really intend to expose it directly",
		listen, allowPublicListenEnv,
	)
}

// isLoopbackHost reports whether host (as split from a listen address by
// net.SplitHostPort, or the raw address if that failed) refers to the
// loopback interface: 127.0.0.0/8, ::1, or the literal name "localhost".
// An empty host (e.g. the listen address ":3080", which binds every
// interface) and any other hostname are NOT loopback.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
