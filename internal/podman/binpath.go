package podman

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// PodmanBinEnvVar is the environment variable that overrides BinPath's
// `podman` binary resolution — see BinPath's doc comment.
const PodmanBinEnvVar = "BASEPOD_PODMAN_BIN"

// binPathOnce, binPathVal, and binPathErr cache BinPath's result for the
// life of the process — see BinPath.
var (
	binPathOnce sync.Once
	binPathVal  string
	binPathErr  error
)

// BinPath resolves the absolute path to the `podman` binary once per
// process and caches the result (audit finding L3): every CLI shell-out
// this package and internal/caddy's PodmanExec make uses this instead of
// invoking the bare "podman" command name and trusting whatever $PATH
// happens to resolve it to at call time.
//
// Resolution order:
//  1. $BASEPOD_PODMAN_BIN, if set — an explicit operator override, used
//     verbatim (not re-validated via LookPath, so it also works for a
//     binary made executable but not itself on $PATH).
//  2. exec.LookPath("podman") — the normal case.
//
// If neither resolves, BinPath returns a clear, actionable error naming
// both what was tried and how to fix it; it never falls back to the bare
// "podman" string.
func BinPath() (string, error) {
	binPathOnce.Do(func() {
		if override := os.Getenv(PodmanBinEnvVar); override != "" {
			binPathVal = override
			return
		}
		p, err := exec.LookPath("podman")
		if err != nil {
			binPathErr = fmt.Errorf(
				"podman: could not locate the \"podman\" binary on $PATH (%w) — "+
					"install podman, or set %s to its absolute path",
				err, PodmanBinEnvVar,
			)
			return
		}
		binPathVal = p
	})
	return binPathVal, binPathErr
}

// resetBinPathForTest clears BinPath's cached result — test-only, since
// BinPath's sync.Once is deliberately process-lifetime in production (the
// resolved path can't meaningfully change while the process runs) but
// tests need to exercise different $BASEPOD_PODMAN_BIN / $PATH scenarios
// against a fresh resolution each time.
func resetBinPathForTest() {
	binPathOnce = sync.Once{}
	binPathVal = ""
	binPathErr = nil
}
