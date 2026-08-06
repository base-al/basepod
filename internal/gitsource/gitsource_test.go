package gitsource_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/gitsource"
)

// requireGit skips the test if no "git" binary is on $PATH — every CI
// runner and dev machine this suite is expected to run on has one (see
// the v0.5 git+compose plan's Task 3), but skipping rather than failing
// keeps this suite portable to an unusual environment that genuinely
// lacks it.
func requireGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found on $PATH, skipping")
	}
	return p
}

// runFixtureGit runs a real git command inside dir, failing the test on
// any error.
func runFixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_DATE=2024-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2024-01-01T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFixtureFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFixtureRepo creates a local git repository with two branches
// carrying deliberately disjoint content ("main" has on-main.txt only,
// "other" is an orphan branch with on-other.txt only), so a clone that
// picks up the wrong branch's content is easy to detect.
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	runFixtureGit(t, dir, "init", "--initial-branch=main")
	runFixtureGit(t, dir, "config", "user.email", "test@example.com")
	runFixtureGit(t, dir, "config", "user.name", "Test")
	writeFixtureFile(t, dir, "on-main.txt", "main content\n")
	runFixtureGit(t, dir, "add", ".")
	runFixtureGit(t, dir, "commit", "-m", "main commit")

	runFixtureGit(t, dir, "checkout", "--orphan", "other")
	runFixtureGit(t, dir, "rm", "-rf", ".")
	writeFixtureFile(t, dir, "on-other.txt", "other content\n")
	runFixtureGit(t, dir, "add", ".")
	runFixtureGit(t, dir, "commit", "-m", "other commit")

	runFixtureGit(t, dir, "checkout", "main")
	return dir
}

// fileURL turns an absolute filesystem path into a file:// URL —
// gitsource.Options.AllowedSchemes injects "file" in every test below so
// Fetch can clone these local fixtures without a real remote.
func fileURL(dir string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dir)}
	return u.String()
}

// tarNames gunzips and untars r, returning the sorted list of entry
// names.
func tarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	gz, err := gzip.NewReader(r)
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

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func testOptions(schemes ...string) gitsource.Options {
	return gitsource.Options{
		Timeout:        15 * time.Second,
		MaxRepoBytes:   10 << 20,
		AllowedSchemes: schemes,
	}
}

