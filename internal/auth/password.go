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

// Bounds enforced on parameters parsed out of an encoded hash before they
// are ever passed to argon2.IDKey.
//
// golang.org/x/crypto/argon2's deriveKey panics if time<1 or threads<1
// (it does NOT clamp them), and allocates on the order of 1KB per unit of
// memory with no upper bound. A crafted-but-parseable encoded string could
// therefore crash the process (time=0 or threads=0) or exhaust memory/CPU
// (huge m or t) if fed straight into IDKey. Recomputing a hash we produced
// ourselves never needs parameters outside our own defaults' order of
// magnitude, so anything beyond these ceilings is rejected as malformed.
const (
	minArgon2Time    = 1
	maxArgon2Time    = 16
	minArgon2Memory  = 1
	maxArgon2Memory  = 1 << 21 // 2 GiB, in KiB
	minArgon2Threads = 1
	maxArgon2Threads = 16
)

// DummyHash is a valid argon2id-encoded hash (same shape and cost
// parameters as HashPassword produces) of a fixed, non-secret string —
// not tied to any real user's credential; the salt and plaintext are
// both published right here in source, so anyone can trivially confirm
// it doesn't correspond to a real password.
//
// It exists for audit finding L1 (login timing enumeration): the login
// handler's unknown-email path currently returns before ever calling
// VerifyPassword, while the "known email, wrong password" path always
// pays argon2's real cost — an attacker measuring response latency can
// use that gap to enumerate valid emails without guessing a single
// password. VerifyPassword(pw, DummyHash) costs the same as verifying
// against a real user's hash (same m/t/p parameters), so calling it
// (and discarding the result — it must never succeed) on the
// unknown-email path closes the timing gap.
//
// This file only provides the primitive: internal/api/auth.go — this
// milestone's file-ownership split puts it under a different agent
// working concurrently — is where the actual call-site fix belongs. See
// the security-sweep report for the exact change.
const DummyHash = "$argon2id$v=19$m=65536,t=3,p=2$YmFzZXBvZC1kdW1teS1zeA$9pNVKgjvwoMRRFduPCoPQwJrz+OHUbVFSUh6gB8JSHs"

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
// parameters (after validating they fall within sane bounds), comparing
// in constant time. It never panics; any malformed or out-of-range
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

	// Reject out-of-range params before they ever reach argon2.IDKey: it
	// panics on time<1 or threads<1, and has no upper bound on memory/time,
	// so an attacker-crafted encoding could otherwise crash the process or
	// exhaust resources (see bounds doc comment above).
	if time < minArgon2Time || time > maxArgon2Time {
		return false
	}
	if memory < minArgon2Memory || memory > maxArgon2Memory {
		return false
	}
	if threads < minArgon2Threads || threads > maxArgon2Threads {
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
