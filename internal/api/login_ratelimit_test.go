package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/base-al/basepod/internal/store"
)

// trustedRequest returns a shallow copy of r carrying the trusted-listener
// marker TrustedProxyMiddleware sets on every request that reaches it —
// used by TestClientIP so its trusted cases can exercise clientIP's
// trusted path without spinning up a real unix-socket listener.
func trustedRequest(r *http.Request) *http.Request {
	var got *http.Request
	TrustedProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r2 *http.Request) {
		got = r2
	})).ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// TestClientIP proves clientIP's trust-aware behavior end to end (audit
// finding H1): on a trusted request it reads the LAST hop of
// X-Forwarded-For (falling back to RemoteAddr if that header is absent
// or doesn't parse as an IP), and on an untrusted request it ALWAYS uses
// RemoteAddr, never even looking at X-Forwarded-For.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		trusted    bool
		remoteAddr string
		xff        string
		want       string
	}{
		{"trusted single hop uses it", true, "10.0.0.1:9999", "203.0.113.5", "203.0.113.5"},
		{"trusted multi-hop uses the last (closest to us)", true, "10.0.0.1:9999", "203.0.113.5, 198.51.100.7", "198.51.100.7"},
		{"trusted absent xff falls back to remote addr", true, "203.0.113.9:1234", "", "203.0.113.9"},
		{"trusted garbage xff falls back to remote addr", true, "203.0.113.9:1234", "not-an-ip-at-all", "203.0.113.9"},
		{"untrusted ignores spoofed xff", false, "203.0.113.9:1234", "9.9.9.9", "203.0.113.9"},
		{"untrusted absent xff uses remote addr", false, "203.0.113.9:1234", "", "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.trusted {
				req = trustedRequest(req)
			}
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// newTrustedTestServer is newTestServer's trusted-listener counterpart:
// it wraps New(...)'s handler in TrustedProxyMiddleware, standing in for
// internal/server's dashboard unix-socket listener — the only listener
// ever allowed to grant trusted status in the real process.
func newTrustedTestServer(t *testing.T, st *store.Store, dep Deployer, routes RoutesApplier) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(TrustedProxyMiddleware(New(st, dep, fakePinger(nil), "test-version", testSeal, testOpen, routes, unusedLogSource, nil)))
	t.Cleanup(srv.Close)
	return srv
}

// loginAsWithXFF posts a login request carrying an X-Forwarded-For header
// (skipped entirely if xff is ""), returning the raw response so callers
// can assert on status codes across a run of many attempts without
// decoding a body each time.
func loginAsWithXFF(t *testing.T, srv *httptest.Server, email, password, xff string) *http.Response {
	t.Helper()
	body, err := json.Marshal(loginRequest{Email: email, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// loginWithXFF is loginAsWithXFF fixed to the seeded admin user's email —
// the common case for every test below except
// TestHandleLoginGlobalCeilingTrips, which deliberately uses an unknown
// email instead (see that test's doc comment for why).
func loginWithXFF(t *testing.T, srv *httptest.Server, password, xff string) *http.Response {
	t.Helper()
	return loginAsWithXFF(t, srv, "admin@example.com", password, xff)
}

// TestHandleLoginXFFIgnoredWhenUntrusted proves the public (untrusted)
// listener's login route never honors X-Forwarded-For: loginRateLimit
// failed attempts, each claiming a different spoofed source IP, must
// still all land in the SAME bucket (the real RemoteAddr, always
// 127.0.0.1 for an httptest.Server), so the next attempt is rate
// limited despite every request "claiming" to be from a fresh client.
func TestHandleLoginXFFIgnoredWhenUntrusted(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	for i := 0; i < loginRateLimit; i++ {
		resp := loginWithXFF(t, srv, "wrong-password", fmt.Sprintf("10.0.0.%d", i))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, resp.StatusCode)
		}
	}
	resp := loginWithXFF(t, srv, "wrong-password", "10.0.0.250")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt %d (spoofed XFF must not evade the shared bucket): status = %d, want 429", loginRateLimit, resp.StatusCode)
	}
}