// TestFetchClonesSelectedBranchOnly proves Fetch checks out exactly the
// requested branch's content (not some other branch, not the source
// repo's currently-checked-out HEAD), strips .git, and returns a
// non-empty commit SHA.
func TestFetchClonesSelectedBranchOnly(t *testing.T) {
	src := newFixtureRepo(t)
	c := gitsource.New(requireGit(t), testOptions("file"))

	rc, headSHA, err := c.Fetch(context.Background(), fileURL(src), "other", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()

	if headSHA == "" {
		t.Fatal("headSHA is empty, want a commit SHA")
	}
	if strings.ContainsAny(headSHA, " \n\t") {
		t.Fatalf("headSHA contains whitespace, want a trimmed SHA: %q", headSHA)
	}

	names := tarNames(t, rc)
	if !contains(names, "on-other.txt") {
		t.Fatalf("missing on-other.txt (the 'other' branch's file): %v", names)
	}
	if contains(names, "on-main.txt") {
		t.Fatalf("contains on-main.txt — cloned the wrong branch's content: %v", names)
	}
	for _, n := range names {
		if n == ".git" || strings.HasPrefix(n, ".git/") {
			t.Fatalf(".git leaked into the packed tar: %v", names)
		}
	}
}

// TestFetchClosingReadCloserRemovesTempFile proves the returned
// io.ReadCloser cleans up its own backing temp file on Close, so the
// caller (a future deploy handler) never needs to know its on-disk path.
func TestFetchClosingReadCloserRemovesTempFile(t *testing.T) {
	src := newFixtureRepo(t)
	c := gitsource.New(requireGit(t), testOptions("file"))

	rc, _, err := c.Fetch(context.Background(), fileURL(src), "main", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	f, ok := rc.(interface{ Name() string })
	if !ok {
		t.Fatal("returned io.ReadCloser doesn't expose Name() — test can't verify temp file cleanup")
	}
	path := f.Name()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("temp file %s missing before Close: %v", path, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file %s still exists after Close (err=%v), want removed", path, err)
	}
}

// TestFetchMissingBranchErrorIsSanitized proves a nonexistent branch
// surfaces git's own error, with the deploy token nowhere in it — this
// fails without the sanitize() guard if git's stderr (or the error
// wrapping) ever echoed back anything token-bearing.
func TestFetchMissingBranchErrorIsSanitized(t *testing.T) {
	src := newFixtureRepo(t)
	c := gitsource.New(requireGit(t), testOptions("file"))

	const token = "leaked-token-should-never-appear-in-errors"
	_, _, err := c.Fetch(context.Background(), fileURL(src), "does-not-exist", token)
	if err == nil {
		t.Fatal("expected an error for a nonexistent branch")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error message leaked the token: %v", err)
	}
}

// TestFetchRejectsCheckoutOverSizeCap proves MaxRepoBytes is enforced —
// a repo whose checkout exceeds the cap is rejected with a clear error
// naming it, and this fails without the checkoutSize() guard.
func TestFetchRejectsCheckoutOverSizeCap(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	runFixtureGit(t, dir, "init", "--initial-branch=main")
	runFixtureGit(t, dir, "config", "user.email", "test@example.com")
	runFixtureGit(t, dir, "config", "user.name", "Test")
	big := bytes.Repeat([]byte("x"), 200*1024) // 200 KiB, well over the cap below
	writeFixtureFile(t, dir, "big.bin", string(big))
	runFixtureGit(t, dir, "add", ".")
	runFixtureGit(t, dir, "commit", "-m", "big file")

	opts := testOptions("file")
	opts.MaxRepoBytes = 50 * 1024 // 50 KiB cap
	c := gitsource.New(requireGit(t), opts)

	_, _, err := c.Fetch(context.Background(), fileURL(dir), "main", "")
	if err == nil {
		t.Fatal("expected a size-cap rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error doesn't name the size cap: %v", err)
	}
}

// TestFetchTimeoutKillsChild proves a clone that runs past Options.Timeout
// is not just abandoned by Wait() but actually terminated — both the
// direct "git" process AND a subprocess it spawns (standing in for
// git's own transport helpers, e.g. git-remote-https, which inherit its
// process group).
//
// This is deliberately not just a wall-clock assertion: a grandchild
// that inherits the Cmd's stdout/stderr pipes keeps them open even
// after the direct child dies, which blocks Wait() until the grandchild
// exits on its own — a naive fix (e.g. only killing cmd.Process, or
// relying solely on a long WaitDelay) can make Fetch *return* promptly
// while leaving that grandchild running in the background, still
// racing to finish whatever it was doing. So this test checks both:
// Fetch returns within a small bound, AND the grandchild's own PID is
// confirmed dead afterward — the property that actually matters (no
// leaked process, no lingering background work), which a timing-only
// assertion cannot distinguish from "Wait() gave up but the process
// lives on".
func TestFetchTimeoutKillsChild(t *testing.T) {
	scriptDir := t.TempDir()
	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, "ran-to-completion")
	pidPath := filepath.Join(workDir, "child.pid")

	// Backgrounds a long sleep (well past the timeout below) as its own
	// child process, records that child's PID, then waits on it — so the
	// PID this test inspects belongs to a genuine grandchild of the Go
	// test process, exactly the shape that leaked pipe file descriptors
	// in the bug this test guards against.
	fakeGit := writeFakeGit(t, scriptDir,
		"#!/bin/sh\n"+
			"sleep 5 &\n"+
			"child=$!\n"+
			"echo $child > "+shQuote(pidPath)+"\n"+
			"wait $child\n"+
			"touch "+shQuote(markerPath)+"\n")

	opts := testOptions("file")
	// Long enough for the shell to actually fork+exec `sleep 5 &` and
	// write the pid file before being killed (a too-short timeout risks
	// killing the script before it gets that far, which would fail this
	// test for the wrong reason); short enough that the assertion below
	// still meaningfully proves the grandchild didn't run anywhere near
	// its full 5s.
	opts.Timeout = 300 * time.Millisecond
	c := gitsource.New(fakeGit, opts)

	start := time.Now()
	_, _, err := c.Fetch(context.Background(), "file:///nonexistent/repo", "main", "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	// A tight bound: the fake git's sleep is 5s and WaitDelay's backstop
	// is 3s, so anything approaching either of those means the
	// grandchild was left running rather than actually killed at the
	// ~300ms deadline.
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("Fetch took %s to return, want well under 1.5s — the process group was not killed promptly on timeout", elapsed)
	}

	pidBytes, rerr := os.ReadFile(pidPath)
	if rerr != nil {
		t.Fatalf("fake git never recorded its background child's PID: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if perr != nil {
		t.Fatalf("bad pid file contents %q: %v", pidBytes, perr)
	}

	// Poll briefly rather than assert instantaneously: the kill signal
	// and the OS reaping it are asynchronous with Fetch's return.
	deadline := time.Now().Add(1 * time.Second)
	alive := processAlive(pid)
	for alive && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		alive = processAlive(pid)
	}
	if alive {
		t.Fatalf("fake git's background child (pid %d) is still running after Fetch returned — "+
			"it was abandoned, not killed (see newGitCmd's process-group kill)", pid)
	}

	if _, statErr := os.Stat(markerPath); statErr == nil {
		t.Fatal("marker file exists — the fake git script ran to completion instead of being killed on timeout")
	}
}

// processAlive reports whether pid refers to a still-running process, by
// sending it signal 0 (a no-op existence probe — POSIX-portable, unlike
// reading /proc which isn't available on macOS).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestFetchNeverPutsTokenInArgv proves the deploy token reaches git only
// via the environment (for the GIT_ASKPASS helper to read), never as a
// command-line argument — the property the whole GIT_ASKPASS transport
// exists for. Uses a fake "git" that dumps its own argv and the
// GIT_TOKEN env var it sees to a log file, then asserts the token
// appears in the env dump (proving the transport actually works) but
// never in the argv dump (the security property).
func TestFetchNeverPutsTokenInArgv(t *testing.T) {
	scriptDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "invocations.log")
	fakeGit := writeFakeGit(t, scriptDir,
		"#!/bin/sh\n"+
			"{ echo \"ARGV:$*\"; echo \"ENV_TOKEN:${BASEPOD_GIT_TOKEN:-<unset>}\"; } >> "+shQuote(logPath)+"\n"+
			"exit 1\n")

	const token = "super-secret-deploy-token-xyz-123"
	c := gitsource.New(fakeGit, testOptions("https"))

	_, _, err := c.Fetch(context.Background(), "https://example.com/org/repo.git", "main", token)
	if err == nil {
		t.Fatal("expected an error since the fake git always exits 1")
	}

	data, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("fake git was apparently never invoked: %v", rerr)
	}
	log := string(data)

	var argvLines []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "ARGV:") {
			argvLines = append(argvLines, line)
		}
	}
	if len(argvLines) == 0 {
		t.Fatalf("fake git logged no ARGV lines at all:\n%s", log)
	}
	for _, line := range argvLines {
		if strings.Contains(line, token) {
			t.Fatalf("token leaked into git's argv:\n%s", log)
		}
	}
	if !strings.Contains(argvLines[0], "clone") {
		t.Fatalf("first invocation wasn't `git clone ...` as expected:\n%s", log)
	}
	if !strings.Contains(log, "ENV_TOKEN:"+token) {
		t.Fatalf("token was not passed through the environment as documented:\n%s", log)
	}
}

