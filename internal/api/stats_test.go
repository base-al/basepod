package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// statsFrame builds one raw JSON stats object matching podman's wire shape
// (see podman.ContainerStats' doc comment) with just enough fields set for
// podman.StreamStats to decode a nonzero, checkable sample: memory usage
// and limit, PID count, and a network entry. CPU is left at its
// degenerate first-sample shape (cpu_stats == precpu_stats) since these
// tests only assert on the fields the SSE payload carries end to end, not
// on the CPU delta math already covered by internal/podman's own tests.
func statsFrame(memUsed, memLimit, pids uint64) string {
	fu := strconv.FormatUint
	return `{"cpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000,"online_cpus":1},` +
		`"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000},` +
		`"memory_stats":{"usage":` + fu(memUsed, 10) + `,"limit":` + fu(memLimit, 10) + `},` +
		`"pids_stats":{"current":` + fu(pids, 10) + `},` +
		`"blkio_stats":{"io_service_bytes_recursive":[]},` +
		`"networks":{}}`
}

// TestHandleAppStatsEventFraming proves the SSE handler decodes the raw
// stats stream a StatsSource returns into `event: stats` / `data:
// {...}` frames, one per sample — mirrors
// TestHandleAppLogsEventFraming.
func TestHandleAppStatsEventFraming(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	raw := statsFrame(1000, 2000, 5)
	var gotSlug string
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		gotSlug = slug
		return io.NopCloser(strings.NewReader(raw)), nil
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", nil)
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
	if gotSlug != "blog" {
		t.Errorf("slug = %q, want blog", gotSlug)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "event: stats\ndata: {\"cpu_percent\":0,\"mem_used_bytes\":1000,\"mem_limit_bytes\":2000,\"pids\":5,\"net_rx_bytes\":0,\"net_tx_bytes\":0,\"block_read_bytes\":0,\"block_write_bytes\":0}\n\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// TestHandleAppStatsAppNotFound proves store.ErrNotFound from the
// StatsSource maps to 404 "app_not_found" — mirrors
// TestHandleAppLogsAppNotFound.
func TestHandleAppStatsAppNotFound(t *testing.T) {
	st := newTestStore(t)
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return nil, store.ErrNotFound
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/nope/stats", token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "app_not_found" {
		t.Fatalf("error code = %q, want app_not_found", errBody.Error.Code)
	}
}

// TestHandleAppStatsNotRunning proves deploy.ErrNotRunning from the
// StatsSource maps to 409 "not_running" — mirrors
// TestHandleAppLogsNotRunning.
func TestHandleAppStatsNotRunning(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return nil, deploy.ErrNotRunning
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", token, nil, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "not_running" {
		t.Fatalf("error code = %q, want not_running", errBody.Error.Code)
	}
}

// TestHandleAppStatsOtherErrorIs502 proves an unexpected StatsSource error
// (neither sentinel) surfaces as 502 "stats_failed" with the underlying
// message — mirrors TestHandleAppLogsOtherErrorIs502.
func TestHandleAppStatsOtherErrorIs502(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return nil, errors.New("podman: socket gone")
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "stats_failed" {
		t.Fatalf("error code = %q, want stats_failed", errBody.Error.Code)
	}
	if !strings.Contains(errBody.Error.Message, "socket gone") {
		t.Fatalf("message = %q, want it to contain the underlying error", errBody.Error.Message)
	}
}

// TestHandleAppStatsUnauthorized proves the endpoint sits behind auth like
// every other /api/v1/apps/* route — mirrors TestHandleAppLogsUnauthorized.
func TestHandleAppStatsUnauthorized(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHandleAppStatsDisconnectClosesSource proves that when the client
// disconnects mid-stream, the handler closes the raw source reader — the
// mechanism that lets the decode goroutine's blocked Read return and the
// goroutine exit rather than leaking for the life of the process. Mirrors
// TestHandleAppLogsDisconnectClosesSource exactly, substituting a stats
// frame for a log frame as the scripted bytes.
func TestHandleAppStatsDisconnectClosesSource(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	src := newScriptedReadCloser([]byte(statsFrame(1000, 2000, 5)))
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return src, nil
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	// WithTimeout rather than plain WithCancel: doubles as a safety net so
	// a bug that makes resp.Body.Read block forever fails this test with a
	// clear timeout instead of hanging the whole suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", nil)
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

// TestHandleAppStatsQueryTokenAuth proves the stats endpoint accepts a
// valid stream token (minted via POST /stream-token with scope "stats"
// for this exact slug) passed as ?access_token=, mirroring
// TestHandleAppLogsQueryTokenAuth.
func TestHandleAppStatsQueryTokenAuth(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)

	streamTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeStats, Slug: "blog"})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/stats?access_token=" + streamTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHandleAppStatsSessionTokenInQueryRejected proves a session token
