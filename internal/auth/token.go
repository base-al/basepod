package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

const sessionTokenPrefix = "bp_sess_"

// NewSessionToken generates a new random session token and its sha256 hex
// digest. token has the form "bp_sess_"+48 hex characters (24 random bytes).
// tokenHash is the sha256 hex digest of the full token string; only the
// hash should be persisted.
func NewSessionToken() (token, tokenHash string) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read failing indicates a broken system RNG; there is
		// no safe way to continue issuing session tokens.
		panic("auth: failed to read random bytes: " + err.Error())
	}
	token = sessionTokenPrefix + hex.EncodeToString(buf)
	tokenHash = HashToken(token)
	return token, tokenHash
}

// HashToken returns the sha256 hex digest of the full token string.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
