// Package gitsource shallow-clones a single branch of a git repository
// into a packed, gzipped tar build context — the git push-to-deploy
// counterpart to internal/tarpack packing a local directory or an
// uploaded tarball, feeding the exact same downstream build pipeline
// (Containerfile check, symlink rejection, manifest parsing, size caps
// all inherited for free — see the v0.5 git+compose plan's Task 3).
//
// Cloning shells out to the host `git` binary, resolved once by absolute
// path (see BinPath, mirroring internal/podman's BinPath) rather than
// trusting whatever bare "git" happens to resolve to on $PATH at call
// time. A private repo's deploy token is transported to git exclusively
// via a GIT_ASKPASS helper backed by the hidden `internal-git-askpass`
// subcommand (see Askpass, wired in cmd/basepod/main.go) that reads the
// token from its own process's environment — the token is never part of
// argv (world-readable via /proc for any process on the host, for as
// long as it runs) and never part of the clone URL or any on-disk git
// config.
package gitsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/base-al/basepod/internal/tarpack"
)

// GitBinEnvVar overrides BinPath's "git" binary resolution — mirrors
// internal/podman's BASEPOD_PODMAN_BIN (see podman.BinPath's doc
// comment) as the v0.5 plan's Task 3 directs.
const GitBinEnvVar = "BASEPOD_GIT_BIN"

// AllowHTTPEnvVar, when set to exactly "1", allows Fetch to clone a plain
// http:// URL — logged loudly on every use — for an operator's LAN Gitea
// instance with no TLS in front of it. https is otherwise the only
// scheme allowed in this version; ssh:// is deferred (https+token covers
// the forges' recommended CI/automation pattern — see the plan's
// self-review notes).
const AllowHTTPEnvVar = "BASEPOD_GIT_ALLOW_HTTP"

// GitTokenEnvVar is the environment variable the GIT_ASKPASS helper (see
// Askpass) reads the deploy token from. It is set only in the cloned
// git child process's own environment, for the duration of a single
// Fetch call — never in argv, never in the clone URL, never written to
// the checkout's .git/config.
const GitTokenEnvVar = "BASEPOD_GIT_TOKEN"

// AskpassUsername is what Askpass prints in response to a "Username for
// ..." askpass prompt — a fixed, non-secret placeholder, NEVER the
// token. This matters more than it looks: git embeds the just-supplied
// username back into the *next* prompt string it passes as an argv
// argument to the askpass helper it invokes for the password ("Password
// for 'https://<username>@host':" — verified empirically against git
// 2.50), and argv is readable by any other user on the host via /proc
// for that short-lived process. Answering the username prompt with the
// token itself would leak it into exactly the argv surface this whole
// askpass scheme exists to avoid.
const AskpassUsername = "basepod-git-deploy"

// BinPath resolves the absolute path to the "git" binary, mirroring
// internal/podman.BinPath's resolution order and error shape:
//
//  1. override (the server config's git_path setting), if non-empty —
//     used verbatim, not re-validated via LookPath.
//  2. $BASEPOD_GIT_BIN, if set.
//  3. exec.LookPath("git").
//
// Unlike podman.BinPath, this is not cached process-wide: internal/server
// calls it exactly once at boot and threads the result into New, so a
// package-level sync.Once cache would only add surface for tests to
// manage without buying anything in production.
func BinPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if v := os.Getenv(GitBinEnvVar); v != "" {
		return v, nil
	}
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf(
			"gitsource: could not locate the \"git\" binary on $PATH (%w) — "+
				"install git, set the config file's git_path, or set %s to its absolute path",
			err, GitBinEnvVar,
		)
	}
	return p, nil
}

// ErrGitUnavailable is returned by Fetch when the Cloner was constructed
// with no resolved git binary (see New) — internal/api maps this to a
// 503 "git_unavailable" response rather than attempting (and failing) an
// exec on every call.
var ErrGitUnavailable = errors.New("gitsource: git binary unavailable")

const (
	defaultTimeout      = 5 * time.Minute
	defaultMaxRepoBytes = 512 << 20 // 512 MiB
)

