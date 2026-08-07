package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// liveCaddyStatsSample is a byte-for-byte capture (2026-08-06) of one JSON
// object from a live podman 6.0.1 client / 5.7.1 server's response to
// `curl --unix-socket <sock> http://d/v5.0.0/libpod/containers/bp-caddy/stats?stream=true`
// — the second object in a two-sample stream (see ContainerStats' doc
// comment for the full verification trail), i.e. a "real" delta sample
// with distinct cpu_stats/precpu_stats readings, not the degenerate
// first-sample case where the two are identical.
const liveCaddyStatsSample = `{"read":"2026-08-06T23:49:32.787286539+02:00","preread":"2026-08-06T23:49:27.782514132+02:00","pids_stats":{"current":11},"blkio_stats":{"io_service_bytes_recursive":[{"major":0,"minor":253,"op":"read","value":45178880},{"major":0,"minor":253,"op":"write","value":1200000},{"major":0,"minor":253,"op":"rios","value":391},{"major":0,"minor":253,"op":"wios","value":20},{"major":0,"minor":253,"op":"dbytes","value":0},{"major":0,"minor":253,"op":"dios","value":0}],"io_serviced_recursive":null,"io_queue_recursive":null,"io_service_time_recursive":null,"io_wait_time_recursive":null,"io_merged_recursive":null,"io_time_recursive":null,"sectors_recursive":null},"num_procs":0,"storage_stats":{},"cpu_stats":{"cpu_usage":{"total_usage":3835950000,"usage_in_kernelmode":1059536,"usage_in_usermode":3829254464},"system_cpu_usage":45449120000000,"online_cpus":5,"cpu":0.05,"throttling_data":{"periods":0,"throttled_periods":0,"throttled_time":0}},"precpu_stats":{"cpu_usage":{"total_usage":3835656000,"usage_in_kernelmode":1059536,"usage_in_usermode":3829254464},"system_cpu_usage":45449109566000,"cpu":0.0157,"throttling_data":{"periods":0,"throttled_periods":0,"throttled_time":0}},"memory_stats":{"usage":11882496,"limit":8293711872},"name":"bp-caddy","Id":"d7aef7babfdbaf63e7c9ef05feb660594936f3e43abfc9da15f7def8e8fa1dde","networks":{"eth0":{"rx_bytes":3691,"rx_packets":46,"rx_errors":0,"rx_dropped":0,"tx_bytes":2452,"tx_packets":34,"tx_errors":0,"tx_dropped":0}}}`

// liveFirstSample mirrors a stream's first emitted object, where podman's
// compat handler seeds precpu_stats from the very same read as cpu_stats
// (see ContainerStats' doc comment) — cpu_usage.total_usage and
// system_cpu_usage identical between the two.
const liveFirstSample = `{"read":"2026-08-06T23:49:27.782514132+02:00","preread":"2026-08-06T23:49:27.782514132+02:00","pids_stats":{"current":11},"blkio_stats":{"io_service_bytes_recursive":[]},"cpu_stats":{"cpu_usage":{"total_usage":3835656000},"system_cpu_usage":45449109566000,"online_cpus":5},"precpu_stats":{"cpu_usage":{"total_usage":3835656000},"system_cpu_usage":45449109566000},"memory_stats":{"usage":11882496,"limit":8293711872},"name":"bp-caddy","Id":"abc","networks":{"eth0":{"rx_bytes":100,"tx_bytes":50}}}`

