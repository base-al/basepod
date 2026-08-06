package auth

import (
	"strings"
	"testing"
	"time"
)

// TestDummyHashNeverVerifies proves auth.DummyHash (see its doc comment —
// audit finding L1) is a well-formed argon2id hash that never verifies
// against any password a real login attempt would plausibly send —
// exactly the property the L1 fix relies on: calling
// VerifyPassword(attackerSuppliedPassword, DummyHash) on the
// unknown-email path must always return false, the same as a real
// "wrong password" outcome.
func TestDummyHashNeverVerifies(t *testing.T) {
	if !strings.HasPrefix(DummyHash, "$argon2id$") {
		t.Fatalf("DummyHash = %q, want a well-formed argon2id encoding", DummyHash)
	}
	for _, pw := range []string{"", "password", "correct horse battery staple", "hunter2"} {
		if VerifyPassword(pw, DummyHash) {
			t.Fatalf("VerifyPassword(%q, DummyHash) = true, want false (DummyHash must never verify)", pw)
		}
	}
}

// TestDummyHashCostsComparableTime proves VerifyPassword against
// DummyHash pays the same argon2 cost as verifying against a real
// user's hash — the entire point of L1's fix: an unknown-email login
// must cost the same as a known-email one. Compares wall-clock time
// against a generous ratio bound (5x) rather than a tight one, since CI
// scheduling jitter makes an exact-timing assertion flaky; the two calls
// use identical argon2 parameters (see HashPassword/DummyHash), so any
// gap this test would actually catch (e.g. DummyHash silently using
// cheaper parameters) is far larger than scheduling noise.
func TestDummyHashCostsComparableTime(t *testing.T) {
	realHash, err := HashPassword("some-real-password")
	if err != nil {
		t.Fatal(err)
	}

	timeIt := func(hash string) time.Duration {
		start := time.Now()
		VerifyPassword("wrong-guess", hash)
		return time.Since(start)
	}

	// Warm up (first call on a cold CPU cache/scheduler can be an outlier).
	timeIt(realHash)
	timeIt(DummyHash)

	realElapsed := timeIt(realHash)
	dummyElapsed := timeIt(DummyHash)

	ratio := float64(dummyElapsed) / float64(realElapsed)
	if ratio < 0.2 || ratio > 5 {
		t.Fatalf("dummy/real verify time ratio = %.2f (dummy=%v real=%v), want roughly comparable (0.2x-5x)",
			ratio, dummyElapsed, realElapsed)
	}
}

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

// TestStreamToken mirrors TestSessionToken for NewStreamToken, and also
// proves the two token spaces never collide on prefix — a stream token
// must never be mistakable for (or accepted where BasePod expects) a
// session token.
func TestStreamToken(t *testing.T) {
	tok, hash := NewStreamToken()
	if !strings.HasPrefix(tok, "bp_stream_") || len(tok) != len("bp_stream_")+48 {
		t.Fatalf("token shape: %q", tok)
	}
	if HashToken(tok) != hash || len(hash) != 64 {
		t.Fatal("hash mismatch")
	}
	if strings.HasPrefix(tok, "bp_sess_") {
		t.Fatal("stream token must not also look like a session token")
	}
}