// Options configures a Cloner. A zero Options is not directly usable;
// New fills in DefaultOptions' values for any zero field.
type Options struct {
	// Timeout bounds the clone and HEAD-resolution exec calls (not the
	// subsequent size walk or packing, which are local disk operations
	// bounded by MaxRepoBytes rather than wall-clock time). Zero means
	// DefaultOptions' 5 minutes.
	Timeout time.Duration
	// MaxRepoBytes caps the checkout's total on-disk size, measured by
	// walking the working tree after the clone completes but before
	// .git is stripped (see Fetch). Zero means DefaultOptions' 512 MiB.
	MaxRepoBytes int64
	// AllowedSchemes is the clone URL scheme allowlist. Production wires
	// this to []string{"https"}; tests inject "file" to clone local
	// fixture repositories without a real remote. "http" is a special
	// case honored regardless of whether it appears here — see
	// AllowHTTPEnvVar.
	AllowedSchemes []string
}

// DefaultOptions returns the production defaults: a 5 minute timeout, a
// 512 MiB checkout size cap, and https as the only allowed scheme.
func DefaultOptions() Options {
	return Options{
		Timeout:        defaultTimeout,
		MaxRepoBytes:   defaultMaxRepoBytes,
		AllowedSchemes: []string{"https"},
	}
}

// Cloner shallow-clones a single branch of a git repository into a
// packed build context (see Fetch).
type Cloner struct {
	gitPath string
	opts    Options
}

// New returns a Cloner that shells out to gitPath (resolved once by the
// caller — see BinPath — typically at server boot, mirroring
// internal/podman's binpath approach). gitPath == "" is valid and means
// "git is unavailable": every Fetch call then returns ErrGitUnavailable
// immediately without attempting an exec, so internal/server can
// construct a Cloner unconditionally at boot and only warn — never fail
// — when git isn't installed; image/tarball deploys are unaffected
// either way. Zero-valued fields in opts fall back to DefaultOptions'
// values.
func New(gitPath string, opts Options) *Cloner {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxRepoBytes <= 0 {
		opts.MaxRepoBytes = defaultMaxRepoBytes
	}
	if len(opts.AllowedSchemes) == 0 {
		opts.AllowedSchemes = []string{"https"}
	}
	return &Cloner{gitPath: gitPath, opts: opts}
}

// Fetch shallow-clones branch of rawURL (depth 1, single branch, no
// tags), packs the resulting checkout into a gzipped tar (see
// internal/tarpack.PackToTempFile — .git is stripped first, so it never
// reaches the tar), and returns it open for reading, along with the
// cloned commit's full SHA. The caller must Close the returned
// io.ReadCloser, which also removes its backing temp file — the caller
// never needs to know its on-disk path.
//
// token, if non-empty, reaches git exclusively via a GIT_ASKPASS helper
// (see Askpass) invoked with the token available only in its own child
// process's environment (GitTokenEnvVar) — never as part of rawURL,
// never as a git command-line argument, and never persisted to the
// checkout's .git/config (which is stripped anyway). This is load-bearing:
// argv is readable by any other user on the host via /proc for the
// lifetime of the process, so a URL or argument containing embedded
// credentials would leak the moment it's handed to `git clone`.
func (c *Cloner) Fetch(ctx context.Context, rawURL, branch, token string) (gzTar io.ReadCloser, headSHA string, err error) {
	if c.gitPath == "" {
		return nil, "", ErrGitUnavailable
	}

	u, err := c.validateURL(rawURL)
	if err != nil {
		return nil, "", err
	}
	if err := validateBranch(branch); err != nil {
		return nil, "", err
	}

	root, err := os.MkdirTemp("", "basepod-gitclone-*")
	if err != nil {
		return nil, "", fmt.Errorf("gitsource: create temp dir: %w", err)
	}
	defer os.RemoveAll(root)

	homeDir := filepath.Join(root, "home")
	if err := os.Mkdir(homeDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("gitsource: create scratch HOME: %w", err)
	}
	checkoutDir := filepath.Join(root, "checkout")

	env, err := c.buildEnv(root, homeDir, token)
	if err != nil {
		return nil, "", err
	}

	cctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	cloneArgs := []string{
		"clone", "--depth", "1", "--single-branch", "--branch=" + branch, "--no-tags",
		u.String(), checkoutDir,
	}
	if err := c.runGit(cctx, env, cloneArgs, token); err != nil {
		return nil, "", err
	}

	headOut, err := c.runGitOutput(cctx, env, []string{"-C", checkoutDir, "rev-parse", "HEAD"}, token)
	if err != nil {
		return nil, "", err
	}
	headSHA = strings.TrimSpace(headOut)

	size, err := checkoutSize(checkoutDir)
	if err != nil {
		return nil, "", fmt.Errorf("gitsource: measure checkout size: %w", err)
	}
	if size > c.opts.MaxRepoBytes {
		return nil, "", fmt.Errorf(
			"gitsource: checkout is %d bytes, which exceeds the %d byte cap (MaxRepoBytes) — repository too large to deploy from git",
			size, c.opts.MaxRepoBytes,
		)
	}

	if err := os.RemoveAll(filepath.Join(checkoutDir, ".git")); err != nil {
		return nil, "", fmt.Errorf("gitsource: strip .git from checkout: %w", err)
	}

	tmpFile, _, err := tarpack.PackToTempFile(checkoutDir)
	if err != nil {
		return nil, "", fmt.Errorf("gitsource: pack checkout: %w", err)
	}

	return &selfDeletingFile{File: tmpFile}, headSHA, nil
}

