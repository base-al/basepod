package podman

import (
	"encoding/binary"
	"io"
	"strings"
)

// frameHeaderSize is the size of a libpod/Docker-compatible multiplex log
// frame header: 1 byte stream type, 3 reserved/unused bytes, 4 bytes
// big-endian payload length.
const frameHeaderSize = 8

// streamName maps a multiplex frame's stream-type byte to its name.
// Podman's container log stream (like Docker's before it) tags each frame
// 1 for stdout or 2 for stderr; any other value is a stream type this
// client doesn't understand and its payload is skipped entirely.
func streamName(streamType byte) string {
	switch streamType {
	case 1:
		return "stdout"
	case 2:
		return "stderr"
	default:
		return ""
	}
}

// DemuxLogs reads r as a stream of 8-byte-header multiplex frames (as
// returned by ContainerLogs) and calls emit(stream, line) for every
// complete line found, where stream is "stdout" or "stderr". A frame's
// payload may contain zero or more newlines; text after the last newline
// in a frame is buffered (per stream, so interleaved stdout/stderr frames
// don't corrupt each other's partial line) and prepended to that stream's
// next frame rather than emitted immediately, since it isn't a complete
// line yet.
//
// DemuxLogs returns nil (not an error) both on a clean end of stream
// (io.EOF reading the next frame header) and on a truncated final frame
// (a partial header, or a header whose declared payload length isn't
// fully available) — the latter is how a live `follow` stream ends when
// the container stops and the daemon closes the connection mid-frame.
// Either way, any partial line still buffered when the stream ends is
// flushed as a final line via emit before returning.
func DemuxLogs(r io.Reader, emit func(stream, line string)) error {
	header := make([]byte, frameHeaderSize)
	partial := make(map[byte]string, 2)

	flush := func() {
		for st, buf := range partial {
			if buf == "" {
				continue
			}
			emit(streamName(st), buf)
			partial[st] = ""
		}
	}

	for {
		if _, err := io.ReadFull(r, header); err != nil {
			flush()
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}

		streamType := header[0]
		length := binary.BigEndian.Uint32(header[4:8])

		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			flush()
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// Truncated final frame: the header arrived but its
				// declared payload didn't. Discard it (we can't trust a
				// partially written frame) — this is how a live `follow`
				// stream ends when the daemon closes the connection
				// mid-write.
				return nil
			}
			return err
		}

		name := streamName(streamType)
		if name == "" {
			continue // unknown stream type: skip this frame's payload
		}

		text := partial[streamType] + string(payload)
		lines := strings.Split(text, "\n")
		partial[streamType] = lines[len(lines)-1]
		for _, line := range lines[:len(lines)-1] {
			emit(name, line)
		}
	}
}
