package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
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

	// removeErrIDs, if set, overrides removeErr for specific container
	// IDs — used to make exactly one container in a multi-container
	// teardown fail while the rest succeed.
	removeErrIDs map[string]error

	// createdSpecs records the full CreateSpec passed to CreateContainer,
	// keyed by container name, so tests can assert on fields (like Env)
	// that ContainerInfo doesn't carry.
	createdSpecs map[string]podman.CreateSpec

	// logsReader/logsErr script ContainerLogs; logsCalls records every
	// call's arguments for assertions.
	logsReader io.ReadCloser
	logsErr    error
	logsCalls  []loggedCall
}

// loggedCall records one ContainerLogs invocation.
type loggedCall struct {
	nameOrID string
	follow   bool
	tail     int
}

func newFakeRuntime(ops *[]string) *fakeRuntime {
	return &fakeRuntime{ops: ops, containers: map[string]podman.ContainerInfo{}}
}

func (f *fakeRuntime) record(s string) { *f.ops = append(*f.ops, s) }

func (f *fakeRuntime) PullImage(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("pull:" + ref)
	return f.pullErr
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, spec podman.CreateSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.createErr != nil {
		f.record("create-err:" + spec.Name)
		return "", f.createErr
	}
	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	labels := map[string]string{}
	maps.Copy(labels, spec.Labels)
	f.containers[id] = podman.ContainerInfo{ID: id, Name: spec.Name, State: "created", Labels: labels}
	if f.createdSpecs == nil {
		f.createdSpecs = map[string]podman.CreateSpec{}
	}
	f.createdSpecs[spec.Name] = spec
	f.record("create:" + spec.Name)
	return id, nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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

// StopContainer and RemoveContainer deliberately honor ctx cancellation
// (unlike, say, always succeeding regardless of ctx) so tests can prove
// that deploy.go's cleanup paths use a context.WithoutCancel-derived
// context rather than the caller's ctx directly — see
// TestFailDetachesCleanupFromCancelledContext.
func (f *fakeRuntime) StopContainer(ctx context.Context, id string, timeoutSec int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("remove:" + id)
	if err, ok := f.removeErrIDs[id]; ok {
		return err
	}
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.containers, id)
	return nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, nameOrID string) (*podman.ContainerInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, c := range f.containers {
		if c.ID == nameOrID || c.Name == nameOrID {
			cc := c
			return &cc, nil
		}
	}
	return nil, podman.ErrNotFound
}

func (f *fakeRuntime) ListContainers(ctx context.Context, labelFilters map[string]string) ([]podman.ContainerInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

func (f *fakeRuntime) ContainerLogs(ctx context.Context, nameOrID string, follow bool, tail int) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.logsCalls = append(f.logsCalls, loggedCall{nameOrID: nameOrID, follow: follow, tail: tail})
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.logsReader != nil {
		return f.logsReader, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// fakeRouter is a test double for Router.
type fakeRouter struct {
	ops           *[]string
	applyErr      error
	lastRoutes    []caddy.AppRoute
	lastDashboard *caddy.DashboardRoute
	calls         int
}

func (f *fakeRouter) Apply(ctx context.Context, routes []caddy.AppRoute, dashboard *caddy.DashboardRoute) error {
	*f.ops = append(*f.ops, "apply-routes")
	f.calls++
	f.lastRoutes = routes
	f.lastDashboard = dashboard
	return f.applyErr
}

// blockingRouter is a test double for Router whose Apply blocks until
// release() is called, and tracks how many Apply calls are concurrently
// in flight. It exists to prove routesMu serializes the Routes()+Apply
// critical section: entered fires the instant a call is inside Apply
// (i.e. holding routesMu), and maxInFlight() reports whether any two
// Apply calls were ever in flight at once — see
// TestApplyRoutesSerializesConcurrentCalls (I1).
type blockingRouter struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	calls    [][]caddy.AppRoute

	entered chan struct{}
	block   chan struct{}
}

func newBlockingRouter() *blockingRouter {
	return &blockingRouter{
		entered: make(chan struct{}, 4),
		block:   make(chan struct{}),
	}
}

func (r *blockingRouter) Apply(ctx context.Context, routes []caddy.AppRoute, dashboard *caddy.DashboardRoute) error {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.maxSeen {
		r.maxSeen = r.inFlight
	}
	r.mu.Unlock()

	r.entered <- struct{}{}
	<-r.block // released by the test once it's done setting up the race

	r.mu.Lock()
	r.inFlight--
	// Copy: the caller (ApplyRoutes) owns routes's backing array and
	// nothing here should alias it past this call.
	cp := append([]caddy.AppRoute(nil), routes...)
	r.calls = append(r.calls, cp)
	r.mu.Unlock()
	return nil
}

// release unblocks every Apply call currently (or later) parked on
// <-r.block. It's called once, after the race is set up, which is safe
// because closing a channel unblocks all current and future receivers.
func (r *blockingRouter) release() { close(r.block) }

func (r *blockingRouter) maxInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxSeen
}