// TestFetchRejectsDisallowedScheme proves an scheme outside
// AllowedSchemes (ssh, in this case — deferred entirely in this version)
// is rejected before any exec is attempted.
func TestFetchRejectsDisallowedScheme(t *testing.T) {
	c := gitsource.New("/does/not/matter", testOptions("https"))
	_, _, err := c.Fetch(context.Background(), "ssh://git@example.com/org/repo.git", "main", "")
	if err == nil {
		t.Fatal("expected scheme rejection for ssh://")
	}
}

// TestFetchRejectsPlainHTTPWithoutEnvOverride and
// TestFetchAllowsPlainHTTPWithEnvOverride together prove the
// BASEPOD_GIT_ALLOW_HTTP escape hatch's exact behavior: http:// is
// rejected by default, and only accepted (scheme validation passes —
// the fake git still runs and fails, proving we got past validation)
// once the env var is set.
func TestFetchRejectsPlainHTTPWithoutEnvOverride(t *testing.T) {
	c := gitsource.New("/does/not/matter", testOptions("https"))
	_, _, err := c.Fetch(context.Background(), "http://example.com/org/repo.git", "main", "")
	if err == nil {
		t.Fatal("expected http:// rejection without BASEPOD_GIT_ALLOW_HTTP=1")
	}
}

func TestFetchAllowsPlainHTTPWithEnvOverride(t *testing.T) {
	t.Setenv(gitsource.AllowHTTPEnvVar, "1")
	scriptDir := t.TempDir()
	fakeGit := writeFakeGit(t, scriptDir, "#!/bin/sh\nexit 7\n")
	c := gitsource.New(fakeGit, testOptions("https"))

	_, _, err := c.Fetch(context.Background(), "http://example.com/org/repo.git", "main", "")
	// Scheme validation must have passed (the fake git ran and exited 7,
	// which surfaces as *some* error) — we're only asserting it's not the
	// scheme-rejection error, by asserting an exec was actually attempted.
	if err == nil {
		t.Fatal("expected the fake git's failure to surface as an error")
	}
	if strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("http:// was still rejected as a disallowed scheme despite %s=1: %v", gitsource.AllowHTTPEnvVar, err)
	}
}

// TestFetchRejectsEmbeddedCredentialsInURL proves a URL with a
// "user:pass@" or "token@" prefix is rejected outright — allowing it
// through would put a credential directly into git's argv (the clone URL
// is a command-line argument), defeating the GIT_ASKPASS transport.
func TestFetchRejectsEmbeddedCredentialsInURL(t *testing.T) {
	c := gitsource.New("/does/not/matter", testOptions("https"))
	_, _, err := c.Fetch(context.Background(), "https://sometoken@example.com/org/repo.git", "main", "")
	if err == nil {
		t.Fatal("expected rejection of a URL with embedded credentials")
	}
}

