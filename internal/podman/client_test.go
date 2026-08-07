package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeDaemon spins up an httptest server listening on a Unix socket and
// returns a Client pointed at it, mimicking a local podman daemon closely
// enough to exercise Client's request/response handling.
//
// t.TempDir() paths can exceed macOS's 104-char sun_path limit for Unix
// sockets once nested under the test's temp root; fall back to a short
// os.MkdirTemp("", "bp") directory (cleaned up explicitly) if so.
func fakeDaemon(t *testing.T, mux *http.ServeMux) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "podman.sock")
	if len(sock) > 100 {
		dir, err := os.MkdirTemp("", "bp")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock = filepath.Join(dir, "podman.sock")
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := New("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateContainerSpec(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": "abc123"})
	})
	c := fakeDaemon(t, mux)
	id, err := c.CreateContainer(context.Background(), CreateSpec{
		Name: "bp-blog-1", Image: "nginx:alpine",
		Labels:         map[string]string{"basepod.app": "blog"},
		NetworkName:    "basepod",
		NetworkAliases: []string{"bp-blog"},
		RestartPolicy:  "always",
	})
	if err != nil || id != "abc123" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if got["name"] != "bp-blog-1" || got["image"] != "nginx:alpine" {
		t.Fatalf("spec body: %v", got)
	}
	nets := got["Networks"].(map[string]any)
	if _, ok := nets["basepod"]; !ok {
		t.Fatalf("network missing: %v", got)
	}
	if got["restart_policy"] != "always" {
		t.Fatalf("restart policy: %v", got)
	}
	// netns must be explicitly set to bridge mode alongside a populated
	// Networks map: some podman versions (observed: 4.9.3, as shipped on
	// GitHub Actions' ubuntu-24.04 runners) reject the request otherwise
	// ("networks and static ip/mac address can only be used with Bridge
	// mode networking"), even though the CLI infers it automatically.
	netns, ok := got["netns"].(map[string]any)
	if !ok {
		t.Fatalf("netns missing: %v", got)
	}
	if netns["nsmode"] != "bridge" {
		t.Fatalf("netns.nsmode = %v, want %q", netns["nsmode"], "bridge")
	}
}

// TestCreateContainerNoNetworkOmitsNetNS covers the other branch: a
// container created without NetworkName (none exist in BasePod today, but
// nothing should break if one is ever added) must not send a netns
// override, since there is no Networks map for it to accompany.
func TestCreateContainerNoNetworkOmitsNetNS(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": "abc123"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.CreateContainer(context.Background(), CreateSpec{Name: "x", Image: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["netns"]; ok {
		t.Fatalf("netns should be omitted when NetworkName is empty: %v", got)
	}
	if _, ok := got["Networks"]; ok {
		t.Fatalf("Networks should be omitted when NetworkName is empty: %v", got)
	}
}

// TestCreateContainerHardeningAlwaysApplied covers audit finding H2's
// fixed hardening: every container gets no-new-privileges and a NET_RAW
// capability drop, even one with no resource limits set at all — this
// isn't gated by CreateSpec (see CreateContainer's doc comment for why).
func TestCreateContainerHardeningAlwaysApplied(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": "abc123"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.CreateContainer(context.Background(), CreateSpec{Name: "x", Image: "y"}); err != nil {
		t.Fatal(err)
	}
	if got["no_new_privileges"] != true {
		t.Fatalf("no_new_privileges = %v, want true", got["no_new_privileges"])
	}
	capDrop, ok := got["cap_drop"].([]any)
	if !ok || len(capDrop) != 1 || capDrop[0] != "NET_RAW" {
		t.Fatalf("cap_drop = %v, want [NET_RAW]", got["cap_drop"])
	}
}

// TestCreateContainerResourceLimits covers audit finding H2's resource
// caps: MemoryLimitBytes/CPUQuota/PidsLimit map onto libpod's
// resource_limits.{memory.limit, cpu.quota, cpu.period, pids.limit} —
// field names verified against podman v5.7.1's actual specgen source (see
// the v0.4 Task 2 report). CPUQuota is in cores; period is fixed at
// cpuPeriodMicros (100ms), so 1.5 cores -> quota 150000.
func TestCreateContainerResourceLimits(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": "abc123"})
	})
	c := fakeDaemon(t, mux)
	_, err := c.CreateContainer(context.Background(), CreateSpec{
		Name: "x", Image: "y",
		MemoryLimitBytes: 512 * 1024 * 1024,
		CPUQuota:         1.5,
		PidsLimit:        256,
	})
	if err != nil {
		t.Fatal(err)
	}

	rl, ok := got["resource_limits"].(map[string]any)
	if !ok {
		t.Fatalf("resource_limits missing: %v", got)
	}

	mem, ok := rl["memory"].(map[string]any)
	if !ok || mem["limit"] != float64(512*1024*1024) {
		t.Fatalf("resource_limits.memory = %v, want limit=536870912", rl["memory"])
	}

	cpu, ok := rl["cpu"].(map[string]any)
	if !ok || cpu["quota"] != float64(150000) || cpu["period"] != float64(100000) {
		t.Fatalf("resource_limits.cpu = %v, want quota=150000 period=100000", rl["cpu"])
	}

	pids, ok := rl["pids"].(map[string]any)
	if !ok || pids["limit"] != float64(256) {
		t.Fatalf("resource_limits.pids = %v, want limit=256", rl["pids"])
	}
}

