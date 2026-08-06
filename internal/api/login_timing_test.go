package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// TestLoginUnknownEmailInvokesDummyVerify proves handleLogin's
// unknown-email branch actually calls verifyDummyLogin (audit finding
// L1: without this, an unknown email returns before ever paying
// argon2's cost, while a known-email/wrong-password attempt always pays
// it — a gap an attacker can use to enumerate valid emails by response
// latency). A timing assertion would be flaky under test load, so this
// asserts the call happened, not how long it took — see
// internal/auth's own TestDummyHashCostsComparableTime for the
// latency-parity proof.
func TestLoginUnknownEmailInvokesDummyVerify(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	var calls int32
	orig := verifyDummyLogin
	verifyDummyLogin = func(pw string) bool {
		atomic.AddInt32(&calls, 1)
		return false // DummyHash must never verify — see its doc comment.
	}
	t.Cleanup(func() { verifyDummyLogin = orig })

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: "no-such-user@example.com", Password: "whatever"}, &errBody)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if errBody.Error.Code != "invalid_credentials" {
		t.Fatalf("error code = %q, want invalid_credentials", errBody.Error.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("verifyDummyLogin call count = %d, want exactly 1", got)
	}
}

// TestLoginWrongPasswordDoesNotInvokeDummyVerify proves the known-email/
// wrong-password branch — which already pays argon2's cost via
// auth.VerifyPassword against the user's real hash — does NOT also call
// verifyDummyLogin, so a known user's failed login isn't paying the cost
// twice.
func TestLoginWrongPasswordDoesNotInvokeDummyVerify(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	var calls int32
	orig := verifyDummyLogin
	verifyDummyLogin = func(pw string) bool {
		atomic.AddInt32(&calls, 1)
		return false
	}
	t.Cleanup(func() { verifyDummyLogin = orig })

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: "admin@example.com", Password: "definitely-wrong"}, &errBody)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if errBody.Error.Code != "invalid_credentials" {
		t.Fatalf("error code = %q, want invalid_credentials", errBody.Error.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("verifyDummyLogin call count = %d, want 0 (wrong-password path already pays real VerifyPassword's cost)", got)
	}
}
