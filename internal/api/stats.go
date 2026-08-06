package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

// statsHeartbeatInterval is how often handleAppStats writes an SSE comment
// line to keep the connection alive while waiting between podman's own
// ~5s sample ticks (see podman.Client.ContainerStats' doc comment) — a
// shorter interval than logHeartbeatInterval since a stats stream's normal
// inter-event gap is itself close to that constant, and a proxy/load
// balancer idle-timeout should never be the thing that decides a stats
// view goes stale-looking.
const statsHeartbeatInterval = 15 * time.Second

// statsEventPayload is the JSON body of each `event: stats` SSE message —
// BasePod's own small, stable shape (see
// podman.ContainerStatsSample's doc comment for why this is never
// libpod's/Docker's raw wire struct): CPU percent, memory used/limit, PID
// count, and network/block IO totals, all of which come free from the same
// libpod sample.
type statsEventPayload struct {
	CPUPercent      float64 `json:"cpu_percent"`
	MemUsedBytes    uint64  `json:"mem_used_bytes"`
	MemLimitBytes   uint64  `json:"mem_limit_bytes"`
	PIDs            uint64  `json:"pids"`
	NetRxBytes      uint64  `json:"net_rx_bytes"`
	NetTxBytes      uint64  `json:"net_tx_bytes"`
	BlockReadBytes  uint64  `json:"block_read_bytes"`
	BlockWriteBytes uint64  `json:"block_write_bytes"`
}

// statsEventPayloadFrom converts a podman.ContainerStatsSample into the
// wire payload — a one-to-one field mapping kept as an explicit function
// (rather than an embedded/aliased type) so the two shapes are free to
// diverge independently: podman.ContainerStatsSample can grow fields the
// UI doesn't need yet without this route silently starting to expose them.
func statsEventPayloadFrom(s podman.ContainerStatsSample) statsEventPayload {
	return statsEventPayload{
		CPUPercent:      s.CPUPercent,
		MemUsedBytes:    s.MemUsedBytes,
		MemLimitBytes:   s.MemLimitBytes,
		PIDs:            s.PIDs,
		NetRxBytes:      s.NetRxBytes,
		NetTxBytes:      s.NetTxBytes,
		BlockReadBytes:  s.BlockReadBytes,
		BlockWriteBytes: s.BlockWriteBytes,
	}
}

// handleAppStats streams an app's container resource-usage stats as
// Server-Sent Events, one `event: stats` message per sample podman.
// StreamStats decodes from the raw source a.stats (StatsSource) returns —
// mirrors handleAppLogs' structure closely: a background goroutine drives
// the (blocking, synchronous) decode loop over the raw reader, feeding
// samples to this handler's writer loop through an unbuffered channel so
// the decode goroutine never runs ahead of what's actually been written to
// the client, and a heartbeat comment keeps the connection alive between
// podman's own ~5s sample ticks.
//
// Errors: 404 "app_not_found" if the slug doesn't exist, 409
// "not_running" if the app has no running container, 502 "stats_failed"
// for any other failure obtaining the stats stream.
func (a *api) handleAppStats(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	src, err := a.stats(r.Context(), slug)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "app_not_found", "app not found")
		case errors.Is(err, deploy.ErrNotRunning):
			writeError(w, http.StatusConflict, "not_running", "app has no running container")
		default:
			writeError(w, http.StatusBadGateway, "stats_failed", err.Error())
		}
		return
	}
	// Closing src is what lets the decode goroutine below exit once this
	// handler returns via any path — normal stream end, client disconnect,
	// or an unexpected error — mirroring handleAppLogs' identical
	// defer src.Close() reasoning.
	defer src.Close()

	// Reserve a concurrent-stream slot (audit finding L6, v0.4) exactly
	// like handleAppLogs — a stats stream is just as long-lived and holds
	// just as real a goroutine+connection, so it counts against the same
	// per-user/global caps.
	release, slotOK := a.acquireStreamSlot(w, r)
	if !slotOK {
		return
	}
	defer release()

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

	// samples is unbuffered for the same reason lines is in handleAppLogs:
	// StreamStats' synchronous decode loop over src never runs ahead of
	// what this writer loop has actually consumed.
	samples := make(chan podman.ContainerStatsSample)
	decodeDone := make(chan struct{})
	go func() {
		defer close(decodeDone)
		_ = podman.StreamStats(src, func(s podman.ContainerStatsSample) {
			select {
			case samples <- s:
			case <-r.Context().Done():
			}
		})
	}()

	heartbeat := time.NewTicker(statsHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-decodeDone:
			return
		case s := <-samples:
			payload, err := json.Marshal(statsEventPayloadFrom(s))
			if err != nil {
				continue // unreachable in practice: statsEventPayload always marshals
			}
			if _, err := fmt.Fprintf(w, "event: stats\ndata: %s\n\n", payload); err != nil {
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
