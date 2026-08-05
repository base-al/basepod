package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/store"
)

// SetupOptions holds parameters for the Setup command.
type SetupOptions struct {
	RootDomain    string
	AdminEmail    string
	AdminPassword string
	DataDir       string
}

// Setup initializes a new BasePod installation: writes config.yaml, creates the database,
// and registers a superadmin user. It errors if the config already exists or if any users exist.
func Setup(cfgPath string, opts SetupOptions) error {
	// Check if config already exists
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("config file already exists at %s", cfgPath)
	}

	// Validate inputs
	if opts.RootDomain == "" {
		return fmt.Errorf("--root-domain is required")
	}
	if opts.AdminEmail == "" {
		return fmt.Errorf("--admin-email is required")
	}
	if !strings.Contains(opts.AdminEmail, "@") {
		return fmt.Errorf("--admin-email must be a valid email address")
	}
	if opts.AdminPassword == "" {
		return fmt.Errorf("--admin-password is required")
	}
	if len(opts.AdminPassword) < 8 {
		return fmt.Errorf("--admin-password must be at least 8 characters")
	}
	if opts.DataDir == "" {
		return fmt.Errorf("--data-dir is required")
	}

	// Open the store (creates database if needed)
	dbPath := filepath.Join(opts.DataDir, "basepod.db")
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	defer s.Close()

	// Check if any users already exist
	count, err := s.CountUsers()
	if err != nil {
		return fmt.Errorf("failed to check users: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("admin user already exists")
	}

	// Hash the admin password
	hash, err := auth.HashPassword(opts.AdminPassword)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Create the superadmin user
	if _, err := s.CreateUser(opts.AdminEmail, opts.AdminEmail, hash, true); err != nil {
		return fmt.Errorf("failed to create admin user: %w", err)
	}

	// Set the root_domain setting
	if err := s.SetSetting("root_domain", opts.RootDomain); err != nil {
		return fmt.Errorf("failed to set root_domain: %w", err)
	}

	// Write config.yaml with only root_domain and data_dir
	cfgToWrite := struct {
		RootDomain string `yaml:"root_domain"`
		DataDir    string `yaml:"data_dir"`
	}{
		RootDomain: opts.RootDomain,
		DataDir:    opts.DataDir,
	}

	// Create parent directories if needed
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Marshal and write config file
	data, err := yaml.Marshal(cfgToWrite)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}
