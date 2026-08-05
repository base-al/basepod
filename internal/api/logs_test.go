package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// frame builds one podman multiplex log frame (8-byte header: stream
// type, 3 reserved bytes, big-endian payload length; then payload),
// mirroring podman.DemuxLogs's wire format. Duplicated here (rather than
// exported from package podman) since it's test-only fixture data.
func frame(streamType byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = streamType
	binary.BigEndian.PutUint32(h[4:8], uint32(len(payload)))
	return append(h, []byte(payload)...)
}

// scriptedReadCloser is a test double for the raw ReadCloser a LogSource
// returns: it serves fixed scripted bytes, then — instead of returning
// EOF — blocks (simulating a live `follow` stream still waiting on more
// container output) until Close is called, which is what a real
// follow=true HTTP response body would do once the handler stops reading
// it. This lets tests prove the SSE handler actually calls Close on
// disconnect rather than leaking the read.
type scriptedReadCloser struct {
	mu       sync.Mutex
	data     []byte
	pos      int
	closed   bool
	closedCh chan struct{}
}

func newScriptedReadCloser(data []byte) *scriptedReadCloser {
	return &scriptedReadCloser{data: data, closedCh: make(chan struct{})}
}

func (s *scriptedReadCloser) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.pos < len(s.data) {
		n := copy(p, s.data[s.pos:])
		s.pos += n
		s.mu.Unlock()
		return n, nil
	}
	closedCh := s.closedCh
	s.mu.Unlock()

	<-closedCh
	return 0, io.EOF
}

func (s *scriptedReadCloser) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.closedCh)
	}
	return nil
}

func (s *scriptedReadCloser) wasClosed() bool {
	select {
	case <-s.closedCh:
		return true
	default:
		return false
	}
}

// createTestApp creates one app ("blog") directly in the store, without
// going through the deploy engine — the logs endpoint's behavior is
// scripted entirely via LogSource, so no real container is needed.
func createTestApp(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.CreateApp("blog", "nginx:v1", 80); err != nil {
		t.Fatal(err)
	}
}

// TestHandleAppLogsEventFraming proves the SSE handler demuxes the raw
// multiplexed reader a LogSource returns into `event: log` / `data:
// {"stream":...,"line":...}` frames, one per line, and defaults/forwards
// follow and tail correctly.
func TestHandleAppLogsEventFraming(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	raw := append(frame(1, "hello\n"), frame(2, "world\n")...)
	var gotFollow bool
	var gotTail int
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		if slug != "blog" {
			t.Errorf("slug = %q, want blog", slug)
		}
		gotFollow, gotTail = follow, tail
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/logs?tail=50", nil)
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
	want := "event: log\ndata: {\"stream\":\"stdout\",\"line\":\"hello\"}\n\n" +
		"event: log\ndata: {\"stream\":\"stderr\",\"line\":\"world\"}\n\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}

	if gotFollow {
		t.Error("expected follow=false (default when ?follow= is omitted)")
	}
	if gotTail != 50 {
		t.Errorf("tail = %d, want 50 (from ?tail=50)", gotTail)
	}
}

// TestHandleAppLogsDefaultAndCappedTail proves tail defaults to 200 when
// omitted, is clamped up to 1 when present but zero or negative, and is
// capped at 5000 rather than passed through uncapped.
func TestHandleAppLogsDefaultAndCappedTail(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	_, session := loginOnly(t, st)
	token := session.Token

	cases := []struct {
		query    string
		wantTail int
	}{
		{"", 200},            // absent -> default
		{"?tail=0", 1},       // present, zero -> clamped up to 1
		{"?tail=-5", 1},      // present, negative -> clamped up to 1
		{"?tail=9999", 5000}, // present, over max -> capped at 5000
		{"?tail=9999999", 5000},
		{"?tail=10", 10},
	}
	for _, tc := range cases {
		var gotTail int
		logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
			gotTail = tail
			return io.NopCloser(strings.NewReader("")), nil
		}
		srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)

		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/logs"+tc.query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if gotTail != tc.wantTail {
			t.Errorf("query %q: tail = %d, want %d", tc.query, gotTail, tc.wantTail)
		}
	}
}

// TestHandleAppLogsInvalidTail proves a non-numeric tail is rejected with
// 400 rather than silently falling back to the default.
func TestHandleAppLogsInvalidTail(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		t.Fatal("logs func should not be called for an invalid tail")
		return nil, nil
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/logs?tail=nope", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHandleAppLogsAppNotFound proves store.ErrNotFound from the
// LogSource maps to 404 "app_not_found".
func TestHandleAppLogsAppNotFound(t *testing.T) {
	st := newTestStore(t)
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return nil, store.ErrNotFound
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/nope/logs", token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "app_not_found" {
		t.Fatalf("error code = %q, want app_not_found", errBody.Error.Code)
	}
}

