package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/base-al/basepod/internal/auth"
)

// TestListSessionsMarksExactlyOneCurrent proves GET /auth/sessions flags
// exactly the session this request is itself authenticated with, out of
// several live sessions for the same user.
func TestListSessionsMarksExactlyOneCurrent(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	_, sessionA := login(t, srv, testPassword)
	_, sessionB := login(t, srv, testPassword)

	var sessions []sessionResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/sessions", sessionA.Token, nil, &sessions)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	currentCount := 0
	for _, s := range sessions {
		if s.Current {
			currentCount++
		}
		if s.CreatedAt == "" || s.ExpiresAt == "" || s.ID == 0 {
			t.Fatalf("session missing expected fields: %+v", s)
		}
	}
	if currentCount != 1 {
		t.Fatalf("got %d sessions flagged current, want exactly 1", currentCount)
	}

	// Requesting as sessionB instead flags the other one.
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/sessions", sessionB.Token, nil, &sessions)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	currentCount = 0
	for _, s := range sessions {
		if s.Current {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Fatalf("got %d sessions flagged current for sessionB, want exactly 1", currentCount)
	}
}

// TestDeleteSessionOwnerScoping proves DELETE /auth/sessions/{id} is scoped
// to the caller's own sessions: a second user cannot revoke the first
// user's session by id — newTestStore only seeds one user, so this seeds a
// second one directly through the store to exercise the cross-owner case.
func TestDeleteSessionOwnerScoping(t *testing.T) {
	st := newTestStore(t)

	otherHash, err := auth.HashPassword("other-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("other@example.com", "Other", otherHash, false); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	_, admin := login(t, srv, testPassword)

	var otherLogin loginResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: "other@example.com", Password: "other-password"}, &otherLogin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other user login: got status %d, want 200", resp.StatusCode)
	}

	var otherSessions []sessionResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/sessions", otherLogin.Token, nil, &otherSessions)
	if resp.StatusCode != http.StatusOK || len(otherSessions) != 1 {
		t.Fatalf("other user sessions: status=%d body=%+v", resp.StatusCode, otherSessions)
	}
	otherSessionID := otherSessions[0].ID

	// admin cannot delete the other user's session.
	resp = doJSON(t, http.MethodDelete, srv.URL+"/api/v1/auth/sessions/"+strconv.FormatInt(otherSessionID, 10), admin.Token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner delete: got status %d, want 404", resp.StatusCode)
	}

	// the other user's session is still valid.
	meResp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", otherLogin.Token, nil, nil)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("other user's session was revoked by admin's request: got status %d, want 200", meResp.StatusCode)
	}
}

func TestDeleteSessionSelfAndUnknown(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var sessions []sessionResponse
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/sessions", session.Token, nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}

	// Unknown id -> 404.
	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/auth/sessions/999999", session.Token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: got status %d, want 404", resp.StatusCode)
	}

	// Revoking one's own current session succeeds and immediately logs it out.
	resp = doJSON(t, http.MethodDelete, srv.URL+"/api/v1/auth/sessions/"+strconv.FormatInt(sessions[0].ID, 10), session.Token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("self delete: got status %d, want 204", resp.StatusCode)
	}

	meResp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", session.Token, nil, nil)
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after self-revoke: got status %d, want 401", meResp.StatusCode)
	}
}

// TestChangePasswordHappyPathRevokesOtherSessionsOnly proves a password
// change: (1) requires the correct current password, (2) enforces the
// 8-character minimum on the new one, and (3) — the security-critical
// direction, see store.DeleteSessionsExcept's doc comment — revokes every
// OTHER session for this user while leaving the caller's own session (the
// one the request itself is authenticated with) alive.
func TestChangePasswordHappyPathRevokesOtherSessionsOnly(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	_, caller := login(t, srv, testPassword)
	_, other := login(t, srv, testPassword)

	// wrong current password -> 401 invalid_credentials
	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/password", caller.Token,
		passwordChangeRequest{CurrentPassword: "not-the-password", NewPassword: "newlongpassword"}, &errBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong current password: got status %d, want 401", resp.StatusCode)
	}
	if errBody.Error.Code != "invalid_credentials" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	// too-short new password -> 422 validation
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/password", caller.Token,
		passwordChangeRequest{CurrentPassword: testPassword, NewPassword: "short"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("short new password: got status %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("unexpected error code: %+v", errBody)
	}

	// Both sessions still valid — nothing should have been revoked by the
	// failed attempts above.
	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", other.Token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("other session invalidated by a failed password-change attempt: got status %d, want 200", resp.StatusCode)
	}

	// happy path
	const newPassword = "newlongpassword"
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/password", caller.Token,
		passwordChangeRequest{CurrentPassword: testPassword, NewPassword: newPassword}, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: got status %d, want 204", resp.StatusCode)
	}

	// The caller's own session survives.
	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", caller.Token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("caller's own session was revoked: got status %d, want 200", resp.StatusCode)
	}

	// Every other session for this user is dead.
	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", other.Token, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session survived a password change: got status %d, want 401", resp.StatusCode)
	}

	// The old password no longer works; the new one does.
	if resp, _ := login(t, srv, testPassword); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still works: got status %d, want 401", resp.StatusCode)
	}
	if resp, _ := login(t, srv, newPassword); resp.StatusCode != http.StatusOK {
		t.Fatalf("new password rejected: got status %d, want 200", resp.StatusCode)
	}
}
