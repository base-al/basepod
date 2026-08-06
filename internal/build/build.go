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
	"bytes"
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

	"github.com/base-al/basepod/internal/manifest"
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

// ErrContextTooLarge is returned when an uploaded build context
// decompresses past Builder.maxDecompressedContext. The API layer's own
// upload cap (see maxTarballBody in internal/api) only bounds the
// *compressed* body size — gzip's compression ratio means a small
// compressed upload can still expand into an enormous decompressed one (a
// "gzip bomb"), which would otherwise fill the data directory's disk
// (shared with the SQLite store) well before that cap, or any request
// timeout, ever kicks in.
var ErrContextTooLarge = errors.New("build: decompressed build context exceeds the size limit")

// defaultMaxDecompressedContext is Builder's default
// maxDecompressedContext (see New): 1 GiB, four times maxTarballBody's
// 256 MiB compressed-upload cap — generous for any legitimate build
// context, while still bounding worst-case disk use from a hostile or
// buggy upload to a fixed, small multiple of the compressed cap rather
// than leaving it unbounded.
const defaultMaxDecompressedContext = 1 << 30 // 1 GiB

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

	// maxDecompressedContext bounds how many bytes spool will write from
	// a decompressed upload before giving up with ErrContextTooLarge (see
	// its doc comment). It's a field — defaulting to
	// defaultMaxDecompressedContext in New — rather than a bare constant
	// so tests can shrink it and exercise the limit without needing a
	// multi-hundred-MiB fixture.
	maxDecompressedContext int64

	// maxLogBytes bounds how many bytes of build output Build writes to a
	// build's log file before truncating it (see limitedLogWriter and
	// defaultMaxLogBytes's doc comment). It's a field — defaulting to
	// defaultMaxLogBytes in New — rather than a bare constant so tests can
	// shrink it and exercise the cap without needing a multi-hundred-MiB
	// fixture.
	maxLogBytes int64

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
		rt:                     rt,
		dataDir:                dataDir,
		maxDecompressedContext: defaultMaxDecompressedContext,
		maxLogBytes:            defaultMaxLogBytes,
		sem:                    make(chan struct{}, maxConcurrent),
		perApp:                 make(map[string]*sync.Mutex),
	}
}