// TestCreateContainerZeroLimitsOmitResourceLimits covers the "zero means
// unlimited" contract (CreateSpec's doc comment): when
// MemoryLimitBytes/CPUQuota/PidsLimit are all left at their zero value
// (e.g. internal/caddy.Manager's own CreateSpec, which never sets them),
// resource_limits must be omitted from the wire request entirely — not
// present with a zero/empty value, and not regressing an unlimited
// container into an accidentally memory/cpu/pids-capped one.
func TestCreateContainerZeroLimitsOmitResourceLimits(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": "abc123"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.CreateContainer(context.Background(), CreateSpec{Name: "x", Image: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["resource_limits"]; ok {
		t.Fatalf("resource_limits should be omitted when all limits are 0: %v", got)
	}
}

func TestStopAlreadyStoppedIsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/containers/x/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(304)
	})
	c := fakeDaemon(t, mux)
	if err := c.StopContainer(context.Background(), "x", 10); err != nil {
		t.Fatal(err)
	}
}

func TestInspectNotFound(t *testing.T) {
	c := fakeDaemon(t, http.NewServeMux()) // 404 everything
	if _, err := c.InspectContainer(context.Background(), "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPullImageStreamError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/images/pull", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("{\"stream\":\"x\"}\n{\"error\":\"manifest unknown\"}\n"))
	})
	c := fakeDaemon(t, mux)
	err := c.PullImage(context.Background(), "example.com/missing:latest")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("want error containing %q, got %v", "manifest unknown", err)
	}
}

// TestPullImageStreamErrorSurvivesTrailingGarbage regression-tests that a
// captured stream error is not discarded when the stream ends with a
// decode failure (e.g. a trailing non-JSON line from an
// interrupted/malformed stream) after the error line.
func TestPullImageStreamErrorSurvivesTrailingGarbage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/images/pull", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("{\"stream\":\"x\"}\n{\"error\":\"manifest unknown\"}\nnot-json-trailer\n"))
	})
	c := fakeDaemon(t, mux)
	err := c.PullImage(context.Background(), "example.com/missing:latest")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("want error containing %q, got %v", "manifest unknown", err)
	}
}

// TestBuildImageStreamsLogAndSetsTagDockerfile proves BuildImage sends
// dockerfile/t as query params, writes every {"stream":...} line verbatim
// to logSink, and returns nil on a clean stream.
func TestBuildImageStreamsLogAndSetsTagDockerfile(t *testing.T) {
	var gotQuery url.Values
	var gotContentType string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/build", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotContentType = r.Header.Get("Content-Type")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.Write([]byte("{\"stream\":\"Step 1/2 : FROM alpine\\n\"}\n{\"stream\":\"Step 2/2 : RUN true\\n\"}\n"))
	})
	c := fakeDaemon(t, mux)

	var logBuf bytes.Buffer
	err := c.BuildImage(context.Background(), "localhost/basepod/blog:1", "Containerfile", strings.NewReader("fake-tar-bytes"), &logBuf)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if gotQuery.Get("dockerfile") != "Containerfile" || gotQuery.Get("t") != "localhost/basepod/blog:1" {
		t.Fatalf("query = %v, want dockerfile=Containerfile t=localhost/basepod/blog:1", gotQuery)
	}
	if gotContentType != "application/x-tar" {
		t.Fatalf("Content-Type = %q, want application/x-tar", gotContentType)
	}
	want := "Step 1/2 : FROM alpine\nStep 2/2 : RUN true\n"
	if logBuf.String() != want {
		t.Fatalf("logSink = %q, want %q", logBuf.String(), want)
	}
}

// TestBuildImageDefaultsDockerfileToContainerfile proves an empty
// dockerfile argument defaults the query param to "Containerfile".
func TestBuildImageDefaultsDockerfileToContainerfile(t *testing.T) {
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/build", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(200)
		w.Write([]byte("{\"stream\":\"ok\\n\"}\n"))
	})
	c := fakeDaemon(t, mux)
	if err := c.BuildImage(context.Background(), "x:1", "", strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("dockerfile") != "Containerfile" {
		t.Fatalf("dockerfile = %q, want Containerfile", gotQuery.Get("dockerfile"))
	}
}

