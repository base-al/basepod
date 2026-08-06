package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/crypto"
	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// Compile-time assertion that *deploy.Engine satisfies the Deployer
// interface this package defines and consumes.
var _ Deployer = (*deploy.Engine)(nil)

// Compile-time assertion that *deploy.Engine satisfies the RoutesApplier
// interface this package defines and consumes.
var _ RoutesApplier = (*deploy.Engine)(nil)

// Compile-time assertion that (*deploy.Engine).AppLogs satisfies the
// LogSource func type this package defines and consumes.
var _ LogSource = (*deploy.Engine)(nil).AppLogs

const testPassword = "correct-password"

// testKey is a fixed 32-byte encryption key used by testSeal/testOpen so
// tests can decrypt a stored EnvVar.ValueEncrypted directly to verify
// keep-on-empty-secret semantics.
var testKey = bytes.Repeat([]byte{0x42}, 32)

// testSeal/testOpen mirror the AAD-bound seal/open closures
// internal/server.Run builds (see its doc comment) — SealAAD/OpenAAD
// under crypto.AAD(appID, key) — so tests exercising the API layer's env
// endpoints go through the exact same L9 binding (audit finding L9:
// every env value is bound to its owning app's ID and its own key) real
// traffic does.
func testSeal(appID int64, key, plaintext string) (string, error) {
	return crypto.SealAAD(testKey, plaintext, crypto.AAD(appID, key))
}
func testOpen(appID int64, key, sealed string) (string, error) {
	return crypto.OpenAAD(testKey, sealed, crypto.AAD(appID, key))
}

