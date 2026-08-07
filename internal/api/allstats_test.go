package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAllStatsProvider is a test double for AllStatsProvider: AllStats/
// HostCPUs/RunningAppContainers are each independently scriptable
// (reader-or-error / value-or-error), mirroring how stats_test.go scripts
// StatsSource via closures — this needs a struct instead since
// AllStatsProvider is an interface with three methods, not a single func
// type.
type fakeAllStatsProvider struct {
	mu sync.Mutex

	statsReader io.ReadCloser
	statsErr    error

	hostCPUs    int
	hostCPUsErr error

	// attribution/attributionErr is what RunningAppContainers returns by
	// default. attributionCalls counts every call, letting a test assert
	// it was refreshed once per tick (see handleAllStats' doc comment).
	attribution      map[string]string
	attributionErr   error
	attributionCalls int
}

func (f *fakeAllStatsProvider) AllStats(ctx context.Context) (io.ReadCloser, error) {
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return f.statsReader, nil
}

func (f *fakeAllStatsProvider) HostCPUs(ctx context.Context) (int, error) {
	if f.hostCPUsErr != nil {
		return 0, f.hostCPUsErr
	}
	if f.hostCPUs == 0 {
		return 1, nil
	}
	return f.hostCPUs, nil
}

func (f *fakeAllStatsProvider) RunningAppContainers(ctx context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attributionCalls++
	if f.attributionErr != nil {
		return nil, f.attributionErr
	}
	return f.attribution, nil
}

// bulkStatsTickTwoContainers is a two-container bulk-stats tick in
// podman's real wire shape (see podman.BulkContainerStats' doc comment) —
// "c1" is attributed to an app by the tests below, "c2" deliberately
// isn't, exercising the "silently skip an unattributed container" path.
const bulkStatsTickTwoContainers = `{"Error":null,"Stats":[` +
	`{"ContainerID":"c1","Name":"bp-blog-1","CPU":10,"MemUsage":1000,"MemLimit":2000,"PIDs":5,"Network":{},"BlockInput":0,"BlockOutput":0},` +
	`{"ContainerID":"c2","Name":"bp-other-1","CPU":99,"MemUsage":1,"MemLimit":1,"PIDs":1,"Network":{},"BlockInput":0,"BlockOutput":0}` +
	`]}`

// TestHandleAllStatsEventFraming proves handleAllStats decodes a bulk
// stats tick into one `event: stats` frame per ATTRIBUTED container (see
// AllStatsProvider's RunningAppContainers doc comment): "c1" (mapped to
// "blog") gets an event carrying its slug and a CPU% already normalized
// by the scripted onlineCPUs (10 * 2 = 20); "c2" (not present in the
// attribution map — e.g. a foreign container, or an app that isn't
// running) is silently skipped, not emitted with a guessed slug.
func TestHandleAllStatsEventFraming(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st) // "blog"

	allStats := &fakeAllStatsProvider{
		statsReader: io.NopCloser(strings.NewReader(bulkStatsTickTwoContainers)),
		hostCPUs:    2,
		attribution: map[string]string{"c1": "blog"},
	}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)
	token := session.Token

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "event: stats\ndata: {\"slug\":\"blog\",\"cpu_percent\":20,\"mem_used_bytes\":1000,\"mem_limit_bytes\":2000,\"pids\":5,\"net_rx_bytes\":0,\"net_tx_bytes\":0,\"block_read_bytes\":0,\"block_write_bytes\":0}\n\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// TestHandleAllStatsHostCPUsErrorIs502 proves a failing HostCPUs call
// surfaces as 502 "stats_failed" before the bulk stream is ever opened.
func TestHandleAllStatsHostCPUsErrorIs502(t *testing.T) {
	st := newTestStore(t)
	allStats := &fakeAllStatsProvider{hostCPUsErr: errors.New("host cpus boom")}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/stats", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "stats_failed" {
		t.Fatalf("error code = %q, want stats_failed", errBody.Error.Code)
	}
}