// validateBranch rejects an empty branch, one that would be interpreted
// as a flag by git's argument parser (leading "-" — defense in depth on
// top of Fetch's "--branch=<branch>" form, which already disambiguates
// this), and one containing whitespace or NUL, which no real branch name
// contains.
func validateBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("gitsource: branch must not be empty")
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("gitsource: invalid branch %q", branch)
	}
	if strings.ContainsAny(branch, " \t\n\r\x00") {
		return fmt.Errorf("gitsource: invalid branch %q", branch)
	}
	return nil
}

// validateURL enforces the scheme allowlist and rejects anything that
// isn't a plausible git URL: it must parse, have a scheme, and (unless
// that scheme is "file", used only by tests against local fixture
// repos) a non-empty host. Embedded userinfo (a "user:pass@" or
// "token@" prefix) is rejected outright — not because it couldn't
// technically be used for auth, but because doing so would put a
// credential directly into the string handed to `git clone` as a
// command-line argument, defeating the whole point of the GIT_ASKPASS
// transport this package uses instead (see Fetch's doc comment).
func (c *Cloner) validateURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("gitsource: invalid git URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return nil, fmt.Errorf("gitsource: git URL %q has no scheme", rawURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("gitsource: git URL must not embed credentials — pass the deploy token separately")
	}
	if scheme != "file" && u.Host == "" {
		return nil, fmt.Errorf("gitsource: git URL %q has no host", rawURL)
	}

	for _, allowed := range c.opts.AllowedSchemes {
		if scheme == strings.ToLower(allowed) {
			return u, nil
		}
	}
	if scheme == "http" && os.Getenv(AllowHTTPEnvVar) == "1" {
		log.Printf(
			"gitsource: WARNING cloning a plain http:// URL (%s=1) — this connection, and any deploy token sent over it, is unencrypted: %s",
			AllowHTTPEnvVar, u.String(),
		)
		return u, nil
	}
	return nil, fmt.Errorf(
		"gitsource: scheme %q not allowed (allowed: %v) — ssh:// is not supported by this version, see the README",
		scheme, c.opts.AllowedSchemes,
	)
}

// buildEnv assembles the environment the git child process runs under:
// a minimal allowlisted subset of the host's own environment (just
// enough for git to function — not the full os.Environ(), so nothing
// else the basepod server process happens to carry is handed to a git
// child), a scratch HOME/XDG_CONFIG_HOME under root so the operator's own
// ~/.gitconfig (with whatever credential helpers or url rewrites it
// might carry) is never consulted, GIT_TERMINAL_PROMPT=0 (never fall
// back to an interactive prompt), GIT_CONFIG_NOSYSTEM=1, and — the
// credential transport itself — GIT_ASKPASS pointing at a freshly
// written wrapper script (see Askpass) that re-execs this same basepod
// binary's hidden `internal-git-askpass` subcommand. If token is
// non-empty, GitTokenEnvVar is also set, read only by that subcommand.
func (c *Cloner) buildEnv(root, homeDir, token string) ([]string, error) {
	selfPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("gitsource: resolve own executable path for GIT_ASKPASS: %w", err)
	}

	askpassPath := filepath.Join(root, "askpass.sh")
	script := "#!/bin/sh\nexec " + shellQuote(selfPath) + " internal-git-askpass \"$@\"\n"
	if err := os.WriteFile(askpassPath, []byte(script), 0o700); err != nil {
		return nil, fmt.Errorf("gitsource: write askpass helper: %w", err)
	}

	env := filteredHostEnv()
	env = append(env,
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+homeDir,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ASKPASS="+askpassPath,
	)
	if token != "" {
		env = append(env, GitTokenEnvVar+"="+token)
	}
	return env, nil
}

