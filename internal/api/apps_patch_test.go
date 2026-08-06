package api

// Tests for handlePatchApp (PATCH /api/v1/apps/{slug} — audit finding
// H2's admin-facing knob for the per-app resource limits every deploy now
// carries into podman.CreateSpec, see internal/deploy/deploy.go's
// runRollout).
//
// NOTE ON ROUTING: as of this commit, the route itself is NOT registered
// in router.go — Task 2 of the v0.4 plan owns internal/api/apps.go only;
// router.go is owned by a different concurrent task (see the v0.4 Task 2
// report for the full explanation). These tests therefore exercise
// a.handlePatchApp directly through a minimal single-route chi router
// built here, rather than through newTestServer's full New(...) handler —
// once router.go registers `r.Patch("/apps/{slug}", a.handlePatchApp)`
// alongside the other authenticated /apps/{slug} routes, this handler is
// wired in with no further change needed here.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/store"
)

// newPatchAppTestServer builds a standalone httptest.Server exposing only
// PATCH /apps/{slug} -> a.handlePatchApp, backed by st. See the file doc
// comment for why this doesn't go through the full New(...) router.
func newPatchAppTestServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	a := &api{st: st}
	r := chi.NewRouter()
	r.Patch("/apps/{slug}", a.handlePatchApp)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func patchApp(t *testing.T, srv *httptest.Server, slug string, payload any) (*http.Response, appResponse) {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/apps/"+slug, &body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	var out appResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

func openPatchTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestHandlePatchAppUpdatesAllThreeLimits proves a full-body PATCH updates
// memory/cpu/pids together and returns the new values in the app JSON.
func TestHandlePatchAppUpdatesAllThreeLimits(t *testing.T) {
	st := openPatchTestStore(t)
	app, err := st.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	srv := newPatchAppTestServer(t, st)

	resp, out := patchApp(t, srv, "blog", map[string]any{
		"memory_limit_mb": 1024,
		"cpu_limit":       2.0,
		"pids_limit":      1024,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.MemoryLimitMB != 1024 || out.CPULimit != 2.0 || out.PidsLimit != 1024 {
		t.Fatalf("response = %+v", out)
	}

	got, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 1024 || got.CPULimit != 2.0 || got.PidsLimit != 1024 {
		t.Fatalf("stored app = %+v", got)
	}
	_ = app
}

// TestHandlePatchAppPartialUpdateLeavesOtherFieldsUnchanged proves an
// omitted field is left at its current value, not zeroed — the pointer
// fields on patchAppRequest exist specifically so "not sent" and "sent as
// 0 (unlimited)" are distinguishable.
func TestHandlePatchAppPartialUpdateLeavesOtherFieldsUnchanged(t *testing.T) {
	st := openPatchTestStore(t)
	app, err := st.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppLimits(app.ID, 1024, 2.0, 1024); err != nil {
		t.Fatal(err)
	}
	srv := newPatchAppTestServer(t, st)

	resp, out := patchApp(t, srv, "blog", map[string]any{"cpu_limit": 4.0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.MemoryLimitMB != 1024 || out.CPULimit != 4.0 || out.PidsLimit != 1024 {
		t.Fatalf("response = %+v, want memory/pids unchanged and cpu=4", out)
	}
}

// TestHandlePatchAppExplicitZeroMeansUnlimited proves an explicit 0 is
// applied (not treated as "field omitted") — the whole reason
// patchAppRequest uses pointers instead of plain values.
func TestHandlePatchAppExplicitZeroMeansUnlimited(t *testing.T) {
	st := openPatchTestStore(t)
	app, err := st.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppLimits(app.ID, 1024, 2.0, 1024); err != nil {
		t.Fatal(err)
	}
	srv := newPatchAppTestServer(t, st)

	resp, out := patchApp(t, srv, "blog", map[string]any{"memory_limit_mb": 0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.MemoryLimitMB != 0 {
		t.Fatalf("MemoryLimitMB = %d, want 0 (unlimited)", out.MemoryLimitMB)
	}
	if out.CPULimit != 2.0 || out.PidsLimit != 1024 {
		t.Fatalf("response = %+v, want cpu/pids unchanged", out)
	}
}

// TestHandlePatchAppAppNotFound proves an unknown slug 404s and never
// reaches store.UpdateAppLimits.
func TestHandlePatchAppAppNotFound(t *testing.T) {
	st := openPatchTestStore(t)
	srv := newPatchAppTestServer(t, st)

	resp, _ := patchApp(t, srv, "does-not-exist", map[string]any{"memory_limit_mb": 1024})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandlePatchAppValidationMatrix is the plan's required "PATCH
// validation matrix": each field's lower/upper bound, on both sides, plus
// the 0-is-always-valid sentinel — one field varied at a time so a bad
// value in one field can't mask a bug in another's bound check.
func TestHandlePatchAppValidationMatrix(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		wantOK  bool
	}{
		{"memory just below min (63) rejected", map[string]any{"memory_limit_mb": 63}, false},
		{"memory at min (64) accepted", map[string]any{"memory_limit_mb": 64}, true},
		{"memory at max (262144) accepted", map[string]any{"memory_limit_mb": 262144}, true},
		{"memory just above max (262145) rejected", map[string]any{"memory_limit_mb": 262145}, false},
		{"memory 0 (unlimited) accepted", map[string]any{"memory_limit_mb": 0}, true},
		{"memory negative rejected", map[string]any{"memory_limit_mb": -1}, false},

		{"cpu just below min (0.09) rejected", map[string]any{"cpu_limit": 0.09}, false},
		{"cpu at min (0.1) accepted", map[string]any{"cpu_limit": 0.1}, true},
		{"cpu at max (64) accepted", map[string]any{"cpu_limit": 64}, true},
		{"cpu just above max (64.1) rejected", map[string]any{"cpu_limit": 64.1}, false},
		{"cpu 0 (unlimited) accepted", map[string]any{"cpu_limit": 0}, true},
		{"cpu negative rejected", map[string]any{"cpu_limit": -0.5}, false},

		{"pids just below min (15) rejected", map[string]any{"pids_limit": 15}, false},
		{"pids at min (16) accepted", map[string]any{"pids_limit": 16}, true},
		{"pids at max (65536) accepted", map[string]any{"pids_limit": 65536}, true},
		{"pids just above max (65537) rejected", map[string]any{"pids_limit": 65537}, false},
		{"pids 0 (unlimited) accepted", map[string]any{"pids_limit": 0}, true},
		{"pids negative rejected", map[string]any{"pids_limit": -1}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openPatchTestStore(t)
			if _, err := st.CreateApp("blog", "nginx:alpine", 8080); err != nil {
				t.Fatal(err)
			}
			srv := newPatchAppTestServer(t, st)

			resp, _ := patchApp(t, srv, "blog", tc.payload)
			if tc.wantOK && resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if !tc.wantOK {
				if resp.StatusCode != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422", resp.StatusCode)
				}
				var body errorResponse
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != "validation" {
					t.Fatalf("error code = %q, want validation", body.Error.Code)
				}
			}
		})
	}
}

// TestHandlePatchAppInvalidJSONBody proves a malformed body is rejected
// with the standard decode-error envelope rather than a panic or a silent
// no-op update.
func TestHandlePatchAppInvalidJSONBody(t *testing.T) {
	st := openPatchTestStore(t)
	if _, err := st.CreateApp("blog", "nginx:alpine", 8080); err != nil {
		t.Fatal(err)
	}
	srv := newPatchAppTestServer(t, st)

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/apps/blog", bytes.NewBufferString("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
