package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/base-al/basepod/internal/build"
)

// TestHandleDeleteAppRemovesDataDir proves a successful DELETE
// /apps/{slug} removes <dataDir>/apps/<slug> from disk (build spool
// artifacts and build logs — see internal/build.Builder), on top of
// tearing down containers/routes and the store row, which
// TestHandleDeleteApp* elsewhere already covers indirectly via
// fakeDeployer.RemoveApp — see audit finding H3.
func TestHandleDeleteAppRemovesDataDir(t *testing.T) {
	st := newTestStore(t)
	dataDir := t.TempDir()
	// A fake build.Runtime is enough: DataDir() only reads the dataDir
	// Builder was constructed with, and this test never actually builds
	// anything.
	builder := build.New(nil, dataDir, 1)
	dep := &fakeDeployer{st: st}
	srv := newTestServerWithBuilder(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder)

	appDir := filepath.Join(dataDir, "apps", "blog", "builds")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "1.log"), []byte("build output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateApp("blog", "nginx:latest", 80); err != nil {
		t.Fatal(err)
	}
	_, loginBody := login(t, srv, testPassword)

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/apps/blog", loginBody.Token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /apps/blog: got status %d, want 204", resp.StatusCode)
	}
	if !dep.removeCalled {
		t.Fatal("Deployer.RemoveApp was never called")
	}

	if _, err := os.Stat(filepath.Join(dataDir, "apps", "blog")); !os.IsNotExist(err) {
		t.Fatalf("apps/blog data dir still present after delete: stat err = %v, want IsNotExist", err)
	}
	// The dataDir itself (a sibling of apps/) must survive — only the
	// app's own subdirectory is removed.
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("data dir itself was removed: %v", err)
	}
}

// TestHandleDeleteAppNilBuilderIsANoop proves a nil *build.Builder (some
// tests, and any future caller that doesn't wire one) never panics
// handleDeleteApp — the data-dir cleanup step is skipped entirely rather
// than dereferencing a nil builder.
func TestHandleDeleteAppNilBuilderIsANoop(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{}) // nil builder

	if _, err := st.CreateApp("blog", "nginx:latest", 80); err != nil {
		t.Fatal(err)
	}
	_, loginBody := login(t, srv, testPassword)

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/apps/blog", loginBody.Token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /apps/blog: got status %d, want 204", resp.StatusCode)
	}
}

// TestAppDataDirRejectsTraversal proves appDataDir refuses any slug that
// would resolve outside <dataDir>/apps once cleaned — defense in depth
// for handleDeleteApp's os.RemoveAll, even though slugPattern already
// forbids a slug containing "/" or ".." at app-creation time.
func TestAppDataDirRejectsTraversal(t *testing.T) {
	dataDir := "/data"

	cases := []struct {
		name string
		slug string
		ok   bool
	}{
		{"plain slug", "blog", true},
		{"another plain slug", "my-app-2", true},
		{"parent traversal", "..", false},
		{"nested parent traversal", "../../etc", false},
		// A slug containing "/" is already rejected by slugPattern at
		// app-creation time (see handleCreateApp) and never reaches this
		// helper in practice, but if it somehow did, filepath.Join treats
		// a leading "/" as just another path segment (not as re-rooting
		// the join) — the result still resolves safely inside base, which
		// is the only property appDataDir actually needs to guarantee.
		{"absolute-looking slug stays contained", "/etc/passwd", true},
		{"empty slug", "", false},
		{"dot slug", ".", false},
		{"slug with embedded traversal", "foo/../../bar", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, ok := appDataDir(dataDir, tc.slug)
			if ok != tc.ok {
				t.Fatalf("appDataDir(%q, %q) ok = %v, want %v (dir = %q)", dataDir, tc.slug, ok, tc.ok, dir)
			}
			if ok {
				wantPrefix := filepath.Join(dataDir, "apps") + string(filepath.Separator)
				if dir != filepath.Join(dataDir, "apps", tc.slug) {
					t.Fatalf("dir = %q, want %q", dir, filepath.Join(dataDir, "apps", tc.slug))
				}
				if len(dir) <= len(wantPrefix) || dir[:len(wantPrefix)] != wantPrefix {
					t.Fatalf("dir = %q, want it to stay inside %q", dir, wantPrefix)
				}
			}
		})
	}
}
