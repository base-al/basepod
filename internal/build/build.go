// Package build orchestrates BasePod's tarball-upload build pipeline: it
// decompresses an uploaded gzipped tar, validates its contents are a safe
// build context (a Containerfile or Dockerfile at root, no path
// traversal), and drives a BuildRuntime (satisfied by *podman.Client) to
// build it into a local image tag while streaming build output to a log
// file on disk. It enforces both a global build-concurrency cap and
// per-app serialization, since a container build is CPU/IO-heavy enough
// that letting an unbounded number run at once (or two builds for the
// same app race each other's build log) would be a foot-gun.
package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/base-al/basepod/internal/podman"
)

// ErrNoContainerfile is returned when an uploaded build context has
// neither a Containerfile nor a Dockerfile at its root.
var ErrNoContainerfile = errors.New("build: no Containerfile or Dockerfile found at the root of the build context")

// ErrBadPath is returned when an uploaded build context contains a tar
// entry with an absolute path or a ".." path-traversal segment — either
// of which could, if extracted naively, write outside the intended build
// directory. libpod's own build endpoint may or may not guard against
// this itself, but BasePod validates the tar index before ever handing
// it to podman rather than relying on that.
var ErrBadPath = errors.New("build: tar contains an entry with an unsafe path")

// BuildRuntime is the podman capability the build pipeline needs
// (satisfied by *podman.Client; see the compile-time assertion in
// internal/server, which is the only place that constructs a real one).
// It's the seam tests substitute a fake for.
type BuildRuntime interface {
	BuildImage(ctx context.Context, tag, dockerfile string, contextTar io.Reader, logSink io.Writer) error
}

// Compile-time check that the production podman client satisfies the
// seam Builder consumes.
var _ BuildRuntime = (*podman.Client)(nil)

// Builder turns uploaded gzipped tar build contexts into local images.
type Builder struct {
	rt      BuildRuntime
	dataDir string

	// sem bounds the number of builds running concurrently across every
	// app (maxConcurrent, set by New).
	sem chan struct{}

	// mu guards perApp, the lazily-created set of per-app mutexes Build
	// locks around a whole build (spool, validate, run) to serialize
	// concurrent build requests for the *same* app slug — two builds for
	// one app racing each other would otherwise build in whatever order
	// the daemon happened to schedule them and clobber each other's spool
	// file naming and build log.
	mu     sync.Mutex
	perApp map[string]*sync.Mutex
}

// New builds a Builder. dataDir is BasePod's data directory (the same one
// passed to store.Open et al.): spool files live under
// <dataDir>/builds/, and build logs under
// <dataDir>/apps/<slug>/builds/<n>.log. maxConcurrent bounds the number
// of builds running at once across every app; values less than 1 are
// treated as 1.
func New(rt BuildRuntime, dataDir string, maxConcurrent int) *Builder {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Builder{
		rt:      rt,
		dataDir: dataDir,
		sem:     make(chan struct{}, maxConcurrent),
		perApp:  make(map[string]*sync.Mutex),
	}
}

// appLock returns the (lazily created) mutex serializing builds for slug.
func (b *Builder) appLock(slug string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.perApp[slug]
	if !ok {
		m = &sync.Mutex{}
		b.perApp[slug] = m
	}
	return m
}