// TestBuildImageStreamError proves a {"error":...} line within an
// otherwise-200 body surfaces as an error (mirroring PullImage's stream
// error handling).
func TestBuildImageStreamError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/build", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("{\"stream\":\"Step 1/2\\n\"}\n{\"error\":\"executor failed running [/bin/sh -c false]\"}\n"))
	})
	c := fakeDaemon(t, mux)
	err := c.BuildImage(context.Background(), "x:1", "Containerfile", strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "executor failed running") {
		t.Fatalf("err = %v, want it to contain the stream error message", err)
	}
}

// TestBuildImageStreamErrorPrefersErrorDetailMessage proves that when a
// stream error line carries both a flat "error" string and a nested
// "errorDetail":{"message":...}, the more specific errorDetail message
// wins.
func TestBuildImageStreamErrorPrefersErrorDetailMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/build", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"errorDetail":{"message":"detailed failure reason"},"error":"generic failure"}` + "\n"))
	})
	c := fakeDaemon(t, mux)
	err := c.BuildImage(context.Background(), "x:1", "Containerfile", strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "detailed failure reason") {
		t.Fatalf("err = %v, want it to contain the errorDetail message", err)
	}
	if err != nil && strings.Contains(err.Error(), "generic failure") {
		t.Fatalf("err = %v, want the flat 'error' message to be overridden by errorDetail", err)
	}
}

// TestBuildImageNon2xxStatus proves a non-2xx HTTP response (e.g. a
// malformed request rejected before any streaming begins) is reported via
// apiError rather than treated as a (nonexistent) JSON stream.
func TestBuildImageNon2xxStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v5.0.0/libpod/build", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{"message": "internal build error", "response": 500})
	})
	c := fakeDaemon(t, mux)
	err := c.BuildImage(context.Background(), "x:1", "Containerfile", strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "internal build error") {
		t.Fatalf("err = %v, want it to contain %q", err, "internal build error")
	}
}

// TestImageExistsTrue proves ImageExists reports true on a 204 response
// from libpod's GET /images/{ref}/exists.
// TestImageExistsTrue registers a catch-all handler (rather than a
// literal multi-segment ServeMux pattern) because ref contains a '/':
// ImageExists now escapes the whole ref (see its doc comment and
// TestImageExistsEscapesRefPathSegment), so it no longer arrives as
// several literal path segments a plain pattern could match — the handler
// instead asserts on the exact escaped request path it received, proving
// this 204 really did come from the request ImageExists sent (not from
// falling through to ServeMux's own default 404, which would have made a
// "not found" assertion pass vacuously).
func TestImageExistsTrue(t *testing.T) {
	ref := "localhost/basepod/blog:3"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := "/v5.0.0/libpod/images/" + url.PathEscape(ref) + "/exists"
		if r.URL.EscapedPath() != want {
			t.Errorf("request path = %q, want %q", r.URL.EscapedPath(), want)
		}
		w.WriteHeader(204)
	})
	c := fakeDaemon(t, mux)
	ok, err := c.ImageExists(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ImageExists = false, want true")
	}
}

// TestImageExistsFalse proves ImageExists reports false (not an error) on
// a 404 response. See TestImageExistsTrue for why this uses a catch-all
// handler asserting on the escaped path rather than a literal pattern.
func TestImageExistsFalse(t *testing.T) {
	ref := "example.com/missing:latest"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := "/v5.0.0/libpod/images/" + url.PathEscape(ref) + "/exists"
		if r.URL.EscapedPath() != want {
			t.Errorf("request path = %q, want %q", r.URL.EscapedPath(), want)
		}
		w.WriteHeader(404)
	})
	c := fakeDaemon(t, mux)
	ok, err := c.ImageExists(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ImageExists = true, want false")
	}
}

// TestImageExistsErrorStatus proves a non-204/404 status surfaces as an
// error rather than being silently treated as true/false.
func TestImageExistsErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/images/x/exists", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.ImageExists(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

// TestImageArchitectureReturnsArch proves ImageArchitecture decodes the
// "Architecture" field out of libpod's GET /images/{ref}/json response, via
// a catch-all handler asserting on the escaped path for the same reason as
// TestImageExistsTrue.
func TestImageArchitectureReturnsArch(t *testing.T) {
	ref := "docker.io/library/caddy:2.10-alpine"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := "/v5.0.0/libpod/images/" + url.PathEscape(ref) + "/json"
		if r.URL.EscapedPath() != want {
			t.Errorf("request path = %q, want %q", r.URL.EscapedPath(), want)
		}
		json.NewEncoder(w).Encode(map[string]any{"Architecture": "amd64"})
	})
	c := fakeDaemon(t, mux)
	arch, err := c.ImageArchitecture(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if arch != "amd64" {
		t.Fatalf("ImageArchitecture = %q, want %q", arch, "amd64")
	}
}

