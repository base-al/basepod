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

func testSeal(plaintext string) (string, error) { return crypto.Seal(testKey, plaintext) }
func testOpen(sealed string) (string, error)    { return crypto.Open(testKey, sealed) }

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

func (f *fakeDeployer) RemoveApp(ctx context.Context, app *store.App) error {
	f.removeCalled = true
	return f.removeErr
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
// tests that need to script the app-logs endpoint.
func newTestServerWithLogs(t *testing.T, st *store.Store, dep Deployer, routes RoutesApplier, logs LogSource) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(st, dep, fakePinger(nil), "test-version", testSeal, testOpen, routes, logs))
	t.Cleanup(srv.Close)
	return srv
}

// unusedLogSource is the default LogSource for tests that don't exercise
// GET .../logs; calling it is a test bug, so it fails loudly rather than
// returning an empty stream that would silently hide the mistake.
func unusedLogSource(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
	return nil, errors.New("unusedLogSource: this test's LogSource was not expected to be called")
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
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response body: %v", err)
		}
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
	plain, err := testOpen(sealedAfter)
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
	plain, err = testOpen(envVarByKey(storedFinal, "API_KEY").ValueEncrypted)
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
