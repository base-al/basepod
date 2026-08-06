package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// setupRollbackTest creates a store with one app ("blog") and a healthy
// deployment #1 (image "nginx:v1"), backed by a fakeDeployer whose
// Rollback (see api_test.go) persists a real deployment row through the
// store — mirroring the real engine's own row-creation, without exercising
// any rollout logic (covered by internal/deploy's own tests).
func setupRollbackTest(t *testing.T) (token string, app *store.App, srv string, dep *fakeDeployer, st *store.Store) {
	t.Helper()

	st = newTestStore(t)
	dep = &fakeDeployer{st: st}
	server := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, server, testPassword)

	a, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	firstDep, err := st.CreateDeployment(a.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(firstDep.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}

	return session.Token, a, server.URL, dep, st
}

// TestHandleRollbackSuccess proves a successful rollback returns 200 with
// the new deployment's JSON, and that the request body's "number" reaches
// Deployer.Rollback unchanged.
func TestHandleRollbackSuccess(t *testing.T) {
	token, _, srvURL, fd, _ := setupRollbackTest(t)

	var got deploymentResponse
	resp := doJSON(t, http.MethodPost, srvURL+"/api/v1/apps/blog/rollback", token,
		rollbackRequest{Number: 1}, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fd.rollbackCalledApp != "blog" || fd.rollbackCalledNumber != 1 {
		t.Fatalf("Rollback called with app=%q number=%d, want blog/1", fd.rollbackCalledApp, fd.rollbackCalledNumber)
	}
	if got.Status != "healthy" {
		t.Fatalf("unexpected rollback response: %+v", got)
	}
	if got.Trigger != "rollback" {
		t.Fatalf("trigger = %q, want rollback", got.Trigger)
	}
	if got.Image != "nginx:v1" {
		t.Fatalf("image = %q, want nginx:v1 (the target deployment's image)", got.Image)
	}
	if got.Number != 2 {
		t.Fatalf("number = %d, want 2 (a new deployment row)", got.Number)
	}
}

// TestHandleRollbackAppNotFound proves an unknown slug is reported as 404
// "app_not_found", same as every other per-app route.
func TestHandleRollbackAppNotFound(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/nope/rollback", session.Token,
		rollbackRequest{Number: 1}, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "app_not_found" {
		t.Fatalf("error code = %q, want app_not_found", errBody.Error.Code)
	}
}

// TestHandleRollbackTargetNotFound proves deploy.ErrRollbackTargetNotFound
// maps to 404 "rollback_target_not_found".
func TestHandleRollbackTargetNotFound(t *testing.T) {
	token, _, srvURL, fd, _ := setupRollbackTest(t)
	fd.rollbackErr = deploy.ErrRollbackTargetNotFound

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srvURL+"/api/v1/apps/blog/rollback", token,
		rollbackRequest{Number: 99}, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "rollback_target_not_found" {
		t.Fatalf("error code = %q, want rollback_target_not_found", errBody.Error.Code)
	}
}

// TestHandleRollbackTargetUnhealthy proves
// deploy.ErrRollbackTargetUnhealthy maps to 409 "rollback_target_unhealthy".
func TestHandleRollbackTargetUnhealthy(t *testing.T) {
	token, _, srvURL, fd, _ := setupRollbackTest(t)
	fd.rollbackErr = deploy.ErrRollbackTargetUnhealthy

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srvURL+"/api/v1/apps/blog/rollback", token,
		rollbackRequest{Number: 1}, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "rollback_target_unhealthy" {
		t.Fatalf("error code = %q, want rollback_target_unhealthy", errBody.Error.Code)
	}
}

// TestHandleRollbackImageMissing proves deploy.ErrRollbackImageMissing
// maps to 409 "rollback_image_missing".
func TestHandleRollbackImageMissing(t *testing.T) {
	token, _, srvURL, fd, _ := setupRollbackTest(t)
	fd.rollbackErr = deploy.ErrRollbackImageMissing

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srvURL+"/api/v1/apps/blog/rollback", token,
		rollbackRequest{Number: 1}, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "rollback_image_missing" {
		t.Fatalf("error code = %q, want rollback_image_missing", errBody.Error.Code)
	}
}

// TestHandleRollbackGenericFailureSurfacesAs502 proves any other Rollback
// error (a rollout failure, e.g. a failed probe or pull) maps to the same
// generic 502 "deploy_failed" handleDeploy uses.
func TestHandleRollbackGenericFailureSurfacesAs502(t *testing.T) {
	token, _, srvURL, fd, _ := setupRollbackTest(t)
	fd.rollbackErr = errors.New("rollout failed: probe timeout")

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srvURL+"/api/v1/apps/blog/rollback", token,
		rollbackRequest{Number: 1}, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "deploy_failed" {
		t.Fatalf("error code = %q, want deploy_failed", errBody.Error.Code)
	}
}
