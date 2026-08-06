package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"testing"
)

// linkEntry is one symlink/hardlink tar entry for the link-escape
// regression tests below — like tarEntry in build_test.go, but also
// carrying the tar.Header fields (Typeflag, Linkname) gzipTar's plain
// tarEntry helper doesn't need for its own (regular-file-only) fixtures.
type linkEntry struct {
	name      string
	typeflag  byte
	linkname  string
	isRegular bool // true for a plain file entry (Containerfile), false for a link
	body      string
}

// gzipTarWithLinks builds a gzip-compressed tar stream containing entries
// that may be plain files or symlink/hardlink entries — see linkEntry.
// Duplicated from gzipTar (build_test.go) rather than extending it,
// since this test file's job (audit finding M6, tar link-escape
// validation) is deliberately additive and self-contained given another
// agent is concurrently touching validateTar's manifest-parsing side in
// the same function.
func gzipTarWithLinks(t *testing.T, entries []linkEntry) *bytes.Buffer {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		if e.isRegular {
			hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     0o777,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &gzBuf
}

var containerfileEntry = linkEntry{name: "Containerfile", isRegular: true, body: "FROM alpine\n"}

// TestBuildRejectsAbsoluteSymlink proves a symlink entry whose Linkname is
// an absolute path (e.g. targeting the data dir's secret.key) is rejected
// before ever reaching podman — audit finding M6.
func TestBuildRejectsAbsoluteSymlink(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTarWithLinks(t, []linkEntry{
		containerfileEntry,
		{name: "evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, ErrBadPath) {
		t.Fatalf("err = %v, want ErrBadPath", err)
	}
	if rt.callCount() != 0 {
		t.Fatal("BuildImage should never have been called")
	}
}

// TestBuildRejectsRelativeSymlinkEscape proves a symlink entry whose
// Linkname, resolved relative to its own directory and cleaned, walks
// above the build context root (e.g. "../../etc/passwd") is rejected.
func TestBuildRejectsRelativeSymlinkEscape(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTarWithLinks(t, []linkEntry{
		containerfileEntry,
		{name: "evil-link", typeflag: tar.TypeSymlink, linkname: "../../etc/passwd"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, ErrBadPath) {
		t.Fatalf("err = %v, want ErrBadPath", err)
	}
	if rt.callCount() != 0 {
		t.Fatal("BuildImage should never have been called")
	}
}

// TestBuildRejectsHardlinkToAbsolutePath proves a TypeLink (hardlink)
// entry is checked exactly like a symlink: an absolute Linkname is
// rejected.
func TestBuildRejectsHardlinkToAbsolutePath(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTarWithLinks(t, []linkEntry{
		containerfileEntry,
		{name: "evil-hardlink", typeflag: tar.TypeLink, linkname: "/etc/shadow"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, ErrBadPath) {
		t.Fatalf("err = %v, want ErrBadPath", err)
	}
	if rt.callCount() != 0 {
		t.Fatal("BuildImage should never have been called")
	}
}

// TestBuildAcceptsInContextRelativeSymlink proves a relative symlink that
// stays inside the build context (e.g. the real-world
// "node_modules/.bin/x" -> "../pkg/bin.js" shape) is still allowed — the
// M6 fix must not break legitimate build contexts.
func TestBuildAcceptsInContextRelativeSymlink(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTarWithLinks(t, []linkEntry{
		containerfileEntry,
		{name: "node_modules/pkg/bin.js", isRegular: true, body: "#!/usr/bin/env node\n"},
		{name: "node_modules/.bin/x", typeflag: tar.TypeSymlink, linkname: "../pkg/bin.js"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if err != nil {
		t.Fatalf("Build: %v, want the in-context relative symlink to be accepted", err)
	}
	if rt.callCount() != 1 {
		t.Fatalf("callCount = %d, want 1 (BuildImage should have been called)", rt.callCount())
	}
}

// TestBuildAcceptsDeepButInsideSymlink proves a relative symlink that
// walks up several directories via ".." segments but still resolves
// inside the build context (rather than merely not containing ".." at
// all) is accepted.
func TestBuildAcceptsDeepButInsideSymlink(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTarWithLinks(t, []linkEntry{
		containerfileEntry,
		{name: "target.txt", isRegular: true, body: "hi\n"},
		// a/b/c/link -> ../../../target.txt resolves to "target.txt", still
		// inside the context despite three ".." segments.
		{name: "a/b/c/link", typeflag: tar.TypeSymlink, linkname: "../../../target.txt"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if err != nil {
		t.Fatalf("Build: %v, want the deep-but-inside symlink to be accepted", err)
	}
	if rt.callCount() != 1 {
		t.Fatalf("callCount = %d, want 1 (BuildImage should have been called)", rt.callCount())
	}
}
