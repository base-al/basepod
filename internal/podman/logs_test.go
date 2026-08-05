package podman

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"testing"
)

// frame builds one multiplex log frame: an 8-byte header (stream type,
// 3 reserved bytes, big-endian payload length) followed by payload.
func frame(streamType byte, payload string) []byte {
	h := make([]byte, frameHeaderSize)
	h[0] = streamType
	binary.BigEndian.PutUint32(h[4:8], uint32(len(payload)))
	return append(h, []byte(payload)...)
}

type demuxed struct {
	stream, line string
}

func collect(t *testing.T, data []byte) ([]demuxed, error) {
	t.Helper()
	var got []demuxed
	err := DemuxLogs(bytes.NewReader(data), func(stream, line string) {
		got = append(got, demuxed{stream, line})
	})
	return got, err
}

func TestDemuxLogsInterleavedStreams(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hello\n"))
	buf.Write(frame(2, "world\n"))
	buf.Write(frame(1, "foo\n"))

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{
		{"stdout", "hello"},
		{"stderr", "world"},
		{"stdout", "foo"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDemuxLogsLineSplitAcrossFrames(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hel"))
	buf.Write(frame(1, "lo\nworld\n"))

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{
		{"stdout", "hello"},
		{"stdout", "world"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestDemuxLogsLineSplitPerStream proves the partial-line buffer is kept
// separately per stream type: an in-flight stdout partial line must not be
// corrupted by an interleaved stderr frame arriving before it's completed.
func TestDemuxLogsLineSplitPerStream(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "stdout-par"))
	buf.Write(frame(2, "stderr-line\n"))
	buf.Write(frame(1, "tial\n"))

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{
		{"stderr", "stderr-line"},
		{"stdout", "stdout-partial"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDemuxLogsPartialLineFlushedAtEOF(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "complete\n"))
	buf.Write(frame(1, "no trailing newline"))

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{
		{"stdout", "complete"},
		{"stdout", "no trailing newline"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDemuxLogsUnknownStreamTypeSkipped(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "before\n"))
	buf.Write(frame(99, "should be ignored\n"))
	buf.Write(frame(1, "after\n"))

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{
		{"stdout", "before"},
		{"stdout", "after"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestDemuxLogsTruncatedFinalFrameHeader covers a stream that ends mid
// header (fewer than 8 bytes remaining) — as if the daemon closed the
// connection while writing a new frame's header.
func TestDemuxLogsTruncatedFinalFrameHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "complete\n"))
	buf.Write([]byte{1, 0, 0}) // 3 bytes of a truncated 8-byte header

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{{"stdout", "complete"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestDemuxLogsTruncatedFinalFramePayload covers a stream that ends with a
// full header declaring a payload length longer than what's actually
// available — as if the daemon closed the connection mid-write.
func TestDemuxLogsTruncatedFinalFramePayload(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "complete\n"))
	h := make([]byte, frameHeaderSize)
	h[0] = 1
	binary.BigEndian.PutUint32(h[4:8], 100) // declares 100 bytes of payload
	buf.Write(h)
	buf.WriteString("only 10b") // far short of 100

	got, err := collect(t, buf.Bytes())
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{{"stdout", "complete"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDemuxLogsCleanEOFNoTrailingPartial(t *testing.T) {
	got, err := collect(t, frame(1, "one line only\n"))
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	want := []demuxed{{"stdout", "one line only"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDemuxLogsEmptyInput(t *testing.T) {
	got, err := collect(t, nil)
	if err != nil {
		t.Fatalf("DemuxLogs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// errReader always returns a non-EOF error, to prove DemuxLogs propagates
// a genuine I/O error (as opposed to swallowing it the way it swallows
// EOF/truncation).
type errReader struct{ err error }

func (r errReader) Read(p []byte) (int, error) { return 0, r.err }

func TestDemuxLogsPropagatesReadError(t *testing.T) {
	wantErr := io.ErrClosedPipe
	err := DemuxLogs(errReader{wantErr}, func(string, string) {})
	if err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
