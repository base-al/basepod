package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/config"
	"github.com/base-al/basepod/internal/store"
)

func TestSetupCreatesConfigAndAdmin(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	err := Setup(cfgPath, SetupOptions{
		RootDomain:    "apps.localhost",
		AdminEmail:    "op@example.com",
		AdminPassword: "hunter22",
		DataDir:       filepath.Join(dir, "data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.RootDomain != "apps.localhost" {
		t.Fatalf("config not written: %+v", cfg)
	}
	s, _ := store.Open(filepath.Join(dir, "data", "basepod.db"))
	defer s.Close()
	u, err := s.UserByEmail("op@example.com")
	if err != nil || !u.IsSuperadmin {
		t.Fatalf("admin missing: %v", err)
	}
	if !auth.VerifyPassword("hunter22", u.PasswordHash) {
		t.Fatal("password hash wrong")
	}
	if err := Setup(cfgPath, SetupOptions{
		RootDomain:    "apps.localhost",
		AdminEmail:    "op@example.com",
		AdminPassword: "hunter22",
		DataDir:       filepath.Join(dir, "data"),
	}); err == nil {
		t.Fatal("second setup must error")
	}
}

func TestSetupCleansUpConfigOnDatabaseFailure(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dataDirPath := filepath.Join(dir, "data")

	// Create a FILE at the data dir path so store.Open will fail
	if err := os.WriteFile(dataDirPath, []byte("blocking file"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Setup(cfgPath, SetupOptions{
		RootDomain:    "apps.localhost",
		AdminEmail:    "op@example.com",
		AdminPassword: "hunter22",
		DataDir:       dataDirPath,
	})
	if err == nil {
		t.Fatal("expected Setup to error when data dir is blocked")
	}

	// Config file should NOT exist after failure
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatal("config file should be removed on failure")
	}
}
