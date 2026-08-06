package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/gitsource"
	"github.com/base-al/basepod/internal/store"
)

// gitFetchCall records one GitFetcher.Fetch invocation — used to assert
// exactly what a caller (handleDeployGit, or the webhook receiver's
// triggerGitDeploy) actually asked to be cloned.
type gitFetchCall struct {
	url, branch, token string
}

// fakeGitFetcher is a test double for GitFetcher: scripted to succeed
// (returning tarBody/headSHA) or fail (err), and records every call it
// receives — the record is what proves (see webhook_test.go's
// hostile-payload test) that the clone URL/branch/token always come from
// the stored git_sources config, never from a webhook payload.
type fakeGitFetcher struct {
	mu      sync.Mutex
	calls   []gitFetchCall
	tarBody []byte
	headSHA string
	err     error

	// entered/block, if non-nil, let a test synchronize on "Fetch has
	// actually started" and hold it there until explicitly released — used
	// by the coalescing tests (webhook_test.go) to keep one deploy
	// "running" while further pushes arrive.
	entered chan struct{}
	block   <-chan struct{}
}

func (f *fakeGitFetcher) Fetch(ctx context.Context, rawURL, branch, token string) (io.ReadCloser, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, gitFetchCall{url: rawURL, branch: branch, token: token})
	tarBody, headSHA, err := f.tarBody, f.headSHA, f.err
	f.mu.Unlock()

	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}
	if err != nil {
		return nil, "", err
	}
	return io.NopCloser(bytes.NewReader(tarBody)), headSHA, nil
}

func (f *fakeGitFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeGitFetcher) lastCall() gitFetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

// setupGitTest creates a store + trusted server (with an app "my-blog"
// created and logged in), wired to gitFetcher (nil is fine — tests that
// don't exercise the fetch path pass nil). Returns the server's base URL
// and an authenticated session token.
func setupGitTest(t *testing.T, gitFetcher GitFetcher) (srv string, token string) {
	t.Helper()
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	s := newTestServerWithGit(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder, gitFetcher)

	_, session := login(t, s, testPassword)
	tok := session.Token

	resp := doJSON(t, http.MethodPost, s.URL+"/api/v1/apps", tok,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: got status %d, want 201", resp.StatusCode)
	}

	return s.URL, tok
}

// TestGitSourceConnectReadRotateDisconnect proves the full PUT/GET/DELETE
// lifecycle: first connect mints a hook_id and secret; a re-PUT (no
// rotate_secret) keeps both stable; rotate_secret mints fresh ones;
// DELETE disconnects, after which GET reports 404 "git_not_connected".
func TestGitSourceConnectReadRotateDisconnect(t *testing.T) {
	srv, token := setupGitTest(t, nil)

	var first gitSourceResponse
	resp := doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main"}, &first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: got status %d, want 200", resp.StatusCode)
	}
	if first.HookID == "" || first.Secret == "" {
		t.Fatalf("expected hook_id/secret to be minted, got %+v", first)
	}
	if first.Provider != "github" {
		t.Fatalf("Provider = %q, want github (detected from hostname)", first.Provider)
	}
	if !strings.Contains(first.WebhookURL, first.HookID) {
		t.Fatalf("webhook_url %q does not contain hook_id %q", first.WebhookURL, first.HookID)
	}

	// GET returns the same hook_id/secret.
	var got gitSourceResponse
	resp = doJSON(t, http.MethodGet, srv+"/api/v1/apps/my-blog/git", token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: got status %d, want 200", resp.StatusCode)
	}
	if got.HookID != first.HookID || got.Secret != first.Secret {
		t.Fatalf("GET diverged from PUT: got %+v, want hook_id/secret matching %+v", got, first)
	}

	// Re-PUT (different branch, no rotate_secret) keeps hook_id/secret
	// stable — an operator changing the branch must not have to
	// reconfigure the webhook on the forge side.
	var second gitSourceResponse
	resp = doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "develop"}, &second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-PUT: got status %d, want 200", resp.StatusCode)
	}
	if second.HookID != first.HookID || second.Secret != first.Secret {
		t.Fatalf("re-PUT changed hook_id/secret: got %+v, want them stable from %+v", second, first)
	}
	if second.Branch != "develop" {
		t.Fatalf("Branch = %q, want develop", second.Branch)
	}

	// rotate_secret mints a fresh hook_id and secret.
	var rotated gitSourceResponse
	resp = doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "develop", RotateSecret: true}, &rotated)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate PUT: got status %d, want 200", resp.StatusCode)
	}
	if rotated.HookID == first.HookID || rotated.Secret == first.Secret {
		t.Fatalf("rotate_secret did not change hook_id/secret: got %+v", rotated)
	}

	// DELETE disconnects.
	resp = doJSON(t, http.MethodDelete, srv+"/api/v1/apps/my-blog/git", token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: got status %d, want 204", resp.StatusCode)
	}

	var errBody errorResponse
	resp = doJSON(t, http.MethodGet, srv+"/api/v1/apps/my-blog/git", token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete: got status %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "git_not_connected" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// TestGitSourceTokenMaskedOnGet proves a deploy token is write-only: PUT
// with a token reports it as "set" (never the plaintext, unlike the
// webhook secret — a deliberate deviation, see gitSourceResponse's doc
// comment), and a subsequent PUT with no token keeps the stored one
// (matching env var PUT's keep-on-empty-secret semantics).
func TestGitSourceTokenMaskedOnGet(t *testing.T) {
	srv, token := setupGitTest(t, nil)

	var resp1 gitSourceResponse
	resp := doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main", Token: "ghp_supersecret"}, &resp1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: got status %d, want 200", resp.StatusCode)
	}
	if resp1.Token != "set" {
		t.Fatalf("Token = %q, want \"set\"", resp1.Token)
	}
	if strings.Contains(resp1.Secret, "ghp_supersecret") {
		t.Fatal("the deploy token leaked into the response body")
	}

	var resp2 gitSourceResponse
	resp = doJSON(t, http.MethodGet, srv+"/api/v1/apps/my-blog/git", token, nil, &resp2)
	if resp.StatusCode != http.StatusOK || resp2.Token != "set" {
		t.Fatalf("GET: status=%d Token=%q, want 200/\"set\"", resp.StatusCode, resp2.Token)
	}

	// Re-PUT with no token keeps the stored one ("set" persists, not "").
	var resp3 gitSourceResponse
	resp = doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main"}, &resp3)
	if resp.StatusCode != http.StatusOK || resp3.Token != "set" {
		t.Fatalf("re-PUT without token: status=%d Token=%q, want 200/\"set\" (token preserved)", resp.StatusCode, resp3.Token)
	}
}

