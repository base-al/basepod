package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// newTestRoot builds a fresh basepod-CLI command tree, mirroring how
// cmd/basepod/main.go wires cli.Commands() under its own root — a fresh
// tree per test so cobra's per-command flag state (e.g. login's --email)
// never leaks between tests.
func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "basepod"}
	root.AddCommand(Commands()...)
	return root
}

// runCLI executes args against a fresh command tree, returning captured
// stdout/stderr. stdin is fed verbatim for commands that prompt (login).
func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newTestRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// setTestConfigPath points BASEPOD_CLI_CONFIG at a fresh temp file for the
// duration of the test, isolating it from both the real user config and
// other tests — this is the "inject the config path via env var" factory
// the task brief asks for.
func setTestConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.yaml")
	t.Setenv(configPathEnv, path)
	return path
}

func requireBearer(t *testing.T, r *http.Request, token string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer "+token)
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func saveTestContext(t *testing.T, path, url, token string) {
	t.Helper()
	if err := SaveConfigTo(path, &Config{
		Contexts: map[string]Context{"t": {URL: url, Token: token}},
		Current:  "t",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoginSavesContext(t *testing.T) {
	setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/login" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["email"] != "a@b.com" || body["password"] != "hunter2" {
			t.Fatalf("body = %v", body)
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"token": "tok-123",
			"user":  map[string]string{"email": "a@b.com", "name": "Ada"},
		})
	}))
	defer srv.Close()

	out, _, err := runCLI(t, "hunter2\n", "login", srv.URL, "--email", "a@b.com")
	if err != nil {
		t.Fatalf("login: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "Logged in as a@b.com") {
		t.Fatalf("output = %q", out)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantName := contextNameFromURL(srv.URL)
	if cfg.Current != wantName {
		t.Fatalf("Current = %q, want %q", cfg.Current, wantName)
	}
	got := cfg.Contexts[wantName]
	if got.Token != "tok-123" || got.URL != srv.URL {
		t.Fatalf("context = %+v", got)
	}
}

func TestLoginPromptsForMissingCredentials(t *testing.T) {
	setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["email"] != "prompted@example.com" || body["password"] != "secret" {
			t.Fatalf("body = %v", body)
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"token": "tok-xyz",
			"user":  map[string]string{"email": "prompted@example.com"},
		})
	}))
	defer srv.Close()

	out, _, err := runCLI(t, "prompted@example.com\nsecret\n", "login", srv.URL)
	if err != nil {
		t.Fatalf("login: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "Logged in as prompted@example.com") {
		t.Fatalf("output = %q", out)
	}
}

// TestLogoutClearsOnlyToken proves `basepod logout` revokes the session
// server-side and clears the current context's token, while leaving the
// context entry itself (URL, name, current-ness) in place — a subsequent
// `basepod login` against the same server should still land in the same
// named context, not have it evaporate.
func TestLogoutClearsOnlyToken(t *testing.T) {
	path := setTestConfigPath(t)

	logoutCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/logout" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		requireBearer(t, r, "tok-logout")
		logoutCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	saveTestContext(t, path, srv.URL, "tok-logout")

	out, _, err := runCLI(t, "", "logout")
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !logoutCalled {
		t.Fatal("expected POST /auth/logout to be called")
	}
	if !strings.Contains(out, `Logged out of context "t"`) {
		t.Fatalf("output = %q", out)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Current != "t" {
		t.Fatalf("Current = %q, want the context to still be current", cfg.Current)
	}
	got, ok := cfg.Contexts["t"]
	if !ok {
		t.Fatal("expected the context entry to still exist")
	}
	if got.URL != srv.URL {
		t.Fatalf("URL = %q, want %q (unchanged)", got.URL, srv.URL)
	}
	if got.Token != "" {
		t.Fatalf("Token = %q, want empty after logout", got.Token)
	}
}

// TestLogoutClearsTokenEvenIfServerRevokeFails proves the CLI still clears
// the local token when the server-side revoke fails (e.g. the session had
// already expired) — the whole point of logout is to leave no usable
// credential lying around locally, and an unreachable/already-dead session
// server-side is not a reason to keep one on disk.
func TestLogoutClearsTokenEvenIfServerRevokeFails(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{"code": "unauthorized", "message": "invalid or expired session"},
		})
	}))
	defer srv.Close()

	saveTestContext(t, path, srv.URL, "already-dead")

	_, stderr, err := runCLI(t, "", "logout")
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !strings.Contains(stderr, "warning") {
		t.Fatalf("stderr = %q, want a warning about the failed server-side revoke", stderr)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Contexts["t"].Token != "" {
		t.Fatalf("Token = %q, want empty even though the server-side revoke failed", cfg.Contexts["t"].Token)
	}
}

func TestAppsRendersTable(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "tok-apps")
		if r.URL.Path != "/api/v1/apps" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		writeJSONResponse(w, http.StatusOK, []map[string]any{
			{"slug": "blog", "image": "nginx:latest", "port": 8080, "status": "running"},
		})
	}))
	defer srv.Close()

	saveTestContext(t, path, srv.URL, "tok-apps")

	out, _, err := runCLI(t, "", "apps")
	if err != nil {
		t.Fatalf("apps: %v", err)
	}
	for _, want := range []string{"SLUG", "blog", "running", "nginx:latest", "8080"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestAppsJSON(t *testing.T) {
	path := setTestConfigPath(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusOK, []AppInfo{{Slug: "blog", Image: "img", Port: 80, Status: "running"}})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "apps", "--json")
	if err != nil {
		t.Fatalf("apps --json: %v", err)
	}
	var got []AppInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not valid JSON: %v (%q)", err, out)
	}
	if len(got) != 1 || got[0].Slug != "blog" {
		t.Fatalf("got = %+v", got)
	}
}

func TestDeployImagePostsCorrectBody(t *testing.T) {
	path := setTestConfigPath(t)

	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/blog/deploy" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSONResponse(w, http.StatusOK, Deployment{
			Number: 4, Image: "myimage:v2", Status: "healthy",
			StartedAt: "t1", FinishedAt: "t2", Source: "image", Trigger: "api",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", "--image", "myimage:v2")
	if err != nil {
		t.Fatalf("deploy: %v (out=%s)", err, out)
	}
	if gotBody["image"] != "myimage:v2" {
		t.Fatalf("posted body = %v", gotBody)
	}
	if !strings.Contains(out, "deployment #4") || !strings.Contains(out, "healthy") {
		t.Fatalf("output = %q", out)
	}
}

func TestDeployRequiresApp(t *testing.T) {
	setTestConfigPath(t)
	_, _, err := runCLI(t, "", "deploy", "--image", "x")
	if err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("err = %v, want an --app/-a required error", err)
	}
}