// TestHandleAllStatsOpenErrorIs502 proves a failing AllStats (bulk-stream
// open) call surfaces as 502 "stats_failed".
func TestHandleAllStatsOpenErrorIs502(t *testing.T) {
	st := newTestStore(t)
	allStats := &fakeAllStatsProvider{hostCPUs: 1, statsErr: errors.New("bulk open boom")}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/stats", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "stats_failed" {
		t.Fatalf("error code = %q, want stats_failed", errBody.Error.Code)
	}
}

// TestHandleAllStatsUnauthorized proves the batch route sits behind auth
// like every other /api/v1 route.
func TestHandleAllStatsUnauthorized(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/stats", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHandleAllStatsDisconnectClosesSource proves that when the client
// disconnects mid-stream, the handler closes the raw bulk-stats source
// reader — the mechanism that lets the decode goroutine's blocked Read
// return and the goroutine exit rather than leaking for the life of the
// process. Mirrors TestHandleAppStatsDisconnectClosesSource exactly,
// substituting a bulk-stats tick for a single-app stats frame.
func TestHandleAllStatsDisconnectClosesSource(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	src := newScriptedReadCloser([]byte(bulkStatsTickTwoContainers))
	allStats := &fakeAllStatsProvider{
		statsReader: src,
		hostCPUs:    1,
		attribution: map[string]string{"c1": "blog"},
	}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)
	token := session.Token

	// WithTimeout rather than plain WithCancel: doubles as a safety net so
	// a bug that makes resp.Body.Read block forever fails this test with a
	// clear timeout instead of hanging the whole suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read until the scripted stats event arrives, proving the handler is
	// up and streaming, before disconnecting.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	found := false
	var accum strings.Builder
	for time.Now().Before(deadline) && !found {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			accum.Write(buf[:n])
			if strings.Contains(accum.String(), "event: stats") {
				found = true
			}
		}
		if err != nil {
			break
		}
	}
	if !found {
		t.Fatalf("did not see the scripted stats event before timing out; got %q", accum.String())
	}

	cancel() // simulate client disconnect

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if src.wasClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected src.Close() to be called after the client disconnected")
}

// TestHandleAllStatsQueryTokenAuth proves the batch route accepts a valid
// stream token (minted via POST /stream-token with scope "all_stats" and
// slug "") passed as ?access_token=, mirroring
// TestHandleAppStatsQueryTokenAuth.
func TestHandleAllStatsQueryTokenAuth(t *testing.T) {
	st := newTestStore(t)
	allStats := &fakeAllStatsProvider{statsReader: io.NopCloser(strings.NewReader("")), hostCPUs: 1}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)

	streamTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeAllStats, Slug: ""})

	resp, err := http.Get(srv.URL + "/api/v1/stats?access_token=" + streamTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHandleAllStatsSessionTokenInQueryRejected proves a session token
// placed in ?access_token= is rejected on the batch route too, mirroring
// TestHandleAppStatsSessionTokenInQueryRejected.
func TestHandleAllStatsSessionTokenInQueryRejected(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	resp, err := http.Get(srv.URL + "/api/v1/stats?access_token=" + session.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a session token must never authenticate ?access_token=)", resp.StatusCode)
	}
}

// TestHandleAllStatsAppLogsScopedTokenRejected proves a stream token
// minted with scope "app_logs" cannot open the batch-stats route — each
// SSE route's stream token is its own separate credential (extending the
// existing app_logs/build_log/stats cross-scope pattern to the new
// "all_stats" scope).
func TestHandleAllStatsAppLogsScopedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	logsTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeAppLogs, Slug: "blog"})

	resp, err := http.Get(srv.URL + "/api/v1/stats?access_token=" + logsTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (an app_logs-scoped token must not open the batch-stats route)", resp.StatusCode)
	}
}

// TestHandleAllStatsBuildLogScopedTokenRejected proves a "build_log"
// scoped token cannot open the batch-stats route either.
func TestHandleAllStatsBuildLogScopedTokenRejected(t *testing.T) {
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

	buildTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeBuildLog, Slug: "blog", DeploymentNumber: &dep.Number})

	resp, err := http.Get(srv.URL + "/api/v1/stats?access_token=" + buildTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a build_log-scoped token must not open the batch-stats route)", resp.StatusCode)
	}
}