// fakeRoutesApplier is a test double for RoutesApplier that records how
// many times ApplyRoutes was called and can be scripted to fail (to
// exercise the domain handlers' rollback paths).
type fakeRoutesApplier struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRoutesApplier) ApplyRoutes(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeRoutesApplier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeDeployer is a test double for Deployer that records the last call
// and can be scripted to fail. Like the real deploy.Engine, it is
// responsible for persisting the deployment row itself (the API layer
// only relays whatever Deployer.Deploy returns), so it takes the store
// directly rather than faking that too.
type fakeDeployer struct {
	st *store.Store

	deployErr error
	removeErr error

	deployedApp   string
	deployedImage string
	removeCalled  bool

	// deployBuildAsyncErr, if set, is returned by DeployBuildAsync as-is
	// (after marking the deployment row it creates "failed") — simulates a
	// synchronous bookkeeping failure (e.g. a store write erroring while
	// marking the app "deploying") that happens before any background
	// build goroutine would ever start. Unlike the old (pre-issue-#2)
	// deployBuildErr, this can no longer stand in for a build/validation
	// failure — those are now caught synchronously by
	// builder.PrepareBuild, before DeployBuildAsync is ever called (see
	// deploy_tarball_test.go's real-builder tests for that coverage).
	deployBuildAsyncErr error

	deployBuildCalled   bool
	deployBuildPrepared *build.PreparedBuild
	deployBuildBuilder  *build.Builder

	// rollbackErr, if set, is returned by Rollback as-is (mirroring how
	// deployErr/deployBuildErr script Deploy/DeployBuild) — tests script
	// this to deploy's typed rollback errors (or a generic error) to
	// exercise handleRollback's status/error-code mapping.
	rollbackErr error

	rollbackCalledApp    string
	rollbackCalledNumber int

	// deployExistingErr/deployBuildExistingErr, if set, are returned by
	// DeployExisting/DeployBuildExisting as-is (after finishing the
	// caller-supplied deployment row "failed") — v0.5 Task 8's compose
	// orchestrator additions. deployExistingCalls/deployBuildExistingCalls
	// record every app slug each was called with, in call order, so a
	// test can assert dependency-order sequencing without a real
	// container runtime. compose_test.go defines its own richer fake for
	// scripting per-service failures; this default behavior (always
	// succeed, unless the single shared err is set) only needs to prove
	// every OTHER test in this package that constructs a bare
	// &fakeDeployer{} still compiles and behaves reasonably against the
	// full Deployer interface.
	deployExistingErr        error
	deployBuildExistingErr   error
	deployExistingCalls      []string
	deployBuildExistingCalls []string
}

func (f *fakeDeployer) Deploy(ctx context.Context, app *store.App, imageRef string) (*store.Deployment, error) {
	f.deployedApp = app.Slug
	f.deployedImage = imageRef

	dep, err := f.st.CreateDeployment(app.ID, imageRef)
	if err != nil {
		return nil, err
	}
	if f.deployErr != nil {
		_ = f.st.FinishDeployment(dep.ID, "failed", f.deployErr.Error())
		return nil, f.deployErr
	}
	_ = f.st.FinishDeployment(dep.ID, "healthy", "")
	dep.Status = "healthy"
	return dep, nil
}

// DeployBuildAsync is a lightweight stand-in for
// deploy.Engine.DeployBuildAsync: it persists a real deployment row
// (source "tarball") through the store — like Deploy above, and like the
// real engine's own synchronous bookkeeping half — but never actually
// builds anything and never finishes the row; internal/deploy and
// internal/build already cover the real build pipeline's (and the real
// background goroutine's) own behavior with their own fakes, so this only
// needs to prove the API layer's plumbing (request -> DeployBuildAsync
// call -> 202/error mapping) is wired correctly. It always closes
// prepared, matching the real method's "always takes ownership" contract.
func (f *fakeDeployer) DeployBuildAsync(ctx context.Context, app *store.App, prepared *build.PreparedBuild, builder *build.Builder) (*store.Deployment, error) {
	f.deployBuildCalled = true
	f.deployBuildBuilder = builder
	f.deployBuildPrepared = prepared
	if prepared != nil {
		prepared.Close()
	}

	dep, err := f.st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		return nil, err
	}
	if f.deployBuildAsyncErr != nil {
		_ = f.st.FinishDeployment(dep.ID, "failed", f.deployBuildAsyncErr.Error())
		return nil, f.deployBuildAsyncErr
	}

	logPath := fmt.Sprintf("/fake/data/apps/%s/builds/%d.log", app.Slug, dep.Number)
	_ = f.st.SetDeploymentBuildLog(dep.ID, logPath)
	dep.BuildLogPath = logPath
	// Deliberately left "deploying": the real DeployBuildAsync's own
	// synchronous half never finishes the row itself — a background
	// goroutine does, later (see internal/deploy's own tests for that
	// behavior). A test that needs a terminal state calls
	// f.st.FinishDeployment directly after asserting the 202 response.
	return dep, nil
}

// Rollback is a lightweight stand-in for deploy.Engine.Rollback: like
// DeployBuild above, it persists a real deployment row through the store
// (copying the target deployment's own image/source, as the real engine
// does) rather than exercising any rollout logic — internal/deploy already
// covers Rollback's own behavior (pull-vs-skip, retention, typed errors)
// with its own fakes, so this only needs to prove the API layer's
// plumbing (request -> Rollback call -> response/error mapping) is wired
// correctly.
func (f *fakeDeployer) Rollback(ctx context.Context, app *store.App, targetNumber int) (*store.Deployment, error) {
	f.rollbackCalledApp = app.Slug
	f.rollbackCalledNumber = targetNumber
	if f.rollbackErr != nil {
		return nil, f.rollbackErr
	}

	target, err := f.st.DeploymentByNumber(app.ID, targetNumber)
	if err != nil {
		return nil, err
	}
	dep, err := f.st.CreateDeploymentFull(app.ID, target.ImageRef, target.Source, "rollback")
	if err != nil {
		return nil, err
	}
	_ = f.st.FinishDeployment(dep.ID, "healthy", "")
	dep.Status = "healthy"
	return dep, nil
}

func (f *fakeDeployer) RemoveApp(ctx context.Context, app *store.App) error {
	f.removeCalled = true
	return f.removeErr
}

