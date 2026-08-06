package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// sseEvent is one parsed Server-Sent Event: its event: line (defaulting to
// "message" per the SSE spec when the field is absent) and the
// concatenation of every data: line in the event.
type sseEvent struct {
	event string
	data  string
}

// readSSE reads Server-Sent Events from r, minimally: it recognizes only
// the "event:" and "data:" fields (everything else, including "id:" and
// "retry:", is ignored) and dispatches one sseEvent to fn per blank-line-
// terminated block. A line starting with ":" is a comment (BasePod's
// servers use this for heartbeats) and is skipped without ending the
// current event. Returns nil on a clean EOF (the stream ended, or fn asked
// to stop by returning false), or the underlying read error otherwise.
func readSSE(r io.Reader, fn func(sseEvent) (keepGoing bool)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var cur sseEvent
	var dataLines []string
	flush := func() (bool, bool) {
		if len(dataLines) == 0 && cur.event == "" {
			return true, false // nothing accumulated — not a real event
		}
		if cur.event == "" {
			cur.event = "message"
		}
		cur.data = strings.Join(dataLines, "\n")
		keep := fn(cur)
		cur = sseEvent{}
		dataLines = nil
		return keep, true
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			keep, had := flush()
			if had && !keep {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			// comment/heartbeat — ignored
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	// A final event not terminated by a trailing blank line still counts.
	flush()
	return sc.Err()
}

// logLinePayload mirrors internal/api's logEventPayload: the JSON body of
// each "event: log" message from GET .../logs.
type logLinePayload struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// printLogLine parses an "event: log" SSE event's data as JSON and returns
// its line text. Any other event (or malformed data) yields ok=false and
// is silently ignored by the caller — matching the brief's "ignore
// heartbeats" instruction generalized to "ignore anything that isn't a log
// line".
func parseLogLine(ev sseEvent) (line string, ok bool) {
	if ev.event != "log" {
		return "", false
	}
	var payload logLinePayload
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		return "", false
	}
	return payload.Line, true
}
