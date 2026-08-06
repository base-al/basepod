package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ContainerStats streams a container's resource-usage stats from libpod's
// GET .../containers/{name}/stats?stream=true.
//
// Wire shape verified directly (2026-08-06) two ways: (1) a read-only
// `curl --unix-socket` against a live podman 6.0.1 client / 5.7.1 server
// (matching apiVersion's floor) at exactly this path
// ("/v5.0.0/libpod/containers/{name}/stats?stream=true"), and (2) reading
// podman's own source at the v5.7.1 tag. The two together surface a
// naive-expectation trap worth recording: this path is registered in
// pkg/api/server/register_containers.go as
// `r.HandleFunc(VersionedPath("/libpod/containers/{name}/stats"), s.APIHandler(compat.StatsContainer))`
// — the *docker-compat* handler, not the libpod-native one mounted at
// `/libpod/containers/stats` (no {name}; bulk/multi-container, backing
// `podman stats`, returning `[]define.ContainerStats` with fields like
// CPU/MemUsage/MemLimit/PIDs — the shape a naive reading of "libpod stats"
// would expect). Because THIS route is requested under "/libpod/...",
// compat.StatsContainer's `IsLibpodRequest` branch takes the
// "return unconverted" path — but "unconverted" here still means podman's
// own compat.StatsJSON shape (cpu_stats/precpu_stats/memory_stats/
// pids_stats/blkio_stats/networks/read/preread, mirroring Docker's stats
// API), NOT define.ContainerStats. The per-name endpoint is also marked
// DEPRECATED in podman's swagger comments ("Please use
// /libpod/containers/stats instead") — still fully functional on 5.7.1
// (confirmed live) and is what the v0.5 plan and task brief both specify,
// so this client uses it as directed, decoding the compat shape (see
// statsWire below, whose fields were cross-checked against
// pkg/api/handlers/compat/types.go and vendor/github.com/docker/docker/
// api/types/container/stats.go at v5.7.1).
//
// libpod's internal sample period for this endpoint is a fixed 5 seconds
// (pkg/api/handlers/compat/containers_stats.go's defaultStatsPeriod) —
// confirmed by the ~5s gap between consecutive JSON objects in a captured
// stream=true response — so StreamStats emits at roughly that cadence, not
// on any interval this client controls.
//
// The returned ReadCloser is the raw HTTP response body: consecutive JSON
// objects (see StreamStats for parsing them), never wrapped in a JSON
// array or newline-delimited framing of their own — the caller owns
// closing it, which is what ends a live stream (mirroring ContainerLogs'
// follow=true contract).
//
// Returns ErrNotFound if the container doesn't exist.
func (c *Client) ContainerStats(ctx context.Context, nameOrID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/containers/"+url.PathEscape(nameOrID)+"/stats?stream=true", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman: stats %q: %w", nameOrID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiError(resp.StatusCode, data)
	}
	return resp.Body, nil
}

// ContainerStatsSample is BasePod's own stable, decoded representation of
// one point-in-time container resource-usage reading — deliberately NOT
// libpod's/Docker's wire struct (see ContainerStats' doc comment): this is
// the shape internal/api/stats.go serializes onto the stats SSE stream, so
// a future podman upgrade changing field names or nesting can't move the
// UI's wire contract out from under it.
type ContainerStatsSample struct {
	// CPUPercent is 0-100 per core (so a container fully using 2 cores on
	// a multi-core host reads 200), computed from cpu_stats vs precpu_stats
	// exactly like `docker stats`/`podman stats` — see calcCPUPercent.
	CPUPercent float64
	// MemUsedBytes/MemLimitBytes are memory_stats.usage/.limit verbatim.
	MemUsedBytes  uint64
	MemLimitBytes uint64
	// PIDs is pids_stats.current.
	PIDs uint64
	// NetRxBytes/NetTxBytes sum rx_bytes/tx_bytes across every interface
	// in the networks map (a container has exactly one on BasePod's shared
	// network today, but summing is correct regardless).
	NetRxBytes uint64
	NetTxBytes uint64
	// BlockReadBytes/BlockWriteBytes sum blkio_stats.io_service_bytes_recursive
	// entries by op — "Read"/"Write" (case-insensitive: podman's own
	// calculateBlockIO in libpod/stats_linux.go lowercases before
	// comparing, so this does too) — the same two ops `podman stats`
	// itself reports as block I/O.
	BlockReadBytes  uint64
	BlockWriteBytes uint64
}

// statsCPUUsage mirrors compat.CPUStats.CPUUsage / Docker's
// container.CPUUsage — only the sub-field this client needs.
type statsCPUUsage struct {
	TotalUsage uint64 `json:"total_usage"`
}

