package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
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

// tarEntry is one file to write into a test build context.
type tarEntry struct {
	name string
	body string
}

// gzipTarBody builds a gzip-compressed tar stream from entries — a real,
// valid upload body for handleDeployTarball's now-synchronous
// builder.PrepareBuild step to actually spool and validate, rather than a
// placeholder byte string a fully-faked Deployer never had to parse.
func gzipTarBody(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return gzBuf.Bytes()
}

// validTarballBody is a minimal real gzipped tar with a root Containerfile
// — the "this upload should pass validation" fixture shared by every test
// below that needs handleDeployTarball's synchronous PrepareBuild step to
// actually succeed.
func validTarballBody(t *testing.T) []byte {
	t.Helper()
	return gzipTarBody(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})
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
// with 400 before ever reaching builder.PrepareBuild or
// Deployer.DeployBuildAsync.
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
		t.Fatal("DeployBuildAsync should never have been called for a bad Content-Type")
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

// TestHandleDeployTarballNoContainerfile proves a real upload with no
// Containerfile/Dockerfile at its root is rejected 422 "no_containerfile"
// by the handler's own synchronous builder.PrepareBuild step — before
// Deployer.DeployBuildAsync is ever called, since there is nothing valid
// to hand it.
func TestHandleDeployTarballNoContainerfile(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	srv := newTestServerWithBuilder(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder)
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	body := gzipTarBody(t, []tarEntry{{name: "README.md", body: "no Containerfile here\n"}})

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "no_containerfile" {
		t.Fatalf("error code = %q, want no_containerfile", errBody.Error.Code)
	}
	if dep.deployBuildCalled {
		t.Fatal("DeployBuildAsync should never have been called for an invalid build context")
	}
}

// TestHandleDeployTarballBadPath proves a real upload containing an
// unsafe (path-traversal) tar entry is rejected 422 "bad_path" by
// builder.PrepareBuild, before Deployer.DeployBuildAsync is ever called.
func TestHandleDeployTarballBadPath(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	srv := newTestServerWithBuilder(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder)
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	body := gzipTarBody(t, []tarEntry{
		{name: "Containerfile", body: "FROM alpine\n"},
		{name: "../evil.txt", body: "escape attempt\n"},
	})

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "bad_path" {
		t.Fatalf("error code = %q, want bad_path", errBody.Error.Code)
	}
	if dep.deployBuildCalled {
		t.Fatal("DeployBuildAsync should never have been called for an invalid build context")
	}
}

// TestWritePrepareBuildErrorMapsOversizeAndContextTooLarge unit-tests
// writePrepareBuildError directly for the two cases impractical to trigger
// through a real end-to-end request (a genuinely 256 MiB+ upload, or a
// build.Builder with its own maxDecompressedContext shrunk — an unexported
// field only internal/build's own tests can reach): an *http.MaxBytesError
// (as bubbles up from http.MaxBytesReader deep in a real oversized upload)
// maps to 413 "request_too_large", and build.ErrContextTooLarge (a small
// compressed upload that decompresses past the pipeline's own cap) maps to
// 413 "context_too_large", distinct from each other.
func TestWritePrepareBuildErrorMapsOversizeAndContextTooLarge(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"oversize upload", &http.MaxBytesError{Limit: maxTarballBody}, "request_too_large"},
		{"oversize decompressed context", build.ErrContextTooLarge, "context_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePrepareBuildError(rec, tc.err)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
			var errBody errorResponse
			decodeInto(t, rec.Result(), &errBody)
			if errBody.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", errBody.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestHandleDeployTarballAsyncBookkeepingErrorIs502 proves that when
// Deployer.DeployBuildAsync itself fails (its own synchronous bookkeeping
// — e.g. a store write error marking the app "deploying" — not a
// build/rollout failure, which no longer surfaces here at all; see
// DeployBuildAsync's doc comment), the handler maps it to the generic 502
// "deploy_failed", matching handleDeploy's own catch-all.
func TestHandleDeployTarballAsyncBookkeepingErrorIs502(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployBuildAsyncErr: errBoom("mark deploying: database is locked")}
	builder := build.New(nil, t.TempDir(), 2)
	srv := newTestServerWithBuilder(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder)
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var errBody errorResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", validTarballBody(t))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "deploy_failed" {
		t.Fatalf("error code = %q, want deploy_failed", errBody.Error.Code)
	}
	if !dep.deployBuildCalled {
		t.Fatal("DeployBuildAsync was never called")
	}
}

// TestHandleDeployTarballHappyPath proves a valid upload passes through
// handleDeployTarball's synchronous PrepareBuild step, reaches
// Deployer.DeployBuildAsync with the API's configured *build.Builder, and
// gets back 202 Accepted (not 200 — issue #2's whole point: the handler no
// longer blocks until the build finishes) with a deployment response
// reflecting the freshly created, still-"deploying" row: no image yet (the
// build hasn't run), source/trigger set, and a build log already
// addressable.
func TestHandleDeployTarballHappyPath(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	srv := newTestServerWithBuilder(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder)
	_, session := login(t, srv, testPassword)
	createTestAppForTarball(t, st)

	var deployed deploymentResponse
	resp := postTarball(t, srv.URL+"/api/v1/apps/blog/deploy/tarball", session.Token, "application/gzip", validTarballBody(t))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	decodeInto(t, resp, &deployed)

	if !dep.deployBuildCalled {
		t.Fatal("DeployBuildAsync was never called")
	}
	if dep.deployBuildPrepared == nil {
		t.Fatal("DeployBuildAsync was called with a nil PreparedBuild")
	}
	if dep.deployBuildBuilder != builder {
		t.Fatal("DeployBuildAsync did not receive the API's configured *build.Builder")
	}

	if deployed.Status != "deploying" {
		t.Fatalf("deployed.Status = %q, want deploying (the build has not run yet)", deployed.Status)
	}
	if deployed.Source != "tarball" {
		t.Fatalf("deployed.Source = %q, want tarball", deployed.Source)
	}
	if deployed.Trigger != "api" {
		t.Fatalf("deployed.Trigger = %q, want api", deployed.Trigger)
	}
	if !deployed.HasBuildLog {
		t.Fatal("deployed.HasBuildLog = false, want true (the build-log path is recorded before the build starts)")
	}
	if deployed.Image != "" {
		t.Fatalf("deployed.Image = %q, want empty (no image tag exists until the background build finishes)", deployed.Image)
	}
	if deployed.Number == 0 {
		t.Fatal("deployed.Number is zero, want a real deployment number a caller can poll/stream immediately")
	}

	// GET .../deployments/{n} must resolve immediately — the deployment
	// row exists in full before the 202 response is ever written (see
	// handleGetDeployment's own tests for the endpoint's error cases).
	var polled deploymentResponse
	pollResp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1", session.Token, nil, &polled)
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll: status = %d, want 200", pollResp.StatusCode)
	}
	if polled.Number != deployed.Number || polled.Status != "deploying" {
		t.Fatalf("polled deployment = %+v, want number=%d status=deploying", polled, deployed.Number)
	}
}

// errBoom is a trivial error type distinct from errors.New's so tests
// naming it in a failure message read clearly; it behaves identically.
type errBoom string

func (e errBoom) Error() string { return string(e) }
