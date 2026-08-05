package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tarEntry is one file to write into a test build context.
type tarEntry struct {
	name string
	body string
}

// gzipTar builds a gzip-compressed tar stream from entries, for feeding
// into Builder.Build as a fake upload body.
func gzipTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &gzBuf
}

// fakeRuntime is a test double for BuildRuntime.
type fakeRuntime struct {
	mu sync.Mutex

	err        error
	logLines   []string
	calls      []buildCall
	blockUntil chan struct{} // if non-nil, BuildImage blocks reading from this until closed

	// started/entered signal test goroutines that a BuildImage call has
	// begun, before it blocks on blockUntil — used to prove per-app/global
	// concurrency serialization deterministically without sleeps.
	entered chan struct{}
}

type buildCall struct {
	tag        string
	dockerfile string
}

func (f *fakeRuntime) BuildImage(ctx context.Context, tag, dockerfile string, contextTar io.Reader, logSink io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, buildCall{tag: tag, dockerfile: dockerfile})
	f.mu.Unlock()

	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.blockUntil != nil {
		<-f.blockUntil
	}

	for _, l := range f.logLines {
		io.WriteString(logSink, l)
	}
	return f.err
}

func (f *fakeRuntime) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestBuildHappyPath(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{logLines: []string{"Step 1/1 : FROM alpine\n", "Successfully built abc123\n"}}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})

	tag, logPath, err := b.Build(context.Background(), "blog", 1, gz)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tag != "localhost/basepod/blog:1" {
		t.Fatalf("tag = %q, want localhost/basepod/blog:1", tag)
	}
	if logPath != filepath.Join(dataDir, "apps", "blog", "builds", "1.log") {
		t.Fatalf("logPath = %q", logPath)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "Step 1/1 : FROM alpine\nSuccessfully built abc123\n"
	if string(data) != want {
		t.Fatalf("log contents = %q, want %q", data, want)
	}
	if len(rt.calls) != 1 || rt.calls[0].dockerfile != "Containerfile" {
		t.Fatalf("calls = %+v, want one call with dockerfile=Containerfile", rt.calls)
	}

	// No leftover spool files.
	entries, err := os.ReadDir(filepath.Join(dataDir, "builds"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("builds/ spool dir = %v, want empty (spool must be removed)", entries)
	}
}

func TestBuildDockerfileFallback(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{{name: "Dockerfile", body: "FROM alpine\n"}})

	if _, _, err := b.Build(context.Background(), "blog", 1, gz); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rt.calls) != 1 || rt.calls[0].dockerfile != "Dockerfile" {
		t.Fatalf("calls = %+v, want one call with dockerfile=Dockerfile", rt.calls)
	}
}

func TestBuildPrefersContainerfileWhenBothPresent(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{
		{name: "Containerfile", body: "FROM alpine\n"},
		{name: "Dockerfile", body: "FROM debian\n"},
	})

	if _, _, err := b.Build(context.Background(), "blog", 1, gz); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rt.calls[0].dockerfile != "Containerfile" {
		t.Fatalf("dockerfile = %q, want Containerfile preferred", rt.calls[0].dockerfile)
	}
}

func TestBuildNoContainerfile(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{{name: "readme.txt", body: "hi\n"}})

	_, logPath, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, ErrNoContainerfile) {
		t.Fatalf("err = %v, want ErrNoContainerfile", err)
	}
	if logPath != "" {
		t.Fatalf("logPath = %q, want empty (build never reached the log-creating stage)", logPath)
	}
	if rt.callCount() != 0 {
		t.Fatalf("BuildImage should never have been called")
	}
}

func TestBuildBadPathTraversal(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{
		{name: "Containerfile", body: "FROM alpine\n"},
		{name: "../evil", body: "pwned\n"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, ErrBadPath) {
		t.Fatalf("err = %v, want ErrBadPath", err)
	}
	if rt.callCount() != 0 {
		t.Fatalf("BuildImage should never have been called")
	}
}

func TestBuildBadPathAbsolute(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{
		{name: "Containerfile", body: "FROM alpine\n"},
		{name: "/etc/passwd", body: "pwned\n"},
	})

	_, _, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, ErrBadPath) {
		t.Fatalf("err = %v, want ErrBadPath", err)
	}
}

// TestBuildFailureWritesLogAndCleansSpool proves a BuildImage error still
// leaves the (partial) build log on disk with whatever was streamed
// before the failure, and still cleans up the spool file, while
// propagating the error and returning "" for the image tag.
func TestBuildFailureWritesLogAndCleansSpool(t *testing.T) {
	dataDir := t.TempDir()
	buildErr := errors.New("executor failed running [/bin/sh -c false]")
	rt := &fakeRuntime{logLines: []string{"Step 1/1 : RUN false\n"}, err: buildErr}
	b := New(rt, dataDir, 2)

	gz := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\nRUN false\n"}})

	tag, logPath, err := b.Build(context.Background(), "blog", 1, gz)
	if !errors.Is(err, buildErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, buildErr)
	}
	if tag != "" {
		t.Fatalf("tag = %q, want empty on failure", tag)
	}
	if logPath == "" {
		t.Fatal("logPath empty, want the build log path even on failure")
	}
	data, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != "Step 1/1 : RUN false\n" {
		t.Fatalf("log contents = %q", data)
	}

	entries, rerr := os.ReadDir(filepath.Join(dataDir, "builds"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("builds/ spool dir = %v, want empty even after a build failure", entries)
	}
}