func (r *blockingRouter) callAt(i int) []caddy.AppRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

func (r *blockingRouter) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
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
	eng := New(st, rt, router, prober.probe, "apps.localhost", nil, nil)
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
	want := []caddy.AppRoute{{Slug: "blog", Hostnames: []string{"blog.apps.localhost"}, Upstream: "bp-blog:80"}}
	if len(router.lastRoutes) != 1 || !reflect.DeepEqual(router.lastRoutes[0], want[0]) {
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

// TestDeployInjectsDecryptedEnv proves Deploy reads the app's env vars,
// decrypts each ValueEncrypted through the Engine's decrypt func, and
// passes the resulting plaintext map as CreateSpec.Env to CreateContainer.
func TestDeployInjectsDecryptedEnv(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	decrypt := func(s string) (string, error) { return "plain-" + s, nil }
	eng := New(st, rt, router, prober.probe, "apps.localhost", decrypt, nil)
	eng.probeInterval = time.Millisecond
	eng.probeAttempts = 5
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:old", 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEnvVar(app.ID, "FOO", "enc-foo", false); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEnvVar(app.ID, "BAR", "enc-bar", true); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Deploy(ctx, app, "nginx:new"); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	spec, ok := rt.createdSpecs["bp-blog-1"]
	if !ok {
		t.Fatal("CreateContainer spec not recorded for bp-blog-1")
	}
	want := map[string]string{"FOO": "plain-enc-foo", "BAR": "plain-enc-bar"}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Errorf("spec.Env = %+v, want %+v", spec.Env, want)
	}
}

