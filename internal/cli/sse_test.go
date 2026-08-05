package cli

import (
	"strings"
	"testing"
)

func TestReadSSEParsesLogEventsAndIgnoresHeartbeats(t *testing.T) {
	stream := "" +
		": heartbeat\n\n" +
		"event: log\ndata: {\"stream\":\"stdout\",\"line\":\"hello world\"}\n\n" +
		": heartbeat\n\n" +
		"event: log\ndata: {\"stream\":\"stderr\",\"line\":\"oops\"}\n\n"

	var lines []string
	err := readSSE(strings.NewReader(stream), func(ev sseEvent) bool {
		if line, ok := parseLogLine(ev); ok {
			lines = append(lines, line)
		}
		return true
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	want := []string{"hello world", "oops"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestReadSSEStopsWhenCallbackReturnsFalse(t *testing.T) {
	stream := "event: log\ndata: {\"line\":\"one\"}\n\n" +
		"event: log\ndata: {\"line\":\"two\"}\n\n" +
		"event: log\ndata: {\"line\":\"three\"}\n\n"

	var lines []string
	err := readSSE(strings.NewReader(stream), func(ev sseEvent) bool {
		line, _ := parseLogLine(ev)
		lines = append(lines, line)
		return len(lines) < 2
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2 (stopped early)", lines)
	}
}

func TestParseLogLineIgnoresNonLogEvents(t *testing.T) {
	if _, ok := parseLogLine(sseEvent{event: "message", data: "{}"}); ok {
		t.Fatal("want ok=false for a non-log event")
	}
	if _, ok := parseLogLine(sseEvent{event: "log", data: "not json"}); ok {
		t.Fatal("want ok=false for malformed JSON data")
	}
}

func TestReadSSEMultilineData(t *testing.T) {
	stream := "event: log\ndata: {\"line\":\"a\"}\ndata: b\n\n"
	var got sseEvent
	err := readSSE(strings.NewReader(stream), func(ev sseEvent) bool {
		got = ev
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.data != "{\"line\":\"a\"}\nb" {
		t.Fatalf("data = %q, want joined multi-line data", got.data)
	}
}
