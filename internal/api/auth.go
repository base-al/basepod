package api

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/base-al/basepod/internal/auth"
)

// clientIP extracts the host portion of r.RemoteAddr for rate-limit
// bucketing. Falls back to the raw value if it isn't a host:port pair
// (e.g. in atypical test transports).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

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

// handleLogin verifies email/password, rate-limited per client IP, and on
// success issues a new session token good for sessionDuration.
func (a *api) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts; try again later")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed request body")
		return
	}

	user, err := a.st.UserByEmail(req.Email)
	if err != nil || !auth.VerifyPassword(req.Password, user.PasswordHash) {
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
