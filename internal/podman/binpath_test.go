package podman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinPathHonorsOverride proves BASEPOD_PODMAN_BIN, when set, is used
// verbatim rather than falling through to $PATH lookup (audit finding
// L3).
func TestBinPathHonorsOverride(t *testing.T) {
	resetBinPathForTest()
	t.Cleanup(resetBinPathForTest)

	t.Setenv(PodmanBinEnvVar, "/opt/custom/podman")

	p, err := BinPath()
	if err != nil {
		t.Fatalf("BinPath: %v", err)
	}
	if p != "/opt/custom/podman" {
		t.Fatalf("BinPath = %q, want the override %q", p, "/opt/custom/podman")
	}
}

// TestBinPathCachesResult proves BinPath resolves only once per process:
// changing BASEPOD_PODMAN_BIN after the first call must not change what
// subsequent calls return.
func TestBinPathCachesResult(t *testing.T) {
	resetBinPathForTest()
	t.Cleanup(resetBinPathForTest)

	t.Setenv(PodmanBinEnvVar, "/opt/first/podman")
	first, err := BinPath()
	if err != nil {
		t.Fatalf("BinPath: %v", err)
	}

	t.Setenv(PodmanBinEnvVar, "/opt/second/podman")
	second, err := BinPath()
	if err != nil {
		t.Fatalf("BinPath: %v", err)
	}

	if first != second {
		t.Fatalf("BinPath changed across calls: first=%q second=%q, want it cached (same value both times)", first, second)
	}
	if second != "/opt/first/podman" {
		t.Fatalf("BinPath = %q, want the cached first-call value %q", second, "/opt/first/podman")
	}
}

// TestBinPathFallsBackToLookPath proves that with no override set, BinPath
// resolves via exec.LookPath — verified here against a fake "podman"
// script placed on a PATH built just for this test, so it doesn't depend
// on a real podman binary being installed in CI.
func TestBinPathFallsBackToLookPath(t *testing.T) {
	resetBinPathForTest()
	t.Cleanup(resetBinPathForTest)

	dir := t.TempDir()
	fake := filepath.Join(dir, "podman")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(PodmanBinEnvVar, "")
	t.Setenv("PATH", dir)

	p, err := BinPath()
	if err != nil {
		t.Fatalf("BinPath: %v", err)
	}
	if p != fake {
		t.Fatalf("BinPath = %q, want %q", p, fake)
	}
}

// TestBinPathErrorsClearlyWhenNotFound proves BinPath returns an
// actionable error — naming the override env var — rather than silently
// falling back to the bare "podman" string, when neither the override nor
// $PATH resolves it.
func TestBinPathErrorsClearlyWhenNotFound(t *testing.T) {
	resetBinPathForTest()
	t.Cleanup(resetBinPathForTest)

	t.Setenv(PodmanBinEnvVar, "")
	t.Setenv("PATH", t.TempDir()) // empty directory: nothing resolves

	_, err := BinPath()
	if err == nil {
		t.Fatal("expected an error when podman isn't found on PATH and no override is set")
	}
	if !strings.Contains(err.Error(), PodmanBinEnvVar) {
		t.Fatalf("error %q does not mention %s", err.Error(), PodmanBinEnvVar)
	}
}
