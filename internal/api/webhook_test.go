package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/store"
)

// setupWebhookTest creates a store + server (with app "my-blog" created,
// logged in, and its git source connected: url
// "https://github.com/example/repo.git", branch "main"), wired to
// fetcher. Returns the server's base URL, the connected hook_id/secret,
// the app row, and the store for direct assertions.
func setupWebhookTest(t *testing.T, fetcher GitFetcher) (srv, hookID, secret string, app *store.App, st *store.Store) {
	t.Helper()
	st = newTestStore(t)
	dep := &fakeDeployer{st: st}
	builder := build.New(nil, t.TempDir(), 2)
	s := newTestServerWithGit(t, st, dep, &fakeRoutesApplier{}, unusedLogSource, builder, fetcher)

	_, session := login(t, s, testPassword)
	token := session.Token
	resp := doJSON(t, http.MethodPost, s.URL+"/api/v1/apps", token,
		createAppRequest{Name: "My Blog", Image: "nginx:alpine", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: got status %d, want 201", resp.StatusCode)
	}

	var gs gitSourceResponse
	resp = doJSON(t, http.MethodPut, s.URL+"/api/v1/apps/my-blog/git", token,
		putGitSourceRequest{URL: "https://github.com/example/repo.git", Branch: "main"}, &gs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect git: got status %d, want 200", resp.StatusCode)
	}

	a, err := st.AppBySlug("my-blog")
	if err != nil {
		t.Fatal(err)
	}

	return s.URL, gs.HookID, gs.Secret, a, st
}

// postWebhookRaw issues a raw POST (no auth, no doJSON's automatic JSON
// content-type handling — the webhook route is unauthenticated and its
// signature headers are what this file needs full control over).
func postWebhookRaw(t *testing.T, url string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// pushPayloadJSON builds a push-event JSON body carrying ref/after plus a
// "repository.clone_url"/"ssh_url" pointing at an entirely different,
// attacker-controlled repo — every test in this file uses this shape so
// the hostile field is present throughout, not just in the one test
// dedicated to proving it's ignored (TestWebhookHostileCloneURLIgnored).
func pushPayloadJSON(t *testing.T, ref, after string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"ref":   ref,
		"after": after,
		"repository": map[string]any{
			"clone_url": "https://evil.example.com/malicious.git",
			"ssh_url":   "git@evil.example.com:malicious.git",
			"url":       "https://evil.example.com/malicious",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func githubSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func giteaSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// waitForCondition polls cond until it reports true or 3 seconds elapse —
// used to synchronize on a fake's background goroutine (DeployGitCloneAsync
// runs its fetch/onDone asynchronously, exactly like the real engine)
// without sleeping a fixed, potentially-flaky duration.
func waitForCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// --- constant-time signature verification -----------------------------

// TestVerifyWebhookSignatureConstantTimeCompare exercises
// verifyWebhookSignature directly (white-box) across every
// provider/header shape and a battery of malformed signatures — a
// truncated (wrong-length) hex signature, an invalid-hex signature, a
// signature for the wrong secret — all of which must be rejected via
// hmac.Equal/subtle.ConstantTimeCompare (never ==, which would return
// early on the first mismatched byte and leak timing information about
// how much of the guess was correct). This is the "constant-time
// comparison" property named in the v0.5 plan's Task 5 test list.
func TestVerifyWebhookSignatureConstantTimeCompare(t *testing.T) {
	secret := "test-secret-abcdef0123456789"
	body := []byte(`{"ref":"refs/heads/main","after":"deadbeef"}`)
	valid := githubSig(secret, body)

	cases := []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{"github valid signature", http.Header{"X-Hub-Signature-256": {valid}}, true},
		{"github wrong secret", http.Header{"X-Hub-Signature-256": {githubSig("wrong-secret", body)}}, false},
		{"github truncated (wrong-length) signature", http.Header{"X-Hub-Signature-256": {valid[:20]}}, false},
		{"github malformed (non-hex) signature", http.Header{"X-Hub-Signature-256": {"sha256=not-valid-hex-zz"}}, false},
		{"github missing sha256= prefix", http.Header{"X-Hub-Signature-256": {strings.TrimPrefix(valid, "sha256=")}}, false},
		{"github empty signature", http.Header{"X-Hub-Signature-256": {""}}, false},
		{"gitea valid signature (its own header)", http.Header{"X-Gitea-Signature": {strings.TrimPrefix(valid, "sha256=")}}, true},
		{"gitea also accepts github's header name", http.Header{"X-Hub-Signature-256": {valid}}, true},
		{"gitea wrong secret", http.Header{"X-Gitea-Signature": {strings.TrimPrefix(githubSig("wrong-secret", body), "sha256=")}}, false},
		{"gitlab valid token", http.Header{"X-Gitlab-Token": {secret}}, true},
		{"gitlab wrong token", http.Header{"X-Gitlab-Token": {"not-the-secret"}}, false},
		{"gitlab empty token", http.Header{"X-Gitlab-Token": {""}}, false},
		{"no recognized header at all", http.Header{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := verifyWebhookSignature(c.headers, body, secret)
			if got != c.want {
				t.Errorf("verifyWebhookSignature() = %v, want %v", got, c.want)
			}
		})
	}
}

// --- provider signature verification end-to-end ------------------------

func TestWebhookValidGitHubPushDeploys(t *testing.T) {
	fetcher := &fakeGitFetcher{tarBody: validTarballBody(t), headSHA: "resolved-sha-1"}
	srv, hookID, secret, app, st := setupWebhookTest(t, fetcher)

	body := pushPayloadJSON(t, "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, body)}

	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}

	waitForCondition(t, func() bool { return fetcher.callCount() == 1 })
	call := fetcher.lastCall()
	if call.url != "https://github.com/example/repo.git" || call.branch != "main" {
		t.Fatalf("Fetch called with %+v, want the stored config's url/branch", call)
	}

	waitForCondition(t, func() bool {
		list, _ := st.ListGitDeliveries(app.ID, 10)
		return len(list) == 1 && list[0].Status == "deployed"
	})
}

func TestWebhookValidGiteaPushDeploys(t *testing.T) {
	fetcher := &fakeGitFetcher{tarBody: validTarballBody(t), headSHA: "resolved-sha-2"}
	srv, hookID, secret, _, _ := setupWebhookTest(t, fetcher)

	body := pushPayloadJSON(t, "refs/heads/main", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	headers := map[string]string{"X-Gitea-Event": "push", "X-Gitea-Signature": giteaSig(secret, body)}

	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	waitForCondition(t, func() bool { return fetcher.callCount() == 1 })
}

func TestWebhookValidGitLabPushDeploys(t *testing.T) {
	fetcher := &fakeGitFetcher{tarBody: validTarballBody(t), headSHA: "resolved-sha-3"}
	srv, hookID, secret, _, _ := setupWebhookTest(t, fetcher)

	body := pushPayloadJSON(t, "refs/heads/main", "cccccccccccccccccccccccccccccccccccccccc")
	headers := map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": secret}

	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	waitForCondition(t, func() bool { return fetcher.callCount() == 1 })
}

// TestWebhookForgedSignatureRejected proves a forged/mismatched signature
// on a KNOWN hook is rejected 401 "unauthorized", recorded as delivery
// status "invalid_signature", and — critically — never triggers a clone.
func TestWebhookForgedSignatureRejected(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, _, app, st := setupWebhookTest(t, fetcher)

	body := pushPayloadJSON(t, "refs/heads/main", "dddddddddddddddddddddddddddddddddddddddd")
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64)}

	var errBody errorResponse
	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	decodeInto(t, resp, &errBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", resp.StatusCode)
	}
	if errBody.Error.Code != "unauthorized" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	if fetcher.callCount() != 0 {
		t.Fatal("a forged signature must never trigger a clone")
	}

	list, err := st.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "invalid_signature" {
		t.Fatalf("unexpected deliveries: %+v", list)
	}
}

// TestWebhookUnknownHookIndistinguishableFromBadSignature is the "no such
// hook vs bad signature are indistinguishable" property named in the
// v0.5 plan's Task 5 test list: an unknown hook_id and a known hook's bad
// signature must produce the exact same response (status + body), so a
// caller probing hook_ids can never learn whether one is real just from
// the response shape.
func TestWebhookUnknownHookIndistinguishableFromBadSignature(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, _, _, _ := setupWebhookTest(t, fetcher)

	body := pushPayloadJSON(t, "refs/heads/main", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64)}

	respKnownBadSig := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	bodyKnownBadSig, err := io.ReadAll(respKnownBadSig.Body)
	if err != nil {
		t.Fatal(err)
	}

	// A hook_id that was never issued at all — same shape, same length
	// class as a real one, just never stored.
	respUnknown := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+strings.Repeat("f", len(hookID)), headers, body)
	bodyUnknown, err := io.ReadAll(respUnknown.Body)
	if err != nil {
		t.Fatal(err)
	}

	if respKnownBadSig.StatusCode != http.StatusUnauthorized {
		t.Fatalf("known hook + bad signature: got status %d, want 401", respKnownBadSig.StatusCode)
	}
	if respKnownBadSig.StatusCode != respUnknown.StatusCode {
		t.Fatalf("status differs: known-hook-bad-signature=%d unknown-hook=%d, want identical",
			respKnownBadSig.StatusCode, respUnknown.StatusCode)
	}
	if string(bodyKnownBadSig) != string(bodyUnknown) {
		t.Fatalf("response bodies differ:\n known-hook-bad-signature=%s\n unknown-hook=%s\nwant identical (no oracle for hook_id validity)",
			bodyKnownBadSig, bodyUnknown)
	}
	if ct1, ct2 := respKnownBadSig.Header.Get("Content-Type"), respUnknown.Header.Get("Content-Type"); ct1 != ct2 {
		t.Fatalf("Content-Type differs: known=%q unknown=%q", ct1, ct2)
	}
}

