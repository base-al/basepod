// Package auth provides password hashing and session token primitives.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Time    = 3
	argon2Memory  = 64 * 1024
	argon2Threads = 2
	argon2KeyLen  = 32
	saltLen       = 16
)

// HashPassword hashes pw using argon2id with fixed parameters and a random
// 16-byte salt, returning the PHC-style encoded string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<b64salt>$<b64hash>
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(pw), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)

	encoded := fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyPassword reports whether pw matches the argon2id-encoded hash.
// It parses the parameters from encoded and recomputes with those exact
// parameters, comparing in constant time. It never panics; any malformed
// encoding results in false.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	// Expected: "", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"
	if len(parts) != 6 {
		return false
	}
	if parts[1] != "argon2id" {
		return false
	}
	if parts[2] != "v=19" {
		return false
	}

	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	wantHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	gotHash := argon2.IDKey([]byte(pw), salt, time, memory, threads, uint32(len(wantHash)))

	return subtle.ConstantTimeCompare(gotHash, wantHash) == 1
}
