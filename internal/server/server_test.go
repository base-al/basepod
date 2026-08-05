package server

import (
	"context"
	"net/http"
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

// TestNewHTTPServerTimeouts proves newHTTPServer sets the two bounded
// timeouts (ReadHeaderTimeout, IdleTimeout) and leaves the two
// unbounded ones (ReadTimeout, WriteTimeout) at their zero value — see
// newHTTPServer's doc comment for why those two are deliberately unset.
func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())

	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, idleTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (unset — see newHTTPServer's doc comment)", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (unset — see newHTTPServer's doc comment)", srv.WriteTimeout)
	}
}

// TestCheckPodmanVersion proves the podman version gate accepts
// >= 4.9(.x), with or without a "-dev"-style suffix, and rejects
// anything older with an error naming the found version — tested
// directly against literal version strings (as GET /version's parsed
// "Version" field would be), rather than through Run against a fake
// daemon.
func TestCheckPodmanVersion(t *testing.T) {
	cases := []struct {
		version string
		wantErr bool
	}{
		{"4.9.0", false},
		{"4.9", false},
		{"4.9.3", false},
		{"4.10.0", false},
		{"5.0.0", false},
		{"5.0.0-dev", false},
		{"4.9.0-dev", false},
		{"4.8.0", true},
		{"4.8.9", true},
		{"3.4.4", true},
		{"1.0.0", true},
	}
	for _, tc := range cases {
		err := checkPodmanVersion(tc.version)
		if tc.wantErr {
			if err == nil {
				t.Errorf("version %q: expected an error, got nil", tc.version)
				continue
			}
			if !strings.Contains(err.Error(), tc.version) {
				t.Errorf("version %q: expected error to name the found version, got: %v", tc.version, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("version %q: expected no error, got: %v", tc.version, err)
		}
	}
}

// TestCheckPodmanVersionUnparseable proves a malformed version string
// (not even a dotted major.minor) is reported as an error rather than
// panicking or silently passing the gate.
func TestCheckPodmanVersionUnparseable(t *testing.T) {
	for _, v := range []string{"", "notaversion", "4"} {
		if err := checkPodmanVersion(v); err == nil {
			t.Errorf("version %q: expected a parse error, got nil", v)
		}
	}
}