// TestFetchRejectsURLWithNoHost and TestFetchRejectsUnparseableURL round
// out the "reject anything that isn't a plausible git URL" requirement.
func TestFetchRejectsURLWithNoHost(t *testing.T) {
	c := gitsource.New("/does/not/matter", testOptions("https"))
	_, _, err := c.Fetch(context.Background(), "https:///org/repo.git", "main", "")
	if err == nil {
		t.Fatal("expected rejection of a URL with no host")
	}
}

func TestFetchRejectsUnparseableURL(t *testing.T) {
	c := gitsource.New("/does/not/matter", testOptions("https"))
	_, _, err := c.Fetch(context.Background(), "https://%zz", "main", "")
	if err == nil {
		t.Fatal("expected rejection of an unparseable URL")
	}
}

// TestFetchReturnsErrGitUnavailableWhenGitPathEmpty proves a Cloner
// constructed with no resolved git binary fails fast and typed, without
// attempting an exec.
func TestFetchReturnsErrGitUnavailableWhenGitPathEmpty(t *testing.T) {
	c := gitsource.New("", gitsource.DefaultOptions())
	_, _, err := c.Fetch(context.Background(), "https://example.com/org/repo.git", "main", "")
	if !errors.Is(err, gitsource.ErrGitUnavailable) {
		t.Fatalf("got %v, want ErrGitUnavailable", err)
	}
}

// TestFetchRejectsEmptyAndFlagLikeBranch guards against argument
// injection via a branch name starting with "-".
func TestFetchRejectsEmptyAndFlagLikeBranch(t *testing.T) {
	c := gitsource.New("/does/not/matter", testOptions("https"))
	for _, branch := range []string{"", "-x", "--upload-pack=/bin/sh"} {
		_, _, err := c.Fetch(context.Background(), "https://example.com/org/repo.git", branch, "")
		if err == nil {
			t.Fatalf("branch %q: expected rejection", branch)
		}
	}
}

// TestAskpassPasswordPromptReturnsToken and
// TestAskpassUsernamePromptReturnsPlaceholderNotToken together prove
// Askpass's prompt-dependent behavior, which is what the hidden
// `internal-git-askpass` subcommand (cmd/basepod/main.go) delegates to:
// the token only ever answers a password prompt, never a username one.
func TestAskpassPasswordPromptReturnsToken(t *testing.T) {
	t.Setenv(gitsource.GitTokenEnvVar, "secret-token-abc")
	got := gitsource.Askpass("Password for 'https://example.com': ")
	if got != "secret-token-abc" {
		t.Fatalf("got %q, want the token verbatim and nothing else", got)
	}
}

func TestAskpassUsernamePromptReturnsPlaceholderNotToken(t *testing.T) {
	t.Setenv(gitsource.GitTokenEnvVar, "secret-token-abc")
	got := gitsource.Askpass("Username for 'https://example.com': ")
	if got != gitsource.AskpassUsername {
		t.Fatalf("got %q, want the fixed placeholder %q", got, gitsource.AskpassUsername)
	}
	if strings.Contains(got, "secret-token-abc") {
		t.Fatal("username prompt response must never contain the token")
	}
}

func TestAskpassNoTokenSetReturnsEmptyForPasswordPrompt(t *testing.T) {
	got := gitsource.Askpass("Password for 'https://example.com': ")
	if got != "" {
		t.Fatalf("got %q, want empty when no token env var is set", got)
	}
}

// TestBinPathOverride, TestBinPathEnvVar, and TestBinPathLooksUpPATH
// together prove BinPath's three-level resolution order.
func TestBinPathOverride(t *testing.T) {
	got, err := gitsource.BinPath("/custom/path/to/git")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/path/to/git" {
		t.Fatalf("got %q, want the override used verbatim", got)
	}
}

func TestBinPathEnvVar(t *testing.T) {
	t.Setenv(gitsource.GitBinEnvVar, "/env/path/to/git")
	got, err := gitsource.BinPath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/env/path/to/git" {
		t.Fatalf("got %q, want the env var value", got)
	}
}

func TestBinPathLooksUpPATH(t *testing.T) {
	requireGit(t)
	got, err := gitsource.BinPath("")
	if err != nil {
		t.Fatalf("BinPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("got %q, want an absolute path", got)
	}
}

// writeFakeGit writes an executable shell script at dir/git and returns
// its path, standing in for the real git binary in tests that need to
// observe or control git's own invocation (argv, environment, timing)
// rather than exercise a real clone.
func writeFakeGit(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// shQuote single-quotes s for safe interpolation into a /bin/sh script
// body written out by a test.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