// TestImageArchitectureNotFound proves a 404 (image not present at all)
// surfaces as ErrNotFound, mirroring InspectContainer's not-found handling.
func TestImageArchitectureNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/images/x/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"cause": "no such image", "message": "no such image", "response": 404})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.ImageArchitecture(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestImageArchitectureErrorStatus proves a non-200/404 status surfaces as
// an error.
func TestImageArchitectureErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/images/x/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.ImageArchitecture(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

// TestRemoveImageAlreadyGoneIsSuccess proves RemoveImage treats a 404
// (image already gone) as success, mirroring RemoveContainer. Uses a
// catch-all handler asserting on the escaped path (see TestImageExistsTrue
// for why) rather than a literal ServeMux pattern.
func TestRemoveImageAlreadyGoneIsSuccess(t *testing.T) {
	ref := "localhost/basepod/blog:1"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := "/v5.0.0/libpod/images/" + url.PathEscape(ref)
		if r.URL.EscapedPath() != want {
			t.Errorf("request path = %q, want %q", r.URL.EscapedPath(), want)
		}
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"cause": "no such image", "message": "no such image", "response": 404})
	})
	c := fakeDaemon(t, mux)
	if err := c.RemoveImage(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
}

// TestRemoveImageForceQueryParam proves force=true is sent as a query
// param when requested.
func TestRemoveImageForceQueryParam(t *testing.T) {
	ref := "localhost/basepod/blog:1"
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := "/v5.0.0/libpod/images/" + url.PathEscape(ref)
		if r.URL.EscapedPath() != want {
			t.Errorf("request path = %q, want %q", r.URL.EscapedPath(), want)
		}
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	if err := c.RemoveImage(context.Background(), ref, true); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "force=true" {
		t.Fatalf("query = %q, want force=true", gotQuery)
	}
}

// TestImageExistsEscapesRefPathSegment proves ImageExists percent-encodes
// the whole ref before splicing it into the URL path — matching every
// other method in this file that takes a name/ref (see
// CreateContainer/InspectContainer/EnsureNetwork's url.PathEscape calls) —
// so a ref containing '/' or ':' travels as one opaque, unambiguous path
// segment rather than as raw text an HTTP client's own URL parsing could
// misinterpret. Checked via the raw (still-escaped) request line, since
// Go's URL parsing auto-decodes r.URL.Path for a receiving handler
// regardless of how the client encoded it.
func TestImageExistsEscapesRefPathSegment(t *testing.T) {
	var gotRawPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		w.WriteHeader(204)
	})
	c := fakeDaemon(t, mux)
	ref := "docker.io/lib/x:1"
	if _, err := c.ImageExists(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	want := "/v5.0.0/libpod/images/" + url.PathEscape(ref) + "/exists"
	if gotRawPath != want {
		t.Fatalf("raw request path = %q, want %q", gotRawPath, want)
	}
}

// TestImageExistsHostileRefCannotInjectQueryOrRetargetRoute proves a ref
// crafted to look like a path-traversal + query-injection payload
// ("evil/../networks/basepod?x=") cannot make ImageExists's request land
// on a different libpod endpoint (the networks route) or leak a literal
// '?' into the actual query string. Before url.PathEscape, string-
// concatenating this ref straight into the request URL split it at the
// unescaped '?': everything from "?" onward (including the intended
// "/exists" suffix) became the query string instead of path — verified
// directly against net/http's own URL parsing, independent of this fake.
// Escaping the whole ref first closes that off: the literal '?' becomes
// "%3F", which is never treated as a query delimiter.
func TestImageExistsHostileRefCannotInjectQueryOrRetargetRoute(t *testing.T) {
	hitNetworks := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		hitNetworks = true
		w.WriteHeader(204)
	})
	var gotRawPath, gotRawQuery string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(404) // this daemon has no route for the (escaped, opaque) hostile ref
	})
	c := fakeDaemon(t, mux)

	ref := "evil/../networks/basepod?x="
	ok, err := c.ImageExists(context.Background(), ref)
	if hitNetworks {
		t.Fatal("hostile ref reached the /networks/basepod route — the image ref leaked into request routing")
	}
	if err != nil {
		t.Fatalf("ImageExists: %v (want a clean false, not an error, for the fake's 404)", err)
	}
	if ok {
		t.Fatal("ImageExists reported true against a 404 response")
	}
	if gotRawQuery != "" {
		t.Fatalf("raw query = %q, want empty — the ref's literal '?' must never be interpreted as a query delimiter", gotRawQuery)
	}
	want := "/v5.0.0/libpod/images/" + url.PathEscape(ref) + "/exists"
	if gotRawPath != want {
		t.Fatalf("raw request path = %q, want %q", gotRawPath, want)
	}
}

// TestRemoveImageEscapesRefPathSegment mirrors
// TestImageExistsEscapesRefPathSegment for RemoveImage, additionally
// proving the "?force=true" query suffix is still appended correctly
// after the escaped ref.
func TestRemoveImageEscapesRefPathSegment(t *testing.T) {
	var gotRawPath, gotRawQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	ref := "docker.io/lib/x:1"
	if err := c.RemoveImage(context.Background(), ref, true); err != nil {
		t.Fatal(err)
	}
	want := "/v5.0.0/libpod/images/" + url.PathEscape(ref)
	if gotRawPath != want {
		t.Fatalf("raw request path = %q, want %q", gotRawPath, want)
	}
	if gotRawQuery != "force=true" {
		t.Fatalf("raw query = %q, want force=true", gotRawQuery)
	}
}

