package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/store"
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

// TestPruneSessionsRemovesExpired proves pruneSessions's ticking loop
// actually calls through to store.PruneExpiredSessions (removing only
// expired rows) once its initial delay elapses, and that the goroutine
// exits promptly once ctx is canceled — driven with millisecond-scale
// durations rather than the real hourly interval Run uses.
func TestPruneSessionsRemovesExpired(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	uid, err := st.CreateUser("a@b.c", "A", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(uid, "expired", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(uid, "live", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pruneSessions(ctx, st, 10*time.Millisecond, time.Hour)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := st.UserBySessionTokenHash("expired"); err == store.ErrNotFound {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("expired session was not pruned in time")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := st.UserBySessionTokenHash("live"); err != nil {
		t.Fatalf("live session was pruned: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pruneSessions did not stop after ctx cancellation")
	}
}

// TestResolveDashboardDomain proves the dashboard_domain setting's three
// meaningful shapes are interpreted correctly: unset ("") computes and
// requests persisting a "basepod.<rootDomain>" default, the literal "off"
// disables the route (empty domain, nothing to persist), and any other
// value passes through unchanged as an operator override.
func TestResolveDashboardDomain(t *testing.T) {
	cases := []struct {
		name          string
		current       string
		rootDomain    string
		wantDomain    string
		wantWriteFlag bool
	}{
		{"unset computes default", "", "apps.example.com", "basepod.apps.example.com", true},
		{"off disables", "off", "apps.example.com", "", false},
		{"explicit override passes through", "dash.example.net", "apps.example.com", "dash.example.net", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDomain, gotWrite := resolveDashboardDomain(tc.current, tc.rootDomain)
			if gotDomain != tc.wantDomain || gotWrite != tc.wantWriteFlag {
				t.Errorf("resolveDashboardDomain(%q, %q) = (%q, %v), want (%q, %v)",
					tc.current, tc.rootDomain, gotDomain, gotWrite, tc.wantDomain, tc.wantWriteFlag)
			}
		})
	}
}

// shortTempDir returns a freshly created temp directory with a short
// path, cleaned up via t.Cleanup. Unlike t.TempDir() (which nests under
// this package's test binary path plus the test's own name, easily
// exceeding 100+ characters for a name like
// TestPrepareDashboardListenerBindsAndChmods), this is safe to build a
// unix socket path under: macOS enforces a ~104-byte sun_path limit
// (Linux's is a little more forgiving, at 108), and a long test name
// nested under TempDir() reliably blows past it, failing the bind with
// "invalid argument" for reasons that have nothing to do with the
// behavior under test.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestPrepareDashboardListenerBindsAndChmods proves a fresh socket path
// binds successfully and ends up with dashboardSockPerm (0660: owner+group
// read/write, no "other" access) — the defense-in-depth restriction on
// top of the bind mount itself that only bp-caddy's container can ever
// see this file (see DashboardSockMountDest's doc comment).
func TestPrepareDashboardListenerBindsAndChmods(t *testing.T) {
	sockPath := filepath.Join(shortTempDir(t), "api.sock")

	l, reason := prepareDashboardListener(sockPath)
	if reason != "" {
		t.Fatalf("prepareDashboardListener: unexpected failure reason %q", reason)
	}
	if l == nil {
		t.Fatal("prepareDashboardListener: listener is nil despite empty failReason")
	}
	t.Cleanup(func() { _ = l.Close() })

	if l.Addr().Network() != "unix" || l.Addr().String() != sockPath {
		t.Errorf("listener address = %s %s, want unix %s", l.Addr().Network(), l.Addr().String(), sockPath)
	}

	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != dashboardSockPerm {
		t.Errorf("socket mode = %o, want %o", perm, dashboardSockPerm)
	}
}

// TestPrepareDashboardListenerRemovesStaleSocket proves a leftover file at
// sockPath (as an unclean shutdown might leave behind) doesn't block a
// fresh bind — net.Listen("unix", ...) would otherwise fail with "address
// already in use" against it.
func TestPrepareDashboardListenerRemovesStaleSocket(t *testing.T) {
	sockPath := filepath.Join(shortTempDir(t), "api.sock")
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, reason := prepareDashboardListener(sockPath)
	if reason != "" {
		t.Fatalf("prepareDashboardListener: unexpected failure reason %q", reason)
	}
	t.Cleanup(func() { _ = l.Close() })
}

// TestPrepareDashboardListenerFailsOnBadPath proves a sockPath whose
// parent directory doesn't exist fails through the string-reason path
// (nil listener, non-empty reason naming the path) rather than panicking
// or returning a usable listener.
func TestPrepareDashboardListenerFailsOnBadPath(t *testing.T) {
	sockPath := filepath.Join(shortTempDir(t), "does-not-exist", "api.sock")

	l, reason := prepareDashboardListener(sockPath)
	if l != nil {
		t.Fatalf("prepareDashboardListener: expected nil listener, got %v", l)
	}
	if reason == "" || !strings.Contains(reason, sockPath) {
		t.Errorf("prepareDashboardListener: reason = %q, want it to name %q", reason, sockPath)
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