// TestDecodeStatsSampleLiveCapture proves decodeStatsSample turns a
// byte-for-byte capture of podman's real wire response into the expected
// BasePod ContainerStatsSample, including the standard
// (cpuDelta/systemDelta)*onlineCPUs*100 CPU-percent formula.
func TestDecodeStatsSampleLiveCapture(t *testing.T) {
	var w statsWire
	if err := json.Unmarshal([]byte(liveCaddyStatsSample), &w); err != nil {
		t.Fatal(err)
	}
	got := decodeStatsSample(w)

	wantCPU := (float64(3835950000-3835656000) / float64(45449120000000-45449109566000)) * 5 * 100.0
	if diff := got.CPUPercent - wantCPU; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CPUPercent = %v, want %v", got.CPUPercent, wantCPU)
	}
	if got.MemUsedBytes != 11882496 {
		t.Errorf("MemUsedBytes = %d, want 11882496", got.MemUsedBytes)
	}
	if got.MemLimitBytes != 8293711872 {
		t.Errorf("MemLimitBytes = %d, want 8293711872", got.MemLimitBytes)
	}
	if got.PIDs != 11 {
		t.Errorf("PIDs = %d, want 11", got.PIDs)
	}
	if got.NetRxBytes != 3691 || got.NetTxBytes != 2452 {
		t.Errorf("net = rx:%d tx:%d, want rx:3691 tx:2452", got.NetRxBytes, got.NetTxBytes)
	}
	if got.BlockReadBytes != 45178880 || got.BlockWriteBytes != 1200000 {
		t.Errorf("block = read:%d write:%d, want read:45178880 write:1200000", got.BlockReadBytes, got.BlockWriteBytes)
	}
}

// TestDecodeStatsSampleFirstSampleIsZeroCPU proves the degenerate first
// sample of a stream (cpu_stats byte-identical to precpu_stats — see
// ContainerStats' doc comment) decodes to CPUPercent 0, not a divide
// artifact, since cpuDelta is exactly 0.
func TestDecodeStatsSampleFirstSampleIsZeroCPU(t *testing.T) {
	var w statsWire
	if err := json.Unmarshal([]byte(liveFirstSample), &w); err != nil {
		t.Fatal(err)
	}
	got := decodeStatsSample(w)
	if got.CPUPercent != 0 {
		t.Errorf("CPUPercent = %v, want 0 for a degenerate first sample", got.CPUPercent)
	}
	if got.MemUsedBytes != 11882496 {
		t.Errorf("MemUsedBytes = %d, want 11882496", got.MemUsedBytes)
	}
}

// TestCalcCPUPercentGuardsNonPositiveDeltas proves calcCPUPercent returns 0
// (rather than a negative or NaN/Inf value) whenever either delta is zero
// or negative — e.g. a system clock adjustment, or (mid-stream) a decode
// glitch — matching podman's own client-side robustness for this calc.
func TestCalcCPUPercentGuardsNonPositiveDeltas(t *testing.T) {
	cases := []struct {
		name string
		cur  statsCPUStats
		prev statsCPUStats
	}{
		{"zero cpu delta", statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 100}, SystemUsage: 200, OnlineCPUs: 4}, statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 100}, SystemUsage: 100, OnlineCPUs: 4}},
		{"negative cpu delta", statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 50}, SystemUsage: 300, OnlineCPUs: 4}, statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 100}, SystemUsage: 100, OnlineCPUs: 4}},
		{"zero system delta", statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 200}, SystemUsage: 100, OnlineCPUs: 4}, statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 100}, SystemUsage: 100, OnlineCPUs: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calcCPUPercent(tc.cur, tc.prev); got != 0 {
				t.Errorf("calcCPUPercent = %v, want 0", got)
			}
		})
	}
}

// TestCalcCPUPercentZeroOnlineCPUsDefaultsToOne proves a wire frame that
// omits online_cpus (decoding to the zero value) doesn't zero out the
// whole calculation: it's treated as 1 core rather than multiplying the
// ratio by 0, since a missing/zero online_cpus is a degraded-input case,
// not a legitimate "container gets 0 CPUs" signal.
func TestCalcCPUPercentZeroOnlineCPUsDefaultsToOne(t *testing.T) {
	cur := statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 300}, SystemUsage: 1000, OnlineCPUs: 0}
	prev := statsCPUStats{CPUUsage: statsCPUUsage{TotalUsage: 100}, SystemUsage: 500}
	got := calcCPUPercent(cur, prev)
	want := (200.0 / 500.0) * 1 * 100.0
	if got != want {
		t.Errorf("calcCPUPercent = %v, want %v", got, want)
	}
}

