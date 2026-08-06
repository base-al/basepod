package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

const sessionTokenPrefix = "bp_sess_"

// streamTokenPrefix distinguishes a stream token from a session token at
// a glance (and rules out any accidental prefix collision between the
// two token spaces) — see NewStreamToken.
const streamTokenPrefix = "bp_stream_"

// NewSessionToken generates a new random session token and its sha256 hex
// digest. token has the form "bp_sess_"+48 hex characters (24 random bytes).
// tokenHash is the sha256 hex digest of the full token string; only the
// hash should be persisted.
func NewSessionToken() (token, tokenHash string) {
	return newToken(sessionTokenPrefix)
}

// NewStreamToken generates a new random stream token and its sha256 hex
// digest, exactly like NewSessionToken but with a distinct prefix. Stream
// tokens authenticate BasePod's SSE routes only (audit finding M4): they
// are short-lived (5 minutes) and scoped to exactly one (scope, slug,
// deployment) triple, minted via POST /api/v1/stream-token rather than
// login — see internal/store's stream_tokens table and
// internal/api/stream_token.go. Hashed and persisted the same way a
// session token is (see HashToken); only the hash is ever stored.
func NewStreamToken() (token, tokenHash string) {
	return newToken(streamTokenPrefix)
}

// newToken generates a random token of the form prefix+48 hex characters
// (24 random bytes) and its sha256 hex digest, shared by NewSessionToken
// and NewStreamToken so their randomness/hashing logic can't drift apart.
func newToken(prefix string) (token, tokenHash string) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read failing indicates a broken system RNG; there is
		// no safe way to continue issuing tokens.
		panic("auth: failed to read random bytes: " + err.Error())
	}
	token = prefix + hex.EncodeToString(buf)
	tokenHash = HashToken(token)
	return token, tokenHash
}

// HashToken returns the sha256 hex digest of the full token string.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
