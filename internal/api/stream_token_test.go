package api

import (
	"net/http"
	"testing"
	"time"
)

// TestHandleCreateStreamTokenAppLogs proves a well-formed app_logs mint
// request succeeds and returns a token expiring streamTokenDuration from
// now.
func TestHandleCreateStreamTokenAppLogs(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	before := time.Now()
	out := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeAppLogs, Slug: "blog"})

	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q did not parse as RFC3339: %v", out.ExpiresAt, err)
	}
	wantAround := before.Add(streamTokenDuration)
	if diff := expires.Sub(wantAround); diff < -5*time.Second || diff > 5*time.Second {
		t.Fatalf("expires_at %v too far from expected ~%v (streamTokenDuration=%v)", expires, wantAround, streamTokenDuration)
	}
}

// TestHandleCreateStreamTokenBuildLog proves a well-formed build_log mint
// request (naming an existing deployment) succeeds.
func TestHandleCreateStreamTokenBuildLog(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	app, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeBuildLog, Slug: "blog", DeploymentNumber: &dep.Number})
}

// TestHandleCreateStreamTokenRequiresAuth proves the mint endpoint sits
// behind requireAuth like every other authenticated route — it must not
// be reachable to mint a token for an app the caller was never
// authenticated to see.
func TestHandleCreateStreamTokenRequiresAuth(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", "",
		streamTokenRequest{Scope: streamScopeAppLogs, Slug: "blog"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHandleCreateStreamTokenInvalidScope proves an unrecognized scope
// value is rejected with 422 "validation".
func TestHandleCreateStreamTokenInvalidScope(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: "not_a_real_scope", Slug: "blog"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("error code = %q, want validation", errBody.Error.Code)
	}
}

// TestHandleCreateStreamTokenAppNotFound proves an unknown slug is
// rejected with 404 "app_not_found", matching every other route that
// looks an app up by slug.
func TestHandleCreateStreamTokenAppNotFound(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeAppLogs, Slug: "nope"}, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "app_not_found" {
		t.Fatalf("error code = %q, want app_not_found", errBody.Error.Code)
	}
}

// TestHandleCreateStreamTokenAppLogsWithDeploymentNumberRejected proves
// scope "app_logs" combined with a deployment_number — a mismatched,
// incoherent combination (app_logs streams by slug alone) — is rejected
// with 422 "validation" rather than silently ignoring the extra field.
func TestHandleCreateStreamTokenAppLogsWithDeploymentNumberRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	n := 1
	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeAppLogs, Slug: "blog", DeploymentNumber: &n}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("error code = %q, want validation", errBody.Error.Code)
	}
}

// TestHandleCreateStreamTokenBuildLogMissingDeploymentNumberRejected
// proves scope "build_log" without a deployment_number is rejected with
// 422 "validation" — a build-log token has to name exactly one
// deployment.
func TestHandleCreateStreamTokenBuildLogMissingDeploymentNumberRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeBuildLog, Slug: "blog"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("error code = %q, want validation", errBody.Error.Code)
	}
}

// TestHandleCreateStreamTokenBuildLogUnknownDeploymentNumberRejected
// proves scope "build_log" naming a deployment number that doesn't exist
// for the app is rejected with 404 "deployment_not_found", matching
// handleDeploymentLog's own mapping for the same lookup.
func TestHandleCreateStreamTokenBuildLogUnknownDeploymentNumberRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	n := 99
	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeBuildLog, Slug: "blog", DeploymentNumber: &n}, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "deployment_not_found" {
		t.Fatalf("error code = %q, want deployment_not_found", errBody.Error.Code)
	}
}
