package cli

import (
	"io"
	"os"

	"github.com/base-al/basepod/internal/tarpack"
)

// hasContainerfile, packDir, and packToTempFile are thin wrappers over
// internal/tarpack (see that package's doc comment): the packer itself
// moved there in v0.5 so the git cloner and compose's context repack can
// reuse it server-side, but `basepod deploy`'s call sites in commands.go
// keep these unexported names unchanged, and their behavior — including
// byte-for-byte tarball output — is identical to before the move (see
// TestPackDirWrapperMatchesTarpackGoldenListing).
func hasContainerfile(dir string) bool { return tarpack.HasContainerfile(dir) }

func packDir(dir string, w io.Writer) error { return tarpack.PackDir(dir, w) }

func packToTempFile(dir string) (f *os.File, size int64, err error) {
	return tarpack.PackToTempFile(dir)
}
