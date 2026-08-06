package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/base-al/basepod/internal/store"
)

// TestStreamLimiterEnforcesPerUserAndGlobalCaps proves streamLimiter.acquire
// respects both caps independently: a user hitting their own cap is
// refused even with global room to spare, and a different user can still
// be refused once the global cap (summed across every user) is hit —
// audit finding L6. Exercises streamLimiter directly (a fresh instance,
// not the shared defaultStreamLimiter singleton) so the caps can be sized
// small enough to test without opening dozens of connections.
func TestStreamLimiterEnforcesPerUserAndGlobalCaps(t *testing.T) {
	l := newStreamLimiter(2, 3) // per-user cap 2, global cap 3

	if !l.acquire(1) {
		t.Fatal("expected first acquire for user 1 to succeed")
	}
	if !l.acquire(1) {
		t.Fatal("expected second acquire for user 1 to succeed")
	}
	if l.acquire(1) {
		t.Fatal("expected third acquire for user 1 to fail (per-user cap of 2)")
	}

	// User 2 has per-user room (0/2), but the global cap (3 total, 2
	// already held by user 1) only has 1 slot left.
	if !l.acquire(2) {
		t.Fatal("expected first acquire for user 2 to succeed (1 global slot left)")
	}
	if l.acquire(2) {
		t.Fatal("expected second acquire for user 2 to fail (global cap of 3 hit)")
	}

	// Releasing one of user 1's slots frees room under both caps.
	l.release(1)
	if !l.acquire(1) {
		t.Fatal("expected acquire for user 1 to succeed after a release")
	}
}

// TestStreamLimiterReleaseNeverUnderflows proves release is safe to call
// without a matching acquire (as a defer'd release must be, on an early
// return before acquire ever ran) and never lets the counters go
// negative — which would otherwise silently grant extra capacity beyond
// the configured caps.
func TestStreamLimiterReleaseNeverUnderflows(t *testing.T) {
	l := newStreamLimiter(2, 2)

	// Releases with nothing acquired yet — must be no-ops, not panics or
	// negative counters.
	l.release(1)
	l.release(1)

	if !l.acquire(1) {
		t.Fatal("expected first acquire to succeed")
	}
	if !l.acquire(1) {
		t.Fatal("expected second acquire to succeed")
	}
	if l.acquire(1) {
		t.Fatal("expected third acquire to fail — a prior spurious release must not have granted extra capacity")
	}
}

// TestHandleAppLogsTooManyStreamsReturns503 proves handleAppLogs enforces
// the per-user cap (audit finding L6) end-to-end: with the authenticated
// user's slots already exhausted, a new request is refused with 503
// "too_many_streams" rather than opening yet another stream. The user's
// slots are pre-filled directly against defaultStreamLimiter (rather than
// opening maxStreamsPerUser real connections) to keep this test fast and
// free of concurrency timing, while still exercising the real
// acquireStreamSlot/handleAppLogs integration for the boundary case.
func TestHandleAppLogsTooManyStreamsReturns503(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
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
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/logs", token, nil, &errBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if errBody.Error.Code != "too_many_streams" {
		t.Fatalf("error code = %q, want too_many_streams", errBody.Error.Code)
	}
}

// TestHandleAppLogsReleasesSlotOnNormalCompletion proves the concurrent-
// stream slot handleAppLogs reserves is actually released once the
// handler returns — the property TestHandleAppLogsTooManyStreamsReturns503
// relies on being true in the first place (audit finding L6's "must
// decrement on every exit path" requirement). Reads the full response
// body (rather than just checking the status code) specifically so this
// assertion runs only after the server-side handler has actually
// returned: an SSE handler's deferred release() runs before its response
// body finishes closing, and a streaming HTTP client's Do() can return as
// soon as headers arrive, well before that — io.ReadAll blocks until the
// body is fully drained (i.e. the connection closes), which only happens
// after the handler's defers have already run.
func TestHandleAppLogsReleasesSlotOnNormalCompletion(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	user, err := st.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/logs", nil)
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

// TestHandleAppLogsFailedLookupNeverConsumesASlot proves a request that
// fails before the stream is ever established (e.g. an unknown app,
// mapped to 404 by handleAppLogs) never reserves a concurrent-stream
// slot at all — only requests that actually start streaming should count
// against the cap.
func TestHandleAppLogsFailedLookupNeverConsumesASlot(t *testing.T) {
	st := newTestStore(t)
	logsFn := func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
		return nil, store.ErrNotFound
	}
	srv := newTestServerWithLogs(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{}, logsFn)
	_, session := login(t, srv, testPassword)
	token := session.Token

	user, err := st.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/nope/logs", token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	defaultStreamLimiter.mu.Lock()
	got := defaultStreamLimiter.perUser[user.ID]
	defaultStreamLimiter.mu.Unlock()
	if got != 0 {
		t.Fatalf("defaultStreamLimiter.perUser[%d] = %d after a failed lookup, want 0 (a 404 must never consume a slot)", user.ID, got)
	}
}