// --- body size cap -------------------------------------------------------

// TestWebhookOversizedBodyRejected proves a payload over webhookMaxBody is
// rejected 413 before any signature verification or clone is attempted.
func TestWebhookOversizedBodyRejected(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, secret, app, st := setupWebhookTest(t, fetcher)

	big := bytes.Repeat([]byte("a"), webhookMaxBody+1024)
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, big)}

	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want 413", resp.StatusCode)
	}
	if fetcher.callCount() != 0 {
		t.Fatal("an oversized body must never trigger a clone")
	}
	list, err := st.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "error" {
		t.Fatalf("unexpected deliveries: %+v", list)
	}
}

// --- event/branch filter --------------------------------------------------

// TestWebhookBranchMismatchIgnoredNoDeploy is the "branch filter" property
// named in the v0.5 plan's Task 5 test list: a push to a branch other
// than the one configured is accepted and recorded, but never deploys.
func TestWebhookBranchMismatchIgnoredNoDeploy(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, secret, app, st := setupWebhookTest(t, fetcher) // configured branch: main

	body := pushPayloadJSON(t, "refs/heads/feature-x", "1111111111111111111111111111111111111111")
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, body)}

	var out map[string]string
	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	decodeInto(t, resp, &out)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	if out["status"] != "ignored" || out["reason"] != "branch_mismatch" {
		t.Fatalf("unexpected response: %+v", out)
	}

	time.Sleep(100 * time.Millisecond)
	if fetcher.callCount() != 0 {
		t.Fatal("a push to a non-configured branch must never trigger a clone")
	}

	list, err := st.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "ignored_branch" {
		t.Fatalf("unexpected deliveries: %+v", list)
	}
}

