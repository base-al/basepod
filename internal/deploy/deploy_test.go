package deploy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

// fakeRuntime is a test double for Runtime: an in-memory container table
// that records every call (and, for calls that mutate state, enough
// detail to reconstruct ordering) into a shared ops log.
type fakeRuntime struct {
	ops *[]string

	containers map[string]podman.ContainerInfo
	nextID     int

	pullErr   error
	createErr error
	startErr  error
	stopErr   error
	removeErr error
}

func newFakeRuntime(ops *[]string) *fakeRuntime {
	return &fakeRuntime{ops: ops, containers: map[string]podman.ContainerInfo{}}
}

func (f *fakeRuntime) record(s string) { *f.ops = append(*f.ops, s) }

func (f *fakeRuntime) PullImage(ctx context.Context, ref string) error {
	f.record("pull:" + ref)
	return f.pullErr
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, spec podman.CreateSpec) (string, error) {
	if f.createErr != nil {
		f.record("create-err:" + spec.Name)
		return "", f.createErr
	}
	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	f.containers[id] = podman.ContainerInfo{ID: id, Name: spec.Name, State: "created", Labels: labels}
	f.record("create:" + spec.Name)
	return id, nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id string) error {
	f.record("start:" + id)
	if f.startErr != nil {
		return f.startErr
	}
	if c, ok := f.containers[id]; ok {
		c.State = "running"
		f.containers[id] = c
	}
	return nil
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id string, timeoutSec int) error {
	f.record("stop:" + id)
	if f.stopErr != nil {
		return f.stopErr
	}
	if c, ok := f.containers[id]; ok {
		c.State = "exited"
		f.containers[id] = c
	}
	return nil
}

func (f *fakeRuntime) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.record("remove:" + id)
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.containers, id)
	return nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, nameOrID string) (*podman.ContainerInfo, error) {
	for _, c := range f.containers {
		if c.ID == nameOrID || c.Name == nameOrID {
			cc := c
			return &cc, nil
		}
	}
	return nil, podman.ErrNotFound
}