// TestHandleAppLogsNotRunning proves deploy.ErrNotRunning from the
// LogSource maps to 409 "not_running".
func TestHandleAppLogsNotRunning(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return nil, deploy.ErrNotRunning
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/logs", token, nil, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "not_running" {
		t.Fatalf("error code = %q, want not_running", errBody.Error.Code)
	}
}

// TestHandleAppLogsOtherErrorIs502 proves an unexpected LogSource error
// (neither sentinel) surfaces as 502 "logs_failed" with the underlying
// message, rather than a generic 500.
func TestHandleAppLogsOtherErrorIs502(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return nil, errors.New("podman: socket gone")
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/logs", token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "logs_failed" {
		t.Fatalf("error code = %q, want logs_failed", errBody.Error.Code)
	}
	if !strings.Contains(errBody.Error.Message, "socket gone") {
		t.Fatalf("message = %q, want it to contain the underlying error", errBody.Error.Message)
	}
}

// TestHandleAppLogsUnauthorized proves the endpoint sits behind auth like
// every other /api/v1/apps/* route.
func TestHandleAppLogsUnauthorized(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/logs", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHandleAppLogsDisconnectClosesSource proves that when the client
// disconnects mid-stream (request context canceled — the SSE handler's
// analogue of a browser tab closing a follow=1 connection), the handler
// closes the raw source reader. This is the mechanism that lets the demux
// goroutine's blocked Read return and the goroutine exit, rather than
// leaking for the life of the process.
func TestHandleAppLogsDisconnectClosesSource(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	src := newScriptedReadCloser(frame(1, "hello\n"))
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return src, nil
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	// WithTimeout rather than plain WithCancel: it doubles as a safety net
	// so a bug that makes resp.Body.Read block forever fails this test
	// with a clear timeout instead of hanging the whole suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/apps/blog/logs?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read until the scripted "hello" event arrives, proving the handler
	// is up and streaming, before disconnecting.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	found := false
	var accum strings.Builder
	for time.Now().Before(deadline) && !found {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			accum.Write(buf[:n])
			if strings.Contains(accum.String(), "event: log") {
				found = true
			}
		}
		if err != nil {
			break
		}
	}
	if !found {
		t.Fatalf("did not see the scripted log event before timing out; got %q", accum.String())
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

// loginOnly opens a fresh server backed by st purely to obtain a valid
// session token (for tests that build several throwaway servers scripted
// per sub-case and don't want to repeat the login dance for each one).
func loginOnly(t *testing.T, st *store.Store) (*http.Response, loginResponse) {
	t.Helper()
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	return login(t, srv, testPassword)
}

// TestHandleAppLogsQueryTokenAuth proves the logs endpoint accepts a valid
// session token passed as ?access_token=, the fallback native EventSource
// needs since it cannot set an Authorization header.
func TestHandleAppLogsQueryTokenAuth(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	// No Authorization header at all — only the query param, exactly as a
	// browser's native EventSource would connect.
	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/logs?access_token=" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHandleAppLogsQueryTokenRejectedElsewhere proves the ?access_token=
// fallback is wired to the logs route only: a query token that would
// authenticate /logs must not authenticate any other endpoint, since
// query strings leak into access logs, browser history, and Referer
// headers far more readily than headers do.
func TestHandleAppLogsQueryTokenRejectedElsewhere(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	resp, err := http.Get(srv.URL + "/api/v1/apps?access_token=" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (query token must not authenticate non-logs routes)", resp.StatusCode)
	}
}

// TestHandleAppLogsBearerPrecedenceOverQueryToken proves requireAuthLogs's
// "header first, else query" ordering holds even when both are present: a
// valid Authorization header authenticates the request regardless of what
// (if anything) garbage sits in ?access_token=, since a real client only
// ever sends one or the other and the header — being harder to leak via
// logs/history/Referer — must not lose to a spoofed/stale query value.
func TestHandleAppLogsBearerPrecedenceOverQueryToken(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)

	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/logs?access_token=bogus-token", nil)
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
		t.Fatalf("status = %d, want 200 (valid Bearer header must take precedence over a bogus query token)", resp.StatusCode)
	}
}

// TestHandleAppLogsBadQueryToken proves an invalid/unknown ?access_token=
// value is rejected with 401 rather than silently falling through as
// unauthenticated (or, worse, authenticated).
func TestHandleAppLogsBadQueryToken(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp, err := http.Get(srv.URL + "/api/v1/apps/blog/logs?access_token=not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
