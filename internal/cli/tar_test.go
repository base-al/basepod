package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/base-al/basepod/internal/tarpack"
)

// writeFile creates path (and its parent dirs) under dir with
// contents — a local copy of tarpack's own test helper, since that
// package's test file is not importable from here.
func writeFile(t *testing.T, dir, path, contents string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goldenTarListing(t *testing.T, gzData []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	return names
}

// TestPackDirWrapperMatchesTarpackGoldenListing proves internal/cli's
// packDir/packToTempFile/hasContainerfile (see tar.go) are pure
// pass-throughs to internal/tarpack (audit note from the v0.5 git+compose
// plan's Task 1: `basepod deploy`'s output must stay byte-identical
// across the packer's move out of this package) — packing the same
// fixture directory through both entry points must produce byte-for-byte
// identical tarballs and an identical, expected entry listing.
func TestPackDirWrapperMatchesTarpackGoldenListing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "main.go", "package main\n")
	writeFile(t, dir, "sub/nested.txt", "nested\n")
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, "debug.log", "log line\n")
	writeFile(t, dir, ".basepodignore", "*.log\n")

	wantListing := []string{
		"Containerfile", "main.go", "sub/", "sub/nested.txt",
	}

	var viaWrapper bytes.Buffer
	if err := packDir(dir, &viaWrapper); err != nil {
		t.Fatalf("cli.packDir: %v", err)
	}
	var viaTarpack bytes.Buffer
	if err := tarpack.PackDir(dir, &viaTarpack); err != nil {
		t.Fatalf("tarpack.PackDir: %v", err)
	}

	if !bytes.Equal(viaWrapper.Bytes(), viaTarpack.Bytes()) {
		t.Fatal("cli.packDir and tarpack.PackDir produced different bytes for the same input — the wrapper must be byte-identical")
	}

	got := goldenTarListing(t, viaWrapper.Bytes())
	if len(got) != len(wantListing) {
		t.Fatalf("entry listing = %v, want %v", got, wantListing)
	}
	for i := range wantListing {
		if got[i] != wantListing[i] {
			t.Fatalf("entry listing = %v, want %v", got, wantListing)
		}
	}

	if !hasContainerfile(dir) {
		t.Fatal("cli.hasContainerfile: want true for a fixture with a root Containerfile")
	}

	f, size, err := packToTempFile(dir)
	if err != nil {
		t.Fatalf("cli.packToTempFile: %v", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	if size == 0 {
		t.Fatal("cli.packToTempFile: size == 0, want a non-empty tarball")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, viaWrapper.Bytes()) {
		t.Fatal("cli.packToTempFile produced different bytes than cli.packDir for the same input")
	}
}
