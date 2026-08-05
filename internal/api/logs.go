package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
	// worth failing the request over.
	defaultLogTail = 200
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
// defaultLogTail, capped at maxLogTail).
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
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "tail must be a non-negative integer")
			return
		}
		tail = n
	}
	if tail > maxLogTail {
		tail = maxLogTail
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