// TestWebhookPingEventIgnored proves a ping event (sent when a webhook is
// first configured on a forge) is accepted 200 without attempting a
// deploy.
func TestWebhookPingEventIgnored(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, secret, app, st := setupWebhookTest(t, fetcher)

	body := []byte(`{"zen":"design for failure"}`)
	headers := map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": githubSig(secret, body)}

	var out map[string]string
	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	decodeInto(t, resp, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if out["reason"] != "ping" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if fetcher.callCount() != 0 {
		t.Fatal("a ping event must never trigger a clone")
	}

	list, err := st.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "ignored_event" {
		t.Fatalf("unexpected deliveries: %+v", list)
	}
}

// TestWebhookBranchDeletionIgnored proves a push with an all-zeros "after"
// (a branch deletion) is accepted and recorded but never deploys — there
// is no new HEAD to build.
func TestWebhookBranchDeletionIgnored(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, secret, app, st := setupWebhookTest(t, fetcher)

	body := pushPayloadJSON(t, "refs/heads/main", "0000000000000000000000000000000000000000")
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, body)}

	var out map[string]string
	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	decodeInto(t, resp, &out)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	if out["reason"] != "branch_deleted" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if fetcher.callCount() != 0 {
		t.Fatal("a branch-deletion push must never trigger a clone")
	}

	list, err := st.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "ignored_event" {
		t.Fatalf("unexpected deliveries: %+v", list)
	}
}

