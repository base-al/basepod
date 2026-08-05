package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunFailsFastWithoutSetup verifies that Run refuses to start against a
// data dir with no admin user, and does so before it ever needs podman:
// the config here carries no root_domain and no podman_socket, so if Run
// reached either of those checks first the test would fail for the wrong
// reason instead of surfacing the "basepod setup" message.
func TestRunFailsFastWithoutSetup(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.yaml")

	cfgYAML := "data_dir: " + dataDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Run(context.Background(), cfgPath)
	if err == nil {
		t.Fatal("expected Run to error against an unsetup data dir, got nil")
	}
	if !strings.Contains(err.Error(), "basepod setup") {
		t.Fatalf("expected error mentioning `basepod setup`, got: %v", err)
	}
}
