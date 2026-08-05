package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen       string `yaml:"listen"`
	RootDomain   string `yaml:"root_domain"`
	DataDir      string `yaml:"data_dir"`
	PodmanSocket string `yaml:"podman_socket"`
	HTTPPort     int    `yaml:"http_port"`
	HTTPSPort    int    `yaml:"https_port"`
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
	return cfg, nil
}
