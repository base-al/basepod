package api

import (
	"net/http"
	"testing"
)

// TestHandleGetDeploymentNotFound proves handleGetDeployment reports 404
// "deployment_not_found" for a number with no matching deployment on an
// otherwise-real app — the async tarball deploy's whole polling contract
// (see handleDeployTarball's 202 response) depends on this route
// distinguishing "not created yet" from "exists".
func TestHandleGetDeploymentNotFound(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/99", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "deployment_not_found" {
		t.Fatalf("error code = %q, want deployment_not_found", errBody.Error.Code)
	}
}

// TestHandleGetDeploymentAppNotFound proves an unknown app slug is 404
// "app_not_found" before the deployment number is ever looked up.
func TestHandleGetDeploymentAppNotFound(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/does-not-exist/deployments/1", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "app_not_found" {
		t.Fatalf("error code = %q, want app_not_found", errBody.Error.Code)
	}
}

// TestHandleGetDeploymentBadNumber proves a non-integer {number} is 400
// "invalid_request".
func TestHandleGetDeploymentBadNumber(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/not-a-number", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if errBody.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", errBody.Error.Code)
	}
}

// TestHandleGetDeploymentOK proves the happy path returns 200 with the
// exact deployment the app's own image-deploy flow just created — the
// polling contract handleDeployTarball's 202 response (and
// TestHandleDeployTarballHappyPath) already exercises against a
// still-"deploying" row; this covers the terminal-state case via the
// existing synchronous image-deploy path instead, so the two together
// cover both a deployment's in-flight and finished shapes.
func TestHandleGetDeploymentOK(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var created appResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", session.Token,
		createAppRequest{Name: "Blog", Image: "nginx:alpine", Port: 80}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", resp.StatusCode)
	}

	var deployed deploymentResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/blog/deploy", session.Token, nil, &deployed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy: status = %d, want 200", resp.StatusCode)
	}

	var got deploymentResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1", session.Token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status = %d, want 200", resp.StatusCode)
	}
	if got.Number != 1 || got.Status != "healthy" || got.Image != "nginx:alpine" {
		t.Fatalf("got = %+v, want number=1 status=healthy image=nginx:alpine", got)
	}
}
