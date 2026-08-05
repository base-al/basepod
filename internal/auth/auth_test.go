package auth

import (
	"strings"
	"testing"
	"time"
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

// TestVerifyZeroParamsDoesNotPanic guards against a crafted-but-parseable
// encoding with time=0/threads=0. golang.org/x/crypto/argon2's IDKey
// panics on time<1 or threads<1 (it does not clamp), so VerifyPassword
// must reject these before ever calling IDKey.
func TestVerifyZeroParamsDoesNotPanic(t *testing.T) {
	if VerifyPassword("x", "$argon2id$v=19$m=0,t=0,p=0$$") {
		t.Fatal("zero-param encoding accepted")
	}
}

// TestVerifyHugeMemoryRejectedFast guards against a crafted encoding with
// an enormous m value, which would otherwise cause an unbounded
// allocation (~1KB per unit of memory) inside argon2.IDKey. VerifyPassword
// must reject it quickly, without attempting the allocation.
func TestVerifyHugeMemoryRejectedFast(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- VerifyPassword("x", "$argon2id$v=19$m=4294967295,t=3,p=2$AAAA$AAAA")
	}()
	select {
	case accepted := <-done:
		if accepted {
			t.Fatal("huge-memory encoding accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("VerifyPassword did not return quickly for huge-memory encoding")
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