// TestWebhookUnsupportedEventIgnored proves a non-push, non-ping event
// (e.g. "pull_request") is accepted 202 without attempting a deploy.
func TestWebhookUnsupportedEventIgnored(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, secret, _, _ := setupWebhookTest(t, fetcher)

	body := []byte(`{"action":"opened"}`)
	headers := map[string]string{"X-GitHub-Event": "pull_request", "X-Hub-Signature-256": githubSig(secret, body)}

	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	if fetcher.callCount() != 0 {
		t.Fatal("a non-push event must never trigger a clone")
	}
}

// --- rate limiting ---------------------------------------------------------

// TestWebhookRateLimitTrips proves the per-hook_id rate limit
// (webhookRateLimit per webhookRateWindow) trips after that many requests,
// returning 429 — the "rate limit trips" property named in the v0.5
// plan's Task 5 test list.
func TestWebhookRateLimitTrips(t *testing.T) {
	fetcher := &fakeGitFetcher{}
	srv, hookID, secret, _, _ := setupWebhookTest(t, fetcher)

	// A branch mismatch — accepted and recorded, but never deploys — so
	// this loop never touches the coalescer/clone path at all; it's
	// exercising the rate limiter in isolation.
	body := pushPayloadJSON(t, "refs/heads/other", "2222222222222222222222222222222222222222")
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, body)}

	for i := 0; i < webhookRateLimit; i++ {
		resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("tripped the rate limit too early, at request %d of %d", i+1, webhookRateLimit)
		}
	}

	tripped := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	if tripped.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request %d: got status %d, want 429", webhookRateLimit+1, tripped.StatusCode)
	}
	var errBody errorResponse
	decodeInto(t, tripped, &errBody)
	if errBody.Error.Code != "rate_limited" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}
}

// --- flood coalescing -------------------------------------------------------

// TestGitCoalescerNewestPushWins is a focused, deterministic (no HTTP, no
// goroutines) unit test of gitCoalescer's core contract: at most one
// running slot per app, at most one queued follow-up, and repeatedly
// overwriting that follow-up always keeps only the newest — "the newest
// SHA wins", per the v0.5 plan's Task 5.
func TestGitCoalescerNewestPushWins(t *testing.T) {
	c := newGitCoalescer()

	if !c.tryStart(1) {
		t.Fatal("expected the first tryStart to succeed")
	}
	if c.tryStart(1) {
		t.Fatal("expected a second tryStart while running to fail")
	}

	c.setPending(1, "sha-a", "refs/heads/main")
	c.setPending(1, "sha-b", "refs/heads/main")
	c.setPending(1, "sha-c", "refs/heads/main") // overwrites — newest wins

	next, ok := c.finish(1)
	if !ok {
		t.Fatal("expected finish to report a queued follow-up")
	}
	if next.sha != "sha-c" {
		t.Fatalf("next.sha = %q, want sha-c (the newest queued push)", next.sha)
	}

	// The slot was atomically reclaimed for that follow-up — a second
	// finish with nothing further queued releases it for good.
	if _, ok := c.finish(1); ok {
		t.Fatal("expected no further queued push")
	}
	if !c.tryStart(1) {
		t.Fatal("expected the slot to be free once nothing is queued")
	}

	// Different apps never share a slot.
	c.tryStart(1)
	if !c.tryStart(2) {
		t.Fatal("expected app 2's slot to be independent of app 1's")
	}
}