// DataDir returns the data directory Builder was constructed with (see
// New) — the same one build logs and spool files live under. Exported
// solely so the API layer (internal/api/apps.go's delete handler) can
// derive an app's on-disk data directory without internal/deploy or
// internal/api needing their own separate copy of BasePod's data-dir
// configuration.
func (b *Builder) DataDir() string {
	return b.dataDir
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

// Result is BuildManifest's return value: everything a caller needs from
// a finished (or failed-after-the-log-existed) build, including the
// app's basepod.yaml manifest when the build context had one at its
// root.
type Result struct {
	// ImageTag is the local image tag Build produced, e.g.
	// "localhost/basepod/blog:3". Empty if the build never got far
	// enough to pick one (spooling or tar validation failed).
	ImageTag string
	// LogPath is where the build's log lives on disk. Populated whenever
	// the log file was created, even if the build itself then failed —
	// same contract as Build's logPath return.
	LogPath string
	// Manifest is the app's basepod.yaml/basepod.yml, parsed from the
	// build context's root, or nil if none was present (or it failed to
	// parse — see Warnings in that case).
	Manifest *manifest.Manifest
	// Warnings are non-fatal manifest issues (unknown keys, a manifest
	// that was present but too large or failed to parse, etc.) — see
	// validateTar's doc comment. BuildManifest also writes these to the
	// build log itself, so a caller isn't required to do anything with
	// this field for them to reach the user.
	Warnings []string
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
//
// Build is a thin, signature-preserving wrapper around BuildManifest —
// see that method for the same information plus the build context's
// parsed basepod.yaml manifest. Build exists because
// internal/deploy.Engine.runRollout (outside this package's ownership as
// of the manifest work — see BuildManifest's doc comment) already calls
// it with this exact 3-return signature; changing Build itself would
// break that call site.
func (b *Builder) Build(ctx context.Context, slug string, deploymentNumber int, gzTar io.Reader) (imageTag, logPath string, err error) {
	res, err := b.BuildManifest(ctx, slug, deploymentNumber, gzTar)
	return res.ImageTag, res.LogPath, err
}

// BuildManifest does everything Build does, and additionally parses a
// root-level basepod.yaml/basepod.yml from the build context (if
// present) and returns it as part of Result — see Result's doc comment.
// Any manifest warnings (unknown keys, a manifest that was ignored for
// being too large or unparseable) are written to the build log itself
// before the podman build starts, so they reach the user (the build
// log's SSE stream) even if nothing downstream of BuildManifest ever
// looks at Result.Warnings.
//
// internal/deploy.Engine.DeployBuild (internal/deploy/deploy.go) calls
// this — not Build — and applies Result.Manifest onto the app record via
// applyManifestDefaults/applyManifestEnvDefaults (resolving issue #1)
// before the rollout that follows builds its container spec, so a
// basepod.yaml in the upload takes effect starting with that very
// deploy.
func (b *Builder) BuildManifest(ctx context.Context, slug string, deploymentNumber int, gzTar io.Reader) (Result, error) {
	lock := b.appLock(slug)
	lock.Lock()
	defer lock.Unlock()

	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-b.sem }()

	spoolPath, err := b.spool(gzTar)
	if err != nil {
		return Result{}, err
	}
	// The spool file is a temp artifact this call created for its own use
	// (tar index validation needs to seek, which an io.Reader upload body
	// can't do) — removing it here is cleaning up after ourselves, not
	// touching anything a user owns.
	defer os.Remove(spoolPath)

	dockerfile, mf, warnings, err := validateTar(spoolPath)
	if err != nil {
		return Result{}, err
	}

	spool, err := os.Open(spoolPath)
	if err != nil {
		return Result{}, fmt.Errorf("build: reopen spool for build: %w", err)
	}
	defer spool.Close()

	logPath, logFile, err := b.createLog(slug, deploymentNumber)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()

	// Surface basepod.yaml warnings in the build log immediately — for a
	// zero-config deploy the build log's SSE stream is the user's first
	// (often only) feedback channel, well before any dashboard manifest
	// viewer exists.
	for _, w := range warnings {
		fmt.Fprintf(logFile, "warning: %s\n", w)
	}

	// Cap how much build output ever reaches disk (see limitedLogWriter):
	// the BUILD itself is unaffected by the cap — BuildImage's own stream
	// copy is oblivious to it — only the log stops growing once it's hit.
	sink := newLimitedLogWriter(logFile, b.maxLogBytes)

	imageTag := fmt.Sprintf("localhost/basepod/%s:%d", slug, deploymentNumber)
	if err := b.rt.BuildImage(ctx, imageTag, dockerfile, spool, sink); err != nil {
		// Matches Build's pre-existing contract: on a BuildImage failure
		// the tag is deliberately withheld (there is no usable image),
		// while LogPath/Manifest/Warnings are still returned since the
		// log file (and whatever it was told about the manifest) exists
		// regardless of whether the build itself succeeded.
		return Result{LogPath: logPath, Manifest: mf, Warnings: warnings}, err
	}
	return Result{ImageTag: imageTag, LogPath: logPath, Manifest: mf, Warnings: warnings}, nil
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
	// Read at most one byte past the limit: if the copy consumes the
	// full maxDecompressedContext+1 bytes available from limited, the
	// decompressed context is at least that large and gets rejected below
	// — without ever buffering (or writing to disk) more than
	// maxDecompressedContext+1 bytes of a hostile or buggy upload,
	// however large it claims (or turns out) to be.
	limited := io.LimitReader(gz, b.maxDecompressedContext+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("build: decompress upload: %w", err)
	}
	if n > b.maxDecompressedContext {
		os.Remove(path)
		return "", ErrContextTooLarge
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

// maxManifestSize bounds how many bytes of a root-level
// basepod.yaml/basepod.yml validateTar will read and attempt to parse. A
// manifest is meant to be a few dozen lines; one larger than this is
// almost certainly not a real config, so it's ignored (with a warning)
// rather than either failing the whole build over it or buffering an
// attacker-controlled blob into memory.
const maxManifestSize = 1 << 20 // 1 MiB

// validateTar scans the tar at path's index for a root-level
// Containerfile or Dockerfile (preferring Containerfile if both are
// present) and rejects any entry whose path is absolute or contains a
// ".." traversal segment (see ErrBadPath) — exactly as before the
// manifest work. It additionally looks for a root-level basepod.yaml or
// basepod.yml (basepod.yaml preferred if both exist; a manifest nested
// in a subdirectory is intentionally ignored, matching how a root-only
// Containerfile is required), reads its bytes in the same pass — the tar
// is already spooled to disk for this scan, so re-reading it later just
// to fetch one small entry would be wasted I/O — and parses it via
// manifest.Parse. A missing manifest is not an error (it's optional); an
// oversized or unparseable one is downgraded to a warning rather than
// failing the build, matching manifest.Parse's own "never let a manifest
// problem be the reason a zero-config deploy fails" philosophy.
func validateTar(path string) (dockerfile string, mf *manifest.Manifest, warnings []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, nil, fmt.Errorf("build: open spool for validation: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	found := map[string]bool{}
	var manifestName string
	var manifestBody []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, nil, fmt.Errorf("build: reading tar index: %w", err)
		}

		if !safeTarPath(hdr.Name) {
			return "", nil, nil, ErrBadPath
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			if !safeTarLink(hdr.Name, hdr.Linkname) {
				return "", nil, nil, ErrBadPath
			}
		}
		clean := filepath.ToSlash(filepath.Clean(hdr.Name))
		switch clean {
		case "Containerfile":
			found["Containerfile"] = true
		case "Dockerfile":
			found["Dockerfile"] = true
		case "basepod.yaml", "basepod.yml":
			// basepod.yaml wins if both are present, same "preferred name
			// wins" rule as Containerfile-vs-Dockerfile above.
			if manifestName == "basepod.yaml" {
				continue
			}
			if hdr.Size > maxManifestSize {
				warnings = append(warnings, fmt.Sprintf("%s is larger than %d bytes; ignoring", clean, maxManifestSize))
				continue
			}
			body, readErr := io.ReadAll(tr)
			if readErr != nil {
				warnings = append(warnings, fmt.Sprintf("%s: read failed (%v); ignoring", clean, readErr))
				continue
			}
			manifestName = clean
			manifestBody = body
		}
	}

	if manifestBody != nil {
		parsed, parseWarnings, parseErr := manifest.Parse(bytes.NewReader(manifestBody))
		if parseErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v; deploying without manifest defaults", manifestName, parseErr))
		} else {
			mf = parsed
			for _, w := range parseWarnings {
				warnings = append(warnings, fmt.Sprintf("%s: %s", manifestName, w))
			}
		}
	}

	switch {
	case found["Containerfile"]:
		return "Containerfile", mf, warnings, nil
	case found["Dockerfile"]:
		return "Dockerfile", mf, warnings, nil
	default:
		return "", nil, nil, ErrNoContainerfile
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

// safeTarLink reports whether a TypeSymlink/TypeLink tar entry's target is
// safe: an absolute Linkname is always rejected (audit finding M6 — e.g. a
// symlink into the data dir's secret.key), and a relative Linkname is
// rejected if, resolved against the entry's own directory within the
// build context and cleaned, it would escape the context root (a leading
// ".." segment after cleaning). Nothing is ever extracted host-side from
// this tar (see ErrBadPath's doc comment) — libpod's build endpoint is
// handed the validated stream — but validating here means BasePod never
// relies solely on containers/storage's own extraction hardening.
//
// A relative link that stays inside the context (e.g.
// "node_modules/.bin/x" -> "../pkg/bin.js") is legitimate in real build
// contexts and remains allowed — only linkname is checked here; hdr.Name
// itself is already validated by safeTarPath above.
func safeTarLink(name, linkname string) bool {
	if linkname == "" {
		return false
	}
	if filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") {
		return false
	}
	target := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), linkname)))
	if target == ".." || strings.HasPrefix(target, "../") {
		return false
	}
	return true
}