// placed in ?access_token= is rejected on the stats route too, mirroring
// TestHandleAppLogsSessionTokenInQueryRejected.
func TestHandleAppStatsSessionTokenInQueryRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/stats?access_token=" + session.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a session token must never authenticate ?access_token=)", resp.StatusCode)
	}
}

// TestHandleAppStatsAppLogsScopedTokenRejected proves a stream token
// minted with scope "app_logs" cannot open the stats route, even for the
// same app and user — each SSE route's stream token is its own separate
// credential. This is the extension of the existing app_logs/build_log
// cross-scope tests (TestHandleAppLogsBuildLogScopedTokenRejected) to the
// new "stats" scope, both directions.
func TestHandleAppStatsAppLogsScopedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	logsTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeAppLogs, Slug: "blog"})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/stats?access_token=" + logsTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (an app_logs-scoped token must not open the stats route)", resp.StatusCode)
	}
}

// TestHandleAppLogsStatsScopedTokenRejected proves the reverse: a stream
// token minted with scope "stats" cannot open the container-log route.
func TestHandleAppLogsStatsScopedTokenRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	statsTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeStats, Slug: "blog"})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/logs?access_token=" + statsTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a stats-scoped token must not open the container-log route)", resp.StatusCode)
	}
}

// TestHandleAppStatsBuildLogScopedTokenRejected proves a "build_log"
// scoped token cannot open the stats route either.
func TestHandleAppStatsBuildLogScopedTokenRejected(t *testing.T) {
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

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/stats?access_token=" + buildTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a build_log-scoped token must not open the stats route)", resp.StatusCode)
	}
}

// TestHandleAppStatsStreamTokenWrongSlugRejected proves a stats-scoped
// stream token minted for one app cannot open a different app's stats
// stream, mirroring TestHandleAppLogsStreamTokenWrongSlugRejected.
func TestHandleAppStatsStreamTokenWrongSlugRejected(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st) // "blog"
	if _, err := st.CreateApp("other", "nginx:v1", 80); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	streamTok := mintStreamToken(t, srv, session.Token, streamTokenRequest{Scope: streamScopeStats, Slug: "other"})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/stats?access_token=" + streamTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a stream token minted for a different app must not work here)", resp.StatusCode)
	}
}

// TestHandleAppStatsTooManyStreamsReturns503 proves handleAppStats
// enforces the same shared per-user stream cap (defaultStreamLimiter,
// audit finding L6) handleAppLogs does — a stats stream is a stream —
// mirrors TestHandleAppLogsTooManyStreamsReturns503.
func TestHandleAppStatsTooManyStreamsReturns503(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
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
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", token, nil, &errBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if errBody.Error.Code != "too_many_streams" {
		t.Fatalf("error code = %q, want too_many_streams", errBody.Error.Code)
	}
}

// TestHandleAppStatsReleasesSlotOnNormalCompletion proves the concurrent-
// stream slot handleAppStats reserves is released once the handler
// returns — mirrors TestHandleAppLogsReleasesSlotOnNormalCompletion (see
// its doc comment for why the assertion reads the full response body
// before checking the counter).
func TestHandleAppStatsReleasesSlotOnNormalCompletion(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	statsFn := func(ctx context.Context, slug string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	srv := newTestServerWithStats(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, unusedLogSource, nil, nil, statsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	user, err := st.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/stats", nil)
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

// TestCreateStreamTokenStatsScopeRejectsDeploymentNumber proves
// POST /stream-token rejects a deployment_number alongside scope "stats",
// mirroring the existing "app_logs" coherence check.
func TestCreateStreamTokenStatsScopeRejectsDeploymentNumber(t *testing.T) {
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

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/stream-token", session.Token,
		streamTokenRequest{Scope: streamScopeStats, Slug: "blog", DeploymentNumber: &dep.Number}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}
