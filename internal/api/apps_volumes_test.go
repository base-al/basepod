package api

// Tests for v0.5 Task 6's API surface: app JSON gaining volumes +
// deploy_strategy, GET /api/v1/apps/{slug}/volumes, PATCH
// /api/v1/apps/{slug} accepting deploy_strategy, and app delete keeping
// (and logging) surviving named volumes.

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// TestHandleGetAppIncludesVolumesAndDeployStrategy proves GET
// /apps/{slug} surfaces both of this task's additions: the app's declared
// volumes (name + container_path, never the derived libpod volume name)
// and its deploy_strategy.
func TestHandleGetAppIncludesVolumesAndDeployStrategy(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})

	app, err := st.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertVolume(app.ID, "data", "/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}

	_, loginBody := login(t, srv, testPassword)

	var out appDetailResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/db", loginBody.Token, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps/db: status = %d, want 200", resp.StatusCode)
	}
	if out.DeployStrategy != store.DeployStrategyZeroDowntime {
		t.Errorf("DeployStrategy = %q, want %q (schema default)", out.DeployStrategy, store.DeployStrategyZeroDowntime)
	}
	if len(out.Volumes) != 1 || out.Volumes[0].Name != "data" || out.Volumes[0].ContainerPath != "/var/lib/postgresql/data" {
		t.Fatalf("Volumes = %+v, want exactly one {data, /var/lib/postgresql/data}", out.Volumes)
	}
}

// TestHandleGetAppNoVolumesReturnsEmptySlice proves an app with no
// declared volumes gets an empty (never null/omitted) Volumes list in its
// JSON.
func TestHandleGetAppNoVolumesReturnsEmptySlice(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})

	if _, err := st.CreateApp("blog", "nginx:alpine", 80); err != nil {
		t.Fatal(err)
	}
	_, loginBody := login(t, srv, testPassword)

	var out appDetailResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog", loginBody.Token, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps/blog: status = %d, want 200", resp.StatusCode)
	}
	if out.Volumes == nil || len(out.Volumes) != 0 {
		t.Fatalf("Volumes = %+v, want a non-nil empty slice", out.Volumes)
	}
}

// TestHandleListAppVolumes proves GET /apps/{slug}/volumes returns the
// same volume set as the app JSON's own Volumes field, independently
// addressable.
func TestHandleListAppVolumes(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})

	app, err := st.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertVolume(app.ID, "data", "/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertVolume(app.ID, "logs", "/var/log/postgresql"); err != nil {
		t.Fatal(err)
	}

	_, loginBody := login(t, srv, testPassword)

	var out []volumeResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/db/volumes", loginBody.Token, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps/db/volumes: status = %d, want 200", resp.StatusCode)
	}
	if len(out) != 2 {
		t.Fatalf("volumes = %+v, want 2", out)
	}
	byName := map[string]string{}
	for _, v := range out {
		byName[v.Name] = v.ContainerPath
	}
	if byName["data"] != "/var/lib/postgresql/data" || byName["logs"] != "/var/log/postgresql" {
		t.Fatalf("volumes = %+v, want data+logs with their configured paths", out)
	}
}

// TestHandleListAppVolumesAppNotFound proves the route 404s for an
// unknown slug, matching every other /apps/{slug}/... route's contract.
func TestHandleListAppVolumesAppNotFound(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, loginBody := login(t, srv, testPassword)

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/does-not-exist/volumes", loginBody.Token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleDeleteAppKeepsVolumesAndLogsSurvivors is this task's core
// data-safety regression: DELETE /apps/{slug} removes the app row (and,
// via ON DELETE CASCADE, the volumes bookkeeping rows) but must never
// remove the underlying libpod volume — there is no code path in this
// package that even calls a volume-removal operation, so the meaningful
// assertion is that the operator is told what survived and under what
// real (derived) name, via a log line, matching migration
// 00008_volumes_strategy.sql's and store.DeleteApp's documented
// contract.
func TestHandleDeleteAppKeepsVolumesAndLogsSurvivors(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})

	app, err := st.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertVolume(app.ID, "data", "/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}
	_, loginBody := login(t, srv, testPassword)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/apps/db", loginBody.Token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /apps/db: status = %d, want 204", resp.StatusCode)
	}
	if !dep.removeCalled {
		t.Fatal("Deployer.RemoveApp was never called")
	}

	// The app row (and its volumes bookkeeping rows, cascade-deleted) is
	// gone...
	if _, err := st.AppBySlug("db"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AppBySlug after delete: err = %v, want ErrNotFound", err)
	}

	// ...but the operator must be told the underlying libpod volume was
	// NOT removed, named by its real (derived) libpod name — never just
	// the app-scoped logical name "data" alone, which on its own isn't
	// enough to run `podman volume rm` against.
	wantName := deploy.VolumeName("db", "data")
	if !strings.Contains(logBuf.String(), wantName) {
		t.Fatalf("delete log = %q, want it to mention surviving volume %q", logBuf.String(), wantName)
	}
}