// TestRemoveImageHostileRefCannotInjectQueryOrRetargetRoute mirrors
// TestImageExistsHostileRefCannotInjectQueryOrRetargetRoute for
// RemoveImage.
func TestRemoveImageHostileRefCannotInjectQueryOrRetargetRoute(t *testing.T) {
	hitNetworks := false
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v5.0.0/libpod/networks/basepod", func(w http.ResponseWriter, _ *http.Request) {
		hitNetworks = true
		w.WriteHeader(200)
	})
	var gotRawQuery string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(404)
	})
	c := fakeDaemon(t, mux)

	ref := "evil/../networks/basepod?x="
	err := c.RemoveImage(context.Background(), ref, false)
	if hitNetworks {
		t.Fatal("hostile ref reached the /networks/basepod route — the image ref leaked into request routing")
	}
	// A 404 is treated as success by RemoveImage (already-gone image), so
	// this must return nil, not an error.
	if err != nil {
		t.Fatalf("RemoveImage: %v (want nil — a 404 is treated as success)", err)
	}
	if gotRawQuery != "" {
		t.Fatalf("raw query = %q, want empty — the ref's literal '?' must never be interpreted as a query delimiter", gotRawQuery)
	}
}

// TestListImageTagsFiltersByPrefix proves ListImageTags sends a
// reference filter for "<repoPrefix>:*" and collects only the RepoTags
// actually matching that prefix out of the response.
func TestListImageTagsFiltersByPrefix(t *testing.T) {
	var gotFilters string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/images/json", func(w http.ResponseWriter, r *http.Request) {
		gotFilters = r.URL.Query().Get("filters")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "i1", "RepoTags": []string{"localhost/basepod/blog:1", "localhost/basepod/blog:2"}},
			{"Id": "i2", "RepoTags": []string{"localhost/basepod/blog:3"}},
			{"Id": "i3", "RepoTags": []string{"localhost/basepod/other:1"}}, // must be excluded
			{"Id": "i4", "RepoTags": []string{"<none>:<none>"}},             // dangling, must be excluded
		})
	})
	c := fakeDaemon(t, mux)
	tags, err := c.ListImageTags(context.Background(), "localhost/basepod/blog")
	if err != nil {
		t.Fatal(err)
	}
	var filtersDecoded map[string][]string
	if err := json.Unmarshal([]byte(gotFilters), &filtersDecoded); err != nil {
		t.Fatalf("decoding sent filters: %v", err)
	}
	if len(filtersDecoded["reference"]) != 1 || filtersDecoded["reference"][0] != "localhost/basepod/blog:*" {
		t.Fatalf("filters = %v, want reference=[localhost/basepod/blog:*]", filtersDecoded)
	}
	want := []string{"localhost/basepod/blog:1", "localhost/basepod/blog:2", "localhost/basepod/blog:3"}
	sort.Strings(tags)
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}

// TestListImageTagsEmpty proves an empty result list decodes to an empty
// (not nil-panicking) slice.
func TestListImageTagsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/images/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	c := fakeDaemon(t, mux)
	tags, err := c.ListImageTags(context.Background(), "localhost/basepod/blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Fatalf("tags = %v, want none", tags)
	}
}

func TestRemoveContainerAlreadyGoneIsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v5.0.0/libpod/containers/x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"cause": "no such container", "message": "no such container", "response": 404})
	})
	c := fakeDaemon(t, mux)
	if err := c.RemoveContainer(context.Background(), "x", true); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureNetworkCreatesWhenMissing(t *testing.T) {
	var createBody map[string]any
	existsCalls, createCalls := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		existsCalls++
		w.WriteHeader(404)
	})
	mux.HandleFunc("POST /v5.0.0/libpod/networks/create", func(w http.ResponseWriter, r *http.Request) {
		createCalls++
		json.NewDecoder(r.Body).Decode(&createBody)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"name": "basepod"})
	})
	c := fakeDaemon(t, mux)
	if err := c.EnsureNetwork(context.Background(), "basepod", ""); err != nil {
		t.Fatal(err)
	}
	if existsCalls != 1 || createCalls != 1 {
		t.Fatalf("existsCalls=%d createCalls=%d", existsCalls, createCalls)
	}
	if createBody["name"] != "basepod" {
		t.Fatalf("create body: %v", createBody)
	}
	labels, _ := createBody["labels"].(map[string]any)
	if labels["basepod.managed"] != "true" {
		t.Fatalf("create body labels: %v", createBody)
	}
	// DNS must be explicitly requested: unlike the podman CLI, the raw
	// libpod API defaults dns_enabled to false, which would leave
	// containers unable to resolve each other by name/alias.
	if createBody["dns_enabled"] != true {
		t.Fatalf("create body dns_enabled: %v, want true", createBody["dns_enabled"])
	}
}

