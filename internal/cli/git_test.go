package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitConnectCmdPostsAndPrintsWebhookInfo proves `basepod git connect`
// sends the expected PUT body and prints the returned webhook URL/secret
// so an operator can paste them into a forge's webhook settings.
func TestGitConnectCmdPostsAndPrintsWebhookInfo(t *testing.T) {
	path := setTestConfigPath(t)

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/blog/git" || r.Method != http.MethodPut {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSONResponse(w, http.StatusOK, GitSource{
			URL: "https://github.com/example/repo.git", Branch: "main", Provider: "github",
			HookID: "abc123", Secret: "supersecret", Token: "",
			WebhookURL: "https://basepod.example.com/api/v1/webhooks/git/abc123",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "git", "connect", "blog", "--url", "https://github.com/example/repo.git", "--branch", "main")
	if err != nil {
		t.Fatalf("git connect: %v (out=%s)", err, out)
	}
	if gotBody["url"] != "https://github.com/example/repo.git" || gotBody["branch"] != "main" {
		t.Fatalf("posted body = %v", gotBody)
	}
	if !strings.Contains(out, "supersecret") {
		t.Fatalf("expected the webhook secret in the output, got %q", out)
	}
	if !strings.Contains(out, "not set") {
		t.Fatalf("expected token to be reported as 'not set', got %q", out)
	}
}

// TestGitConnectCmdRequiresURLAndBranch proves --url/--branch are
// required before any request is sent.
func TestGitConnectCmdRequiresURLAndBranch(t *testing.T) {
	setTestConfigPath(t)
	_, _, err := runCLI(t, "", "git", "connect", "blog", "--branch", "main")
	if err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("err = %v, want a --url required error", err)
	}
}

// TestGitStatusCmdPrintsConfigAndDeliveries proves `basepod git status`
// fetches both the config and the deliveries list, printing a table.
func TestGitStatusCmdPrintsConfigAndDeliveries(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/apps/blog/git" && r.Method == http.MethodGet:
			// GET never carries the secret (issue #13: write-only) — this
			// fixture deliberately omits it to match the real server.
			writeJSONResponse(w, http.StatusOK, GitSource{
				URL: "https://github.com/example/repo.git", Branch: "main", Provider: "github",
				HookID: "abc123", Token: "set",
				WebhookURL: "https://basepod.example.com/api/v1/webhooks/git/abc123",
			})
		case r.URL.Path == "/api/v1/apps/blog/git/deliveries" && r.Method == http.MethodGet:
			n := 5
			writeJSONResponse(w, http.StatusOK, []GitDelivery{
				{ID: 1, ReceivedAt: "t1", Event: "push", Ref: "refs/heads/main", CommitSHA: "deadbeef01234567", Status: "deployed", DeploymentNumber: &n},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "git", "status", "blog")
	if err != nil {
		t.Fatalf("git status: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "main") || !strings.Contains(out, "github") {
		t.Fatalf("expected branch/provider in output, got %q", out)
	}
	if !strings.Contains(out, "deadbee") {
		t.Fatalf("expected a shortened commit SHA in output, got %q", out)
	}
	if !strings.Contains(out, "#5") {
		t.Fatalf("expected the linked deployment number in output, got %q", out)
	}
	if !strings.Contains(out, "write-only") || !strings.Contains(out, "rotate-secret") {
		t.Fatalf("expected `git status` to say the secret is write-only and point at `git rotate-secret`, got %q", out)
	}
}

// TestGitRotateSecretCmdPostsAndPrintsFreshSecret proves `basepod git
// rotate-secret` posts to the dedicated rotate route (not a PUT with a
// flag) and prints the freshly minted secret exactly once (issue #13).
func TestGitRotateSecretCmdPostsAndPrintsFreshSecret(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/blog/git/rotate-secret" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSONResponse(w, http.StatusOK, GitSource{
			URL: "https://github.com/example/repo.git", Branch: "main", Provider: "github",
			HookID: "abc123", Secret: "freshlyrotated", Token: "",
			WebhookURL: "https://basepod.example.com/api/v1/webhooks/git/abc123",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "git", "rotate-secret", "blog")
	if err != nil {
		t.Fatalf("git rotate-secret: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "freshlyrotated") {
		t.Fatalf("expected the freshly rotated secret in the output, got %q", out)
	}
}

// TestGitDisconnectCmdSendsDelete proves `basepod git disconnect` issues
// a DELETE.
func TestGitDisconnectCmdSendsDelete(t *testing.T) {
	path := setTestConfigPath(t)

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/blog/git" || r.Method != http.MethodDelete {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "git", "disconnect", "blog")
	if err != nil {
		t.Fatalf("git disconnect: %v (out=%s)", err, out)
	}
	if !called {
		t.Fatal("expected the DELETE request to be sent")
	}
	if !strings.Contains(out, "disconnected") {
		t.Fatalf("output = %q", out)
	}
}

// TestDeployGitFlagPostsToDeployGitAndFollows proves `basepod deploy --git`
// hits POST .../deploy/git (not the tarball or image path) and follows the
// returned deployment to a terminal status.
func TestDeployGitFlagPostsToDeployGitAndFollows(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/apps/blog/deploy/git" && r.Method == http.MethodPost:
			writeJSONResponse(w, http.StatusAccepted, Deployment{
				Number: 7, Image: "localhost/basepod/blog:7", Status: "deploying",
				Source: "git", Trigger: "api", GitSha: "deadbeef01234567",
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/stream-token"):
			writeJSONResponse(w, http.StatusOK, StreamToken{Token: "st", ExpiresAt: "t"})
		case strings.Contains(r.URL.Path, "/log"):
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("build log\n"))
		case r.URL.Path == "/api/v1/apps/blog/deployments/7" && r.Method == http.MethodGet:
			writeJSONResponse(w, http.StatusOK, Deployment{
				Number: 7, Image: "localhost/basepod/blog:7", Status: "healthy",
				Source: "git", Trigger: "api", GitSha: "deadbeef01234567",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok")

	out, _, err := runCLI(t, "", "deploy", "-a", "blog", "--git")
	if err != nil {
		t.Fatalf("deploy --git: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "deadbee") {
		t.Fatalf("expected a short SHA in output, got %q", out)
	}
	if !strings.Contains(out, "deployment #7") || !strings.Contains(out, "healthy") {
		t.Fatalf("output = %q", out)
	}
}

// TestDeployGitAndImageMutuallyExclusive proves --git and --image cannot
// be combined.
func TestDeployGitAndImageMutuallyExclusive(t *testing.T) {
	setTestConfigPath(t)
	_, _, err := runCLI(t, "", "deploy", "-a", "blog", "--git", "--image", "x")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a mutually-exclusive error", err)
	}
}
