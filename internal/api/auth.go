package api

import (
	"net/http"
	"time"

	"github.com/base-al/basepod/internal/auth"
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
	if err != nil || !auth.VerifyPassword(req.Password, user.PasswordHash) {
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