// TestGitSourcePutValidation proves handlePutGitSource rejects an empty
// branch, an unparseable URL, and a non-https scheme.
func TestGitSourcePutValidation(t *testing.T) {
	srv, token := setupGitTest(t, nil)

	cases := []putGitSourceRequest{
		{URL: "https://github.com/example/repo.git", Branch: ""},
		{URL: "not a url", Branch: "main"},
		{URL: "ssh://git@github.com/example/repo.git", Branch: "main"},
		{URL: "http://github.com/example/repo.git", Branch: "main"},
	}
	for _, req := range cases {
		var errBody errorResponse
		resp := doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token, req, &errBody)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("req %+v: got status %d, want 422", req, resp.StatusCode)
		}
		if errBody.Error.Code != "validation" {
			t.Fatalf("req %+v: unexpected error code %+v", req, errBody)
		}
	}
}

// TestDeployGitHappyPath proves POST .../deploy/git clones the stored
// config synchronously, hands the result to DeployGitAsync, and returns
// 202 with the deployment JSON carrying the resolved commit SHA.
func TestDeployGitHappyPath(t *testing.T) {
	fetcher := &fakeGitFetcher{tarBody: validTarballBody(t), headSHA: "resolved0123456"}
	srv, token := setupGitTest(t, fetcher)

	resp := doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main", Token: "ghp_x"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: got status %d, want 200", resp.StatusCode)
	}

	var dep deploymentResponse
	resp = doJSON(t, http.MethodPost, srv+"/api/v1/apps/my-blog/deploy/git", token, nil, &dep)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy/git: got status %d, want 202", resp.StatusCode)
	}
	if dep.GitSha != "resolved0123456" {
		t.Fatalf("deployment response GitSha = %q, want the clone's resolved HEAD", dep.GitSha)
	}

	if fetcher.callCount() != 1 {
		t.Fatalf("expected exactly 1 Fetch call, got %d", fetcher.callCount())
	}
	call := fetcher.lastCall()
	if call.url != "https://github.com/example/repo.git" || call.branch != "main" || call.token != "ghp_x" {
		t.Fatalf("Fetch called with %+v, want the stored config's url/branch/token", call)
	}
}

