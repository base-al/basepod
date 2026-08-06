package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/store"
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type loginResponse struct {
	Token string       `json:"token"`
	User  userResponse `json:"user"`
}

// verifyDummyLogin is auth.VerifyPassword against the fixed,
// non-secret auth.DummyHash (see its doc comment) — a package var
// (rather than a bare call in handleLogin) so tests can substitute a spy
// to prove the unknown-email login path actually invokes it (see
// login_timing_test.go). This closes audit finding L1 (login timing
// enumeration): DummyHash costs argon2 the same m/t/p parameters as a
// real user's hash, so calling it on the unknown-email path pays the
// same latency the wrong-password path already pays via
// auth.VerifyPassword against a real hash — an attacker measuring
// response time can no longer distinguish "no such user" from "wrong
// password".
var verifyDummyLogin = func(pw string) bool { return auth.VerifyPassword(pw, auth.DummyHash) }

// handleLogin verifies email/password, rate-limited per client IP (plus a
// global ceiling across every IP — see the api struct's limiter/
// globalLimiter doc comments), and on success issues a new session token
// good for sessionDuration.
//
// Only a FAILED attempt consumes a rate-limit slot. The limiters are
// peeked (Blocked) before any credential check, so an already-exhausted
// caller is rejected without even reading the body or touching the
// store; a failed credential check then records the attempt against both
// limiters. A successful login records nothing — an admin who mistypes
// their password a few times and then gets it right must not find their
// own next login rate-limited by the success itself.
//
// The unknown-email and wrong-password branches are deliberately kept
// separate (rather than one `err != nil || !VerifyPassword(...)` check)
// so the unknown-email branch can pay verifyDummyLogin's argon2 cost —
// see its doc comment for why (audit finding L1) — instead of returning
// immediately with none of the wrong-password branch's latency.
func (a *api) handleLogin(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r)
	if a.limiter.Blocked(key) || a.globalLimiter.Blocked(globalLimiterKey) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts; try again later")
		return
	}

	var req loginRequest
	if !readJSON(w, r, &req) {
		return
	}

	user, err := a.st.UserByEmail(req.Email)
	if err != nil {
		// Unknown email: pay the same argon2 cost VerifyPassword against a
		// real hash would (see verifyDummyLogin's doc comment), so this
		// request's timing can't be distinguished from the wrong-password
		// branch below.
		verifyDummyLogin(req.Password)
		a.limiter.Allow(key)
		a.globalLimiter.Allow(globalLimiterKey)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}
	if !auth.VerifyPassword(req.Password, user.PasswordHash) {
		a.limiter.Allow(key)
		a.globalLimiter.Allow(globalLimiterKey)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	token, tokenHash := auth.NewSessionToken()
	if err := a.st.CreateSession(user.ID, tokenHash, time.Now().Add(sessionDuration)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to create session")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		Token: token,
		User:  userResponse{Email: user.Email, Name: user.Name},
	})
}

// handleMe returns the authenticated user's public profile.
func (a *api) handleMe(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	writeJSON(w, http.StatusOK, userResponse{Email: user.Email, Name: user.Name})
}

// handleLogout revokes the caller's current session server-side, so the
// bearer token presented on this request can no longer authenticate future
// requests. It re-extracts the token from the Authorization header (rather
// than threading the hash through the request context from requireAuth)
// since only this one handler needs it. Deleting an already-gone session
// (e.g. a double logout) is not an error — see
// store.DeleteSessionByTokenHash — so this always reports success once the
// caller was authenticated at all.
func (a *api) handleLogout(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || token == "" {
		// requireAuth already guarantees this can't happen in practice, but
		// fail closed rather than deleting nothing silently.
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed authorization header")
		return
	}

	if err := a.st.DeleteSessionByTokenHash(auth.HashToken(token)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to revoke session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// currentTokenHash re-extracts and hashes the bearer token off r, the same
// way handleLogout does — used by handleListSessions (to flag which listed
// session is this one) and handleChangePassword (to know which session to
// spare when revoking the rest). requireAuth has already validated this
// request has a well-formed, live token by the time any of these handlers
// run, so the ok return is not re-checked here.
func currentTokenHash(r *http.Request) string {
	token, _ := bearerToken(r.Header.Get("Authorization"))
	return auth.HashToken(token)
}

// sessionResponse is the wire shape of one GET /auth/sessions entry. The
// session's token hash is deliberately not exposed — Current is the only
// thing a client needs to tell "this session" apart from the rest.
type sessionResponse struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Current   bool   `json:"current"`
}

// handleListSessions lists every live session belonging to the caller,
// newest first, flagging exactly the one this request itself is
// authenticated with as Current.
func (a *api) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())

	sessions, err := a.st.ListSessions(user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list sessions")
		return
	}

	mine := currentTokenHash(r)
	out := make([]sessionResponse, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionResponse{
			ID:        sess.ID,
			CreatedAt: sess.CreatedAt,
			ExpiresAt: sess.ExpiresAt,
			Current:   sess.TokenHash == mine,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteSession revokes one of the caller's own sessions by id.
// store.DeleteSession scopes the delete to the caller's user id, so
// attempting to delete another user's session id (or a nonexistent one)
// both surface as 404 here — the caller can't distinguish "not yours" from
// "doesn't exist". Revoking the session the caller is *currently*
// authenticated with is allowed (it logs that session out immediately,
// same as /auth/logout) — the dashboard warns before doing this to the
// active session, but the API itself doesn't special-case it.
func (a *api) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())

	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}

	if err := a.st.DeleteSession(id, user.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to revoke session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// minPasswordLength mirrors internal/server/setup.go's own
// --admin-password rule — a password change enforces exactly the same
// minimum the initial admin password does, not a separately-invented one.
const minPasswordLength = 8

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword verifies the caller's current password, re-hashes
// and stores the new one, and then revokes every OTHER session belonging to
// this user while leaving the caller's own session (the one this very
// request is authenticated with) alive — see store.DeleteSessionsExcept's
// doc comment for why that direction matters: getting it backwards either
// locks the admin out of the session they're using right now, or leaves
// every other session (e.g. an attacker's, if this change was prompted by a
// suspected compromise) still valid.
func (a *api) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())

	var req passwordChangeRequest
	if !readJSON(w, r, &req) {
		return
	}

	if !auth.VerifyPassword(req.CurrentPassword, user.PasswordHash) {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "current password is incorrect")
		return
	}

	if len(req.NewPassword) < minPasswordLength {
		writeError(w, http.StatusUnprocessableEntity, "validation", "new password must be at least 8 characters")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to hash new password")
		return
	}

	if err := a.st.UpdatePassword(user.ID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update password")
		return
	}

	if _, err := a.st.DeleteSessionsExcept(user.ID, currentTokenHash(r)); err != nil {
		// The password itself is already changed at this point — a failure
		// here would silently leave stale sessions alive, so it's reported
		// as a request failure rather than swallowed, even though the
		// caller must not retry the whole request (VerifyPassword would now
		// fail against the OLD password they'd resend).
		writeError(w, http.StatusInternalServerError, "internal", "password changed, but failed to revoke other sessions")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