// TestEnsureNetworkCreatesWithInstanceLabel proves a non-empty instanceID
// argument gets stamped onto the network as basepod.instance (issue #10),
// alongside the always-present basepod.managed label.
func TestEnsureNetworkCreatesWithInstanceLabel(t *testing.T) {
	var createBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	})
	mux.HandleFunc("POST /v5.0.0/libpod/networks/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&createBody)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"name": "basepod"})
	})
	c := fakeDaemon(t, mux)
	if err := c.EnsureNetwork(context.Background(), "basepod", "inst-abc123"); err != nil {
		t.Fatal(err)
	}
	labels, _ := createBody["labels"].(map[string]any)
	if labels["basepod.managed"] != "true" {
		t.Fatalf("create body labels: %v", createBody)
	}
	if labels["basepod.instance"] != "inst-abc123" {
		t.Fatalf("create body labels[basepod.instance] = %v, want %q", labels["basepod.instance"], "inst-abc123")
	}
}

// TestEnsureNetworkNoopWhenPresent proves EnsureNetwork does nothing
// (beyond the existence and DNS self-heal checks) when the network exists
// and its DNS resolution is already enabled.
func TestEnsureNetworkNoopWhenPresent(t *testing.T) {
	createCalls, removeCalls := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"name": "basepod", "dns_enabled": true})
	})
	mux.HandleFunc("POST /v5.0.0/libpod/networks/create", func(w http.ResponseWriter, _ *http.Request) {
		createCalls++
		w.WriteHeader(200)
	})
	mux.HandleFunc("DELETE /v5.0.0/libpod/networks/basepod", func(w http.ResponseWriter, _ *http.Request) {
		removeCalls++
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	if err := c.EnsureNetwork(context.Background(), "basepod", ""); err != nil {
		t.Fatal(err)
	}
	if createCalls != 0 {
		t.Fatalf("expected no create call, got %d", createCalls)
	}
	if removeCalls != 0 {
		t.Fatalf("expected no remove call, got %d", removeCalls)
	}
}

// TestEnsureNetworkSelfHealsDNSWhenNoContainers proves that a
// DNS-disabled network with zero basepod-managed containers attached is
// removed and recreated with DNS enabled.
func TestEnsureNetworkSelfHealsDNSWhenNoContainers(t *testing.T) {
	var createBody map[string]any
	removeCalls, createCalls := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"name": "basepod", "dns_enabled": false})
	})
	mux.HandleFunc("GET /v5.0.0/libpod/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode([]map[string]any{}) // no basepod containers attached
	})
	mux.HandleFunc("DELETE /v5.0.0/libpod/networks/basepod", func(w http.ResponseWriter, _ *http.Request) {
		removeCalls++
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v5.0.0/libpod/networks/create", func(w http.ResponseWriter, r *http.Request) {
		createCalls++
		json.NewDecoder(r.Body).Decode(&createBody)
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	if err := c.EnsureNetwork(context.Background(), "basepod", ""); err != nil {
		t.Fatal(err)
	}
	if removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1", removeCalls)
	}
	if createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", createCalls)
	}
	if createBody["dns_enabled"] != true {
		t.Fatalf("recreate body dns_enabled = %v, want true", createBody["dns_enabled"])
	}
}

// TestEnsureNetworkWarnsWhenDNSDisabledButContainersAttached proves that a
// DNS-disabled network with at least one basepod-managed container
// attached is left alone entirely (no remove, no recreate) — recreating
// it out from under a live container would break its networking.
func TestEnsureNetworkWarnsWhenDNSDisabledButContainersAttached(t *testing.T) {
	removeCalls, createCalls := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"name": "basepod", "dns_enabled": false})
	})
	mux.HandleFunc("GET /v5.0.0/libpod/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "c1", "Names": []string{"/bp-blog-1"}, "State": "running", "Labels": map[string]string{"basepod.managed": "true"}},
		})
	})
	mux.HandleFunc("DELETE /v5.0.0/libpod/networks/basepod", func(w http.ResponseWriter, _ *http.Request) {
		removeCalls++
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v5.0.0/libpod/networks/create", func(w http.ResponseWriter, _ *http.Request) {
		createCalls++
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	if err := c.EnsureNetwork(context.Background(), "basepod", ""); err != nil {
		t.Fatal(err)
	}
	if removeCalls != 0 {
		t.Fatalf("removeCalls = %d, want 0 (must not recreate a network with live containers attached)", removeCalls)
	}
	if createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0", createCalls)
	}
}

