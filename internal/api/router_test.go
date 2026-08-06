package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHandleLoginBodyTooLarge proves a JSON body over maxJSONBody trips
// bodyLimit's http.MaxBytesReader and is reported as 413
// "request_too_large" rather than the generic 400 "invalid_request" a
// merely-malformed body gets.
func TestHandleLoginBodyTooLarge(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	big := strings.Repeat("a", maxJSONBody+1024)
	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: big, Password: "x"}, &errBody)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if errBody.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", errBody.Error.Code)
	}
}

// TestHandleCreateAppBodyTooLarge proves bodyLimit is also wired to the
// authenticated route group (not just the login route), by tripping it
// against handleCreateApp.
func TestHandleCreateAppBodyTooLarge(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	big := strings.Repeat("a", maxJSONBody+1024)
	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", session.Token,
		createAppRequest{Name: big, Image: "nginx:alpine", Port: 80}, &errBody)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if errBody.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", errBody.Error.Code)
	}
}

// TestAuthMeBearerCaseInsensitive proves the "Bearer" auth scheme is
// matched case-insensitively, per RFC 7235's auth-scheme being
// case-insensitive and some HTTP clients sending non-canonical casing.
func TestAuthMeBearerCaseInsensitive(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", scheme+" "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("scheme %q: status = %d, want 200", scheme, resp.StatusCode)
		}
	}

	// A scheme that isn't "bearer" at all (case-insensitively) must still
	// be rejected — this isn't a blanket "any scheme goes" relaxation.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-bearer scheme: status = %d, want 401", resp.StatusCode)
	}
}

// TestRateLimiterEvictsStaleEntries proves maybeSweep's time-based
// trigger evicts a key once its newest attempt has aged out of the
// window and more than sweepInterval has passed, using an injected clock
// rather than sleeping in real time.
func TestRateLimiterEvictsStaleEntries(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base

	rl := newRateLimiter(10, time.Minute)
	rl.nowFunc = func() time.Time { return now }
	rl.lastSweep = now

	if !rl.Allow("1.2.3.4") {
		t.Fatal("expected the first attempt to be allowed")
	}
	if len(rl.attempts) != 1 {
		t.Fatalf("expected 1 tracked key after the first attempt, got %d", len(rl.attempts))
	}

	// Advance well past both the 1-minute rate window and the 5-minute
	// sweep interval, then make a call for a different key.
	now = base.Add(10 * time.Minute)

	if !rl.Allow("5.6.7.8") {
		t.Fatal("expected the new key's attempt to be allowed")
	}
	if _, ok := rl.attempts["1.2.3.4"]; ok {
		t.Fatal("expected the stale 1.2.3.4 entry to be evicted by the sweep")
	}
	if _, ok := rl.attempts["5.6.7.8"]; !ok {
		t.Fatal("expected the new key's entry to still be present")
	}
}

// TestRateLimiterSweepsOnSizeThreshold proves maybeSweep's size-based
// trigger fires even when the time-based trigger hasn't (lastSweep is
// "now"), once the map holds more than sweepThreshold keys.
func TestRateLimiterSweepsOnSizeThreshold(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(10, time.Minute)
	rl.nowFunc = func() time.Time { return now }
	rl.lastSweep = now // no time-based trigger

	// Seed sweepThreshold+1 stale entries directly, bypassing Allow (which
	// would itself sweep once the threshold is crossed).
	stale := now.Add(-2 * time.Minute) // older than the 1-minute window
	for i := 0; i < sweepThreshold+1; i++ {
		rl.attempts[fmt.Sprintf("k%d", i)] = []time.Time{stale}
	}

	rl.Allow("new-key")

	if len(rl.attempts) != 1 {
		t.Fatalf("expected the size-triggered sweep to evict all stale keys leaving just the new one, got %d remaining: %v",
			len(rl.attempts), rl.attempts)
	}
	if _, ok := rl.attempts["new-key"]; !ok {
		t.Fatal("expected new-key's entry to be present after the sweep")
	}
}

// TestRateLimiterStillEnforcesLimit is a sanity check that the eviction
// changes above didn't loosen the actual rate-limiting behavior: a key
// still gets blocked once it exceeds the configured limit within the
// window.
func TestRateLimiterStillEnforcesLimit(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(3, time.Minute)
	rl.nowFunc = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("attempt %d: expected allowed", i)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("expected the 4th attempt within the window to be blocked")
	}
}

// TestRateLimiterBlockedDoesNotConsume proves Blocked is a pure read: an
// unlimited number of Blocked checks against an unused key never trips
// the limiter, and Blocked itself never advances a key that's already at
// its limit. Checking status must never be indistinguishable from making
// an attempt (see api.handleLogin's use of Blocked before deciding
// whether a request counts as one).
func TestRateLimiterBlockedDoesNotConsume(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(3, time.Minute)
	rl.nowFunc = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		if rl.Blocked("k") {
			t.Fatalf("Blocked check %d: unexpectedly blocked on a never-attempted key", i)
		}
	}

	for i := 0; i < 3; i++ {
		if !rl.Allow("k") {
			t.Fatalf("Allow %d: expected allowed", i)
		}
	}
	if !rl.Blocked("k") {
		t.Fatal("expected Blocked to report true once the limit is reached")
	}
	// Checking again (repeatedly) must not itself do anything further.
	if !rl.Blocked("k") {
		t.Fatal("expected Blocked to still report true on a second check")
	}
}

// TestRateLimiterGlobalCeilingTripsAndRecovers proves the exact
// configuration handleLogin's global failed-login ceiling uses
// (globalLoginRateLimit attempts within loginRateWindow, all recorded
// under the single fixed globalLimiterKey) trips once exhausted and
// recovers once the window has fully elapsed — using an injected clock
// rather than sleeping for a real minute.
func TestRateLimiterGlobalCeilingTripsAndRecovers(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base

	rl := newRateLimiter(globalLoginRateLimit, loginRateWindow)
	rl.nowFunc = func() time.Time { return now }

	for i := 0; i < globalLoginRateLimit; i++ {
		if rl.Blocked(globalLimiterKey) {
			t.Fatalf("attempt %d: unexpectedly already blocked", i)
		}
		rl.Allow(globalLimiterKey)
	}
	if !rl.Blocked(globalLimiterKey) {
		t.Fatal("expected the global ceiling to be tripped after globalLoginRateLimit failures")
	}

	now = base.Add(loginRateWindow + time.Second)
	if rl.Blocked(globalLimiterKey) {
		t.Fatal("expected the global ceiling to have recovered after the window elapsed")
	}
}