func (f *fakeRuntime) ListContainers(ctx context.Context, labelFilters map[string]string) ([]podman.ContainerInfo, error) {
	var out []podman.ContainerInfo
	for _, c := range f.containers {
		match := true
		for k, v := range labelFilters {
			if c.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fakeRouter is a test double for Router.
type fakeRouter struct {
	ops        *[]string
	applyErr   error
	lastRoutes []caddy.AppRoute
	calls      int
}

func (f *fakeRouter) Apply(ctx context.Context, routes []caddy.AppRoute) error {
	*f.ops = append(*f.ops, "apply-routes")
	f.calls++
	f.lastRoutes = routes
	return f.applyErr
}

// fakeProber is a test double for Prober. failUpstream, if non-empty,
// makes the probe fail only for that exact upstream (so a test can make
// a specific generation's probe fail while others succeed).
type fakeProber struct {
	ops          *[]string
	failUpstream string
	failErr      error
	calls        []string
}

func (f *fakeProber) probe(ctx context.Context, upstream string) error {
	*f.ops = append(*f.ops, "probe:"+upstream)
	f.calls = append(f.calls, upstream)
	if f.failUpstream != "" && upstream == f.failUpstream {
		if f.failErr != nil {
			return f.failErr
		}
		return errors.New("probe failed")
	}
	return nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newTestEngine wires an Engine to a fresh fakeRuntime/fakeRouter/fakeProber
// sharing one ops log, with the probe retry loop shrunk for fast tests.
func newTestEngine(t *testing.T, st *store.Store) (*Engine, *fakeRuntime, *fakeRouter, *fakeProber, *[]string) {
	t.Helper()
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	eng := New(st, rt, router, prober.probe, "apps.localhost")
	eng.probeInterval = time.Millisecond
	eng.probeAttempts = 5
	return eng, rt, router, prober, ops
}

func opIndex(ops []string, prefix string) int {
	for i, o := range ops {
		if strings.HasPrefix(o, prefix) {
			return i
		}
	}
	return -1
}

func TestDeployHappyPath(t *testing.T) {
	st := openStore(t)
	eng, rt, router, prober, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:old", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.Deploy(ctx, app, "nginx:new")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "healthy" {
		t.Errorf("dep.Status = %q, want healthy", dep.Status)
	}

	var found *podman.ContainerInfo
	for _, c := range rt.containers {
		if c.Name == "bp-blog-1" {
			cc := c
			found = &cc
		}
	}
	if found == nil {
		t.Fatal("container bp-blog-1 not found")
	}
	if found.State != "running" {
		t.Errorf("bp-blog-1 state = %q, want running", found.State)
	}
	if found.Labels["basepod.managed"] != "true" || found.Labels["basepod.app"] != "blog" || found.Labels["basepod.deployment"] != "1" {
		t.Errorf("labels = %+v", found.Labels)
	}

	if len(prober.calls) != 1 || prober.calls[0] != "bp-blog-1:80" {
		t.Errorf("prober.calls = %v, want [bp-blog-1:80]", prober.calls)
	}

	if router.calls != 1 {
		t.Fatalf("router.calls = %d, want 1", router.calls)
	}
	want := []caddy.AppRoute{{Slug: "blog", Hostname: "blog.apps.localhost", Upstream: "bp-blog:80"}}
	if len(router.lastRoutes) != 1 || router.lastRoutes[0] != want[0] {
		t.Errorf("router.lastRoutes = %+v, want %+v", router.lastRoutes, want)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "running" {
		t.Errorf("app.Status = %q, want running", gotApp.Status)
	}
	if gotApp.ImageRef != "nginx:new" {
		t.Errorf("app.ImageRef = %q, want nginx:new", gotApp.ImageRef)
	}
}

func TestSecondDeployRemovesOld(t *testing.T) {
	st := openStore(t)
	eng, rt, router, _, ops := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	*ops = nil // only care about ordering within the second deploy
	router.calls = 0

	dep2, err := eng.Deploy(ctx, app, "nginx:v2")
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	if dep2.Number != 2 {
		t.Fatalf("dep2.Number = %d, want 2", dep2.Number)
	}

	var oldID, newID string
	for id, c := range rt.containers {
		switch c.Name {
		case "bp-blog-1":
			oldID = id
		case "bp-blog-2":
			newID = id
		}
	}
	if newID == "" {
		t.Fatal("bp-blog-2 not found (should still exist)")
	}
	if oldID != "" {
		t.Fatalf("bp-blog-1 should have been removed, still present: %+v", rt.containers[oldID])
	}
	if got := rt.containers[newID]; got.State != "running" {
		t.Errorf("bp-blog-2 state = %q, want running", got.State)
	}

	startIdx := opIndex(*ops, "start:")
	applyIdx := opIndex(*ops, "apply-routes")
	stopIdx := opIndex(*ops, "stop:")
	if startIdx == -1 || applyIdx == -1 || stopIdx == -1 {
		t.Fatalf("missing expected op in %v", *ops)
	}
	if !(startIdx < applyIdx && applyIdx < stopIdx) {
		t.Errorf("op order = %v, want start < apply-routes < stop", *ops)
	}

	if router.calls != 1 {
		t.Fatalf("router.calls (this deploy) = %d, want 1", router.calls)
	}
	if len(router.lastRoutes) != 1 || router.lastRoutes[0].Upstream != "bp-blog:80" {
		t.Errorf("router.lastRoutes = %+v", router.lastRoutes)
	}
}

func TestFailedProbeKeepsOld(t *testing.T) {
	st := openStore(t)
	eng, rt, _, prober, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	prober.failUpstream = "bp-blog-2:80"

	dep2, err := eng.Deploy(ctx, app, "nginx:v2")
	if err == nil {
		t.Fatal("expected error from failed probe")
	}
	if dep2.Status != "failed" {
		t.Errorf("dep2.Status = %q, want failed", dep2.Status)
	}
	if dep2.Error == "" {
		t.Error("dep2.Error empty, want probe failure message")
	}

	var oldID, newID string
	for id, c := range rt.containers {
		switch c.Name {
		case "bp-blog-1":
			oldID = id
		case "bp-blog-2":
			newID = id
		}
	}
	if oldID == "" {
		t.Fatal("bp-blog-1 (old, healthy) should still exist")
	}
	if newID != "" {
		t.Fatalf("bp-blog-2 (failed) should have been removed, still present: %+v", rt.containers[newID])
	}
	if got := rt.containers[oldID]; got.State != "running" {
		t.Errorf("bp-blog-1 state = %q, want still running (untouched)", got.State)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "running" {
		t.Errorf("app.Status = %q, want running (old container still up)", gotApp.Status)
	}
	if gotApp.ImageRef != "nginx:v1" {
		t.Errorf("app.ImageRef = %q, want unchanged nginx:v1", gotApp.ImageRef)
	}
}

func TestFailedPull(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:old", 80)
	if err != nil {
		t.Fatal(err)
	}
	rt.pullErr = errors.New("no such image")

	dep, err := eng.Deploy(ctx, app, "nginx:bad")
	if err == nil {
		t.Fatal("expected error from failed pull")
	}
	if dep.Status != "failed" {
		t.Errorf("dep.Status = %q, want failed", dep.Status)
	}
	if len(rt.containers) != 0 {
		t.Errorf("containers = %+v, want none created", rt.containers)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "error" {
		t.Errorf("app.Status = %q, want error (no containers at all)", gotApp.Status)
	}
}

// exitError8 runs a real subprocess that exits 8, so tests get a genuine
// *exec.ExitError (with ExitCode() == 8) rather than a hand-built stub.
func exitError8(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 8").Run()
	if err == nil {
		t.Fatal("expected exit 8 to produce an error")
	}
	return err
}

func TestCaddyProber(t *testing.T) {
	ctx := context.Background()

	t.Run("exit 0 is success", func(t *testing.T) {
		probe := CaddyProber(func(ctx context.Context, container string, cmd ...string) error {
			return nil
		})
		if err := probe(ctx, "bp-blog:80"); err != nil {
			t.Errorf("probe: %v", err)
		}
	})

	t.Run("unwrapped exit 8 is success", func(t *testing.T) {
		e8 := exitError8(t)
		probe := CaddyProber(func(ctx context.Context, container string, cmd ...string) error {
			return e8
		})
		if err := probe(ctx, "bp-blog:80"); err != nil {
			t.Errorf("probe: %v", err)
		}
	})

	t.Run("wrapped exit 8 (production PodmanExec shape) is success", func(t *testing.T) {
		e8 := exitError8(t)
		wrapped := fmt.Errorf("podman exec: %w: %s", e8, "some stderr")
		probe := CaddyProber(func(ctx context.Context, container string, cmd ...string) error {
			return wrapped
		})
		if err := probe(ctx, "bp-blog:80"); err != nil {
			t.Errorf("probe: %v", err)
		}
	})

	t.Run("string-only exit status 8 (no unwrap chain) is success", func(t *testing.T) {
		probe := CaddyProber(func(ctx context.Context, container string, cmd ...string) error {
			return errors.New("wget: exit status 8")
		})
		if err := probe(ctx, "bp-blog:80"); err != nil {
			t.Errorf("probe: %v", err)
		}
	})

	t.Run("other errors are failures", func(t *testing.T) {
		wantErr := errors.New("connection refused")
		probe := CaddyProber(func(ctx context.Context, container string, cmd ...string) error {
			return wantErr
		})
		if err := probe(ctx, "bp-blog:80"); !errors.Is(err, wantErr) {
			t.Errorf("probe = %v, want %v", err, wantErr)
		}
	})

	t.Run("passes bp-caddy and wget args", func(t *testing.T) {
		var gotContainer string
		var gotCmd []string
		probe := CaddyProber(func(ctx context.Context, container string, cmd ...string) error {
			gotContainer = container
			gotCmd = cmd
			return nil
		})
		if err := probe(ctx, "bp-blog:80"); err != nil {
			t.Fatal(err)
		}
		if gotContainer != caddy.ContainerName {
			t.Errorf("container = %q, want %q", gotContainer, caddy.ContainerName)
		}
		want := []string{"wget", "-q", "-T", "2", "--spider", "http://bp-blog:80/"}
		if strings.Join(gotCmd, " ") != strings.Join(want, " ") {
			t.Errorf("cmd = %v, want %v", gotCmd, want)
		}
	})
}