// DeployExisting is a lightweight stand-in for
// deploy.Engine.DeployExisting (v0.5 Task 8): it finishes the
// caller-supplied deployment row itself (mirroring how the real engine's
// runRollout ultimately calls store.FinishDeployment), rather than
// creating a new one the way Deploy above does — reusing an
// already-created row is the whole point of DeployExisting.
func (f *fakeDeployer) DeployExisting(ctx context.Context, app *store.App, dep *store.Deployment, imageRef string) (*store.Deployment, error) {
	f.deployExistingCalls = append(f.deployExistingCalls, app.Slug)
	if f.deployExistingErr != nil {
		_ = f.st.FinishDeployment(dep.ID, "failed", f.deployExistingErr.Error())
		return nil, f.deployExistingErr
	}
	_ = f.st.UpdateAppStatus(app.ID, "running")
	_ = f.st.FinishDeployment(dep.ID, "healthy", "")
	dep.Status = "healthy"
	return dep, nil
}

// DeployBuildExisting is DeployExisting's build-from-tar twin.
func (f *fakeDeployer) DeployBuildExisting(ctx context.Context, app *store.App, dep *store.Deployment, gzTar io.Reader, builder *build.Builder) (*store.Deployment, error) {
	f.deployBuildExistingCalls = append(f.deployBuildExistingCalls, app.Slug)
	if f.deployBuildExistingErr != nil {
		_ = f.st.FinishDeployment(dep.ID, "failed", f.deployBuildExistingErr.Error())
		return nil, f.deployBuildExistingErr
	}
	_ = f.st.UpdateAppStatus(app.ID, "running")
	_ = f.st.FinishDeployment(dep.ID, "healthy", "")
	dep.Status = "healthy"
	return dep, nil
}

func fakePinger(err error) Pinger {
	return func(ctx context.Context) error { return err }
}

// newTestStore opens a real, temp-file-backed store and seeds one user.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("admin@example.com", "Admin", hash, true); err != nil {
		t.Fatal(err)
	}

	return st
}

// newTestServer serves the API handler over httptest against st and dep,
// with seal/open closures over a fixed test key and routes as the
// RoutesApplier. Tests that don't exercise the logs endpoint get a stub
// LogSource that always fails; logs_test.go builds its own server with a
// scripted one instead.
func newTestServer(t *testing.T, st *store.Store, dep Deployer, routes RoutesApplier) *httptest.Server {
	t.Helper()
	return newTestServerWithLogs(t, st, dep, routes, unusedLogSource)
}

// newTestServerWithLogs is newTestServer with an explicit LogSource, for
// tests that need to script the app-logs endpoint. It passes a nil
// builder through to New — fine for every test that doesn't specifically
// assert on the *build.Builder handleDeployTarball forwards to
// Deployer.DeployBuild (see newTestServerWithBuilder).
func newTestServerWithLogs(t *testing.T, st *store.Store, dep Deployer, routes RoutesApplier, logs LogSource) *httptest.Server {
	t.Helper()
	return newTestServerWithBuilder(t, st, dep, routes, logs, nil)
}

// newTestServerWithBuilder is the fullest-control constructor, for tests
// (deploy_tarball_test.go) that need to assert the *build.Builder passed
// to New is exactly what reaches Deployer.DeployBuild.
func newTestServerWithBuilder(t *testing.T, st *store.Store, dep Deployer, routes RoutesApplier, logs LogSource, builder *build.Builder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(st, dep, fakePinger(nil), "test-version", testSeal, testOpen, routes, logs, builder))
	t.Cleanup(srv.Close)
	return srv
}

// unusedLogSource is the default LogSource for tests that don't exercise
// GET .../logs; calling it is a test bug, so it fails loudly rather than
// returning an empty stream that would silently hide the mistake.
func unusedLogSource(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
	return nil, errors.New("unusedLogSource: this test's LogSource was not expected to be called")
}

// decodeInto JSON-decodes resp's body into out, failing the test on a
// decode error. Shared by doJSON and tests (e.g. deploy_tarball_test.go)
// that build their own *http.Request rather than going through doJSON.
func decodeInto(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// doJSON performs an HTTP request with an optional JSON body and optional
// bearer token, decoding the JSON response body into out (if non-nil).
func doJSON(t *testing.T, method, url, token string, payload, out any) *http.Response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if out != nil {
		decodeInto(t, resp, out)
	}
	return resp
}