func TestInspectNetwork(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"name":        "basepod",
			"dns_enabled": true,
			"subnets": []map[string]any{
				{"subnet": "10.89.2.0/24", "gateway": "10.89.2.1"},
			},
		})
	})
	c := fakeDaemon(t, mux)
	info, err := c.InspectNetwork(context.Background(), "basepod")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "basepod" || !info.DNSEnabled || info.Gateway != "10.89.2.1" {
		t.Fatalf("got %+v", info)
	}
}

func TestInspectNetworkNotFound(t *testing.T) {
	c := fakeDaemon(t, http.NewServeMux()) // 404 everything
	if _, err := c.InspectNetwork(context.Background(), "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRemoveNetworkAlreadyGoneIsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v5.0.0/libpod/networks/basepod", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	})
	c := fakeDaemon(t, mux)
	if err := c.RemoveNetwork(context.Background(), "basepod"); err != nil {
		t.Fatal(err)
	}
}

func TestListContainersFiltersByLabel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "true" {
			t.Errorf("expected all=true, got %q", r.URL.Query().Get("all"))
		}
		filters := r.URL.Query().Get("filters")
		if !strings.Contains(filters, "basepod.managed=true") {
			t.Errorf("expected filters to contain label, got %q", filters)
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "c1", "Names": []string{"/bp-blog-1"}, "State": "running", "Labels": map[string]string{"basepod.managed": "true"}},
		})
	})
	c := fakeDaemon(t, mux)
	got, err := c.ListContainers(context.Background(), map[string]string{"basepod.managed": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c1" || got[0].Name != "bp-blog-1" || got[0].State != "running" {
		t.Fatalf("got %+v", got)
	}
}

func TestInspectContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/bp-blog-1/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"Id":   "c1",
			"Name": "/bp-blog-1",
			"State": map[string]any{
				"Status": "running",
			},
			"Config": map[string]any{
				"Labels": map[string]string{"basepod.app": "blog"},
			},
		})
	})
	c := fakeDaemon(t, mux)
	info, err := c.InspectContainer(context.Background(), "bp-blog-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "c1" || info.Name != "bp-blog-1" || info.State != "running" || info.Labels["basepod.app"] != "blog" {
		t.Fatalf("got %+v", info)
	}
}

// TestInspectContainerImageAndPorts proves InspectContainer parses the
// human-readable image ref from "ImageName" (not the digest-ID "Image"
// field) and the container's TCP port bindings from
// HostConfig.PortBindings — both verified against a real podman 6.0.1
// client / 5.7.1 server's inspect payload shape.
func TestInspectContainerImageAndPorts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/bp-caddy/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"Id":        "c1",
			"Name":      "/bp-caddy",
			"Image":     "8f5619aac3ed45304632491507becd99711bcaed375aee3e6317c7b703902969",
			"ImageName": "docker.io/library/caddy:2.10-alpine",
			"State":     map[string]any{"Status": "running"},
			"Config":    map[string]any{"Labels": map[string]string{"basepod.managed": "true"}},
			"HostConfig": map[string]any{
				"PortBindings": map[string]any{
					"80/tcp":  []map[string]any{{"HostIp": "0.0.0.0", "HostPort": "8080"}},
					"443/tcp": []map[string]any{{"HostIp": "0.0.0.0", "HostPort": "8443"}},
				},
			},
		})
	})
	c := fakeDaemon(t, mux)
	info, err := c.InspectContainer(context.Background(), "bp-caddy")
	if err != nil {
		t.Fatal(err)
	}
	if info.Image != "docker.io/library/caddy:2.10-alpine" {
		t.Errorf("Image = %q, want the ImageName ref, not the digest ID", info.Image)
	}
	want := []PortMapping{
		{ContainerPort: 80, HostPort: 8080},
		{ContainerPort: 443, HostPort: 8443},
	}
	if !reflect.DeepEqual(info.Ports, want) {
		t.Errorf("Ports = %+v, want %+v", info.Ports, want)
	}
}

// TestInspectContainerIgnoresNonTCPBindings proves parsePortBindings
// skips non-TCP entries (out of scope for v0.3) rather than erroring or
// misparsing them.
func TestInspectContainerIgnoresNonTCPBindings(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/x/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"Id": "c1", "Name": "/x",
			"State": map[string]any{"Status": "running"},
			"HostConfig": map[string]any{
				"PortBindings": map[string]any{
					"53/udp": []map[string]any{{"HostIp": "0.0.0.0", "HostPort": "53"}},
				},
			},
		})
	})
	c := fakeDaemon(t, mux)
	info, err := c.InspectContainer(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Ports) != 0 {
		t.Errorf("Ports = %+v, want none (UDP filtered out)", info.Ports)
	}
}