// TestBlockIOBytesCaseInsensitiveOp proves op matching is case-insensitive
// (podman/runc have been observed to emit both "Read"/"Write" and
// lowercase "read"/"write" across versions — libpod's own
// calculateBlockIO lowercases before comparing; see stats.go's doc
// comment), and that unrelated ops (rios/wios/dbytes/dios) are ignored.
func TestBlockIOBytesCaseInsensitiveOp(t *testing.T) {
	entries := []statsBlkioEntry{
		{Op: "Read", Value: 10},
		{Op: "WRITE", Value: 20},
		{Op: "read", Value: 5},
		{Op: "rios", Value: 999},
	}
	read, write := blockIOBytes(entries)
	if read != 15 {
		t.Errorf("read = %d, want 15", read)
	}
	if write != 20 {
		t.Errorf("write = %d, want 20", write)
	}
}

// TestStreamStatsEmitsEachConcatenatedObject proves StreamStats decodes a
// sequence of concatenated JSON objects (libpod's actual wire framing —
// no array wrapper, no newline delimiter) into one emit call each, in
// order.
func TestStreamStatsEmitsEachConcatenatedObject(t *testing.T) {
	raw := liveFirstSample + liveCaddyStatsSample // concatenated, no separator
	var got []ContainerStatsSample
	err := StreamStats(strings.NewReader(raw), func(s ContainerStatsSample) {
		got = append(got, s)
	})
	if err != nil {
		t.Fatalf("StreamStats: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if got[0].CPUPercent != 0 {
		t.Errorf("sample 0 CPUPercent = %v, want 0", got[0].CPUPercent)
	}
	if got[1].CPUPercent <= 0 {
		t.Errorf("sample 1 CPUPercent = %v, want > 0", got[1].CPUPercent)
	}
}

// TestStreamStatsCleanEOF proves a clean end of stream (no partial object)
// returns nil, not an error.
func TestStreamStatsCleanEOF(t *testing.T) {
	if err := StreamStats(strings.NewReader(""), func(ContainerStatsSample) {
		t.Fatal("emit should not be called for an empty stream")
	}); err != nil {
		t.Fatalf("StreamStats: %v, want nil", err)
	}
}

// TestStreamStatsTruncatedFinalObject proves a stream that ends mid-object
// (the daemon closing the connection between the header and a full body —
// how a live stream ends when the container stops) returns nil rather than
// an error, mirroring DemuxLogs' identical truncated-final-frame handling.
func TestStreamStatsTruncatedFinalObject(t *testing.T) {
	truncated := liveFirstSample + `{"read":"2026","cpu_stat`
	var got []ContainerStatsSample
	err := StreamStats(strings.NewReader(truncated), func(s ContainerStatsSample) {
		got = append(got, s)
	})
	if err != nil {
		t.Fatalf("StreamStats: %v, want nil for a truncated final object", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1 (only the complete first object)", len(got))
	}
}

// TestStreamStatsPropagatesGenuineDecodeError proves a stream containing
// genuinely malformed (not merely truncated) JSON returns a real error.
func TestStreamStatsPropagatesGenuineDecodeError(t *testing.T) {
	err := StreamStats(strings.NewReader(`not json at all`), func(ContainerStatsSample) {
		t.Fatal("emit should not be called")
	})
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// TestContainerStatsRequestsStreamTrue proves ContainerStats hits the
// exact path+query this file's doc comments document
// ("/v5.0.0/libpod/containers/{name}/stats?stream=true") and returns the
// raw body unparsed for the caller (StreamStats) to decode.
func TestContainerStatsRequestsStreamTrue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/bp-blog-1/stats", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("stream"); got != "true" {
			t.Errorf("stream = %q, want true", got)
		}
		w.WriteHeader(200)
		io.WriteString(w, liveFirstSample)
	})
	c := fakeDaemon(t, mux)

	rc, err := c.ContainerStats(context.Background(), "bp-blog-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(liveFirstSample)) {
		t.Fatalf("body = %q, want the raw scripted response (unparsed)", got)
	}
}

