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