// filteredHostEnv returns the small, explicit subset of the basepod
// server process's own environment that a git child process actually
// needs (PATH, to find any helper binaries it shells out to; the TLS
// trust-store overrides, if an operator has set them) — see buildEnv's
// doc comment for why this is deliberately not the full os.Environ().
func filteredHostEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// shellQuote single-quotes s for safe interpolation into the /bin/sh
// askpass wrapper script buildEnv writes, escaping any embedded single
// quote — defensive, since selfPath (os.Executable()'s result) could in
// principle contain a space or other shell metacharacter depending on
// where the operator installed the binary.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runGit runs git with args and env, discarding stdout, under ctx.
func (c *Cloner) runGit(ctx context.Context, env, args []string, token string) error {
	cmd := exec.CommandContext(ctx, c.gitPath, args...)
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return c.wrapGitError(ctx, err, stderr.String(), token)
	}
	return nil
}

// runGitOutput runs git with args and env under ctx, returning its
// captured stdout.
func (c *Cloner) runGitOutput(ctx context.Context, env, args []string, token string) (string, error) {
	cmd := exec.CommandContext(ctx, c.gitPath, args...)
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", c.wrapGitError(ctx, err, stderr.String(), token)
	}
	return stdout.String(), nil
}

// wrapGitError turns a failed git invocation into an error naming what
// happened: a timeout if ctx expired, otherwise git's own (sanitized —
// see sanitize) stderr output, or runErr's message if stderr was empty.
func (c *Cloner) wrapGitError(ctx context.Context, runErr error, stderr, token string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("gitsource: git operation timed out after %s: %w", c.opts.Timeout, ctx.Err())
	}
	msg := sanitize(strings.TrimSpace(stderr), token)
	if msg == "" {
		msg = sanitize(runErr.Error(), token)
	}
	return fmt.Errorf("gitsource: git failed: %s", msg)
}

// sanitize strips every occurrence of token from msg. By construction,
// token is never passed to git as an argument or URL component (see
// Fetch's doc comment), so git's own stderr should never contain it in
// the first place — this is defense in depth against some future git
// behavior change (e.g. echoing back a malformed credential response)
// leaking it into an error string a caller might log or return to a
// client.
func sanitize(msg, token string) string {
	if token == "" || msg == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[REDACTED]")
}

// checkoutSize returns the total size, in bytes, of every regular file
// under dir (symlinks are not followed — see internal/tarpack's own
// symlink handling for why). Called before .git is stripped, so a large
// object database counts against MaxRepoBytes just as much as a large
// working tree would.
func checkoutSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// Askpass implements the hidden `internal-git-askpass` subcommand's
// logic (wired in cmd/basepod/main.go): given the single prompt argument
// a GIT_ASKPASS invocation receives, return what the helper should print
// to stdout. A prompt naming "password" (case-insensitive — git's own
// prompts are "Username for '<url>': " and "Password for '<url>': ")
// gets $BASEPOD_GIT_TOKEN; anything else gets AskpassUsername, a fixed
// non-secret placeholder — see AskpassUsername's doc comment for why the
// token must never be used as the answer to a username prompt.
func Askpass(prompt string) string {
	if strings.Contains(strings.ToLower(prompt), "password") {
		return os.Getenv(GitTokenEnvVar)
	}
	return AskpassUsername
}

// selfDeletingFile wraps an *os.File so Close both closes the underlying
// file descriptor and removes the file from disk — Fetch's caller
// shouldn't need to know the packed tarball lives at a temp path in
// order to clean it up (mirrors how internal/tarpack.PackToTempFile
// leaves cleanup to its caller, except here the caller only ever sees an
// io.ReadCloser, not an *os.File, so it has no Name() to remove itself).
type selfDeletingFile struct {
	*os.File
}

func (f *selfDeletingFile) Close() error {
	closeErr := f.File.Close()
	removeErr := os.Remove(f.File.Name())
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
