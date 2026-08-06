package build

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestLimitedLogWriterUnderCap proves writes that never reach the cap
// pass through untouched, with no truncation notice appended.
func TestLimitedLogWriterUnderCap(t *testing.T) {
	var buf bytes.Buffer
	lw := newLimitedLogWriter(&buf, 100)

	n, err := lw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5", n)
	}
	if buf.String() != "hello" {
		t.Fatalf("buf = %q, want %q", buf.String(), "hello")
	}
}

// TestLimitedLogWriterExactlyAtCap proves a write that lands exactly on
// the cap is written in full, with the truncation notice appended
// exactly once right after it.
func TestLimitedLogWriterExactlyAtCap(t *testing.T) {
	var buf bytes.Buffer
	lw := newLimitedLogWriter(&buf, 5)

	n, err := lw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5", n)
	}
	want := "hello" + logTruncationNotice
	if buf.String() != want {
		t.Fatalf("buf = %q, want %q", buf.String(), want)
	}

	// A further write past the cap must be silently discarded, and must
	// not append a second notice.
	n2, err := lw.Write([]byte("more"))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if n2 != 4 {
		t.Fatalf("n2 = %d, want 4 (caller must never see a short write)", n2)
	}
	if buf.String() != want {
		t.Fatalf("buf changed after cap was hit = %q, want unchanged %q", buf.String(), want)
	}
}

// TestLimitedLogWriterOverCapSingleWrite proves a single write larger
// than the cap is truncated mid-write, with the notice appended once
// right after the truncated portion.
func TestLimitedLogWriterOverCapSingleWrite(t *testing.T) {
	var buf bytes.Buffer
	lw := newLimitedLogWriter(&buf, 5)

	payload := "hello world, this keeps going well past the cap"
	n, err := lw.Write([]byte(payload))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("n = %d, want %d (caller must never see a short write)", n, len(payload))
	}
	want := "hello" + logTruncationNotice
	if buf.String() != want {
		t.Fatalf("buf = %q, want %q", buf.String(), want)
	}
}

// TestLimitedLogWriterOverCapMultipleWrites proves the cap and its
// one-time notice hold across many small writes accumulating past it,
// the shape podman.Client.BuildImage actually drives this writer with
// (one io.WriteString call per streamed build-log line).
func TestLimitedLogWriterOverCapMultipleWrites(t *testing.T) {
	var buf bytes.Buffer
	lw := newLimitedLogWriter(&buf, 10)

	for i := 0; i < 5; i++ {
		if _, err := lw.Write([]byte("1234567890")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	want := strings.Repeat("1234567890", 1) + logTruncationNotice
	if buf.String() != want {
		t.Fatalf("buf = %q, want %q", buf.String(), want)
	}

	// The notice must appear exactly once no matter how many more writes
	// follow.
	if n := strings.Count(buf.String(), "truncated"); n != 1 {
		t.Fatalf("truncation notice appeared %d times, want exactly 1", n)
	}
}

// TestLimitedLogWriterPropagatesUnderlyingError proves a genuine
// underlying I/O failure (distinct from the writer's own capping
// behavior) is still surfaced to the caller.
func TestLimitedLogWriterPropagatesUnderlyingError(t *testing.T) {
	failErr := errors.New("disk full")
	lw := newLimitedLogWriter(failingWriter{err: failErr}, 100)

	_, err := lw.Write([]byte("hello"))
	if !errors.Is(err, failErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, failErr)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }
