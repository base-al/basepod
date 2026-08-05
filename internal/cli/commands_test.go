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

// TestDeployFromSourceHappyPath proves the tarball path of `basepod
// deploy` uploads a gzipped build context (Content-Type application/gzip)
// to .../deploy/tarball and reports the deployment healthy when the
// server's synchronous response says so.
func TestDeployFromSourceHappyPath(t *testing.T) {
	path := setTestConfigPath(t)

	var gotContentType string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/apps/blog/deploy/tarball" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotContentType = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		// Drain the body so the handler behaves like a real server that
		// actually reads the upload before responding.
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSONResponse(w, http.StatusOK, Deployment{
			Number: 7, Image: "localhost/basepod/blog:7", Status: "healthy",
			Source: "tarball", Trigger: "api",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n")
	writeFile(t, dir, "main.go", "package main\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", dir)
	if err != nil {
		t.Fatalf("deploy: %v (out=%s)", err, out)
	}
	if gotPath != "/api/v1/apps/blog/deploy/tarball" {
		t.Fatalf("request path = %q", gotPath)
	}
	if gotContentType != "application/gzip" {
		t.Fatalf("Content-Type = %q, want application/gzip", gotContentType)
	}
	if !strings.Contains(out, "deployment #7") || !strings.Contains(out, "healthy") {
		t.Fatalf("output = %q", out)
	}
}

// TestDeployFromSourcePrintsBuildLogOnFailure proves that when the
// tarball upload fails server-side (502 deploy_failed — no deployment
// number in that response body at all, see internal/api's
// handleDeployTarball), the CLI infers which deployment just failed by
// GETting the app (deployments come back newest-first — see
// store.ListDeployments' ORDER BY number DESC — so Deployments[0] is the
// one that just failed) and prints its build log, then still reports the
// original error.
func TestDeployFromSourcePrintsBuildLogOnFailure(t *testing.T) {
	path := setTestConfigPath(t)

	var appDetailRequested, logRequested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/apps/blog/deploy/tarball":
			_, _ = io.Copy(io.Discard, r.Body)
			writeJSONResponse(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"code": "deploy_failed", "message": "build failed: exit status 1"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/blog":
			appDetailRequested = true
			writeJSONResponse(w, http.StatusOK, AppDetail{
				AppInfo: AppInfo{Slug: "blog", Status: "failed"},
				Deployments: []Deployment{
					// Newest first, matching the real server's ordering.
					{Number: 5, Status: "failed", Source: "tarball", HasBuildLog: true},
					{Number: 4, Status: "healthy", Source: "tarball", HasBuildLog: true},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/blog/deployments/5/log":
			logRequested = true
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Step 1/2: FROM scratch\nStep 2/2: RUN false\nexit status 1\n"))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\nRUN false\n")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", dir)
	if err == nil {
		t.Fatal("want a non-nil error for a failed deploy")
	}
	if err.Error() != "build failed: exit status 1" {
		t.Fatalf("err = %q, want the server's message verbatim", err.Error())
	}
	if !appDetailRequested {
		t.Fatal("CLI never GET the app to find the failed deployment")
	}
	if !logRequested {
		t.Fatal("CLI never fetched deployment #5's build log")
	}
	if !strings.Contains(out, "Step 2/2: RUN false") || !strings.Contains(out, "exit status 1") {
		t.Fatalf("output missing build log text: %q", out)
	}
	if !strings.Contains(out, "--- build log ---") {
		t.Fatalf("output missing build log delimiter: %q", out)
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