// Build decompresses gzTar (a gzipped tar stream — the raw upload body),
// spools it to a temp file under <dataDir>/builds/ so its index can be
// validated and then rewound for the actual build, validates that
// context (see ErrNoContainerfile / ErrBadPath), and — if valid — builds
// it via BuildRuntime.BuildImage into localhost/basepod/<slug>:<n>,
// streaming build output to <dataDir>/apps/<slug>/builds/<n>.log
// (created, along with its parent directories, before the build starts).
//
// Only one Build call per slug runs at a time (a second concurrent call
// for the same app blocks until the first returns), and at most
// maxConcurrent (see New) run at once system-wide.
//
// logPath is returned (non-empty) whenever the log file was created, even
// if the build itself then fails — so a caller can still show the
// caller-facing error via the log file. It is "" only when Build fails
// before the log file exists (spooling or tar validation).
func (b *Builder) Build(ctx context.Context, slug string, deploymentNumber int, gzTar io.Reader) (imageTag, logPath string, err error) {
	lock := b.appLock(slug)
	lock.Lock()
	defer lock.Unlock()

	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	defer func() { <-b.sem }()

	spoolPath, err := b.spool(gzTar)
	if err != nil {
		return "", "", err
	}
	// The spool file is a temp artifact this call created for its own use
	// (tar index validation needs to seek, which an io.Reader upload body
	// can't do) — removing it here is cleaning up after ourselves, not
	// touching anything a user owns.
	defer os.Remove(spoolPath)

	dockerfile, err := validateTar(spoolPath)
	if err != nil {
		return "", "", err
	}

	spool, err := os.Open(spoolPath)
	if err != nil {
		return "", "", fmt.Errorf("build: reopen spool for build: %w", err)
	}
	defer spool.Close()

	logPath, logFile, err := b.createLog(slug, deploymentNumber)
	if err != nil {
		return "", "", err
	}
	defer logFile.Close()

	imageTag = fmt.Sprintf("localhost/basepod/%s:%d", slug, deploymentNumber)
	if err := b.rt.BuildImage(ctx, imageTag, dockerfile, spool, logFile); err != nil {
		return "", logPath, err
	}
	return imageTag, logPath, nil
}

// spool decompresses gzTar into a fresh temp file under <dataDir>/builds/
// and returns its path, ready to be reopened for reading. The caller owns
// removing it.
func (b *Builder) spool(gzTar io.Reader) (string, error) {
	spoolDir := filepath.Join(b.dataDir, "builds")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return "", fmt.Errorf("build: create spool dir: %w", err)
	}
	f, err := os.CreateTemp(spoolDir, "upload-*.tar")
	if err != nil {
		return "", fmt.Errorf("build: create spool file: %w", err)
	}
	defer f.Close()
	path := f.Name()

	gz, err := gzip.NewReader(gzTar)
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("build: upload is not valid gzip: %w", err)
	}
	if _, err := io.Copy(f, gz); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("build: decompress upload: %w", err)
	}
	// gzip.Reader.Close only checks the trailing checksum/size, which is
	// immaterial once every byte has already been copied out above — a
	// bad trailer must not fail an otherwise fully-decompressed upload.
	_ = gz.Close()

	return path, nil
}

// LogPath returns the path Build writes slug's deploymentNumber build log
// to. It's a pure path computation (no I/O, and safe to call before a
// build even starts) so a caller — deploy.Engine.DeployBuild — can record
// it on a deployment row immediately after creating it, making the log
// addressable (e.g. for live tailing while the build is still running)
// for the log's entire lifetime rather than only once Build returns.
func (b *Builder) LogPath(slug string, deploymentNumber int) string {
	return filepath.Join(b.dataDir, "apps", slug, "builds", strconv.Itoa(deploymentNumber)+".log")
}

// createLog creates (with parent directories) the build log file for
// slug's deploymentNumber and returns its path and an open, writable
// handle. The caller owns closing it.
func (b *Builder) createLog(slug string, deploymentNumber int) (string, *os.File, error) {
	logPath := b.LogPath(slug, deploymentNumber)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return "", nil, fmt.Errorf("build: create log dir: %w", err)
	}
	f, err := os.Create(logPath)
	if err != nil {
		return "", nil, fmt.Errorf("build: create log file: %w", err)
	}
	return logPath, f, nil
}

// validateTar scans the tar at path's index (headers only — file bodies
// are never read) for a root-level Containerfile or Dockerfile,
// preferring Containerfile if both are present, and rejects any entry
// whose path is absolute or contains a ".." traversal segment.
func validateTar(path string) (dockerfile string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("build: open spool for validation: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("build: reading tar index: %w", err)
		}

		if !safeTarPath(hdr.Name) {
			return "", ErrBadPath
		}
		switch filepath.ToSlash(filepath.Clean(hdr.Name)) {
		case "Containerfile":
			found["Containerfile"] = true
		case "Dockerfile":
			found["Dockerfile"] = true
		}
	}

	switch {
	case found["Containerfile"]:
		return "Containerfile", nil
	case found["Dockerfile"]:
		return "Dockerfile", nil
	default:
		return "", ErrNoContainerfile
	}
}

// safeTarPath reports whether a tar entry's name is safe to extract: not
// absolute, and containing no ".." path-traversal segment once cleaned.
func safeTarPath(name string) bool {
	if name == "" {
		return false
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	return true
}