// TestInspectContainerParsesBindMounts proves InspectContainer parses the
// top-level "Mounts" array into []BindMount, keeping only bind-type
// entries (e.g. skipping a volume mount) and sorting the result
// deterministically regardless of the daemon's own ordering.
func TestInspectContainerParsesBindMounts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/bp-caddy/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"Id":     "c1",
			"Name":   "/bp-caddy",
			"State":  map[string]any{"Status": "running"},
			"Config": map[string]any{"Labels": map[string]string{}},
			"Mounts": []map[string]any{
				{"Type": "bind", "Source": "/data/caddy-sock", "Destination": "/run/basepod"},
				{"Type": "bind", "Source": "/data/caddy-config", "Destination": "/etc/caddy"},
				{"Type": "volume", "Name": "somevolume", "Source": "/var/lib/containers/vol", "Destination": "/ignored"},
			},
		})
	})
	c := fakeDaemon(t, mux)
	info, err := c.InspectContainer(context.Background(), "bp-caddy")
	if err != nil {
		t.Fatal(err)
	}
	want := []BindMount{
		{Source: "/data/caddy-config", Dest: "/etc/caddy"},
		{Source: "/data/caddy-sock", Dest: "/run/basepod"},
	}
	if !reflect.DeepEqual(info.Mounts, want) {
		t.Errorf("Mounts = %+v, want %+v (volume mount filtered out, sorted by Destination)", info.Mounts, want)
	}
}

func TestPingSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestVersion proves Version reads the "Version" field out of libpod's
// GET /version response, requesting the correct versioned libpod path.
func TestVersion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/version", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"Version": "4.8.0"})
	})
	c := fakeDaemon(t, mux)
	got, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "4.8.0" {
		t.Fatalf("Version() = %q, want %q", got, "4.8.0")
	}
}

func TestVersionErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/version", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.Version(context.Background()); err == nil {
		t.Fatal("expected an error for a non-200 /version response")
	}
}

// TestHostCPUs proves HostCPUs reads host.cpus out of libpod's GET /info
// response, requesting the correct versioned libpod path. The response
// shape here is a byte-for-byte trim of a live capture (2026-08-07,
// podman 6.0.1 client / 5.7.1 server) — see Client.HostCPUs' doc comment
// for the full verification trail, including the live cross-check
// against the per-container compat endpoint's online_cpus for the same
// host at the same moment.
func TestHostCPUs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/info", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{"host":{"arch":"arm64","cpus":5,"hostname":"localhost.localdomain"},"store":{}}`)
	})
	c := fakeDaemon(t, mux)
	got, err := c.HostCPUs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("HostCPUs() = %d, want 5", got)
	}
}

func TestHostCPUsErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/info", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	})
	c := fakeDaemon(t, mux)
	if _, err := c.HostCPUs(context.Background()); err == nil {
		t.Fatal("expected an error for a non-200 /info response")
	}
}

// TestHostCPUsZeroIsError proves a decoded host.cpus of 0 (never actually
// observed live, but a caller multiplying by this must not silently treat
// it as "correction factor of 0") is reported as an error rather than
// returned as a valid CPU count.
func TestHostCPUsZeroIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/info", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{"host":{"cpus":0}}`)
	})
	c := fakeDaemon(t, mux)
	if _, err := c.HostCPUs(context.Background()); err == nil {
		t.Fatal("expected an error for host.cpus = 0")
	}
}

// TestContainerLogsQueryAndRawBody proves ContainerLogs sends the expected
// query parameters and returns the response body completely unparsed
// (raw multiplex bytes, not JSON-decoded) for the caller to stream.
func TestContainerLogsQueryAndRawBody(t *testing.T) {
	rawBody := []byte{1, 0, 0, 0, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/bp-blog-1/logs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("stdout"); got != "true" {
			t.Errorf("stdout = %q, want true", got)
		}
		if got := r.URL.Query().Get("stderr"); got != "true" {
			t.Errorf("stderr = %q, want true", got)
		}
		if got := r.URL.Query().Get("follow"); got != "true" {
			t.Errorf("follow = %q, want true", got)
		}
		if got := r.URL.Query().Get("tail"); got != "200" {
			t.Errorf("tail = %q, want 200", got)
		}
		w.WriteHeader(200)
		w.Write(rawBody)
	})
	c := fakeDaemon(t, mux)

	rc, err := c.ContainerLogs(context.Background(), "bp-blog-1", true, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rawBody) {
		t.Fatalf("body = %v, want %v (must not be JSON-decoded)", got, rawBody)
	}
}

// TestContainerLogsNoTailOmitsParam proves tail<=0 omits the tail query
// param entirely (podman's daemon default is "all logs").
func TestContainerLogsNoTailOmitsParam(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/bp-blog-1/logs", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["tail"]; ok {
			t.Errorf("expected no tail param, got %q", r.URL.Query().Get("tail"))
		}
		if got := r.URL.Query().Get("follow"); got != "false" {
			t.Errorf("follow = %q, want false", got)
		}
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	rc, err := c.ContainerLogs(context.Background(), "bp-blog-1", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
}

func TestContainerLogsNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/nope/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"cause": "no such container"})
	})
	c := fakeDaemon(t, mux)
	_, err := c.ContainerLogs(context.Background(), "nope", false, 100)
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