// TestDeployFailsOnDecryptError proves that when decrypting a stored env
// var fails, Deploy fails through the normal fail() path: no new
// container is ever created, and the previous (old, healthy) container is
// left completely untouched — a half-injected env must never ship.
func TestDeployFailsOnDecryptError(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	decrypt := func(s string) (string, error) { return "", errors.New("bad key") }
	eng := New(st, rt, router, prober.probe, "apps.localhost", decrypt, nil)
	eng.probeInterval = time.Millisecond
	eng.probeAttempts = 5
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	// First deploy has no env vars yet, so decrypt is never invoked and
	// this establishes a healthy "old" container for the assertion below.
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	if err := st.UpsertEnvVar(app.ID, "FOO", "enc-foo", false); err != nil {
		t.Fatal(err)
	}

	dep2, err := eng.Deploy(ctx, app, "nginx:v2")
	if err == nil {
		t.Fatal("expected error from decrypt failure")
	}
	if dep2.Status != "failed" {
		t.Errorf("dep2.Status = %q, want failed", dep2.Status)
	}
	if dep2.Error == "" {
		t.Error("dep2.Error empty, want decrypt failure message")
	}

	var oldID string
	for id, c := range rt.containers {
		if c.Name == "bp-blog-1" {
			oldID = id
		}
		if c.Name == "bp-blog-2" {
			t.Errorf("bp-blog-2 should never have been created (decrypt fails before CreateContainer), got %+v", c)
		}
	}
	if oldID == "" {
		t.Fatal("bp-blog-1 (old, healthy) should still exist")
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

// TestDeployFailsWhenEnvInjectionDisabled proves that an app with stored
// env vars fails to deploy when the Engine has no decrypt func (env
// injection disabled) — a container must never start silently missing
// its configured env, so this must go through the fail() path exactly
// like a decrypt error: deployment marked failed, no new container ever
// created, and an existing old container left untouched.
func TestDeployFailsWhenEnvInjectionDisabled(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	eng := New(st, rt, router, prober.probe, "apps.localhost", nil, nil) // decrypt disabled
	eng.probeInterval = time.Millisecond
	eng.probeAttempts = 5
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	// First deploy has no env vars yet, so it succeeds even with decrypt
	// disabled, giving us a healthy "old" container to assert stays
	// untouched below.
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	if err := st.UpsertEnvVar(app.ID, "FOO", "enc-foo", false); err != nil {
		t.Fatal(err)
	}

	dep2, err := eng.Deploy(ctx, app, "nginx:v2")
	if err == nil {
		t.Fatal("expected error: app has env vars but env injection is disabled")
	}
	if dep2.Status != "failed" {
		t.Errorf("dep2.Status = %q, want failed", dep2.Status)
	}
	if dep2.Error == "" {
		t.Error("dep2.Error empty, want env-injection-disabled failure message")
	}

	var oldID string
	for id, c := range rt.containers {
		if c.Name == "bp-blog-1" {
			oldID = id
		}
		if c.Name == "bp-blog-2" {
			t.Errorf("bp-blog-2 should never have been created (env-injection-disabled check runs before CreateContainer), got %+v", c)
		}
	}
	if oldID == "" {
		t.Fatal("bp-blog-1 (old, healthy) should still exist")
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

// TestRoutesIncludeCustomDomains proves Routes() builds each running
// app's Hostnames as [slug.rootDomain] followed by that app's custom
// domains (from ListAllDomains), sorted lexically.
func TestRoutesIncludeCustomDomains(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppStatus(app.ID, "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(app.ID, "blog.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(app.ID, "aaa.example.com"); err != nil {
		t.Fatal(err)
	}

	routes, err := eng.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %+v, want 1 route", routes)
	}
	want := []string{"blog.apps.localhost", "aaa.example.com", "blog.example.com"}
	if !reflect.DeepEqual(routes[0].Hostnames, want) {
		t.Errorf("Hostnames = %v, want %v", routes[0].Hostnames, want)
	}
}

// TestApplyRoutesPassesDashboardToRouter proves ApplyRoutes forwards the
// Engine's dashboard route (set via New) through to every router.Apply
// call unchanged, alongside the computed app route set.
func TestApplyRoutesPassesDashboardToRouter(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	dashboard := &caddy.DashboardRoute{Hostname: "basepod.apps.localhost", Upstream: "10.89.2.1:3080"}
	eng := New(st, rt, router, prober.probe, "apps.localhost", nil, dashboard)
	ctx := context.Background()

	if err := eng.ApplyRoutes(ctx); err != nil {
		t.Fatalf("ApplyRoutes: %v", err)
	}
	if router.lastDashboard != dashboard {
		t.Errorf("router.lastDashboard = %+v, want the Engine's configured dashboard route %+v", router.lastDashboard, dashboard)
	}
}

// TestRemoveAppPassesDashboardToRouter proves RemoveApp forwards the
// Engine's dashboard route through to router.Apply unchanged, just like
// ApplyRoutes — RemoveApp renders and applies its own filtered route set
// rather than calling ApplyRoutes, so this is its own regression test
// rather than being implied by the one above.
func TestRemoveAppPassesDashboardToRouter(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	dashboard := &caddy.DashboardRoute{Hostname: "basepod.apps.localhost", Upstream: "10.89.2.1:3080"}
	eng := New(st, rt, router, prober.probe, "apps.localhost", nil, dashboard)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppStatus(app.ID, "running"); err != nil {
		t.Fatal(err)
	}

	if err := eng.RemoveApp(ctx, app); err != nil {
		t.Fatalf("RemoveApp: %v", err)
	}
	if router.lastDashboard != dashboard {
		t.Errorf("router.lastDashboard = %+v, want the Engine's configured dashboard route %+v", router.lastDashboard, dashboard)
	}
}

// TestRemoveAppBestEffortTeardown is a regression test: RemoveApp must
// not abort teardown (or skip re-applying routes) just because one
// container's removal errors. It should keep going for every remaining
// container and always reach Routes()+router.Apply, since that's the
// step that actually drops the app's stale hostname from Caddy.
func TestRemoveAppBestEffortTeardown(t *testing.T) {
	st := openStore(t)
	eng, rt, router, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppStatus(app.ID, "running"); err != nil {
		t.Fatal(err)
	}

	// Two containers for the app (as if a prior deploy's teardown had
	// itself been interrupted), inserted directly since RemoveApp is
	// exercised in isolation here.
	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-blog-1", State: "running",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "blog", "basepod.deployment": "1"},
	}
	rt.containers["c2"] = podman.ContainerInfo{
		ID: "c2", Name: "bp-blog-2", State: "running",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "blog", "basepod.deployment": "2"},
	}
	// fakeRuntime.ListContainers sorts by Name, so bp-blog-1 (c1) is
	// processed first — make exactly that one fail.
	rt.removeErrIDs = map[string]error{"c1": errors.New("remove c1 boom")}

	if err := eng.RemoveApp(ctx, app); err != nil {
		t.Fatalf("RemoveApp: %v", err)
	}

	if _, stillThere := rt.containers["c1"]; !stillThere {
		t.Error("c1's RemoveContainer errored, so the fake never deletes it — sanity check failed")
	}
	if _, stillThere := rt.containers["c2"]; stillThere {
		t.Error("c2 should have been removed despite c1's error")
	}
	if router.calls != 1 {
		t.Fatalf("router.calls = %d, want 1 (Routes()+Apply must still run after a per-container error)", router.calls)
	}

	// Regression for I1: the app's store row is still "running" at this
	// point (RemoveApp never touches the store — the caller deletes the
	// row afterwards), so Routes() would include blog's own route unless
	// RemoveApp explicitly filters it out before calling router.Apply.
	// Assert on the *applied* set, not just the call count.
	for _, rt := range router.lastRoutes {
		if rt.Slug == "blog" {
			t.Errorf("router.lastRoutes still contains removed app's route: %+v", router.lastRoutes)
		}
		for _, h := range rt.Hostnames {
			if h == "blog.apps.localhost" {
				t.Errorf("router.lastRoutes still contains removed app's hostname: %+v", router.lastRoutes)
			}
		}
	}
}

// TestApplyRoutesSerializesConcurrentCalls is a regression test for I1: two
// concurrent route-apply callers (e.g. a domain-add HTTP handler racing an
// in-flight deploy's cutover) must never interleave their Routes() DB read
// with their router.Apply call, since both write the same current.json.tmp
// (internal/caddy/manager.go writeFileAtomic). It uses a blockingRouter to
// force the interleaving deterministically: the first ApplyRoutes call is
// parked inside Apply (proven by <-router.entered, which only fires once
// Apply — and therefore routesMu — has been entered) while a new domain is
// inserted and a second ApplyRoutes call is started. Because the first
// call still holds routesMu, the second's own Lock() call blocks until the
// first releases it via router.release() — Go's mutex semantics guarantee
// this ordering regardless of goroutine scheduling, so the test needs no
// sleeps to be deterministic. The assertions then confirm: (1)
// maxInFlight() never exceeded 1, i.e. the two Apply calls never
// overlapped; and (2) the second call's applied route set includes the
// domain inserted after the first call had already read Routes() — which
// only holds if the second call re-read Routes() under the lock rather
// than reusing a stale snapshot.
func TestApplyRoutesSerializesConcurrentCalls(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppStatus(app.ID, "running"); err != nil {
		t.Fatal(err)
	}

	router := newBlockingRouter()
	eng.router = router

	// First call: acquires routesMu, reads Routes() (blog only, no custom
	// domain yet), and parks inside Apply holding the lock.
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- eng.ApplyRoutes(ctx)
	}()
	<-router.entered // first call is now inside Apply, holding routesMu

	// Insert a new domain while the first call is blocked mid-Apply,
	// still holding routesMu.
	if _, err := st.AddDomain(app.ID, "new.example.com"); err != nil {
		t.Fatal(err)
	}

	// Second call: its Lock() must block behind the first (routesMu is
	// still held), so it cannot read Routes() until after the first call
	// has fully returned.
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- eng.ApplyRoutes(ctx)
	}()

	// Release the first call. Because closing router.block unblocks all
	// current and future receivers, this is safe even though the second
	// call may not have reached its own <-r.block yet (it can't have —
	// it's still blocked on routesMu.Lock() — but even if scheduling
	// changed that, the close is still correct).
	router.release()

	if err := <-firstDone; err != nil {
		t.Fatalf("first ApplyRoutes: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second ApplyRoutes: %v", err)
	}

	if max := router.maxInFlight(); max > 1 {
		t.Fatalf("router.Apply had %d calls in flight simultaneously, want at most 1: routesMu did not serialize ApplyRoutes", max)
	}

	if got := router.callCount(); got != 2 {
		t.Fatalf("router.Apply called %d times, want 2", got)
	}

	firstRoutes := router.callAt(0)
	if len(firstRoutes) != 1 || len(firstRoutes[0].Hostnames) != 1 {
		t.Errorf("first call's applied routes = %+v, want just blog.apps.localhost (new.example.com was added after this call's Routes() read)", firstRoutes)
	}

	secondRoutes := router.callAt(1)
	if len(secondRoutes) != 1 {
		t.Fatalf("second call's applied routes = %+v, want 1 route", secondRoutes)
	}
	found := false
	for _, h := range secondRoutes[0].Hostnames {
		if h == "new.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("second call's applied Hostnames = %v, want it to include new.example.com (its Routes() read must happen under routesMu, after the domain was added, not before)", secondRoutes[0].Hostnames)
	}
}