// TestDeployGitNotConnected proves POST .../deploy/git 422s with
// "git_not_connected" for an app with no git source configured.
func TestDeployGitNotConnected(t *testing.T) {
	srv, token := setupGitTest(t, &fakeGitFetcher{})

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv+"/api/v1/apps/my-blog/deploy/git", token, nil, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "git_not_connected" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// TestDeployGitUnavailable proves a nil gitFetcher (git binary not
// installed at boot — see gitsource.BinPath) maps to 503
// "git_unavailable" rather than a panic or a generic 500.
func TestDeployGitUnavailable(t *testing.T) {
	srv, token := setupGitTest(t, nil)

	doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main"}, nil)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv+"/api/v1/apps/my-blog/deploy/git", token, nil, &errBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", resp.StatusCode)
	}
	if errBody.Error.Code != "git_unavailable" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// TestDeployGitCloneFailureMapsTo502 proves a clone failure (bad branch,
// unreachable remote, ...) is reported synchronously as 502
// "git_clone_failed" with the underlying (sanitized) error message — the
// v0.5 plan's Task 4 explicitly wants this synchronous, unlike the
// webhook path, so an operator testing a fresh connection gets actionable
// feedback immediately.
func TestDeployGitCloneFailureMapsTo502(t *testing.T) {
	fetcher := &fakeGitFetcher{err: newGitCloneTestErr()}
	srv, token := setupGitTest(t, fetcher)

	doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "no-such-branch"}, nil)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv+"/api/v1/apps/my-blog/deploy/git", token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got status %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "git_clone_failed" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
	if !strings.Contains(errBody.Error.Message, "unknown branch") {
		t.Fatalf("expected message to surface the underlying error, got %+v", errBody)
	}
}

// TestDeployGitUnavailableViaErrGitUnavailable proves the specific
// gitsource.ErrGitUnavailable sentinel (as a real *gitsource.Cloner with
// no resolved git binary would return) is mapped to 503, not the generic
// 502 every other Fetch error gets.
func TestDeployGitUnavailableViaErrGitUnavailable(t *testing.T) {
	fetcher := &fakeGitFetcher{err: gitsource.ErrGitUnavailable}
	srv, token := setupGitTest(t, fetcher)

	doJSON(t, http.MethodPut, srv+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main"}, nil)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv+"/api/v1/apps/my-blog/deploy/git", token, nil, &errBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", resp.StatusCode)
	}
	if errBody.Error.Code != "git_unavailable" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// newGitCloneTestErr builds a realistic gitsource.Fetch-shaped clone
// error for TestDeployGitCloneFailureMapsTo502 — a plain error with the
// message shape gitsource itself would produce, not a real clone.
func newGitCloneTestErr() error {
	return errors.New("gitsource: git failed: fatal: unknown branch")
}

// TestListGitDeliveriesResolvesDeploymentNumber proves GET
// .../apps/{slug}/git/deliveries resolves a delivery's stored
// deployment_id into the deployment's own per-app Number for display, and
// omits it entirely for a delivery that never triggered a deployment.
func TestListGitDeliveriesResolvesDeploymentNumber(t *testing.T) {
	st := newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	s := newTestServerWithGit(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder, nil)

	_, session := login(t, s, testPassword)
	token := session.Token
	resp := doJSON(t, http.MethodPost, s.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: got status %d, want 201", resp.StatusCode)
	}
	app, err := st.AppBySlug("my-blog")
	if err != nil {
		t.Fatal(err)
	}

	deployment, err := st.CreateDeploymentFull(app.ID, "", "git", "webhook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertGitDelivery(store.GitDelivery{
		AppID: app.ID, Provider: "github", Event: "push", Ref: "refs/heads/main",
		CommitSHA: "sha-linked", Status: "deployed", DeploymentID: &deployment.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertGitDelivery(store.GitDelivery{
		AppID: app.ID, Provider: "github", Event: "push", Ref: "refs/heads/other",
		CommitSHA: "sha-unlinked", Status: "ignored_branch",
	}); err != nil {
		t.Fatal(err)
	}

	var out []gitDeliveryResponse
	resp = doJSON(t, http.MethodGet, s.URL+"/api/v1/apps/my-blog/git/deliveries", token, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 deliveries, got %d: %+v", len(out), out)
	}

	byCommit := make(map[string]gitDeliveryResponse, len(out))
	for _, d := range out {
		byCommit[d.CommitSHA] = d
	}

	linked, ok := byCommit["sha-linked"]
	if !ok {
		t.Fatalf("missing the linked delivery in %+v", out)
	}
	if linked.DeploymentNumber == nil || *linked.DeploymentNumber != deployment.Number {
		t.Fatalf("linked delivery's DeploymentNumber = %v, want %d", linked.DeploymentNumber, deployment.Number)
	}

	unlinked, ok := byCommit["sha-unlinked"]
	if !ok {
		t.Fatalf("missing the unlinked delivery in %+v", out)
	}
	if unlinked.DeploymentNumber != nil {
		t.Fatalf("unlinked delivery's DeploymentNumber = %v, want nil", unlinked.DeploymentNumber)
	}
}
