package podman

import (
	"strings"
	"testing"
)

func TestNewRejectsNonUnixScheme(t *testing.T) {
	_, err := New("ssh://core@127.0.0.1:12345/run/podman/podman.sock")
	if err == nil {
		t.Fatal("expected error for ssh:// socket URI")
	}
	for _, want := range []string{"podman machine start", "podman_socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestSocketPathParsesUnixURI(t *testing.T) {
	p, err := socketPath("unix:///var/run/podman/podman.sock")
	if err != nil {
		t.Fatal(err)
	}
	if p != "/var/run/podman/podman.sock" {
		t.Fatalf("got %q", p)
	}
}

// TestMachineForwardedSocketRuns exercises machineForwardedSocket against
// whatever `podman` binary is on PATH (if any). It only asserts the
// call doesn't panic and, when it succeeds, that the output is trimmed of
// trailing whitespace/newlines — the actual exec + `podman machine
// inspect` behavior is covered end-to-end by TestRealPing (behind
// BASEPOD_TEST_PODMAN=1), which exercises the full DetectSocket fallback
// chain including this step against a real daemon.
func TestMachineForwardedSocketRuns(t *testing.T) {
	p, err := machineForwardedSocket()
	if err != nil {
		t.Skipf("podman machine inspect unavailable in this environment: %v", err)
	}
	if strings.TrimSpace(p) != p {
		t.Fatalf("machineForwardedSocket did not trim output: %q", p)
	}
}