// TestWebhookFloodCoalescesToAtMostTwoDeploys is the end-to-end "coalescing
// under 5 rapid pushes -> <=2 deploys" property named in the v0.5 plan's
// Task 5 test list: the first push claims the running slot and is held
// there (via fakeGitFetcher's block channel) while 4 more rapid pushes
// arrive — each must coalesce rather than starting its own clone. Once
// the first clone is released, exactly one chained follow-up runs for
// whatever was queued, and no more.
func TestWebhookFloodCoalescesToAtMostTwoDeploys(t *testing.T) {
	entered := make(chan struct{}, 10)
	block := make(chan struct{})
	fetcher := &fakeGitFetcher{tarBody: validTarballBody(t), headSHA: "resolved-flood-sha", entered: entered, block: block}
	srv, hookID, secret, app, st := setupWebhookTest(t, fetcher)

	push := func(commit string) *http.Response {
		body := pushPayloadJSON(t, "refs/heads/main", commit)
		headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, body)}
		return postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	}

	// Push #1 claims the running slot and blocks inside Fetch.
	if resp := push("1111111111111111111111111111111111111111"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push 1: got status %d, want 202", resp.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("push 1's clone never started")
	}

	// 4 more rapid pushes (#2-#5) while #1 is still running: each must
	// coalesce — recorded as its own "coalesced" delivery even though
	// #2-#4 are immediately superseded by the next one, leaving only #5 as
	// the actual queued follow-up (see TestGitCoalescerNewestPushWins for
	// that "newest wins" property in isolation).
	for i := 2; i <= 5; i++ {
		commit := strconv.Itoa(i) + strings.Repeat("2", 39)
		if resp := push(commit); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("push %d: got status %d, want 202", i, resp.StatusCode)
		}
	}

	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 clone in flight while push 1 is blocked, got %d", got)
	}

	// Release push #1's clone — the coalesced follow-up (for the newest
	// pending push, #5) should now run, and only that one.
	close(block)

	waitForCondition(t, func() bool { return fetcher.callCount() == 2 })
	// Give any (incorrect) extra chained deploy a moment to show up.
	time.Sleep(200 * time.Millisecond)
	if got := fetcher.callCount(); got != 2 {
		t.Fatalf("expected at most 2 total clones for a flood of 5 rapid pushes, got %d", got)
	}

	var list []store.GitDelivery
	waitForCondition(t, func() bool {
		var err error
		list, err = st.ListGitDeliveries(app.ID, 20)
		if err != nil {
			t.Fatal(err)
		}
		deployed := 0
		for _, d := range list {
			if d.Status == "deployed" {
				deployed++
			}
		}
		return deployed == 2
	})

	var deployed, coalesced int
	for _, d := range list {
		switch d.Status {
		case "deployed":
			deployed++
		case "coalesced":
			coalesced++
		}
	}
	if deployed != 2 {
		t.Fatalf("expected 2 'deployed' deliveries (push 1 + the one chained follow-up), got %d: %+v", deployed, list)
	}
	if coalesced != 4 {
		t.Fatalf("expected 4 'coalesced' deliveries (pushes 2-5, each recorded even though 2-4 were overwritten before push 5 became the pending one), got %d: %+v", coalesced, list)
	}
}

// --- payload cannot steer the clone -----------------------------------

// TestWebhookHostileCloneURLIgnored is the "payload with a different repo
// URL still clones the stored URL" property named in the v0.5 plan's
// Task 5 test list: the push payload's repository.clone_url/ssh_url point
// at an entirely different, attacker-controlled repository, but the
// fetcher is asserted to have been called with ONLY the stored
// git_sources config's URL/branch — the payload can trigger a deploy and
// filter which branch triggers one, but can never steer what gets cloned.
func TestWebhookHostileCloneURLIgnored(t *testing.T) {
	fetcher := &fakeGitFetcher{tarBody: validTarballBody(t), headSHA: "sha-legit"}
	srv, hookID, secret, _, _ := setupWebhookTest(t, fetcher) // stored: https://github.com/example/repo.git, branch main

	body := pushPayloadJSON(t, "refs/heads/main", "3333333333333333333333333333333333333333")
	if !strings.Contains(string(body), "evil.example.com") {
		t.Fatal("test bug: payload does not actually carry a hostile clone_url")
	}
	headers := map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(secret, body)}

	resp := postWebhookRaw(t, srv+"/api/v1/webhooks/git/"+hookID, headers, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}

	waitForCondition(t, func() bool { return fetcher.callCount() == 1 })
	call := fetcher.lastCall()
	if call.url != "https://github.com/example/repo.git" {
		t.Fatalf("Fetch called with url %q — the payload's hostile clone_url must NEVER be used, only the stored config's", call.url)
	}
	if call.branch != "main" {
		t.Fatalf("Fetch called with branch %q, want main (from stored config)", call.branch)
	}
	if strings.Contains(call.url, "evil.example.com") {
		t.Fatal("the hostile URL leaked into the actual clone call")
	}
}
