package auth

import (
	"strings"
	"testing"
)

func TestPasswordRoundtrip(t *testing.T) {
	h, err := HashPassword("s3cret")
	if err != nil || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash: %q err: %v", h, err)
	}
	if !VerifyPassword("s3cret", h) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("s3cret")
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestVerifyGarbageEncoding(t *testing.T) {
	if VerifyPassword("x", "not-a-hash") || VerifyPassword("x", "$argon2id$v=19$bad") {
		t.Fatal("garbage encoding accepted")
	}
}

func TestSessionToken(t *testing.T) {
	tok, hash := NewSessionToken()
	if !strings.HasPrefix(tok, "bp_sess_") || len(tok) != len("bp_sess_")+48 {
		t.Fatalf("token shape: %q", tok)
	}
	if HashToken(tok) != hash || len(hash) != 64 {
		t.Fatal("hash mismatch")
	}
}
