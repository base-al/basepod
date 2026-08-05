package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeFile creates path (and its parent dirs) under dir with contents.
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

// packAndList packs dir and returns the sorted list of entry names in the
// resulting tarball (directories keep their trailing "/").
func packAndList(t *testing.T, dir string) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := packDir(dir, &buf); err != nil {
		t.Fatalf("packDir: %v", err)
	}
	return listTarEntries(t, buf.Bytes())
}

func listTarEntries(t *testing.T, gzData []byte) []string {
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

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func TestHasContainerfile(t *testing.T) {
	t.Run("Containerfile at root", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "Containerfile", "FROM scratch\n")
		if !hasContainerfile(dir) {
			t.Fatal("want true")
		}
	})
	t.Run("Dockerfile at root", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "Dockerfile", "FROM scratch\n")
		if !hasContainerfile(dir) {
			t.Fatal("want true")
		}
	})
	t.Run("neither present", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "README.md", "hi\n")
		if hasContainerfile(dir) {
			t.Fatal("want false")
		}
	})
	t.Run("nested Dockerfile does not count", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "sub/Dockerfile", "FROM scratch\n")
		if hasContainerfile(dir) {
			t.Fatal("want false — Containerfile/Dockerfile must be at the root")
		}
	})
}

func TestPackDirAlwaysExcludesGitAndIgnoreFileItself(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, ".git/objects/pack/foo.pack", "binary\n")
	writeFile(t, dir, ".basepodignore", "*.log\n")
	writeFile(t, dir, "main.go", "package main\n")

	names := packAndList(t, dir)

	for _, n := range names {
		if n == ".git/" || strings.HasPrefix(n, ".git/") {
			t.Fatalf("names contains %q, .git must always be excluded: %v", n, names)
		}
	}
	if containsName(names, ".basepodignore") {
		t.Fatalf("names contains .basepodignore, it must always be excluded: %v", names)
	}
	if !containsName(names, "main.go") {
		t.Fatalf("names missing main.go: %v", names)
	}
	if !containsName(names, "Containerfile") {
		t.Fatalf("names missing Containerfile: %v", names)
	}
}

func TestPackDirIgnorePatternsMatrix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "keep.go", "package main\n")
	writeFile(t, dir, "debug.log", "log line\n")
	writeFile(t, dir, "sub/debug.log", "nested log\n")
	writeFile(t, dir, "node_modules/pkg/index.js", "module.exports = {}\n")
	writeFile(t, dir, "build/output.bin", "binary\n")
	writeFile(t, dir, "sub/build/output.bin", "nested binary, build/ is unanchored so this goes too\n")
	writeFile(t, dir, "vendor/lib.go", "package vendor\n")
	writeFile(t, dir, "keep/vendor-ish.go", "package keep\n") // must NOT be excluded by "vendor" glob (different name)
	writeFile(t, dir, ".env", "SECRET=1\n")
	writeFile(t, dir, "notes.txt", "# not a comment, a real file\n")

	writeFile(t, dir, ".basepodignore", strings.Join([]string{
		"# comment lines and blank lines are skipped",
		"",
		"*.log",         // unanchored glob — matches at any depth by base name
		"node_modules/", // unanchored dir suffix — matches this dir at any depth
		"/build/",       // anchored dir suffix — matches only at root
		"/vendor",       // anchored, non-dir — matches only the root-level literal path
		".env",          // unanchored literal (no glob chars) — exact base-name match
	}, "\n"))

	names := packAndList(t, dir)

	wantAbsent := []string{
		"debug.log", "sub/debug.log",
		"node_modules/", "node_modules/pkg/index.js",
		"build/", "build/output.bin",
		"vendor/", "vendor/lib.go",
		".env",
	}
	for _, n := range wantAbsent {
		if containsName(names, n) {
			t.Errorf("names contains %q, want excluded: %v", n, names)
		}
	}

	wantPresent := []string{
		"Containerfile", "keep.go", "notes.txt",
		"keep/", "keep/vendor-ish.go",
		"sub/", "sub/build/", "sub/build/output.bin", // /build/ is anchored to root, so sub/build/ survives
	}
	for _, n := range wantPresent {
		if !containsName(names, n) {
			t.Errorf("names missing %q, want present: %v", n, names)
		}
	}
}

func TestPackDirDeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "a.txt", "a\n")
	writeFile(t, dir, "sub/b.txt", "b\n")
	writeFile(t, dir, "sub/c.txt", "c\n")

	var buf1, buf2 bytes.Buffer
	if err := packDir(dir, &buf1); err != nil {
		t.Fatalf("packDir #1: %v", err)
	}
	if err := packDir(dir, &buf2); err != nil {
		t.Fatalf("packDir #2: %v", err)
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Fatal("two packDir runs over the same input produced different bytes, want identical (deterministic) output")
	}
}

func TestPackDirEntryOrderIsLexicallySorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "zzz.txt", "z\n")
	writeFile(t, dir, "aaa.txt", "a\n")
	writeFile(t, dir, "mmm/nested.txt", "m\n")

	var buf bytes.Buffer
	if err := packDir(dir, &buf); err != nil {
		t.Fatalf("packDir: %v", err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var order []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, hdr.Name)
	}
	if !sort.StringsAreSorted(order) {
		t.Fatalf("entries not in sorted order: %v", order)
	}
}

func TestPackToTempFileSizeAndReadability(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "app.py", "print('hi')\n")

	f, size, err := packToTempFile(dir)
	if err != nil {
		t.Fatalf("packToTempFile: %v", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Fatalf("reported size %d != actual file size %d", size, info.Size())
	}
	if size == 0 {
		t.Fatal("size == 0, want a non-empty tarball")
	}

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	names := listTarEntries(t, data)
	if !containsName(names, "app.py") {
		t.Fatalf("names missing app.py: %v", names)
	}
}
