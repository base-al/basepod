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

// BulkContainerStats streams every currently-running container's
// resource-usage stats from libpod's GET .../libpod/containers/stats
// (no {name} segment) — the batch stats route (v0.5 plan's follow-up
// milestone) driving GET /api/v1/stats: ONE connection to podman covering
// every running app, instead of one per-container connection the way the
// per-app route (ContainerStats above) necessarily works.
//
// Wire shape and behavior verified three ways against a live podman
// 6.0.1 client / 5.7.1 server (2026-08-07), each cross-checked against
// podman's own source at the matching v5.7.1 tag:
//
//  1. Route registration. pkg/api/server/register_containers.go registers
//     this exact path ("/libpod/containers/stats", no {name}) as
//     `r.HandleFunc(VersionedPath("/libpod/containers/stats"),
//     s.APIHandler(libpod.StatsContainer))` — the libpod-NATIVE handler
//     this time (unlike the per-{name} route, which — see ContainerStats'
//     doc comment — is actually served by the docker-compat handler
//     despite living under "/libpod/..."). Confirmed via
//     pkg/api/handlers/libpod/containers_stats.go: it decodes `containers`
//     (query array of names/IDs, optional), `stream` (bool, default
//     true), `interval` (int seconds, default 5), and `all` (bool,
//     default false) — and streams `entities.ContainerStatsReport`
//     (`{Error error, Stats []define.ContainerStats}`) values one per
//     tick via `json.Encoder.Encode` (which appends '\n' after each
//     value — confirmed newline-delimited framing in a live capture, but
//     StreamBulkStats' decode loop below doesn't depend on that: like
//     StreamStats, it decodes successive values with encoding/json's own
//     Decoder, which tolerates any whitespace, none, between them).
//     define.ContainerStats (libpod/define/containerstate.go) has NO
//     json tags, so the wire keys are its bare Go field names —
//     confirmed byte-for-byte against a live capture: `{"Error":null,
//     "Stats":[{"AvgCPU":...,"ContainerID":"...",  "Name":"...",
//     "PerCPU":null,"CPU":...,"CPUNano":...,"CPUSystemNano":...,
//     "SystemNano":...,"MemUsage":...,"MemLimit":...,"MemPerc":...,
//     "Network":{"eth0":{"RxBytes":...,"TxBytes":...,...}},
//     "BlockInput":...,"BlockOutput":...,"PIDs":...,"UpTime":...,
//     "Duration":...}]}`.
//
//  2. Default enumeration scope. pkg/domain/infra/abi/containers.go's
//     ContainerEngine.ContainerStats: with no `containers=` query values
//     and `all=false` (this client's chosen mode — see below), the
//     container set comes from `ic.Libpod.GetRunningContainers` — i.e.
//     omitting `containers=` already scopes to exactly "every running
//     container", matching this client's contract with no client-side
//     filtering needed for that part. Per-container errors while
//     collecting that set (a container stopped between listing and
//     reading its stats) are silently skipped in this mode
//     (`queryAll` — the container-vanished-mid-tick case a live,
//     ever-changing app fleet hits routinely) rather than failing the
//     whole tick — confirmed live: sending `curl
//     .../containers/stats?containers=<a-name-that-does-not-exist>`
//     returns a clean top-level 404 for the WHOLE request instead
//     (verified live), because passing explicit `containers=` names
//     switches the abi code to a DIFFERENT, non-skipping path
//     (`GetContainersByList`) — this is exactly why
//     BulkContainerStats deliberately never passes `containers=`: doing
//     so would make one vanished container able to kill the entire
//     stream. This client also verified live that `containers=`, when
//     used, filters correctly by exact name/ID — but that a `filters=`
//     query param (the label-filter shape ListContainers itself uses) is
//     NOT understood by this endpoint at all (silently ignored, still
//     returns every running container) — so this client relies on
//     ListContainers (labels) for BasePod-vs-foreign attribution
//     entirely client-side, matching every other per-tick sample against
//     the container-ID set deploy.Engine.RunningAppContainers returns,
//     rather than ever trying to filter podman's response itself.
//
//  3. CPU-percent scale (the specific trap worth flagging loudly: three
//     agents were burned this milestone by assuming libpod field names
//     without checking their SCALE, not just their spelling).
//     libpod/stats_linux.go's calculateCPUPercent — what actually
//     computes define.ContainerStats.CPU — is
//     `(cpuDelta/systemDelta)*100` with NO online-CPU-count factor
//     (unlike this package's own calcCPUPercent for the per-container
//     endpoint, which additionally multiplies by online_cpus to match
//     `docker stats`/`podman stats` CLI convention). The bulk wire shape
//     doesn't even expose online-CPU-count as a field. Concretely: a
//     container fully saturating 1 of a 5-core host reads ~20 from this
//     endpoint's raw CPU field but ~100 from the per-container endpoint's
//     cpu_percent (BasePod's existing per-app SSE contract, statsWire's
//     calcCPUPercent) — the SAME real usage, two different numbers, if
//     naively wired straight through. Confirmed by direct comparison: a
//     live sample from THIS endpoint (bp-caddy, CPU 0.0125) against the
//     same container's per-container-endpoint online_cpus (5) and this
//     host's GET /info host.cpus (5, see Client.HostCPUs) at the same
//     moment — 0.0125*5 = 0.0625, in the same ballpark as this app's
//     near-idle load, while 0.0125 alone would silently under-report by
//     5x. onlineCPUs below is the caller-supplied correction: BulkStats
//     Sample.CPUPercent is `wireCPU*onlineCPUs`, putting this endpoint's
//     numbers on the exact same 0-100-per-core scale
//     podman.ContainerStatsSample.CPUPercent already promises — so a
//     dashboard sparkline fed by this endpoint reads consistently with
//     any per-app view fed by the other one.
//
// The returned ReadCloser is the raw HTTP response body, same contract as
// ContainerStats' (caller owns closing it; that's what ends the stream).
// stream=true (the default the query omits, matching podman's own
// default) keeps it open indefinitely at libpod's own ~5s cadence — not a
// rate this client controls, mirroring ContainerStats.
func (c *Client) BulkContainerStats(ctx context.Context) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/containers/stats?stream=true", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman: bulk stats: %w", err)
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiError(resp.StatusCode, data)
	}
	return resp.Body, nil
}

