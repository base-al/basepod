package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/base-al/basepod/internal/podman"
)

// allStatsEventPayload is the JSON body of each `event: stats` SSE
// message on the batch route (GET /api/v1/stats) — statsEventPayload
// (the per-app route's payload) plus Slug, since one connection here
// carries every running app's samples interleaved and each event must
// identify which app it's for. Field-for-field identical otherwise, so a
// client that already knows how to render a per-app StatsSample only has
// to add slug-based routing, not a second parsing path.
type allStatsEventPayload struct {
	Slug            string  `json:"slug"`
	CPUPercent      float64 `json:"cpu_percent"`
	MemUsedBytes    uint64  `json:"mem_used_bytes"`
	MemLimitBytes   uint64  `json:"mem_limit_bytes"`
	PIDs            uint64  `json:"pids"`
	NetRxBytes      uint64  `json:"net_rx_bytes"`
	NetTxBytes      uint64  `json:"net_tx_bytes"`
	BlockReadBytes  uint64  `json:"block_read_bytes"`
	BlockWriteBytes uint64  `json:"block_write_bytes"`
}

// allStatsEventPayloadFrom converts a podman.BulkStatsSample (identified
// only by container ID/name — see BulkStatsSample's doc comment) plus the
// slug handleAllStats already resolved it to into the wire payload —
// mirrors statsEventPayloadFrom's one-to-one field mapping for the same
// reason: podman.BulkStatsSample can grow fields this route doesn't need
// yet without silently starting to expose them.
func allStatsEventPayloadFrom(slug string, s podman.BulkStatsSample) allStatsEventPayload {
	return allStatsEventPayload{
		Slug:            slug,
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

// handleAllStats streams every currently-running app's container
// resource-usage stats over ONE Server-Sent Events connection — the
// substrate for the apps-list sparklines: rather than one SSE connection
// per app (what the per-app route, handleAppStats, would require if the
// UI opened one per row), the client opens exactly one connection here
// and routes each `event: stats` message by its `slug` field.
//
// Structure mirrors handleAppStats closely (background decode goroutine
// feeding this handler's writer loop through an unbuffered channel, a
// heartbeat between podman's own ~5s ticks, the same concurrent-stream
// slot), with two differences that follow directly from being a batch
// route rather than a per-app one:
//
//   - There is no 404/409 failure mode: no single app or container this
//     request is "about", so the only failure this maps to error
//     responses is a genuine backend problem (502 "stats_failed") —
//     see AllStatsProvider's doc comment for AllStats' "nothing running
//     is still a valid, if silent, stream" contract.
//   - Each podman.StreamBulkStats tick (one per app currently running,
//     covering every app — see podman.BulkContainerStats' doc comment)
//     is attributed to an app slug via a.allStats.RunningAppContainers,
//     refreshed once per tick (not once per sample within a tick: it's
//     one ListContainers call covering every app, so refreshing it more
//     often buys nothing) — a container ID with no entry in that mapping
//     (an app that isn't running, or something else's container
//     entirely — see podman.BulkContainerStats' doc comment for a real
//     example) is silently skipped rather than emitted with a guessed
//     slug, which is exactly how "apps that aren't running simply don't
//     emit" falls out with no special-casing. A transient
//     RunningAppContainers failure on a given tick reuses the last
//     known-good mapping (or, if there has never been a successful one
//     yet, attributes nothing for that tick) rather than ending the
//     whole connection — one bad lookup must not kill every app's live
//     data.
//
// Errors: 502 "stats_failed" if either precondition call
// (a.allStats.HostCPUs or a.allStats.AllStats) fails.
func (a *api) handleAllStats(w http.ResponseWriter, r *http.Request) {
	// HostCPUs is a cheap, local metadata call (see AllStatsProvider's doc
	// comment) — fetched before ever opening the bulk stream so a
	// misconfigured/unreachable podman never pays the cost of opening (and
	// then immediately discarding) that connection first.
	hostCPUs, err := a.allStats.HostCPUs(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "stats_failed", err.Error())
		return
	}

	src, err := a.allStats.AllStats(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "stats_failed", err.Error())
		return
	}
	// Closing src is what lets the decode goroutine below exit once this
	// handler returns via any path — normal stream end, client
	// disconnect, or an unexpected error — mirroring handleAppStats'
	// identical defer src.Close() reasoning.
	defer src.Close()

	// Reserve a concurrent-stream slot exactly like handleAppStats/
	// handleAppLogs — a batch stats stream is just as long-lived and
	// holds just as real a goroutine+connection, so it counts against the
	// same per-user/global caps. Only reserved once both preconditions
	// above have already succeeded, so a request that was always going to
	// fail never consumes one.
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

	// ticks is unbuffered for the same reason handleAppStats' samples
	// channel is: podman.StreamBulkStats' synchronous decode loop over
	// src never runs ahead of what this writer loop has actually
	// consumed.
	ticks := make(chan []podman.BulkStatsSample)
	decodeDone := make(chan struct{})
	go func() {
		defer close(decodeDone)
		_ = podman.StreamBulkStats(src, hostCPUs, func(tick []podman.BulkStatsSample) {
			select {
			case ticks <- tick:
			case <-r.Context().Done():
			}
		})
	}()

	// attribution maps containerID -> app slug, refreshed once per tick
	// below (see this function's doc comment for why per-tick, not
	// per-sample, and why a refresh failure falls back to the last
	// known-good value instead of ending the stream). Starts nil: a tick
	// that arrives before the very first successful refresh attributes
	// nothing, rather than blocking the stream's own startup on an extra
	// synchronous call before the SSE headers are even written.
	var attribution map[string]string

	heartbeat := time.NewTicker(statsHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-decodeDone:
			return
		case tick := <-ticks:
			if fresh, err := a.allStats.RunningAppContainers(r.Context()); err == nil {
				attribution = fresh
			}
			for _, s := range tick {
				slug, ok := attribution[s.ContainerID]
				if !ok {
					continue
				}
				payload, err := json.Marshal(allStatsEventPayloadFrom(slug, s))
				if err != nil {
					continue // unreachable in practice: allStatsEventPayload always marshals
				}
				if _, err := fmt.Fprintf(w, "event: stats\ndata: %s\n\n", payload); err != nil {
					return
				}
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
