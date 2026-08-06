package build

import "io"

// defaultMaxLogBytes is Builder's default maxLogBytes (see its field doc
// comment): the number of bytes of build-log output Build will write to a
// build's log file before truncating it (see limitedLogWriter). A
// misbehaving or malicious Containerfile step (e.g. `RUN yes AAAA | head
// -c 50G`) can produce effectively unbounded stdout well within the
// build's own timeout; without a cap, streaming that verbatim to a log
// file under <dataDir>/apps/<slug>/builds/ could fill the data
// directory's disk — the same disk basepod.db and the encryption key
// (internal/crypto) live on — long before the build itself times out or
// the operator notices. 32 MiB is generous for any legitimate build's
// output while bounding the worst case to a small, fixed amount per
// build; the build itself is never affected by the cap — only the log.
const defaultMaxLogBytes int64 = 32 << 20 // 32 MiB

// logTruncationNotice is appended to a build log exactly once, the moment
// maxLogBytes is reached — see limitedLogWriter.Write.
const logTruncationNotice = "\n--- build log truncated at 32 MiB ---\n"

// limitedLogWriter wraps an underlying build-log writer (a *os.File in
// production; see Build) and caps how many bytes of build output it will
// ever forward to it: once cap bytes have been written, it appends
// logTruncationNotice exactly once and silently discards every byte after
// that.
//
// Write never returns an error of its own and always reports the full
// len(p) as written, regardless of how much (if any) of p actually
// reached the underlying writer once the cap is hit — a full log must
// never look like a failed write to its caller (podman.Client.BuildImage,
// which streams build output line by line): the BUILD keeps running
// normally for its own full timeout; only the log stops growing. An
// error from the underlying writer itself (e.g. a genuine disk-full
// before the cap is even reached) is still propagated, since that's a
// real I/O failure distinct from the writer's own capping behavior.
type limitedLogWriter struct {
	w         io.Writer
	cap       int64
	written   int64
	truncated bool
}

// newLimitedLogWriter returns a limitedLogWriter forwarding at most cap
// bytes to w before switching to silent-discard mode (see
// limitedLogWriter's doc comment). cap <= 0 is treated as "write nothing
// at all" (an immediately-truncated writer) rather than "unlimited" —
// Builder.maxLogBytes is always positive in practice (defaulted in New),
// so this only matters for a deliberately zeroed test fixture.
func newLimitedLogWriter(w io.Writer, cap int64) *limitedLogWriter {
	if cap < 0 {
		cap = 0
	}
	return &limitedLogWriter{w: w, cap: cap}
}

func (lw *limitedLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	if lw.written >= lw.cap {
		return n, nil
	}

	remaining := lw.cap - lw.written
	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
	}
	written, err := lw.w.Write(toWrite)
	lw.written += int64(written)
	if err != nil {
		return n, err
	}

	if lw.written >= lw.cap && !lw.truncated {
		lw.truncated = true
		// Best-effort: if this write itself fails there is nothing more
		// useful to do than continue silently discarding subsequent
		// output, exactly as if the notice had never been attempted.
		_, _ = lw.w.Write([]byte(logTruncationNotice))
	}
	return n, nil
}