// BulkStatsSample is one container's resource-usage reading within a
// single bulk-stats tick — BulkStatsSample's ContainerID/Name identify
// which container it's for (the batch route's caller,
// internal/api/allstats.go, maps ContainerID onto an app slug via
// deploy.Engine.RunningAppContainers), the rest mirrors
// ContainerStatsSample field-for-field (same units, same meaning) so both
// endpoints' samples can share one downstream shape.
type BulkStatsSample struct {
	ContainerID string
	Name        string

	// CPUPercent is already normalized by onlineCPUs (see
	// StreamBulkStats' onlineCPUs parameter and BulkContainerStats' doc
	// comment point 3) — 0-100 per core, matching
	// ContainerStatsSample.CPUPercent's scale exactly.
	CPUPercent float64

	MemUsedBytes  uint64
	MemLimitBytes uint64
	PIDs          uint64
	NetRxBytes    uint64
	NetTxBytes    uint64

	BlockReadBytes  uint64
	BlockWriteBytes uint64
}

// bulkNetworkEntry mirrors libpod/define.ContainerNetworkStats — no json
// tags (see BulkContainerStats' doc comment point 1), only the two fields
// this client sums.
type bulkNetworkEntry struct {
	RxBytes uint64
	TxBytes uint64
}

// bulkStatsWire is the on-the-wire shape of one entry in a bulk-stats
// tick's "Stats" array — libpod's define.ContainerStats (see
// BulkContainerStats' doc comment point 1), trimmed to the sub-fields
// BasePod actually consumes (AvgCPU/PerCPU/CPUNano/CPUSystemNano/
// SystemNano/MemPerc/UpTime/Duration are intentionally dropped, mirroring
// how statsWire already trims the per-container endpoint's shape).
type bulkStatsWire struct {
	ContainerID string
	Name        string
	CPU         float64
	MemUsage    uint64
	MemLimit    uint64
	PIDs        uint64
	Network     map[string]bulkNetworkEntry
	BlockInput  uint64
	BlockOutput uint64
}