// TestContainerStatsNotFound proves an unknown container maps to
// ErrNotFound, mirroring ContainerLogs' identical handling.
func TestContainerStatsNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/nope/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"cause": "no such container"})
	})
	c := fakeDaemon(t, mux)
	_, err := c.ContainerStats(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// liveBulkStatsTickOneContainer is a byte-for-byte capture (2026-08-07) of
// one line from a live podman 6.0.1 client / 5.7.1 server's response to
// `curl --unix-socket <sock> "http://d/v5.0.0/libpod/containers/stats?stream=false&containers=bp-caddy"`
// — see BulkContainerStats' doc comment for the full verification trail.
const liveBulkStatsTickOneContainer = `{"Error":null,"Stats":[{"AvgCPU":0.012472037152735335,"ContainerID":"d7aef7babfdbaf63e7c9ef05feb660594936f3e43abfc9da15f7def8e8fa1dde","Name":"bp-caddy","PerCPU":null,"CPU":0.012472037152735335,"CPUNano":4676114000,"CPUSystemNano":1383053,"SystemNano":1786066073281580181,"MemUsage":11886592,"MemLimit":8293711872,"MemPerc":0.1433205322713193,"Network":{"eth0":{"RxBytes":4181,"RxDropped":0,"RxErrors":0,"RxPackets":53,"TxBytes":2662,"TxDropped":0,"TxErrors":0,"TxPackets":37}},"BlockInput":45178880,"BlockOutput":755712,"PIDs":11,"UpTime":4676114000,"Duration":4676114000}]}`

// liveBulkStatsTickTwoContainers is a byte-for-byte capture (2026-08-07)
// of one line from the same live daemon's response with no `containers=`
// filter — every currently-running container on the host, including one
// (lisa-shell-build) that carries no basepod.managed label, proving this
// endpoint (unlike ListContainers) does no BasePod-vs-foreign filtering
// of its own; see BulkContainerStats' doc comment point 2.
const liveBulkStatsTickTwoContainers = `{"Error":null,"Stats":[{"AvgCPU":0.8351721515337748,"ContainerID":"ad4107191c804d3d97a46ee97e33f9c7a853006c3e651181e3b014949a6b28a7","Name":"lisa-shell-build","PerCPU":null,"CPU":0.8351721515337748,"CPUNano":1892691628000,"CPUSystemNano":78952788,"SystemNano":1786066016374656767,"MemUsage":136683520,"MemLimit":8293711872,"MemPerc":1.6480379606801945,"Network":{"eth0":{"RxBytes":548431857,"RxDropped":0,"RxErrors":0,"RxPackets":99919,"TxBytes":2250022,"TxDropped":0,"TxErrors":0,"TxPackets":38981}},"BlockInput":37580800,"BlockOutput":3710695936,"PIDs":1023,"UpTime":1892691628000,"Duration":1892691628000},{"AvgCPU":0.012487109302159374,"ContainerID":"d7aef7babfdbaf63e7c9ef05feb660594936f3e43abfc9da15f7def8e8fa1dde","Name":"bp-caddy","PerCPU":null,"CPU":0.012487109302159374,"CPUNano":4674659000,"CPUSystemNano":1381801,"SystemNano":1786066016375147186,"MemUsage":11886592,"MemLimit":8293711872,"MemPerc":0.1433205322713193,"Network":{"eth0":{"RxBytes":4181,"RxDropped":0,"RxErrors":0,"RxPackets":53,"TxBytes":2662,"TxDropped":0,"TxErrors":0,"TxPackets":37}},"BlockInput":45178880,"BlockOutput":755712,"PIDs":11,"UpTime":4674659000,"Duration":4674659000}]}`

// TestStreamBulkStatsLiveCapture proves StreamBulkStats decodes a
// byte-for-byte capture of podman's real bulk-stats wire response into
// the expected BulkStatsSample, in particular that CPUPercent is the raw
// wire CPU multiplied by the supplied onlineCPUs (see BulkContainerStats'
// doc comment point 3 for why that correction is necessary at all).
func TestStreamBulkStatsLiveCapture(t *testing.T) {
	var got []BulkStatsSample
	err := StreamBulkStats(strings.NewReader(liveBulkStatsTickOneContainer), 5, func(tick []BulkStatsSample) {
		got = append(got, tick...)
	})
	if err != nil {
		t.Fatalf("StreamBulkStats: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1", len(got))
	}
	s := got[0]
	if s.ContainerID != "d7aef7babfdbaf63e7c9ef05feb660594936f3e43abfc9da15f7def8e8fa1dde" {
		t.Errorf("ContainerID = %q", s.ContainerID)
	}
	if s.Name != "bp-caddy" {
		t.Errorf("Name = %q, want bp-caddy", s.Name)
	}
	wantCPU := 0.012472037152735335 * 5
	if diff := s.CPUPercent - wantCPU; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CPUPercent = %v, want %v (raw CPU * onlineCPUs)", s.CPUPercent, wantCPU)
	}
	if s.MemUsedBytes != 11886592 || s.MemLimitBytes != 8293711872 {
		t.Errorf("mem = %d/%d, want 11886592/8293711872", s.MemUsedBytes, s.MemLimitBytes)
	}
	if s.PIDs != 11 {
		t.Errorf("PIDs = %d, want 11", s.PIDs)
	}
	if s.NetRxBytes != 4181 || s.NetTxBytes != 2662 {
		t.Errorf("net = rx:%d tx:%d, want rx:4181 tx:2662", s.NetRxBytes, s.NetTxBytes)
	}
	if s.BlockReadBytes != 45178880 || s.BlockWriteBytes != 755712 {
		t.Errorf("block = read:%d write:%d, want read:45178880 write:755712", s.BlockReadBytes, s.BlockWriteBytes)
	}
}