// login logs in with the given password and returns the response and its
// decoded body.
func login(t *testing.T, srv *httptest.Server, password string) (*http.Response, loginResponse) {
	t.Helper()
	var out loginResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: "admin@example.com", Password: password}, &out)
	return resp, out
}

func TestLoginAndMe(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	if resp, _ := login(t, srv, "wrong-password"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: got status %d, want 401", resp.StatusCode)
	}

	resp, body := login(t, srv, testPassword)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good password: got status %d, want 200", resp.StatusCode)
	}
	if body.Token == "" {
		t.Fatal("expected non-empty token")
	}
	if body.User.Email != "admin@example.com" || body.User.Name != "Admin" {
		t.Fatalf("unexpected user in login response: %+v", body.User)
	}

	var me userResponse
	meResp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", body.Token, nil, &me)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/me: got status %d, want 200", meResp.StatusCode)
	}
	if me.Email != "admin@example.com" {
		t.Fatalf("/me returned %+v", me)
	}
}

func TestLogout(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	_, body := login(t, srv, testPassword)

	logoutResp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/logout", body.Token, nil, nil)
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: got status %d, want 204", logoutResp.StatusCode)
	}

	meResp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", body.Token, nil, nil)
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after logout: got status %d, want 401", meResp.StatusCode)
	}
}

func TestLogoutWithoutTokenUnauthorized(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/logout", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout without token: got status %d, want 401", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	for _, tok := range []string{"", "garbage-token"} {
		resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps", tok, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: got status %d, want 401", tok, resp.StatusCode)
		}
	}
}

func TestAppLifecycle(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	// create
	var created appResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got status %d, want 201", resp.StatusCode)
	}
	if created.Slug != "my-blog" || created.Image != "nginx:alpine" || created.Port != 80 {
		t.Fatalf("unexpected created app: %+v", created)
	}

	// duplicate slug -> 409
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: got status %d, want 409", resp.StatusCode)
	}

	// bad name -> 422
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My App!", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad name: got status %d, want 422", resp.StatusCode)
	}

	// deploy (no image in body -> falls back to the app's image from create)
	var deployed deploymentResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/deploy", token, nil, &deployed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy: got status %d, want 200", resp.StatusCode)
	}
	if dep.deployedApp != "my-blog" || dep.deployedImage != "nginx:alpine" {
		t.Fatalf("deployer called with app=%q image=%q, want my-blog/nginx:alpine", dep.deployedApp, dep.deployedImage)
	}
	if deployed.Status != "healthy" {
		t.Fatalf("unexpected deployment response: %+v", deployed)
	}
	if deployed.StartedAt == "" {
		t.Fatalf("expected non-empty started_at on deploy response: %+v", deployed)
	}

	// get -> contains the deployment
	var got appDetailResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog", token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: got status %d, want 200", resp.StatusCode)
	}
	if len(got.Deployments) != 1 || got.Deployments[0].Image != "nginx:alpine" {
		t.Fatalf("expected 1 deployment with image nginx:alpine, got %+v", got.Deployments)
	}
	if got.Deployments[0].StartedAt == "" || got.Deployments[0].FinishedAt == "" {
		t.Fatalf("expected non-empty started_at/finished_at on a finished deployment, got %+v", got.Deployments[0])
	}

	// delete -> 204
	resp = doJSON(t, http.MethodDelete, srv.URL+"/api/v1/apps/my-blog", token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got status %d, want 204", resp.StatusCode)
	}
	if !dep.removeCalled {
		t.Fatal("expected RemoveApp to be called")
	}

	// then 404
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog", token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: got status %d, want 404", resp.StatusCode)
	}
}

