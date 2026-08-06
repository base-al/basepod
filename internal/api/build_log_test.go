package api

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHandleDeploymentLogNoBuildLog proves an image-sourced deployment
// (BuildLogPath == "") is reported as 404 "no_build_log".
func TestHandleDeploymentLogNoBuildLog(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	app, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(app.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1/log", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "no_build_log" {
		t.Fatalf("error code = %q, want no_build_log", errBody.Error.Code)
	}
}

// TestHandleDeploymentLogDeploymentNotFound proves an unknown deployment
// number is reported as 404 "deployment_not_found".
func TestHandleDeploymentLogDeploymentNotFound(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/99/log", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if errBody.Error.Code != "deployment_not_found" {
		t.Fatalf("error code = %q, want deployment_not_found", errBody.Error.Code)
	}
}

// TestHandleDeploymentLogDanglingPathDegradesGracefully proves that a
// finished deployment whose build_log_path is recorded but whose file
// doesn't actually exist on disk (e.g. a build that failed before ever
// creating the log file — DeployBuild records the path immediately,
// before the build starts, so it can be tailed live; see its doc
// comment) is reported as 404 "no_build_log", not a 500 — the dangling
// path must never read to a caller as a BasePod bug.
func TestHandleDeploymentLogDanglingPathDegradesGracefully(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	app, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}

	// Points at a file that is never created — simulating a build that
	// failed (e.g. disk full, permissions) before createLog ever ran.
	if err := st.SetDeploymentBuildLog(dep.ID, filepath.Join(t.TempDir(), "never-created.log")); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep.ID, "failed", "build: create log file: boom"); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1/log", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (not 500)", resp.StatusCode)
	}
	if errBody.Error.Code != "no_build_log" {
		t.Fatalf("error code = %q, want no_build_log", errBody.Error.Code)
	}
}

// TestHandleDeploymentLogTextModeWhenFinished proves a finished (any
// terminal status) tarball deployment's build log is served as the full
// plain-text file content.
func TestHandleDeploymentLogTextModeWhenFinished(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	app, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "1.log")
	want := "Step 1/2 : FROM alpine\nStep 2/2 : RUN true\nSuccessfully built abc123\n"
	if err := os.WriteFile(logPath, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentBuildLog(dep.ID, logPath); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentImage(dep.ID, "localhost/basepod/blog:1"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1/log", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// TestHandleDeploymentLogSSEStreamsThenTerminatesOnStatusFlip proves that
// while a deployment is still "deploying", the log endpoint streams
// `event: log` SSE frames for lines appended to the build log file, and
// the stream ends once the deployment's status flips away from
// "deploying" (driven here by directly updating the store row mid-test,
// simulating the build pipeline finishing concurrently).
func TestHandleDeploymentLogSSEStreamsThenTerminatesOnStatusFlip(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	app, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "1.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	if _, err := logFile.WriteString("Step 1/2 : FROM alpine\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentBuildLog(dep.ID, logPath); err != nil {
		t.Fatal(err)
	}
	// Deployment status stays "deploying" (CreateDeploymentFull's default)
	// until FinishDeployment below.

	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1/log", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)
	readEvent := func(t *testing.T) string {
		t.Helper()
		// Each event is "event: log\ndata: <line>\n\n" — read the
		// "event:" line, then the "data:" line, and discard the blank
		// terminator.
		evLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event line: %v", err)
		}
		if !strings.HasPrefix(evLine, "event: log") {
			t.Fatalf("unexpected line %q, want an \"event: log\" line", evLine)
		}
		dataLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading data line: %v", err)
		}
		if _, err := reader.ReadString('\n'); err != nil { // blank terminator
			t.Fatalf("reading terminator: %v", err)
		}
		return strings.TrimPrefix(strings.TrimSuffix(dataLine, "\n"), "data: ")
	}

	if got := readEvent(t); got != "Step 1/2 : FROM alpine" {
		t.Fatalf("first event = %q, want %q", got, "Step 1/2 : FROM alpine")
	}

	// Append a second line while the client is still connected; the
	// handler polls the file every buildLogPollInterval, so this should
	// show up as a second event without needing to reconnect.
	if _, err := logFile.WriteString("Step 2/2 : RUN true\n"); err != nil {
		t.Fatal(err)
	}
	if got := readEvent(t); got != "Step 2/2 : RUN true" {
		t.Fatalf("second event = %q, want %q", got, "Step 2/2 : RUN true")
	}

	// Flip the deployment to a terminal status, as the real build
	// pipeline would via FinishDeployment once the build completes, with
	// one final line landing in the log first.
	if _, err := logFile.WriteString("Successfully built abc123\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}

	if got := readEvent(t); got != "Successfully built abc123" {
		t.Fatalf("final event = %q, want %q", got, "Successfully built abc123")
	}

	// The stream must now end (server closes the response) rather than
	// keep polling forever.
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not terminate after the deployment's status flipped away from \"deploying\"")
	}
}

// TestHandleDeploymentLogSSEStripsCarriageReturn proves build output
// containing a bare \r cannot forge additional SSE frames (audit finding
// L2): the SSE spec treats \r, \n, and \r\n all as line terminators, so a
// build log line like "data\revent: log\rdata: forged" — legitimate
// build output, e.g. from a progress bar or spinner, or attacker-
// controlled RUN-step output — would otherwise be interpreted by a
// compliant SSE client as multiple additional event:/data: field lines
// rather than the single literal line it actually is.
func TestHandleDeploymentLogSSEStripsCarriageReturn(t *testing.T) {
	st := newTestStore(t)
	createTestApp(t, st)
	app, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "1.log")
	// A single log line whose content itself contains bare \r bytes,
	// crafted to look like it's injecting a second event/data pair.
	forged := "data\revent: log\rdata: forged\n"
	if err := os.WriteFile(logPath, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentBuildLog(dep.ID, logPath); err != nil {
		t.Fatal(err)
	}
	// Deployment status stays "deploying" (CreateDeploymentFull's default)
	// for the whole test — no need to flip it; we just read the one frame
	// and disconnect.

	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/apps/blog/deployments/1/log", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	evLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading event line: %v", err)
	}
	if evLine != "event: log\n" {
		t.Fatalf("event line = %q, want exactly %q (a bare \\r in the payload must never split into a second frame)", evLine, "event: log\n")
	}
	dataLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading data line: %v", err)
	}
	wantData := "data: dataevent: logdata: forged\n"
	if dataLine != wantData {
		t.Fatalf("data line = %q, want %q (\\r stripped, not treated as a line terminator)", dataLine, wantData)
	}
	if strings.Contains(dataLine, "\r") {
		t.Fatalf("data line still contains a bare \\r: %q", dataLine)
	}
}