// TestHandleAllStatsAppStatsScopedTokenRejected proves a stream token
// minted with the PER-APP "stats" scope cannot open the batch route —
// the two "stats" scopes are deliberately separate credentials with very
// different blast radii (one app vs every app) despite the similar name.
func TestHandleAllStatsAppStatsScopedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	statsTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeStats, Slug: "blog"})

	resp, err := http.Get(srv.URL + "/api/v1/stats?access_token=" + statsTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a per-app stats-scoped token must not open the batch-stats route)", resp.StatusCode)
	}
}

// TestHandleAppStatsAllStatsScopedTokenRejected proves the reverse: a
// stream token minted with scope "all_stats" cannot open the per-app
// stats route.
func TestHandleAppStatsAllStatsScopedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	allStatsTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeAllStats, Slug: ""})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/stats?access_token=" + allStatsTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (an all_stats-scoped token must not open the per-app stats route)", resp.StatusCode)
	}
}

// TestHandleAppLogsAllStatsScopedTokenRejected proves an "all_stats"
// scoped token cannot open the container-log route either.
func TestHandleAppLogsAllStatsScopedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	allStatsTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeAllStats, Slug: ""})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/logs?access_token=" + allStatsTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (an all_stats-scoped token must not open the container-log route)", resp.StatusCode)
	}
}

// TestHandleAllStatsTooManyStreamsReturns503 proves handleAllStats
// enforces the same shared per-user stream cap (defaultStreamLimiter,
// audit finding L6) every other SSE handler does.
func TestHandleAllStatsTooManyStreamsReturns503(t *testing.T) {
	st := newTestStore(t)
	allStats := &fakeAllStatsProvider{statsReader: io.NopCloser(strings.NewReader("")), hostCPUs: 1}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)
	token := session.Token

	user, err := st.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxStreamsPerUser; i++ {
		if !defaultStreamLimiter.acquire(user.ID) {
			t.Fatalf("failed to pre-fill slot %d/%d for user %d", i+1, maxStreamsPerUser, user.ID)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < maxStreamsPerUser; i++ {
			defaultStreamLimiter.release(user.ID)
		}
	})

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/stats", token, nil, &errBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if errBody.Error.Code != "too_many_streams" {
		t.Fatalf("error code = %q, want too_many_streams", errBody.Error.Code)
	}
}

// TestHandleAllStatsReleasesSlotOnNormalCompletion proves the concurrent-
// stream slot handleAllStats reserves is released once the handler
// returns — mirrors TestHandleAppStatsReleasesSlotOnNormalCompletion.
func TestHandleAllStatsReleasesSlotOnNormalCompletion(t *testing.T) {
	st := newTestStore(t)
	allStats := &fakeAllStatsProvider{statsReader: io.NopCloser(strings.NewReader("")), hostCPUs: 1}
	srv := newTestServerWithAllStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, allStats)
	_, session := login(t, srv, testPassword)
	token := session.Token

	user, err := st.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	defaultStreamLimiter.mu.Lock()
	got := defaultStreamLimiter.perUser[user.ID]
	defaultStreamLimiter.mu.Unlock()
	if got != 0 {
		t.Fatalf("defaultStreamLimiter.perUser[%d] = %d after the request completed, want 0 (slot must be released)", user.ID, got)
	}
}

// TestCreateStreamTokenAllStatsScopeRejectsSlug proves POST /stream-token
// rejects a non-empty slug alongside scope "all_stats" — it names no
// single app, so a slug here would be misleading about what the minted
// token actually grants.
func TestCreateStreamTokenAllStatsScopeRejectsSlug(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeAllStats, Slug: "blog"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// TestCreateStreamTokenAllStatsScopeRejectsDeploymentNumber proves
// POST /stream-token rejects a deployment_number alongside scope
// "all_stats", mirroring the existing "stats"/"app_logs" coherence
// checks.
func TestCreateStreamTokenAllStatsScopeRejectsDeploymentNumber(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	n := 1
	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeAllStats, Slug: "", DeploymentNumber: &n}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// TestCreateStreamTokenAllStatsScopeSucceeds proves the coherent case (no
// slug, no deployment_number) mints a token successfully.
func TestCreateStreamTokenAllStatsScopeSucceeds(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var out streamTokenResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeAllStats, Slug: ""}, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.Token == "" {
		t.Fatal("expected a non-empty token")
	}
}
