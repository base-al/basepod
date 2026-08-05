package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

const (
	// defaultLogTail and maxLogTail bound the ?tail= query param: a caller
	// that omits it gets the last defaultLogTail lines; one that asks for
	// more than maxLogTail is silently capped rather than rejected, since
	// an enormous tail is a resource-usage mistake, not a client error
	// worth failing the request over. A present-but-non-positive tail
	// (0 or negative) is clamped up to minLogTail rather than treated as
	// "unset": podman's own client treats tail<=0 as "all logs" (see
	// internal/podman.Client.ContainerLogs), which is a very different,
	// far more expensive request than the caller most likely intended by
	// passing 0.
	defaultLogTail = 200
	minLogTail     = 1
	maxLogTail     = 5000

	// logHeartbeatInterval is how often handleAppLogs writes an SSE
	// comment line to keep the connection alive (and prove to the client
	// it's still live) while a follow=1 stream is otherwise quiet.
	logHeartbeatInterval = 15 * time.Second
)

// logEventPayload is the JSON body of each `event: log` SSE message.
type logEventPayload struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// demuxedLine is one line handed from the demux goroutine to the SSE
// writer loop in handleAppLogs.
type demuxedLine struct {
	stream, text string
}

// handleAppLogs streams an app's container logs as Server-Sent Events. It
// starts DemuxLogs on its own goroutine over the raw multiplexed reader
// a.logs returns, feeding parsed lines to this handler's writer loop
// through an unbounded-free (unbuffered) channel; the writer loop selects
// between new lines, a periodic heartbeat, the demux goroutine finishing,
// and the client disconnecting.
//
// Query params: follow (bool, default false) and tail (int, default
// defaultLogTail when omitted; clamped to minLogTail when zero or
// negative, and to maxLogTail when it exceeds it).
//
// Errors: 404 "app_not_found" if the slug doesn't exist, 409
// "not_running" if the app has no running container, 502 "logs_failed"
// for any other failure obtaining the log stream.
func (a *api) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	follow := parseBoolParam(r.URL.Query().Get("follow"))

	tail := defaultLogTail
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "tail must be an integer")
			return
		}
		switch {
		case n <= 0:
			tail = minLogTail
		case n > maxLogTail:
			tail = maxLogTail
		default:
			tail = n
		}
	}

	src, err := a.logs(r.Context(), slug, follow, tail)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "app_not_found", "app not found")
		case errors.Is(err, deploy.ErrNotRunning):
			writeError(w, http.StatusConflict, "not_running", "app has no running container")
		default:
			writeError(w, http.StatusBadGateway, "logs_failed", err.Error())
		}
		return
	}
	// Closing src (in particular for follow=1, whose read otherwise never
	// reaches EOF on its own) is what lets the demux goroutine below exit
	// once this handler returns — via any path: normal stream end,
	// client disconnect, or an unexpected error.
	defer src.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// lines is unbuffered so the demux goroutine (and thus DemuxLogs'
	// synchronous read loop over src) never runs ahead of what this
	// writer loop has actually consumed — no unbounded backlog can build
	// up if the client is slow, and the goroutine blocks (rather than
	// leaking) for at most as long as it takes ctx to cancel once the
	// writer loop stops reading from it.
	lines := make(chan demuxedLine)
	demuxDone := make(chan struct{})
	go func() {
		defer close(demuxDone)
		_ = podman.DemuxLogs(src, func(stream, text string) {
			select {
			case lines <- demuxedLine{stream, text}:
			case <-r.Context().Done():
			}
		})
	}()

	heartbeat := time.NewTicker(logHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-demuxDone:
			return
		case l := <-lines:
			payload, err := json.Marshal(logEventPayload{Stream: l.stream, Line: l.text})
			if err != nil {
				continue // unreachable in practice: logEventPayload always marshals
			}
			if _, err := fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseBoolParam reports whether v is a truthy query-param spelling
// ("1" or "true"); anything else (including "") is false.
func parseBoolParam(v string) bool {
	return v == "1" || v == "true"
}

// buildLogPollInterval is how often the SSE build-log stream (see
// streamBuildLogSSE) polls its log file for newly-written bytes and
// re-checks the deployment's status, while a tarball build is still in
// progress ("deploying").
const buildLogPollInterval = 500 * time.Millisecond

// handleDeploymentLog serves a tarball deploy's build log (see
// deploy.Engine.DeployBuild and internal/build.Builder): while the
// deployment is still "deploying" it streams the log file as
// Server-Sent Events, tailing new lines as the build writes them and
// stopping once the deployment reaches a terminal status or the client
// disconnects; once the deploy has finished (any terminal status), it
// serves the whole file as plain text instead.
//
// Like handleAppLogs, this route is mounted behind requireAuthLogs
// (see router.go) rather than requireAuth, so it also accepts
// ?access_token= for browser EventSource clients that can't set an
// Authorization header.
//
// Errors: 404 "app_not_found" for an unknown slug, 400 "invalid_request"
// if {number} isn't an integer, 404 "deployment_not_found" for an
// unknown deployment number, 404 "no_build_log" if the deployment has no
// build log (an image-sourced deploy) *or* has one recorded but the file
// doesn't actually exist on disk (a build that failed before ever
// creating it — see the plain-text branch below) — both read to a caller
// as "there's no build log to show", not a 500.
func (a *api) handleDeploymentLog(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	number, err := strconv.Atoi(chi.URLParam(r, "number"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "deployment number must be an integer")
		return
	}

	dep, err := a.st.DeploymentByNumber(app.ID, number)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "deployment_not_found", "deployment not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up deployment")
		}
		return
	}
	if dep.BuildLogPath == "" {
		writeError(w, http.StatusNotFound, "no_build_log", "deployment has no build log")
		return
	}

	if dep.Status == "deploying" {
		a.streamBuildLogSSE(w, r, app.ID, number, dep.BuildLogPath)
		return
	}

	data, err := os.ReadFile(dep.BuildLogPath)
	if err != nil {
		// The deployment row can carry a build_log_path before the file at
		// that path is actually guaranteed to exist on disk (DeployBuild
		// records it immediately — before the build starts — so a live
		// SSE tail can find it from the very first poll; see
		// deploy.Engine.DeployBuild). If the build then failed before ever
		// creating the file (e.g. disk full, permissions), or the file was
		// otherwise removed out of band, that's the same caller-visible
		// situation as "no build log" — 404, not a 500 that would read as
		// a BasePod bug.
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "no_build_log", "deployment has no build log")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to read build log")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// streamBuildLogSSE tails path as Server-Sent Events for as long as
// deployment (appID, number)'s status stays "deploying", polling both the
// file (for newly-written bytes) and the store (for the status flip)
// every buildLogPollInterval. It tolerates path not existing yet — the
// deployment row's build_log_path is recorded before the build creates
// the file on disk (see DeployBuild), so a request that lands in that
// narrow window must keep polling rather than failing outright — and
// stops (after a final drain of whatever was written) as soon as the
// status is no longer "deploying", or the client disconnects.
func (a *api) streamBuildLogSSE(w http.ResponseWriter, r *http.Request, appID int64, number int, path string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var f *os.File
	var reader *bufio.Reader
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	// ensureOpen lazily opens path once it exists, so a request that
	// arrives before the build has created the file on disk yet doesn't
	// fail outright — it just sees no lines until the next poll finds it.
	ensureOpen := func() {
		if f != nil {
			return
		}
		opened, err := os.Open(path)
		if err != nil {
			return
		}
		f = opened
		reader = bufio.NewReader(f)
	}

	// drain writes every complete (and any final partial) line currently
	// available from reader as one SSE "event: log" message each.
	drain := func() {
		if reader == nil {
			return
		}
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				fmt.Fprintf(w, "event: log\ndata: %s\n\n", strings.TrimSuffix(line, "\n"))
				flusher.Flush()
			}
			if err != nil {
				return
			}
		}
	}

	ticker := time.NewTicker(buildLogPollInterval)
	defer ticker.Stop()

	for {
		ensureOpen()
		drain()

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			dep, err := a.st.DeploymentByNumber(appID, number)
			if err != nil || dep.Status != "deploying" {
				// One last catch-up read in case the build's final output
				// landed between the last drain and the status flip.
				ensureOpen()
				drain()
				return
			}
		}
	}
}