// TestHandleDeleteAppNoVolumesLogsNothingVolumeRelated proves an app with
// no declared volumes doesn't get a spurious "volumes survived" log line
// — the log call is conditioned on len(volumes) > 0.
func TestHandleDeleteAppNoVolumesLogsNothingVolumeRelated(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})

	if _, err := st.CreateApp("blog", "nginx:alpine", 80); err != nil {
		t.Fatal(err)
	}
	_, loginBody := login(t, srv, testPassword)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/apps/blog", loginBody.Token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /apps/blog: status = %d, want 204", resp.StatusCode)
	}
	if strings.Contains(logBuf.String(), "named volume") {
		t.Fatalf("delete log = %q, want no volume-survival message for an app with no volumes", logBuf.String())
	}
}

// TestHandlePatchAppDeployStrategyRoundTrips proves PATCH accepts
// deploy_strategy independently of the resource-limit fields, and that it
// persists.
func TestHandlePatchAppDeployStrategyRoundTrips(t *testing.T) {
	st := openPatchTestStore(t)
	if _, err := st.CreateApp("db", "postgres:16", 5432); err != nil {
		t.Fatal(err)
	}
	srv := newPatchAppTestServer(t, st)

	resp, out := patchApp(t, srv, "db", map[string]any{"deploy_strategy": "replace"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.DeployStrategy != store.DeployStrategyReplace {
		t.Fatalf("response DeployStrategy = %q, want %q", out.DeployStrategy, store.DeployStrategyReplace)
	}

	got, err := st.AppBySlug("db")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployStrategy != store.DeployStrategyReplace {
		t.Fatalf("stored DeployStrategy = %q, want %q", got.DeployStrategy, store.DeployStrategyReplace)
	}

	// Omitting it on a later PATCH leaves it unchanged (same "not sent"
	// contract as the resource-limit fields).
	resp, out = patchApp(t, srv, "db", map[string]any{"cpu_limit": 2.0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.DeployStrategy != store.DeployStrategyReplace {
		t.Fatalf("DeployStrategy after unrelated PATCH = %q, want unchanged %q", out.DeployStrategy, store.DeployStrategyReplace)
	}
}

// TestHandlePatchAppDeployStrategyRejectsUnknownValue proves an unknown
// deploy_strategy value 422s with the "validation" error code, matching
// every other field's validation-error shape in this handler, and leaves
// the app's stored strategy untouched.
func TestHandlePatchAppDeployStrategyRejectsUnknownValue(t *testing.T) {
	st := openPatchTestStore(t)
	if _, err := st.CreateApp("db", "postgres:16", 5432); err != nil {
		t.Fatal(err)
	}
	srv := newPatchAppTestServer(t, st)

	cases := []string{"rolling", "Replace", "zero_downtime", ""}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			resp, _ := patchApp(t, srv, "db", map[string]any{"deploy_strategy": bad})
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", resp.StatusCode)
			}

			got, err := st.AppBySlug("db")
			if err != nil {
				t.Fatal(err)
			}
			if got.DeployStrategy != store.DeployStrategyZeroDowntime {
				t.Fatalf("stored DeployStrategy = %q, want unchanged %q after a rejected PATCH", got.DeployStrategy, store.DeployStrategyZeroDowntime)
			}
		})
	}
}