// TestFailDetachesCleanupFromCancelledContext is a regression test for I2:
// fail()'s cleanup (RemoveContainer of the just-created, not-yet-live
// container) must happen even when the ctx passed into Deploy has already
// been cancelled — e.g. Ctrl-C during pull, or the deploy's own timeout
// expiring — otherwise that container is orphaned still holding the live
// bp-<slug> DNS alias. fakeRuntime's Stop/Remove/List calls honor ctx
// cancellation (see their comments), so this test only passes if fail()
// derives its cleanup context via context.WithoutCancel(ctx) rather than
// using the (cancelled) ctx directly.
func TestFailDetachesCleanupFromCancelledContext(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Simulate the ctx being cancelled mid-probe (Ctrl-C / deploy
	// timeout) and the probe surfacing that as its own error, the way a
	// real HTTP probe would once its request context dies.
	probe := func(pctx context.Context, upstream string) error {
		cancel()
		return pctx.Err()
	}

	eng := New(st, rt, router, probe, "apps.localhost", nil, nil)
	eng.probeInterval = time.Millisecond
	eng.probeAttempts = 1

	app, err := st.CreateApp("blog", "nginx:old", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.Deploy(ctx, app, "nginx:new")
	if err == nil {
		t.Fatal("expected error from cancelled context during probe")
	}
	if dep.Status != "failed" {
		t.Errorf("dep.Status = %q, want failed", dep.Status)
	}

	if len(rt.containers) != 0 {
		t.Errorf("containers = %+v, want the new container removed despite the cancelled ctx (fail() must use a detached cleanup context)", rt.containers)
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

// TestCleanupOrphansRemovesUnknownSlug proves a basepod.managed container
// whose basepod.app slug has no matching row in the store (the app was
// deleted while its container somehow survived) is stopped and removed.
func TestCleanupOrphansRemovesUnknownSlug(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-ghost-1", State: "running",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "ghost", "basepod.deployment": "1"},
	}

	removed, err := eng.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, stillThere := rt.containers["c1"]; stillThere {
		t.Error("bp-ghost-1 should have been removed (unknown app slug)")
	}
}

// TestCleanupOrphansRemovesStaleGeneration proves a container whose
// basepod.deployment generation is not the app's latest *healthy*
// deployment is stopped and removed, even though its app still exists —
// e.g. left behind by a cutover that was interrupted mid-teardown.
func TestCleanupOrphansRemovesStaleGeneration(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v2", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep1, err := st.CreateDeployment(app.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep1.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	dep2, err := st.CreateDeployment(app.ID, "nginx:v2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep2.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	// dep2 (number 2) is the latest healthy deployment; a leftover
	// container still labeled for generation 1 is stale.
	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-blog-1", State: "exited",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "blog", "basepod.deployment": "1"},
	}

	removed, err := eng.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, stillThere := rt.containers["c1"]; stillThere {
		t.Error("bp-blog-1 (stale generation 1) should have been removed")
	}
}

// TestCleanupOrphansKeepsCurrentHealthyGeneration proves a container
// whose basepod.deployment generation IS the app's latest healthy
// deployment is left completely untouched.
func TestCleanupOrphansKeepsCurrentHealthyGeneration(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(app.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-blog-1", State: "running",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "blog", "basepod.deployment": "1"},
	}

	removed, err := eng.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, stillThere := rt.containers["c1"]; !stillThere {
		t.Error("bp-blog-1 (current healthy generation) should not have been removed")
	}
}

// TestCleanupOrphansSkipsCaddyByName proves bp-caddy is never touched by
// orphan GC even though it carries basepod.managed=true and, in this
// test, a basepod.app label it wouldn't normally have — the skip is by
// container name, not by the absence of app labels.
func TestCleanupOrphansSkipsCaddyByName(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	rt.containers["ccaddy"] = podman.ContainerInfo{
		ID: "ccaddy", Name: caddy.ContainerName, State: "running",
		// Deliberately labeled as if it belonged to a nonexistent app, to
		// prove the name-based skip runs before (and instead of) the
		// slug-lookup path that would otherwise remove it.
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "ghost", "basepod.deployment": "1"},
	}

	removed, err := eng.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, stillThere := rt.containers["ccaddy"]; !stillThere {
		t.Error("bp-caddy should never be touched by orphan GC")
	}
}

// TestCleanupOrphansWarnsAndKeepsUnlabeledContainer proves a
// basepod.managed container with no basepod.app label is left alone
// (not removed) — GC has no record of what it belongs to, so it isn't
// safe to guess.
func TestCleanupOrphansWarnsAndKeepsUnlabeledContainer(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-mystery", State: "running",
		Labels: map[string]string{"basepod.managed": "true"},
	}

	removed, err := eng.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, stillThere := rt.containers["c1"]; !stillThere {
		t.Error("unlabeled container should have been left alone, not removed")
	}
}

