package podman

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if err := c.EnsureNetwork(context.Background(), "basepod"); err != nil {
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

func TestEnsureNetworkNoopWhenPresent(t *testing.T) {
	createCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/networks/basepod/exists", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v5.0.0/libpod/networks/create", func(w http.ResponseWriter, _ *http.Request) {
		createCalls++
		w.WriteHeader(200)
	})
	c := fakeDaemon(t, mux)
	if err := c.EnsureNetwork(context.Background(), "basepod"); err != nil {
		t.Fatal(err)
	}
	if createCalls != 0 {
		t.Fatalf("expected no create call, got %d", createCalls)
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