// bulkStatsFrame is the on-the-wire shape of one line StreamBulkStats
// decodes — libpod's entities.ContainerStatsReport (see
// BulkContainerStats' doc comment point 1): `{Error error, Stats
// []define.ContainerStats}`. Error is decoded as raw JSON rather than a
// typed field: Go's `error` interface has no exported fields, so a
// non-nil error value with an unexported-only concrete type (e.g.
// *errors.errorString from errors.New/fmt.Errorf — the common case)
// marshals to `{}`, not a string — there is no reconstructable message,
// only a null-vs-non-null signal, which is all StreamBulkStats needs
// (see its doc comment for when this can actually happen).
type bulkStatsFrame struct {
	Error json.RawMessage `json:"Error"`
	Stats []bulkStatsWire `json:"Stats"`
}

// frameHasError reports whether a decoded bulkStatsFrame's Error field is
// present and non-null — `{}` (an error with no recoverable message,
// the common case) and any other non-null JSON value both count; a
// missing key or literal `null` (the overwhelmingly common per-tick case
// — see BulkContainerStats' doc comment point 2) does not.
func frameHasError(f bulkStatsFrame) bool {
	return len(f.Error) > 0 && string(f.Error) != "null"
}

// decodeBulkStatsSample converts one bulkStatsWire entry into a
// BulkStatsSample, applying onlineCPUs' CPU-percent correction (see
// BulkContainerStats' doc comment point 3) and summing Network/BlockIO
// exactly like decodeStatsSample does for the per-container endpoint.
func decodeBulkStatsSample(w bulkStatsWire, onlineCPUs int) BulkStatsSample {
	var netRx, netTx uint64
	for _, n := range w.Network {
		netRx += n.RxBytes
		netTx += n.TxBytes
	}
	return BulkStatsSample{
		ContainerID:     w.ContainerID,
		Name:            w.Name,
		CPUPercent:      w.CPU * float64(onlineCPUs),
		MemUsedBytes:    w.MemUsage,
		MemLimitBytes:   w.MemLimit,
		PIDs:            w.PIDs,
		NetRxBytes:      netRx,
		NetTxBytes:      netTx,
		BlockReadBytes:  w.BlockInput,
		BlockWriteBytes: w.BlockOutput,
	}
}

// StreamBulkStats reads r (the ReadCloser BulkContainerStats returns) as
// a sequence of JSON "tick" objects (see BulkContainerStats' doc comment
// point 1 for the exact framing/decode-tolerance reasoning, identical to
// StreamStats'), calling emit once per tick with that tick's full sample
// slice — one entry per container libpod reported stats for at that
// moment (every currently-running container on the host; see
// BulkContainerStats' doc comment point 2 — filtering down to BasePod's
// own apps is the caller's job, not this decoder's). onlineCPUs is
// applied to every sample's CPU% (see BulkContainerStats' doc comment
// point 3) — callers get it once via Client.HostCPUs before starting the
// stream, since a host's core count never changes mid-process.
//
// Mirrors StreamStats' EOF handling: a clean end of stream (io.EOF) or
// one truncated mid-object (io.ErrUnexpectedEOF) returns nil. A tick
// whose "Error" field is non-null (see frameHasError — libpod's own
// signal that container enumeration itself failed, e.g. a runtime-level
// failure rather than one container vanishing, which never surfaces this
// way — see BulkContainerStats' doc comment point 2) returns a genuine
// error and stops: libpod itself closes the stream right after emitting
// that tick (pkg/api/handlers/libpod/containers_stats.go's
// StatsContainer returns as soon as it forwards a report with a non-nil
// Error), so there is nothing further to read regardless.
func StreamBulkStats(r io.Reader, onlineCPUs int, emit func([]BulkStatsSample)) error {
	dec := json.NewDecoder(r)
	for {
		var f bulkStatsFrame
		if err := dec.Decode(&f); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if frameHasError(f) {
			return fmt.Errorf("podman: bulk stats tick reported an error: %s", f.Error)
		}
		samples := make([]BulkStatsSample, len(f.Stats))
		for i, w := range f.Stats {
			samples[i] = decodeBulkStatsSample(w, onlineCPUs)
		}
		emit(samples)
	}
}