// TestHandleLoginXFFHonoredWhenTrustedIsolatesPerIP proves the trusted
// (dashboard) listener DOES key off X-Forwarded-For, and that doing so
// gives each real client its own independent bucket — the core fix for
// audit finding H1: an attacker exhausting their own bucket must never
// affect a different client's ability to log in.
func TestHandleLoginXFFHonoredWhenTrustedIsolatesPerIP(t *testing.T) {
	st := newTestStore(t)
	srv := newTrustedTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	const attackerIP = "203.0.113.10"
	const adminIP = "198.51.100.20"

	for i := 0; i < loginRateLimit; i++ {
		resp := loginWithXFF(t, srv, "wrong-password", attackerIP)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attacker attempt %d: status = %d, want 401", i, resp.StatusCode)
		}
	}
	resp := loginWithXFF(t, srv, "wrong-password", attackerIP)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attacker attempt %d: status = %d, want 429 (own bucket exhausted)", loginRateLimit, resp.StatusCode)
	}

	resp = loginWithXFF(t, srv, testPassword, adminIP)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login from a different IP: status = %d, want 200 (must not be blocked by the attacker's bucket)", resp.StatusCode)
	}
}

// TestHandleLoginSuccessDoesNotConsumeRateLimit proves a successful login
// leaves its IP's failed-attempt budget fully intact — only failures
// count. Without this, a legitimate admin who logs in successfully a
// handful of times could find themselves rate limited out of a
// subsequent (mistyped-password) attempt for no attacker-driven reason.
func TestHandleLoginSuccessDoesNotConsumeRateLimit(t *testing.T) {
	st := newTestStore(t)
	srv := newTrustedTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	const ip = "203.0.113.50"

	for i := 0; i < 3; i++ {
		resp := loginWithXFF(t, srv, testPassword, ip)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("successful login %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	for i := 0; i < loginRateLimit; i++ {
		resp := loginWithXFF(t, srv, "wrong-password", ip)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d after successful logins: status = %d, want 401 (successful logins must not consume slots)", i, resp.StatusCode)
		}
	}
	resp := loginWithXFF(t, srv, "wrong-password", ip)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want 429", loginRateLimit, resp.StatusCode)
	}
}

// TestHandleLoginGlobalCeilingTrips proves the global failed-login
// ceiling (globalLoginRateLimit, across every IP combined) is actually
// wired into handleLogin: globalLoginRateLimit failed attempts, each
// from its OWN distinct IP (so no individual per-IP bucket comes
// anywhere near loginRateLimit), still trip a block on the very next
// attempt — even one from a completely fresh IP that has never been
// seen before. This is the backstop a distributed flood needs, which
// the per-IP limiter alone can't see.
//
// This deliberately logs in as an UNKNOWN email rather than the seeded
// admin with a wrong password: handleLogin's credential check
// short-circuits UserByEmail's "not found" before ever reaching
// auth.VerifyPassword's argon2 hashing (see auth.go), so
// globalLoginRateLimit sequential requests stay fast and — more
// importantly — stay well inside loginRateWindow's real 1-minute wall
// clock. (a.globalLimiter runs on the real clock here, not an injected
// one — see TestRateLimiterGlobalCeilingTripsAndRecovers for the
// clock-independent version of this same trip/recover behavior.) Using
// the slow argon2 path here risked the earliest attempts aging back out
// of the window before the last one was even sent, especially under
// `go test -race`, making the test flaky rather than wrong.
func TestHandleLoginGlobalCeilingTrips(t *testing.T) {
	st := newTestStore(t)
	srv := newTrustedTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	for i := 0; i < globalLoginRateLimit; i++ {
		email := fmt.Sprintf("nobody-%d@example.com", i)
		resp := loginAsWithXFF(t, srv, email, "whatever", fmt.Sprintf("198.51.100.%d", i))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, resp.StatusCode)
		}
	}

	resp := loginAsWithXFF(t, srv, "still-nobody@example.com", "whatever", "203.0.113.200")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("fresh IP after the global ceiling tripped: status = %d, want 429", resp.StatusCode)
	}
}