// TestStreamBulkStatsMultiContainerTick proves a single tick carrying
// multiple containers' stats (the normal shape when no `containers=`
// filter is passed — every running container on the host, see
// BulkContainerStats' doc comment point 2) is delivered to emit as one
// slice, in the wire order.
func TestStreamBulkStatsMultiContainerTick(t *testing.T) {
	var ticks [][]BulkStatsSample
	err := StreamBulkStats(strings.NewReader(liveBulkStatsTickTwoContainers), 5, func(tick []BulkStatsSample) {
		ticks = append(ticks, tick)
	})
	if err != nil {
		t.Fatalf("StreamBulkStats: %v", err)
	}
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1", len(ticks))
	}
	if len(ticks[0]) != 2 {
		t.Fatalf("got %d samples in the tick, want 2", len(ticks[0]))
	}
	if ticks[0][0].Name != "lisa-shell-build" || ticks[0][1].Name != "bp-caddy" {
		t.Errorf("unexpected sample order: %q, %q", ticks[0][0].Name, ticks[0][1].Name)
	}
}

// TestStreamBulkStatsEmitsPerTick proves multiple newline-delimited ticks
// (this endpoint's actual live framing — see BulkContainerStats' doc
// comment point 1) each produce their own emit call, in order.
func TestStreamBulkStatsEmitsPerTick(t *testing.T) {
	raw := liveBulkStatsTickOneContainer + "\n" + liveBulkStatsTickOneContainer
	var ticks int
	err := StreamBulkStats(strings.NewReader(raw), 1, func(tick []BulkStatsSample) {
		ticks++
	})
	if err != nil {
		t.Fatalf("StreamBulkStats: %v", err)
	}
	if ticks != 2 {
		t.Fatalf("got %d ticks, want 2", ticks)
	}
}

// TestStreamBulkStatsCleanEOF proves a clean end of stream (no partial
// object) returns nil, not an error — mirrors TestStreamStatsCleanEOF.
func TestStreamBulkStatsCleanEOF(t *testing.T) {
	if err := StreamBulkStats(strings.NewReader(""), 1, func([]BulkStatsSample) {
		t.Fatal("emit should not be called for an empty stream")
	}); err != nil {
		t.Fatalf("StreamBulkStats: %v, want nil", err)
	}
}