func TestDeployFailureSurfaces(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st, deployErr: errors.New("pull failed: no such image")}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "Broken", Image: "bad:image", Port: 8080}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got status %d, want 201", resp.StatusCode)
	}

	var errBody errorResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/broken/deploy", token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("deploy failure: got status %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "deploy_failed" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
	if !strings.Contains(errBody.Error.Message, "pull failed") {
		t.Fatalf("expected message to surface underlying error, got %+v", errBody)
	}
}

// setupEnvDomainsTest creates a store (with root_domain "example.com"
// set) and a server backed by a fresh fakeRoutesApplier, logs in, and
// creates one app ("my-blog"). It returns everything env/domains tests
// need to exercise those endpoints and verify store-level side effects
// directly.
func setupEnvDomainsTest(t *testing.T) (srv *httptest.Server, st *store.Store, token string, appID int64, routes *fakeRoutesApplier) {
	t.Helper()

	st = newTestStore(t)
	if err := st.SetSetting("root_domain", "example.com"); err != nil {
		t.Fatal(err)
	}
	routes = &fakeRoutesApplier{}
	srv = newTestServer(t, st, &fakeDeployer{st: st}, routes)

	_, session := login(t, srv, testPassword)
	token = session.Token

	var created appResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: got status %d, want 201", resp.StatusCode)
	}

	app, err := st.AppBySlug(created.Slug)
	if err != nil {
		t.Fatal(err)
	}
	return srv, st, token, app.ID, routes
}

func TestEnvRoundtrip(t *testing.T) {
	srv, st, token, appID, _ := setupEnvDomainsTest(t)

	// initially empty
	var got []envVarResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/env", token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: got status %d, want 200", resp.StatusCode)
	}
	if len(got) != 0 {
		t.Fatalf("expected no env vars, got %+v", got)
	}

	// first PUT: one plain var, one secret
	put := []envVarResponse{
		{Key: "PORT", Value: "8080", IsSecret: false},
		{Key: "API_KEY", Value: "supersecret", IsSecret: true},
	}
	var putResp []envVarResponse
	resp = doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put, &putResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: got status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get(redeployRequiredHeader) != "true" {
		t.Fatalf("expected %s: true on first put, got %q", redeployRequiredHeader, resp.Header.Get(redeployRequiredHeader))
	}

	byKey := envByKey(putResp)
	if byKey["PORT"].Value != "8080" || byKey["PORT"].IsSecret {
		t.Fatalf("unexpected PORT in put response: %+v", byKey["PORT"])
	}
	if byKey["API_KEY"].Value != "" || !byKey["API_KEY"].IsSecret {
		t.Fatalf("expected API_KEY masked in put response, got %+v", byKey["API_KEY"])
	}

	// GET matches
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/env", token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: got status %d, want 200", resp.StatusCode)
	}
	byKey = envByKey(got)
	if byKey["PORT"].Value != "8080" || byKey["API_KEY"].Value != "" || !byKey["API_KEY"].IsSecret {
		t.Fatalf("unexpected env after first put: %+v", got)
	}

	// capture the stored sealed secret value to compare against later
	storedBefore, err := st.ListEnvVars(appID)
	if err != nil {
		t.Fatal(err)
	}
	sealedBefore := envVarByKey(storedBefore, "API_KEY").ValueEncrypted

	// second PUT: keep-on-empty-secret (same PORT value, empty API_KEY value)
	put2 := []envVarResponse{
		{Key: "PORT", Value: "8080", IsSecret: false},
		{Key: "API_KEY", Value: "", IsSecret: true},
	}
	resp = doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put2, &putResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put2: got status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get(redeployRequiredHeader) != "false" {
		t.Fatalf("expected %s: false when nothing changed, got %q", redeployRequiredHeader, resp.Header.Get(redeployRequiredHeader))
	}

	storedAfter, err := st.ListEnvVars(appID)
	if err != nil {
		t.Fatal(err)
	}
	sealedAfter := envVarByKey(storedAfter, "API_KEY").ValueEncrypted
	if sealedAfter != sealedBefore {
		t.Fatal("expected keep-on-empty-secret to leave the stored sealed value unchanged")
	}
	plain, err := testOpen(appID, "API_KEY", sealedAfter)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "supersecret" {
		t.Fatalf("expected stored secret to still decrypt to %q, got %q", "supersecret", plain)
	}

	// third PUT: actually change the secret's value
	put3 := []envVarResponse{
		{Key: "PORT", Value: "8080", IsSecret: false},
		{Key: "API_KEY", Value: "newsecret", IsSecret: true},
	}
	resp = doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put3, &putResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put3: got status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get(redeployRequiredHeader) != "true" {
		t.Fatalf("expected %s: true when secret value changed, got %q", redeployRequiredHeader, resp.Header.Get(redeployRequiredHeader))
	}

	storedFinal, err := st.ListEnvVars(appID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = testOpen(appID, "API_KEY", envVarByKey(storedFinal, "API_KEY").ValueEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "newsecret" {
		t.Fatalf("expected updated secret to decrypt to %q, got %q", "newsecret", plain)
	}

	// dropping a key removes it entirely (full replace semantics)
	put4 := []envVarResponse{{Key: "PORT", Value: "8080", IsSecret: false}}
	resp = doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put4, &putResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put4: got status %d, want 200", resp.StatusCode)
	}
	if len(putResp) != 1 || putResp[0].Key != "PORT" {
		t.Fatalf("expected only PORT to remain, got %+v", putResp)
	}
}