// TestCleanupOrphansKeepsInFlightDeployWithNoHealthyGenerationYet proves
// that a container for an app which exists but has never had a healthy
// deployment (e.g. its first deploy is still probing) is left alone
// rather than removed for "not matching" a latest-healthy number that
// doesn't exist yet.
func TestCleanupOrphansKeepsInFlightDeployWithNoHealthyGenerationYet(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(app.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	// Deployment left in "deploying" — never finished healthy.
	_ = dep

	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-blog-1", State: "running",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "blog", "basepod.deployment": "1"},
	}

	removed, err := eng.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (no healthy deployment recorded yet)", removed)
	}
	if _, stillThere := rt.containers["c1"]; !stillThere {
		t.Error("bp-blog-1 should have been left alone (app has no healthy deployment yet)")
	}
}

// TestAppLogsAppNotFound proves AppLogs passes store.ErrNotFound through
// unchanged for an unknown slug.
func TestAppLogsAppNotFound(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	_, err := eng.AppLogs(context.Background(), "does-not-exist", false, 200)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}

// TestAppLogsNotRunning proves AppLogs returns ErrNotRunning for an app
// that exists but has no running container (never deployed).
func TestAppLogsNotRunning(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	if _, err := st.CreateApp("blog", "nginx:v1", 80); err != nil {
		t.Fatal(err)
	}

	_, err := eng.AppLogs(context.Background(), "blog", false, 200)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

// TestAppLogsRunning proves AppLogs finds the app's running container and
// forwards follow/tail to Runtime.ContainerLogs, returning its reader.
func TestAppLogsRunning(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	wantReader := io.NopCloser(strings.NewReader("scripted log data"))
	rt.logsReader = wantReader

	rc, err := eng.AppLogs(ctx, "blog", true, 500)
	if err != nil {
		t.Fatalf("AppLogs: %v", err)
	}
	if rc != wantReader {
		t.Errorf("AppLogs returned a different reader than Runtime.ContainerLogs supplied")
	}

	if len(rt.logsCalls) != 1 {
		t.Fatalf("logsCalls = %+v, want exactly 1 call", rt.logsCalls)
	}
	call := rt.logsCalls[0]
	if !call.follow || call.tail != 500 {
		t.Errorf("logsCalls[0] = %+v, want follow=true tail=500", call)
	}
	var wantID string
	for id, c := range rt.containers {
		if c.Name == "bp-blog-1" {
			wantID = id
		}
	}
	if call.nameOrID != wantID {
		t.Errorf("logsCalls[0].nameOrID = %q, want %q (bp-blog-1's container ID)", call.nameOrID, wantID)
	}
}

// TestAppLogsPropagatesRuntimeError proves a Runtime.ContainerLogs error
// (e.g. the container vanished between ListContainers and the logs call)
// surfaces from AppLogs rather than being swallowed.
func TestAppLogsPropagatesRuntimeError(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rt.logsErr = errors.New("logs boom")

	_, err = eng.AppLogs(ctx, "blog", false, 200)
	if err == nil || !strings.Contains(err.Error(), "logs boom") {
		t.Fatalf("err = %v, want it to wrap %q", err, "logs boom")
	}
}
