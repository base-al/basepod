package api

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/store"
)

// createTestAppForTarball creates an app directly through the store
// (bypassing the HTTP create-app flow, which these tests don't otherwise
// exercise) and returns it.
func createTestAppForTarball(t *testing.T, st *store.Store) *store.App {
	t.Helper()
	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// postTarball issues a raw POST with the given Content-Type and body
// (skipping doJSON, which always marshals JSON) and returns the response.
func postTarball(t *testing.T, url, token, contentType string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestHandleDeployTarballBadContentType proves a request whose
// Content-Type isn't application/gzip or application/x-gzip is rejected
// with 400 before ever reaching Deployer.DeployBuild.
func TestHandleDeployTarballBadContentType(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/octet-stream", []byte("not a tarball"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "invalid_content_type" {
		t.Fatalf("error code = %q, want invalid_content_type", errBody.Error.Code)
	}
	if dep.deployBuildCalled {
		t.Fatal("DeployBuild should never have been called for a bad Content-Type")
	}
}

// TestHandleDeployTarballOversize proves an *http.MaxBytesError surfacing
// from DeployBuild (in production, from http.MaxBytesReader tripping
// during the upload's decompression) is mapped to 413
// "request_too_large" rather than the generic 502.
func TestHandleDeployTarballOversize(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployBuildErr: &http.MaxBytesError{Limit: maxTarballBody}}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", []byte("fake-gzip-bytes"))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", errBody.Error.Code)
	}
}

// TestHandleDeployTarballContextTooLarge proves build.ErrContextTooLarge
// from DeployBuild (a small compressed upload that decompresses past the
// build pipeline's own size cap — see maxDecompressedContext in
// internal/build) maps to 413 "context_too_large", distinct from the
// compressed-body "request_too_large" case above.
func TestHandleDeployTarballContextTooLarge(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployBuildErr: build.ErrContextTooLarge}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", []byte("fake-gzip-bytes"))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "context_too_large" {
		t.Fatalf("error code = %q, want context_too_large", errBody.Error.Code)
	}
}

// TestHandleDeployTarballNoContainerfile proves build.ErrNoContainerfile
// from DeployBuild maps to 422 "no_containerfile".
func TestHandleDeployTarballNoContainerfile(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployBuildErr: build.ErrNoContainerfile}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/x-gzip", []byte("fake-gzip-bytes"))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "no_containerfile" {
		t.Fatalf("error code = %q, want no_containerfile", errBody.Error.Code)
	}
}

// TestHandleDeployTarballBadPath proves build.ErrBadPath from
// DeployBuild maps to 422 "bad_path".
func TestHandleDeployTarballBadPath(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployBuildErr: build.ErrBadPath}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", []byte("fake-gzip-bytes"))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "bad_path" {
		t.Fatalf("error code = %q, want bad_path", errBody.Error.Code)
	}
}

// TestHandleDeployTarballOtherErrorIs502 proves any other DeployBuild
// error (a real build/runtime failure) maps to the generic 502
// "deploy_failed", matching handleDeploy's own catch-all.
func TestHandleDeployTarballOtherErrorIs502(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployBuildErr: errBoom("executor failed running [/bin/sh -c false]")}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", []byte("fake-gzip-bytes"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "deploy_failed" {
		t.Fatalf("error code = %q, want deploy_failed", errBody.Error.Code)
	}
}

// TestHandleDeployTarballHappyPath proves a successful DeployBuild call
// streams the raw request body through to Deployer.DeployBuild unchanged,
// forwards the API's configured *build.Builder, and returns 200 with a
// deployment response whose source/trigger/has_build_log fields reflect
// the tarball path.
func TestHandleDeployTarballHappyPath(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	srv := newTestServerWithBuilder(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder)
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	uploadBody := []byte("fake-gzip-bytes-representing-a-tar-context")
	var deployed deploymentResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", uploadBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	decodeInto(t, resp, &deployed)

	if !dep.deployBuildCalled {
		t.Fatal("DeployBuild was never called")
	}
	if !bytes.Equal(dep.deployBuildBody, uploadBody) {
		t.Fatalf("DeployBuild received body %q, want %q", dep.deployBuildBody, uploadBody)
	}
	if dep.deployBuildBuilder != builder {
		t.Fatal("DeployBuild did not receive the API's configured *build.Builder")
	}

	if deployed.Status != "healthy" {
		t.Fatalf("deployed.Status = %q, want healthy", deployed.Status)
	}
	if deployed.Source != "tarball" {
		t.Fatalf("deployed.Source = %q, want tarball", deployed.Source)
	}
	if deployed.Trigger != "api" {
		t.Fatalf("deployed.Trigger = %q, want api", deployed.Trigger)
	}
	if !deployed.HasBuildLog {
		t.Fatal("deployed.HasBuildLog = false, want true")
	}
	if deployed.Image != "localhost/basepod/blog:1" {
		t.Fatalf("deployed.Image = %q, want localhost/basepod/blog:1", deployed.Image)
	}
}

// TestHandleDeployTarballAppNotFound proves an unknown slug is rejected
// with 404 before any Content-Type or body handling.
func TestHandleDeployTarballAppNotFound(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	resp := postTarball(t, srv.URL+"/api/v1/apps/does-not-exist/deploy/tarball", session.Token, "application/gzip", []byte("x"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// errBoom is a trivial error type distinct from errors.New's so tests
// naming it in a failure message read clearly; it behaves identically.
type errBoom string

func (e errBoom) Error() string { return string(e) }