func envByKey(vars []envVarResponse) map[string]envVarResponse {
	out := make(map[string]envVarResponse, len(vars))
	for _, v := range vars {
		out[v.Key] = v
	}
	return out
}

func envVarByKey(vars []store.EnvVar, key string) store.EnvVar {
	for _, v := range vars {
		if v.Key == key {
			return v
		}
	}
	return store.EnvVar{}
}

func TestEnvKeyValidation(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	for _, key := range []string{"port", "My-Key", "1KEY", ""} {
		var errBody errorResponse
		resp := doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token,
			[]envVarResponse{{Key: key, Value: "x"}}, &errBody)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("key %q: got status %d, want 422", key, resp.StatusCode)
		}
		if errBody.Error.Code != "validation" {
			t.Fatalf("key %q: unexpected error code %+v", key, errBody)
		}
		if !strings.Contains(errBody.Error.Message, key) {
			t.Fatalf("key %q: expected message to name the offending key, got %+v", key, errBody)
		}
	}
}

// TestCreateAppRejectsReservedSlugs proves handleCreateApp rejects the two
// exact reserved slugs ("caddy", the managed proxy's own container name;
// "basepod", the control plane itself) and any slug beginning with either
// generated-name prefix ("bp-", the container-name prefix; "app-", the
// network-alias prefix — audit finding M1) with a 422 validation error
// naming the rule, before ever reaching CreateApp — and that a
// non-reserved slug is unaffected.
func TestCreateAppRejectsReservedSlugs(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	for _, name := range []string{"Caddy", "Basepod", "bp-anything", "app-anything", "bp-", "app-"} {
		var errBody errorResponse
		resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
			createAppRequest{Name: name, Image: "nginx:alpine", Port: 80}, &errBody)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("name %q: got status %d, want 422", name, resp.StatusCode)
		}
		if errBody.Error.Code != "validation" {
			t.Fatalf("name %q: unexpected error code %+v", name, errBody)
		}
		if !strings.Contains(errBody.Error.Message, "reserved") {
			t.Fatalf("name %q: expected message to explain the slug is reserved, got %+v", name, errBody)
		}
	}

	// A non-reserved slug still succeeds.
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("non-reserved slug: got status %d, want 201", resp.StatusCode)
	}
}

