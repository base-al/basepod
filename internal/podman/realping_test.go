package podman

import (
	"context"
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