// TestBuildRejectsNonGzipUpload proves an upload that isn't valid gzip is
// rejected up front rather than reaching tar validation.
func TestBuildRejectsNonGzipUpload(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 2)

	_, _, err := b.Build(context.Background(), "blog", 1, strings.NewReader("not gzip at all"))
	if err == nil {
		t.Fatal("expected an error for a non-gzip upload")
	}
	if rt.callCount() != 0 {
		t.Fatal("BuildImage should never have been called")
	}
}

// TestBuildSerializesPerApp proves two concurrent Build calls for the
// *same* app slug never run at once: the second blocks until the first
// (still inside BuildImage) completes.
func TestBuildSerializesPerApp(t *testing.T) {
	dataDir := t.TempDir()
	block := make(chan struct{})
	entered := make(chan struct{}, 2)
	rt := &fakeRuntime{blockUntil: block, entered: entered}
	b := New(rt, dataDir, 2) // global concurrency 2, so only the per-app lock can be what serializes this

	gz1 := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})
	gz2 := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := b.Build(context.Background(), "blog", 1, gz1)
		firstDone <- err
	}()
	<-entered // first call is now inside BuildImage, blocked on `block`

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, _, err := b.Build(context.Background(), "blog", 2, gz2)
		secondDone <- err
	}()
	<-secondStarted

	// The second call must not have entered BuildImage yet (it's parked
	// on the per-app mutex behind the first, still-blocked call).
	select {
	case <-entered:
		t.Fatal("second Build call entered BuildImage while the first (same app) was still in flight — per-app serialization not enforced")
	case <-time.After(50 * time.Millisecond):
	}

	close(block) // release both calls' BuildImage invocations

	if err := <-firstDone; err != nil {
		t.Fatalf("first Build: %v", err)
	}
	<-entered // second call's BuildImage now runs (and returns instantly, since block is closed)
	if err := <-secondDone; err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if rt.callCount() != 2 {
		t.Fatalf("callCount = %d, want 2", rt.callCount())
	}
}

// TestBuildGlobalConcurrencyLimit proves at most maxConcurrent builds
// (across different apps) run BuildImage at once; a build beyond the cap
// waits for the semaphore.
func TestBuildGlobalConcurrencyLimit(t *testing.T) {
	dataDir := t.TempDir()
	block := make(chan struct{})
	entered := make(chan struct{}, 3)
	rt := &fakeRuntime{blockUntil: block, entered: entered}
	b := New(rt, dataDir, 2) // global cap of 2

	start := func(slug string, n int) chan error {
		gz := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})
		done := make(chan error, 1)
		go func() {
			_, _, err := b.Build(context.Background(), slug, n, gz)
			done <- err
		}()
		return done
	}

	done1 := start("app1", 1)
	done2 := start("app2", 1)
	<-entered
	<-entered // both of the first two are now inside BuildImage, blocked

	done3 := start("app3", 1)
	select {
	case <-entered:
		t.Fatal("third Build call entered BuildImage while 2 others (the configured max) were already in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(block)
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := <-done3; err != nil {
		t.Fatal(err)
	}
}

// TestBuildContextCanceledWaitingForSemaphore proves a Build call whose
// ctx is canceled while waiting for a free semaphore slot returns the
// context's error rather than blocking forever.
func TestBuildContextCanceledWaitingForSemaphore(t *testing.T) {
	dataDir := t.TempDir()
	block := make(chan struct{})
	defer close(block)
	entered := make(chan struct{}, 1)
	rt := &fakeRuntime{blockUntil: block, entered: entered}
	b := New(rt, dataDir, 1) // global cap of 1

	gz1 := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})
	go b.Build(context.Background(), "app1", 1, gz1)
	<-entered // holds the only slot, blocked

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gz2 := gzipTar(t, []tarEntry{{name: "Containerfile", body: "FROM alpine\n"}})
	_, _, err := b.Build(ctx, "app2", 1, gz2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func fatalIfErrNotContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want it to contain %q", err, substr)
	}
}

// TestBuildEmptyUploadIsNotValidGzip is a sanity regression proving an
// empty body is rejected as invalid gzip rather than panicking or hanging.
func TestBuildEmptyUploadIsNotValidGzip(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	b := New(rt, dataDir, 1)
	_, _, err := b.Build(context.Background(), "blog", 1, bytes.NewReader(nil))
	fatalIfErrNotContains(t, err, "gzip")
}