// statsCPUStats mirrors podman's compat.CPUStats (see ContainerStats' doc
// comment for the source trail) — only the sub-fields this client needs to
// compute CPUPercent.
type statsCPUStats struct {
	CPUUsage    statsCPUUsage `json:"cpu_usage"`
	SystemUsage uint64        `json:"system_cpu_usage"`
	OnlineCPUs  uint32        `json:"online_cpus"`
}

// statsBlkioEntry mirrors Docker's container.BlkioStatEntry.
type statsBlkioEntry struct {
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

// statsNetworkEntry mirrors Docker's container.NetworkStats — only the two
// fields this client sums.
type statsNetworkEntry struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// statsWire is the on-the-wire shape one JSON object from ContainerStats'
// stream decodes into — podman's compat.StatsJSON (see ContainerStats' doc
// comment), trimmed to the sub-fields BasePod actually consumes. Fields
// not captured here (Windows-only StorageStats/NumProcs, ThrottlingData,
// PerCPU usage, endpoint/instance IDs, ...) are intentionally dropped: see
// the ContainerStatsSample doc comment for why this client never passes
// the raw wire shape through.
type statsWire struct {
	CPUStats    statsCPUStats `json:"cpu_stats"`
	PreCPUStats statsCPUStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
	BlkioStats struct {
		IoServiceBytesRecursive []statsBlkioEntry `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	Networks map[string]statsNetworkEntry `json:"networks"`
}

// calcCPUPercent computes a CPU-percent reading from one wire frame's
// cpu_stats vs precpu_stats, using the same delta formula podman's own CLI
// (and Docker's before it) uses: the ratio of container CPU-nanoseconds
// consumed to total system CPU-nanoseconds elapsed over the same interval,
// scaled by the number of online CPUs and by 100. Guards against a zero or
// negative delta (the first sample of a fresh stream has cpu_stats ==
// precpu_stats byte-for-byte — see ContainerStats' doc comment — so this
// naturally reports 0% rather than NaN or a garbage spike on that first
// event) by returning 0 rather than dividing.
func calcCPUPercent(cur, prev statsCPUStats) float64 {
	cpuDelta := float64(cur.CPUUsage.TotalUsage) - float64(prev.CPUUsage.TotalUsage)
	systemDelta := float64(cur.SystemUsage) - float64(prev.SystemUsage)
	if systemDelta <= 0 || cpuDelta <= 0 {
		return 0
	}
	onlineCPUs := cur.OnlineCPUs
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}
	return (cpuDelta / systemDelta) * float64(onlineCPUs) * 100.0
}

// blockIOBytes sums entries' Value by Op, case-insensitively (see
// ContainerStatsSample's doc comment).
func blockIOBytes(entries []statsBlkioEntry) (read, write uint64) {
	for _, e := range entries {
		switch {
		case equalFoldASCII(e.Op, "read"):
			read += e.Value
		case equalFoldASCII(e.Op, "write"):
			write += e.Value
		}
	}
	return
}

// equalFoldASCII is a tiny case-insensitive ASCII compare, avoiding a
// strings import for this one call site's single use.
func equalFoldASCII(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := 0; i < len(s); i++ {
		a, b := s[i], t[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// decodeStatsSample converts one decoded statsWire frame into BasePod's
// stable ContainerStatsSample.
func decodeStatsSample(w statsWire) ContainerStatsSample {
	var netRx, netTx uint64
	for _, n := range w.Networks {
		netRx += n.RxBytes
		netTx += n.TxBytes
	}
	blockRead, blockWrite := blockIOBytes(w.BlkioStats.IoServiceBytesRecursive)
	return ContainerStatsSample{
		CPUPercent:      calcCPUPercent(w.CPUStats, w.PreCPUStats),
		MemUsedBytes:    w.MemoryStats.Usage,
		MemLimitBytes:   w.MemoryStats.Limit,
		PIDs:            w.PidsStats.Current,
		NetRxBytes:      netRx,
		NetTxBytes:      netTx,
		BlockReadBytes:  blockRead,
		BlockWriteBytes: blockWrite,
	}
}

// StreamStats reads r (the ReadCloser ContainerStats returns) as a
// sequence of concatenated JSON objects — libpod's wire framing for a
// streamed stats response, no delimiter of its own between objects, which
// is exactly what encoding/json.Decoder's successive Decode calls consume
// — decoding each into a ContainerStatsSample and calling emit for it in
// order.
//
// Mirrors DemuxLogs' EOF handling: returns nil (not an error) on a clean
// end of stream (io.EOF) or a stream truncated mid-object
// (io.ErrUnexpectedEOF) — the latter is how a live stream ends when the
// container stops and the daemon closes the connection between samples,
// the JSON-stream analogue of DemuxLogs' truncated-final-frame case.
func StreamStats(r io.Reader, emit func(ContainerStatsSample)) error {
	dec := json.NewDecoder(r)
	for {
		var w statsWire
		if err := dec.Decode(&w); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		emit(decodeStatsSample(w))
	}
}