func TestDomainsLifecycle(t *testing.T) {
	srv, _, token, _, routes := setupEnvDomainsTest(t)

	// list: generated only, no custom domains yet
	var list domainsListResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/domains", token, nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: got status %d, want 200", resp.StatusCode)
	}
	if list.Generated != "my-blog.example.com" || len(list.Custom) != 0 {
		t.Fatalf("unexpected initial domains list: %+v", list)
	}

	// add, with mixed case to verify lowercasing
	var added domainResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "WWW.Example.org"}, &added)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add: got status %d, want 201", resp.StatusCode)
	}
	if added.Hostname != "www.example.org" {
		t.Fatalf("expected lowercased hostname, got %q", added.Hostname)
	}
	if routes.callCount() != 1 {
		t.Fatalf("expected ApplyRoutes called once after add, got %d", routes.callCount())
	}

	// list again: custom domain present
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/domains", token, nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Custom) != 1 || list.Custom[0].Hostname != "www.example.org" {
		t.Fatalf("unexpected domains list after add: status=%d body=%+v", resp.StatusCode, list)
	}

	// delete
	resp = doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/apps/my-blog/domains/%d", srv.URL, added.ID), token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got status %d, want 204", resp.StatusCode)
	}
	if routes.callCount() != 2 {
		t.Fatalf("expected ApplyRoutes called again after delete, got %d", routes.callCount())
	}

	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/domains", token, nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Custom) != 0 {
		t.Fatalf("expected no custom domains after delete, got %+v", list)
	}
}

func TestDomainDuplicateConflict(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "Second App", Image: "nginx:alpine", Port: 81}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create second app: got status %d, want 201", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "shared.example.org"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first add: got status %d, want 201", resp.StatusCode)
	}

	var errBody errorResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/second-app/domains", token,
		addDomainRequest{Hostname: "shared.example.org"}, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: got status %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "domain_exists" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

func TestDomainGeneratedCollision(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "Second App", Image: "nginx:alpine", Port: 81}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create second app: got status %d, want 201", resp.StatusCode)
	}

	// colliding with another app's generated hostname
	var errBody errorResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "second-app.example.com"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("collision with other app: got status %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	// colliding with its own generated hostname
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "my-blog.example.com"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("collision with own generated hostname: got status %d, want 422", resp.StatusCode)
	}
}

// TestDomainDashboardCollision proves handleAddDomain rejects a custom
// domain hostname that equals the dashboard_domain setting — the dashboard
// route is prepended terminal-first in the rendered Caddy config (see
// caddy.Render), so a custom app domain with the same hostname would never
// actually reach the app.
func TestDomainDashboardCollision(t *testing.T) {
	srv, st, token, _, _ := setupEnvDomainsTest(t)
	if err := st.SetSetting("dashboard_domain", "basepod.example.com"); err != nil {
		t.Fatal(err)
	}

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "basepod.example.com"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("collision with dashboard domain: got status %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// TestDomainDashboardOffOrUnsetNoCollision proves a dashboard_domain of ""
// (unset) or the literal "off" never blocks a custom domain add — neither
// value is actually routed anywhere, so there's nothing to collide with.
func TestDomainDashboardOffOrUnsetNoCollision(t *testing.T) {
	for _, dashboardDomain := range []string{"", "off"} {
		t.Run("dashboard_domain="+dashboardDomain, func(t *testing.T) {
			srv, st, token, _, _ := setupEnvDomainsTest(t)
			if dashboardDomain != "" {
				if err := st.SetSetting("dashboard_domain", dashboardDomain); err != nil {
					t.Fatal(err)
				}
			}

			resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
				addDomainRequest{Hostname: "not-a-real-collision.example.com"}, nil)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("got status %d, want 201", resp.StatusCode)
			}
		})
	}
}

// TestCreateAppDashboardCollision proves handleCreateApp rejects a slug
// whose generated hostname (slug.rootDomain) equals the dashboard_domain
// setting.
func TestCreateAppDashboardCollision(t *testing.T) {
	st := newTestStore(t)
	if err := st.SetSetting("root_domain", "example.com"); err != nil {
		t.Fatal(err)
	}
	// Deliberately not "basepod.example.com": "basepod" is itself a
	// reserved slug (see reservedSlugs), which would trip that check
	// first and leave this test not actually exercising the
	// dashboard-domain-collision path it's named for.
	if err := st.SetSetting("dashboard_domain", "control.example.com"); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "Control", Image: "nginx:alpine", Port: 80}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("collision with dashboard domain: got status %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	// A non-colliding slug still succeeds with the same dashboard_domain
	// setting in place.
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("non-colliding slug: got status %d, want 201", resp.StatusCode)
	}
}