// TestDeployDefaultsAppFromManifest proves `basepod deploy` (with
// --image, so no Containerfile/tarball is needed) infers --app from a
// basepod.yaml's `name` field in the deploy path when --app/-a is
// omitted — the zero-config target: "basepod init && basepod deploy" in
// a fresh directory, no flags.
func TestDeployDefaultsAppFromManifest(t *testing.T) {
	path := setTestConfigPath(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSONResponse(w, http.StatusOK, Deployment{
			Number: 1, Image: "myimage:v1", Status: "healthy",
			Source: "image", Trigger: "api",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "basepod.yaml", "name: blog\nport: 8080\n")

	out, _, err := runCLI(t, "", "deploy", "--image", "myimage:v1", dir)
	if err != nil {
		t.Fatalf("deploy: %v (out=%s)", err, out)
	}
	if gotPath != "/api/v1/apps/blog/deploy" {
		t.Fatalf("request path = %q, want the app slug from basepod.yaml (blog)", gotPath)
	}
}

// TestDeployExplicitAppFlagWinsOverManifest proves --app/-a still takes
// priority over a basepod.yaml `name` when both are present.
func TestDeployExplicitAppFlagWinsOverManifest(t *testing.T) {
	path := setTestConfigPath(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSONResponse(w, http.StatusOK, Deployment{
			Number: 1, Image: "myimage:v1", Status: "healthy",
			Source: "image", Trigger: "api",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "basepod.yaml", "name: from-manifest\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "from-flag", "--image", "myimage:v1", dir)
	if err != nil {
		t.Fatalf("deploy: %v (out=%s)", err, out)
	}
	if gotPath != "/api/v1/apps/from-flag/deploy" {
		t.Fatalf("request path = %q, want --app to win over the manifest", gotPath)
	}
}

// TestDeployManifestWithoutNameStillRequiresApp proves a basepod.yaml
// that exists but doesn't set `name` surfaces a clear error rather than
// silently falling back to the generic "--app is required" message.
func TestDeployManifestWithoutNameStillRequiresApp(t *testing.T) {
	setTestConfigPath(t)
	dir := t.TempDir()
	writeFile(t, dir, "basepod.yaml", "port: 8080\n")

	_, _, err := runCLI(t, "", "deploy", "--image", "x", dir)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("err = %v, want an error naming the missing `name` field", err)
	}
}

// TestDeployBadManifestSurfacesParseError proves a basepod.yaml with a
// fatal parse error (not just a missing name) is reported clearly rather
// than silently ignored.
func TestDeployBadManifestSurfacesParseError(t *testing.T) {
	setTestConfigPath(t)
	dir := t.TempDir()
	writeFile(t, dir, "basepod.yaml", "port: not-a-number\n")

	_, _, err := runCLI(t, "", "deploy", "--image", "x", dir)
	if err == nil || !strings.Contains(err.Error(), "basepod.yaml") {
		t.Fatalf("err = %v, want an error mentioning basepod.yaml", err)
	}
}

func TestDeployFromSourceRequiresContainerfile(t *testing.T) {
	path := setTestConfigPath(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no request should have been made, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "README.md", "no Containerfile here\n")

	_, _, err := runCLI(t, "", "deploy", "-a", "blog", dir)
	if err == nil || !strings.Contains(err.Error(), "Containerfile") {
		t.Fatalf("err = %v, want a Containerfile-required error", err)
	}
}

// tarballDeployServerOpts configures newTarballDeployServer's scripted
// responses for the async tarball-deploy flow: POST .../deploy/tarball
// (202 + a "deploying" deployment), POST /stream-token (mints a scoped
// build_log token), GET .../deployments/{n}/log (an SSE stream carrying
// logLine, closed by the handler returning — exactly how the real server
// ends the stream once the deployment reaches a terminal status), and GET
// .../deployments/{n} (polled — always answers with the terminal
// deployment this is configured to report, matching a build that already
// finished by the time the CLI's first poll lands).
type tarballDeployServerOpts struct {
	number       int
	finalStatus  string
	finalImage   string
	finalError   string
	logLine      string
	streamToken  string
	requestsSeen map[string]bool
}

// newTarballDeployServer builds an httptest.Server scripting the full
// async tarball-deploy request sequence a real BasePod server drives
// `basepod deploy`'s non-detach path through: upload -> 202 -> mint a
// build_log stream token -> stream the build log -> poll to terminal.
func newTarballDeployServer(t *testing.T, opts tarballDeployServerOpts) *httptest.Server {
	t.Helper()
	if opts.requestsSeen == nil {
		opts.requestsSeen = map[string]bool{}
	}
	logPath := fmt.Sprintf("/api/v1/apps/blog/deployments/%d/log", opts.number)
	depPath := fmt.Sprintf("/api/v1/apps/blog/deployments/%d", opts.number)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/apps/blog/deploy/tarball":
			opts.requestsSeen["upload"] = true
			_, _ = io.Copy(io.Discard, r.Body)
			writeJSONResponse(w, http.StatusAccepted, Deployment{
				Number: opts.number, Status: "deploying", Source: "tarball", Trigger: "api",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/stream-token":
			opts.requestsSeen["stream-token"] = true
			writeJSONResponse(w, http.StatusOK, StreamToken{Token: opts.streamToken, ExpiresAt: "2099-01-01T00:00:00Z"})
		case r.Method == http.MethodGet && r.URL.Path == logPath:
			opts.requestsSeen["log"] = true
			if got := r.URL.Query().Get("access_token"); got != opts.streamToken {
				t.Fatalf("build-log request access_token = %q, want %q", got, opts.streamToken)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", opts.logLine)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Returning here closes the response body, ending the SSE
			// stream exactly like the real server does once the
			// deployment reaches a terminal status.
		case r.Method == http.MethodGet && r.URL.Path == depPath:
			opts.requestsSeen["poll"] = true
			writeJSONResponse(w, http.StatusOK, Deployment{
				Number: opts.number, Status: opts.finalStatus, Image: opts.finalImage,
				Error: opts.finalError, Source: "tarball", Trigger: "api",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeployFromSourceHappyPath proves the tarball path of `basepod
// deploy` uploads a gzipped build context (Content-Type application/gzip),
// gets back 202 with a "deploying" deployment, streams its build log live
// (via a minted build_log stream token) to stdout, polls until it lands
// healthy, and reports success.
func TestDeployFromSourceHappyPath(t *testing.T) {
	path := setTestConfigPath(t)

	seen := map[string]bool{}
	srv := newTarballDeployServer(t, tarballDeployServerOpts{
		number: 7, finalStatus: "healthy", finalImage: "localhost/basepod/blog:7",
		logLine: "Successfully built abc123", streamToken: "stream-tok-7", requestsSeen: seen,
	})
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "main.go", "package main\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", dir)
	if err != nil {
		t.Fatalf("deploy: %v (out=%s)", err, out)
	}
	for _, step := range []string{"upload", "stream-token", "log", "poll"} {
		if !seen[step] {
			t.Errorf("expected the %q request to have been made, got requests=%v", step, seen)
		}
	}
	if !strings.Contains(out, "Successfully built abc123") {
		t.Fatalf("output missing live build-log line: %q", out)
	}
	if !strings.Contains(out, "deployment #7") || !strings.Contains(out, "healthy") {
		t.Fatalf("output missing final summary: %q", out)
	}
}

// TestDeployFromSourceFailurePolledStatus proves that when the polled
// deployment lands on a terminal status other than "healthy", `basepod
// deploy` exits non-zero with exactly the deployment's own Error message
// (learned by polling GET .../deployments/{n} — not from the upload
// response itself, which is always just 202 "deploying" now), after having
// already streamed the build log live.
func TestDeployFromSourceFailurePolledStatus(t *testing.T) {
	path := setTestConfigPath(t)

	seen := map[string]bool{}
	srv := newTarballDeployServer(t, tarballDeployServerOpts{
		number: 5, finalStatus: "failed", finalError: "build failed: exit status 1",
		logLine: "Step 2/2: RUN false", streamToken: "stream-tok-5", requestsSeen: seen,
	})
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\nRUN false\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", dir)
	if err == nil {
		t.Fatal("want a non-nil error for a failed deploy")
	}
	if err.Error() != "build failed: exit status 1" {
		t.Fatalf("err = %q, want the polled deployment's own error message verbatim", err.Error())
	}
	if !seen["log"] || !seen["poll"] {
		t.Fatalf("expected both the build-log stream and the poll to have been requested, got %v", seen)
	}
	if !strings.Contains(out, "Step 2/2: RUN false") {
		t.Fatalf("output missing live build-log line: %q", out)
	}
	if !strings.Contains(out, "deployment #5") || !strings.Contains(out, "failed") {
		t.Fatalf("output missing final summary: %q", out)
	}
}

// TestFollowDeploymentPollsUntilTerminal proves pollDeploymentUntilTerminal
// actually loops — re-polling GET .../deployments/{n} until the server
// reports a terminal status — rather than trusting a single response.
// Shrinks the package-level deployPollInterval so the test doesn't have to
// wait out the real 2s default.
func TestFollowDeploymentPollsUntilTerminal(t *testing.T) {
	path := setTestConfigPath(t)

	orig := deployPollInterval
	deployPollInterval = time.Millisecond
	t.Cleanup(func() { deployPollInterval = orig })

	var pollCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/apps/blog/deploy/tarball":
			_, _ = io.Copy(io.Discard, r.Body)
			writeJSONResponse(w, http.StatusAccepted, Deployment{Number: 3, Status: "deploying", Source: "tarball", Trigger: "api"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/stream-token":
			writeJSONResponse(w, http.StatusOK, StreamToken{Token: "tok-3", ExpiresAt: "2099-01-01T00:00:00Z"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/blog/deployments/3/log":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// No log lines — this test cares about the polling loop, not
			// the streamed content.
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/blog/deployments/3":
			pollCount++
			status := "deploying"
			if pollCount >= 3 {
				status = "healthy"
			}
			writeJSONResponse(w, http.StatusOK, Deployment{Number: 3, Status: status, Source: "tarball", Trigger: "api"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", dir)
	if err != nil {
		t.Fatalf("deploy: %v (out=%s)", err, out)
	}
	if pollCount < 3 {
		t.Fatalf("pollCount = %d, want at least 3 (the loop must actually re-poll until terminal)", pollCount)
	}
}

// TestDeployDetachSkipsFollow proves --detach prints the deployment
// number and returns immediately after the upload's 202 response, never
// minting a stream token or polling for a terminal status.
func TestDeployDetachSkipsFollow(t *testing.T) {
	path := setTestConfigPath(t)

	seen := map[string]bool{}
	srv := newTarballDeployServer(t, tarballDeployServerOpts{
		number: 9, finalStatus: "healthy", requestsSeen: seen,
	})
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", "--detach", dir)
	if err != nil {
		t.Fatalf("deploy --detach: %v (out=%s)", err, out)
	}
	if !seen["upload"] {
		t.Fatal("expected the upload request to have been made")
	}
	if seen["stream-token"] || seen["log"] || seen["poll"] {
		t.Fatalf("--detach must not follow the deployment, got requests=%v", seen)
	}
	if !strings.Contains(out, "deployment #9") || !strings.Contains(out, "deploying") {
		t.Fatalf("output = %q, want it to name the deployment number and its (non-terminal) status", out)
	}
}

func TestEnvGetPrintsMaskedSecrets(t *testing.T) {
	path := setTestConfigPath(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusOK, []EnvVar{
			{Key: "A", Value: "1", IsSecret: false},
			{Key: "S", Value: "", IsSecret: true},
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "env", "blog")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if !strings.Contains(out, "A=1") || !strings.Contains(out, "S="+secretMask) {
		t.Fatalf("output = %q", out)
	}
}

func TestEnvSetMergePreservesSecretFlags(t *testing.T) {
	path := setTestConfigPath(t)

	var putBody []EnvVar
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/blog/env":
			writeJSONResponse(w, http.StatusOK, []EnvVar{
				{Key: "A", Value: "1", IsSecret: false},
				{Key: "S", Value: "", IsSecret: true},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/apps/blog/env":
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			writeJSONResponse(w, http.StatusOK, putBody)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "env", "set", "blog", "A=99", "B=hello")
	if err != nil {
		t.Fatalf("env set: %v (out=%s)", err, out)
	}

	want := []EnvVar{
		{Key: "A", Value: "99", IsSecret: false},
		{Key: "S", Value: "", IsSecret: true},
		{Key: "B", Value: "hello", IsSecret: false},
	}
	if len(putBody) != len(want) {
		t.Fatalf("putBody = %+v, want %+v", putBody, want)
	}
	for i := range want {
		if putBody[i] != want[i] {
			t.Fatalf("putBody[%d] = %+v, want %+v", i, putBody[i], want[i])
		}
	}
	if !strings.Contains(out, "A=99") || !strings.Contains(out, "S="+secretMask) || !strings.Contains(out, "B=hello") {
		t.Fatalf("output = %q", out)
	}
}

func TestEnvSetSecretFlagMarksAllKeysSecret(t *testing.T) {
	path := setTestConfigPath(t)

	var putBody []EnvVar
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			writeJSONResponse(w, http.StatusOK, []EnvVar{{Key: "A", Value: "1", IsSecret: false}})
		case r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			writeJSONResponse(w, http.StatusOK, putBody)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	if _, _, err := runCLI(t, "", "env", "set", "blog", "--secret", "A=2", "NEW=v"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range putBody {
		if (ev.Key == "A" || ev.Key == "NEW") && !ev.IsSecret {
			t.Fatalf("key %q not marked secret with --secret: %+v", ev.Key, putBody)
		}
	}
}

func TestEnvUnsetRemovesKeys(t *testing.T) {
	path := setTestConfigPath(t)

	var putBody []EnvVar
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			writeJSONResponse(w, http.StatusOK, []EnvVar{
				{Key: "A", Value: "1", IsSecret: false},
				{Key: "B", Value: "2", IsSecret: false},
			})
		case r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			writeJSONResponse(w, http.StatusOK, putBody)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	if _, _, err := runCLI(t, "", "env", "unset", "blog", "A"); err != nil {
		t.Fatal(err)
	}
	if len(putBody) != 1 || putBody[0].Key != "B" {
		t.Fatalf("putBody = %+v, want only B remaining", putBody)
	}
}

func TestRollbackPostsNumber(t *testing.T) {
	path := setTestConfigPath(t)

	var gotBody map[string]int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/blog/rollback" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSONResponse(w, http.StatusOK, Deployment{Number: 2, Image: "img:1", Status: "healthy"})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "rollback", "blog", "2")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if gotBody["number"] != 2 {
		t.Fatalf("posted body = %v", gotBody)
	}
	if !strings.Contains(out, "deployment #2") {
		t.Fatalf("output = %q", out)
	}
}

func TestLogsParsesSSEAndPrintsLines(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/blog/logs" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		requireBearer(t, r, "tok")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ": heartbeat\n\n")
		fmt.Fprint(w, "event: log\ndata: {\"stream\":\"stdout\",\"line\":\"hello\"}\n\n")
		fmt.Fprint(w, "event: log\ndata: {\"stream\":\"stdout\",\"line\":\"world\"}\n\n")
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "logs", "blog")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "heartbeat") {
		t.Fatalf("output contains heartbeat, want it ignored: %q", out)
	}
}

func TestStatusPrintsSystemAndApps(t *testing.T) {
	path := setTestConfigPath(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/system":
			writeJSONResponse(w, http.StatusOK, SystemInfo{Version: "v0.3.0", Podman: "ok", Apps: 1})
		case "/api/v1/apps":
			writeJSONResponse(w, http.StatusOK, []AppInfo{{Slug: "blog", Image: "img", Port: 80, Status: "running"}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"v0.3.0", "ok", "blog", "running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestContextListAndUse(t *testing.T) {
	path := setTestConfigPath(t)
	SaveConfigTo(path, &Config{
		Contexts: map[string]Context{
			"prod": {URL: "https://prod.example.com"},
			"dev":  {URL: "http://localhost:8080"},
		},
		Current: "prod",
	})

	out, _, err := runCLI(t, "", "context", "list")
	if err != nil {
		t.Fatalf("context list: %v", err)
	}
	if !strings.Contains(out, "prod") || !strings.Contains(out, "dev") {
		t.Fatalf("output = %q", out)
	}

	out, _, err = runCLI(t, "", "context", "use", "dev")
	if err != nil {
		t.Fatalf("context use: %v", err)
	}
	if !strings.Contains(out, "dev") {
		t.Fatalf("output = %q", out)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Current != "dev" {
		t.Fatalf("Current = %q, want dev", cfg.Current)
	}
}

func TestContextUseUnknownFails(t *testing.T) {
	setTestConfigPath(t)
	_, _, err := runCLI(t, "", "context", "use", "nope")
	if err == nil {
		t.Fatal("want an error for an unknown context name")
	}
}

func TestNotLoggedInErrorsCleanly(t *testing.T) {
	setTestConfigPath(t) // fresh, empty config
	_, _, err := runCLI(t, "", "apps")
	if err != ErrNotLoggedIn {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestApiErrorMessagePassedThroughVerbatim(t *testing.T) {
	path := setTestConfigPath(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"code": "app_not_found", "message": "app not found"},
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	_, _, err := runCLI(t, "", "env", "ghost")
	if err == nil {
		t.Fatal("want an error")
	}
	if err.Error() != "app not found" {
		t.Fatalf("err = %q, want exactly the server's message %q", err.Error(), "app not found")
	}
	var apiErr *ApiError
	if e, ok := err.(*ApiError); !ok {
		t.Fatalf("err type = %T, want *ApiError", err)
	} else {
		apiErr = e
	}
	if apiErr.Code != "app_not_found" || apiErr.Status != http.StatusNotFound {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestHumanBytesFormatting(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{64 << 20, "64.0 MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
