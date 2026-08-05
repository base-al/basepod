package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// Compile-time assertion that *deploy.Engine satisfies the Deployer
// interface this package defines and consumes.
var _ Deployer = (*deploy.Engine)(nil)

const testPassword = "correct-password"

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

// newTestServer serves the API handler over httptest against st and dep.
func newTestServer(t *testing.T, st *store.Store, dep Deployer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(st, dep, fakePinger(nil), "test-version"))
	t.Cleanup(srv.Close)
	return srv
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
	srv := newTestServer(t, st, &fakeDeployer{st: st})

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
	srv := newTestServer(t, st, &fakeDeployer{st: st})

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
	srv := newTestServer(t, st, dep)
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

	// get -> contains the deployment
	var got appDetailResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog", token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: got status %d, want 200", resp.StatusCode)
	}
	if len(got.Deployments) != 1 || got.Deployments[0].Image != "nginx:alpine" {
		t.Fatalf("expected 1 deployment with image nginx:alpine, got %+v", got.Deployments)
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
	srv := newTestServer(t, st, dep)
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