func TestDomainCrossAppDelete(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "Second App", Image: "nginx:alpine", Port: 81}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create second app: got status %d, want 201", resp.StatusCode)
	}

	var added domainResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "belongs-to-my-blog.example.org"}, &added)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add: got status %d, want 201", resp.StatusCode)
	}

	var errBody errorResponse
	resp = doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/apps/second-app/domains/%d", srv.URL, added.ID), token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-app delete: got status %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "domain_not_found" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	// unaffected: still deletable from the owning app
	resp = doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/apps/my-blog/domains/%d", srv.URL, added.ID), token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owning-app delete: got status %d, want 204", resp.StatusCode)
	}
}

// TestDomainAddRollsBackOnRoutesFailure verifies that when ApplyRoutes
// fails after a domain row was already committed, the API rolls the
// insert back rather than leaving the DB claiming a domain Caddy never
// actually picked up.
func TestDomainAddRollsBackOnRoutesFailure(t *testing.T) {
	srv, _, token, _, routes := setupEnvDomainsTest(t)
	routes.err = errors.New("caddy unreachable")

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "rollback.example.org"}, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("add with routes failure: got status %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "routes_failed" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	var list domainsListResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/domains", token, nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Custom) != 0 {
		t.Fatalf("expected the failed add to be rolled back, got status=%d body=%+v", resp.StatusCode, list)
	}
}

// TestDomainDeleteRollsBackOnRoutesFailure verifies that when ApplyRoutes
// fails after a domain row was already deleted, the API re-adds the
// domain rather than silently dropping it.
func TestDomainDeleteRollsBackOnRoutesFailure(t *testing.T) {
	srv, _, token, _, routes := setupEnvDomainsTest(t)

	var added domainResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "keep.example.org"}, &added)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add: got status %d, want 201", resp.StatusCode)
	}

	routes.err = errors.New("caddy unreachable")

	var errBody errorResponse
	resp = doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/apps/my-blog/domains/%d", srv.URL, added.ID), token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("delete with routes failure: got status %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "routes_failed" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	var list domainsListResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/domains", token, nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Custom) != 1 || list.Custom[0].Hostname != "keep.example.org" {
		t.Fatalf("expected the failed delete to be rolled back, got status=%d body=%+v", resp.StatusCode, list)
	}
}

// TestCreateAppCustomDomainCollision proves handleCreateApp rejects a slug
// whose generated hostname collides with an existing custom domain.
func TestCreateAppCustomDomainCollision(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	// Create a second app and add a custom domain to it with the name
	// that will collide with the slug we'll try to create later.
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "First App", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create first app: got status %d, want 201", resp.StatusCode)
	}

	// Add a custom domain "taken.example.com" to the first app.
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/first-app/domains", token,
		addDomainRequest{Hostname: "taken.example.com"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add domain: got status %d, want 201", resp.StatusCode)
	}

	// Try to create an app with slug "taken", whose generated hostname
	// would be "taken.example.com", colliding with the custom domain.
	var errBody errorResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", token,
		createAppRequest{Name: "Taken", Image: "nginx:alpine", Port: 81}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("collision with custom domain: got status %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// TestEnvDuplicateKey proves handlePutEnv rejects a payload containing
// duplicate keys (case-sensitive exact matches).
func TestEnvDuplicateKey(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	// Submit a payload with the same key twice
	resp := doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token,
		[]envVarResponse{
			{Key: "FOO", Value: "first", IsSecret: false},
			{Key: "FOO", Value: "second", IsSecret: false},
		}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate key: got status %d, want 422", resp.StatusCode)
	}

	var errBody errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
	if !strings.Contains(errBody.Error.Message, "FOO") {
		t.Fatalf("expected error message to name the duplicated key, got %+v", errBody)
	}
}
