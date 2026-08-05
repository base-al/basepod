package podman

import (
	"context"
	"io"
	"os"
	"testing"
)

// TestRealPing is a manual, opt-in integration test against a real local
// podman daemon. It is skipped unless BASEPOD_TEST_PODMAN=1 is set:
//
//	BASEPOD_TEST_PODMAN=1 go test ./internal/podman/ -run TestRealPing -v
func TestRealPing(t *testing.T) {
	if os.Getenv("BASEPOD_TEST_PODMAN") == "" {
		t.Skip("set BASEPOD_TEST_PODMAN=1 to run against a real podman daemon")
	}
	c, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRealContainerLogs is a manual, opt-in, strictly read-only
// integration test against a real local podman daemon's "bp-hello-1"
// container (as started by `basepod`'s example app). It only reads logs
// (follow=false, a small tail) — it never creates, starts, stops, or
// removes anything — then runs the raw response through DemuxLogs to
// prove the two work together against a real daemon's actual framing, not
// just the httptest fixtures in logs_test.go/client_test.go.
//
//	BASEPOD_TEST_PODMAN=1 go test ./internal/podman/ -run TestRealContainerLogs -v
func TestRealContainerLogs(t *testing.T) {
	if os.Getenv("BASEPOD_TEST_PODMAN") == "" {
		t.Skip("set BASEPOD_TEST_PODMAN=1 to run against a real podman daemon")
	}
	c, err := New("")
	if err != nil {
		t.Fatal(err)
	}

	rc, err := c.ContainerLogs(context.Background(), "bp-hello-1", false, 20)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer rc.Close()

	var lines []struct{ stream, text string }
	if err := DemuxLogs(rc, func(stream, text string) {
		lines = append(lines, struct{ stream, text string }{stream, text})
	}); err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	t.Logf("read %d demuxed log line(s) from bp-hello-1", len(lines))
	for _, l := range lines {
		t.Logf("[%s] %s", l.stream, l.text)
	}

	// Sanity check the reader is exhausted (ContainerLogs with
	// follow=false returns a bounded response), i.e. this really is a
	// read-to-completion, not a hung follow stream.
	if n, err := io.Copy(io.Discard, rc); err != nil || n != 0 {
		t.Errorf("expected the log stream to already be fully consumed, got %d more bytes (err=%v)", n, err)
	}
}