// TestStreamBulkStatsTruncatedFinalObject proves a stream that ends
// mid-object (how a live stream ends when the daemon closes the
// connection) returns nil rather than an error — mirrors
// TestStreamStatsTruncatedFinalObject.
func TestStreamBulkStatsTruncatedFinalObject(t *testing.T) {
	truncated := liveBulkStatsTickOneContainer + "\n" + `{"Error":null,"Stat`
	var ticks int
	err := StreamBulkStats(strings.NewReader(truncated), 1, func([]BulkStatsSample) {
		ticks++
	})
	if err != nil {
		t.Fatalf("StreamBulkStats: %v, want nil for a truncated final object", err)
	}
	if ticks != 1 {
		t.Fatalf("got %d ticks, want 1 (only the complete first object)", ticks)
	}
}

// TestStreamBulkStatsPropagatesGenuineDecodeError proves a stream
// containing genuinely malformed (not merely truncated) JSON returns a
// real error — mirrors TestStreamStatsPropagatesGenuineDecodeError.
func TestStreamBulkStatsPropagatesGenuineDecodeError(t *testing.T) {
	err := StreamBulkStats(strings.NewReader(`not json at all`), 1, func([]BulkStatsSample) {
		t.Fatal("emit should not be called")
	})
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// TestStreamBulkStatsStopsOnFrameError proves a tick whose "Error" field
// is non-null (libpod's signal that container enumeration itself failed —
// see BulkContainerStats' doc comment point 2 and frameHasError's doc
// comment for why this is checked as raw JSON rather than a typed field)
// stops the stream with a real error, without calling emit for that tick.
func TestStreamBulkStatsStopsOnFrameError(t *testing.T) {
	frame := `{"Error":{},"Stats":null}`
	err := StreamBulkStats(strings.NewReader(frame), 1, func([]BulkStatsSample) {
		t.Fatal("emit should not be called for an error tick")
	})
	if err == nil {
		t.Fatal("expected an error for a non-null Error field")
	}
}

// TestFrameHasErrorNullVsPresent proves frameHasError treats a missing
// key or literal `null` (the normal per-tick case) as "no error", and any
// other non-null JSON value (however uninformative — see bulkStatsFrame's
// doc comment on why a non-nil Go error often marshals to `{}`) as an
// error.
func TestFrameHasErrorNullVsPresent(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"missing key", `{"Stats":[]}`, false},
		{"literal null", `{"Error":null,"Stats":[]}`, false},
		{"empty object", `{"Error":{},"Stats":null}`, true},
		{"string message", `{"Error":"boom","Stats":null}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f bulkStatsFrame
			if err := json.Unmarshal([]byte(tc.json), &f); err != nil {
				t.Fatal(err)
			}
			if got := frameHasError(f); got != tc.want {
				t.Errorf("frameHasError(%s) = %v, want %v", tc.json, got, tc.want)
			}
		})
	}
}

// TestBulkContainerStatsRequestsStreamTrueNoContainersFilter proves
// BulkContainerStats hits the exact path this file's doc comments
// document ("/v5.0.0/libpod/containers/stats") with stream=true and,
// critically, NO "containers=" query value — see BulkContainerStats' doc
// comment point 2 for why passing one would make a single vanished
// container able to kill the whole stream.
func TestBulkContainerStatsRequestsStreamTrueNoContainersFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/stats", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("stream"); got != "true" {
			t.Errorf("stream = %q, want true", got)
		}
		if r.URL.Query().Has("containers") {
			t.Errorf("containers query param present (%q), want absent", r.URL.Query().Get("containers"))
		}
		w.WriteHeader(200)
		io.WriteString(w, liveBulkStatsTickOneContainer)
	})
	c := fakeDaemon(t, mux)

	rc, err := c.BulkContainerStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(liveBulkStatsTickOneContainer)) {
		t.Fatalf("body = %q, want the raw scripted response (unparsed)", got)
	}
}

// TestBulkContainerStatsErrorStatus proves a non-2xx response surfaces as
// an error (this route has no single-resource ErrNotFound the way the
// per-container endpoint does — it's not scoped to any one container
// name).
func TestBulkContainerStatsErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.BulkContainerStats(context.Background()); err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
}
