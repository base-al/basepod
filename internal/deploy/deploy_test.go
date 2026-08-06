package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/build"
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

	// images is an in-memory set of image refs "present" in this fake's
	// local image store: ImageExists reports whether a ref is a member,
	// PullImage adds one on success (mirroring a real pull leaving the
	// image locally present), and RemoveImage deletes one. Tests can also
	// seed it directly to simulate images already on disk from a prior
	// build/pull.
	images map[string]bool

	imageExistsErr   error
	removeImageErr   error
	listImageTagsErr error

	// removedImages records every RemoveImage call's ref, in call order —
	// used by retention tests to assert exactly which tags were pruned.
	removedImages []string

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
	return &fakeRuntime{ops: ops, containers: map[string]podman.ContainerInfo{}, images: map[string]bool{}}
}

func (f *fakeRuntime) record(s string) { *f.ops = append(*f.ops, s) }

func (f *fakeRuntime) PullImage(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("pull:" + ref)
	if f.pullErr != nil {
		return f.pullErr
	}
	if f.images == nil {
		f.images = map[string]bool{}
	}
	f.images[ref] = true
	return nil
}

func (f *fakeRuntime) ImageExists(ctx context.Context, ref string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.record("image-exists:" + ref)
	if f.imageExistsErr != nil {
		return false, f.imageExistsErr
	}
	return f.images[ref], nil
}

// RemoveImage mirrors real podman's own safety behavior: without force, an
// image referenced by any currently-known container (running or not)
// fails to remove — a fake that let force=false silently "succeed" against
// an in-use image would hide exactly the class of bug this file's
// TestPruneBuiltImagesProtectsCurrentlyRunningImage regression-tests, so
// any future retention/rollback bug that tries to remove the live image
// trips this check loudly rather than passing by accident.
func (f *fakeRuntime) RemoveImage(ctx context.Context, ref string, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("remove-image:" + ref)
	if !force {
		for _, c := range f.containers {
			if c.Image == ref {
				return fmt.Errorf("fakeRuntime: RemoveImage: image %q is in use by container %q (force=false)", ref, c.Name)
			}
		}
	}
	if f.removeImageErr != nil {
		return f.removeImageErr
	}
	f.removedImages = append(f.removedImages, ref)
	delete(f.images, ref)
	return nil
}

func (f *fakeRuntime) ListImageTags(ctx context.Context, repoPrefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.listImageTagsErr != nil {
		return nil, f.listImageTagsErr
	}
	prefix := repoPrefix + ":"
	var tags []string
	for ref := range f.images {
		if strings.HasPrefix(ref, prefix) {
			tags = append(tags, ref)
		}
	}
	return tags, nil
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
	f.containers[id] = podman.ContainerInfo{ID: id, Name: spec.Name, State: "created", Labels: labels, Image: spec.Image}
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
	dashboard := &caddy.DashboardRoute{Hostname: "basepod.apps.localhost", Upstream: caddy.DashboardSockDial()}
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
	dashboard := &caddy.DashboardRoute{Hostname: "basepod.apps.localhost", Upstream: caddy.DashboardSockDial()}
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

// fakeBuildRuntime is a test double for build.BuildRuntime: it writes a
// scripted log line (if any) to the logSink it's given and returns a
// scripted error.
type fakeBuildRuntime struct {
	logLine string
	err     error
	calls   int
}

func (f *fakeBuildRuntime) BuildImage(ctx context.Context, tag, dockerfile string, contextTar io.Reader, logSink io.Writer) error {
	f.calls++
	if f.logLine != "" {
		io.WriteString(logSink, f.logLine)
	}
	return f.err
}

// gzipTarWithContainerfile builds a minimal valid gzipped-tar upload body
// containing just a root Containerfile, for feeding into DeployBuild.
func gzipTarWithContainerfile(t *testing.T) io.Reader {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	body := []byte("FROM alpine\n")
	if err := tw.WriteHeader(&tar.Header{Name: "Containerfile", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
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

// TestDeployBuildHappyPathSkipsPull proves DeployBuild builds the
// uploaded tarball into a local image tag, records it (and the build log
// path) on the deployment row, rolls the new container out exactly like
// Deploy, and — critically — never calls Runtime.PullImage, since a
// "localhost/basepod/..." tag is already local.
func TestDeployBuildHappyPathSkipsPull(t *testing.T) {
	st := openStore(t)
	eng, rt, router, prober, ops := newTestEngine(t, st)
	buildRt := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}
	if dep.Status != "healthy" {
		t.Fatalf("dep.Status = %q, want healthy", dep.Status)
	}
	if dep.Source != "tarball" {
		t.Fatalf("dep.Source = %q, want tarball", dep.Source)
	}
	wantTag := "localhost/basepod/blog:1"
	if dep.ImageRef != wantTag {
		t.Fatalf("dep.ImageRef = %q, want %q", dep.ImageRef, wantTag)
	}
	if dep.BuildLogPath == "" {
		t.Fatal("dep.BuildLogPath empty, want the build log path recorded")
	}
	if buildRt.calls != 1 {
		t.Fatalf("BuildImage calls = %d, want 1", buildRt.calls)
	}

	for _, op := range *ops {
		if strings.HasPrefix(op, "pull:") {
			t.Fatalf("ops contains a pull op %q — DeployBuild must never pull a local build tag: %v", op, *ops)
		}
	}

	var found *podman.ContainerInfo
	for _, c := range rt.containers {
		if c.Name == "bp-blog-1" {
			cc := c
			found = &cc
		}
	}
	if found == nil || found.State != "running" {
		t.Fatalf("bp-blog-1 = %+v, want a running container", found)
	}
	if router.calls != 1 {
		t.Fatalf("router.calls = %d, want 1", router.calls)
	}
	if len(prober.calls) != 1 {
		t.Fatalf("prober.calls = %v, want 1 call", prober.calls)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "running" || gotApp.ImageRef != wantTag {
		t.Fatalf("app = %+v, want running/%s", gotApp, wantTag)
	}
}

// TestDeployBuildFailureMarksDeploymentFailedAndRestoresStatus proves a
// failing build goes through the same fail() path as any other deploy
// failure: the deployment is marked failed with the build error, no
// container is ever created, the build log path is still recorded (with
// whatever was streamed before the failure), and — since this is the
// app's first-ever deploy attempt, with no other container up — the app
// status is restored to "error".
func TestDeployBuildFailureMarksDeploymentFailedAndRestoresStatus(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	buildErr := errors.New("executor failed running [/bin/sh -c false]")
	buildRt := &fakeBuildRuntime{logLine: "Step 1/1 : RUN false\n", err: buildErr}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err == nil {
		t.Fatal("expected an error from the failed build")
	}
	if !strings.Contains(err.Error(), "executor failed running") {
		t.Fatalf("err = %v, want it to contain the build failure message", err)
	}
	if dep.Status != "failed" {
		t.Fatalf("dep.Status = %q, want failed", dep.Status)
	}
	if dep.BuildLogPath == "" {
		t.Fatal("dep.BuildLogPath empty, want the build log path recorded even on failure")
	}
	data, rerr := os.ReadFile(dep.BuildLogPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != "Step 1/1 : RUN false\n" {
		t.Fatalf("build log contents = %q", data)
	}
	if len(rt.containers) != 0 {
		t.Fatalf("containers = %+v, want none created (build fails before any container)", rt.containers)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "error" {
		t.Fatalf("app.Status = %q, want error (no containers at all)", gotApp.Status)
	}
}

// TestDeployBuildFailureKeepsOldContainerRunning proves that when a build
// fails on a *redeploy* (the app already has a healthy container from a
// prior deploy), the app status is restored to "running" rather than
// "error" — mirroring TestFailedProbeKeepsOld's assertion for the
// image-pull path.
func TestDeployBuildFailureKeepsOldContainerRunning(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	buildRt := &fakeBuildRuntime{err: errors.New("build boom")}
	builder := build.New(buildRt, t.TempDir(), 2)

	dep2, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err == nil {
		t.Fatal("expected an error from the failed build")
	}
	if dep2.Status != "failed" {
		t.Fatalf("dep2.Status = %q, want failed", dep2.Status)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "running" {
		t.Fatalf("app.Status = %q, want running (old container still up)", gotApp.Status)
	}
	if gotApp.ImageRef != "nginx:v1" {
		t.Fatalf("app.ImageRef = %q, want unchanged nginx:v1", gotApp.ImageRef)
	}
}

// TestRollbackHappyPathLocalBuiltTagSkipsPull proves Rollback to an
// earlier, still-healthy, locally-built deployment never calls PullImage
// (a "localhost/..." tag is never fetchable from a registry), runs the
// normal rollout (new container created, probed, routed, old removed),
// and records the new deployment with trigger_kind "rollback" and the
// target's own source ("tarball").
func TestRollbackHappyPathLocalBuiltTagSkipsPull(t *testing.T) {
	st := openStore(t)
	eng, rt, router, prober, ops := newTestEngine(t, st)
	buildRt := &fakeBuildRuntime{}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep1, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err != nil {
		t.Fatalf("first DeployBuild: %v", err)
	}
	rt.images[dep1.ImageRef] = true // simulate the built image landing on disk

	dep2, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err != nil {
		t.Fatalf("second DeployBuild: %v", err)
	}
	rt.images[dep2.ImageRef] = true

	*ops = nil // only care about ops during the rollback itself
	router.calls = 0
	prober.calls = nil

	dep3, err := eng.Rollback(ctx, app, dep1.Number)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if dep3.Status != "healthy" {
		t.Fatalf("dep3.Status = %q, want healthy", dep3.Status)
	}
	if dep3.TriggerKind != "rollback" {
		t.Fatalf("dep3.TriggerKind = %q, want rollback", dep3.TriggerKind)
	}
	if dep3.Source != "tarball" {
		t.Fatalf("dep3.Source = %q, want tarball (the rolled-back-to deployment's own source)", dep3.Source)
	}
	if dep3.ImageRef != dep1.ImageRef {
		t.Fatalf("dep3.ImageRef = %q, want %q (dep1's image)", dep3.ImageRef, dep1.ImageRef)
	}
	if dep3.Number != 3 {
		t.Fatalf("dep3.Number = %d, want 3", dep3.Number)
	}

	for _, op := range *ops {
		if strings.HasPrefix(op, "pull:") {
			t.Fatalf("ops contains a pull op %q — rollback to a local built tag must never pull: %v", op, *ops)
		}
	}
	if router.calls != 1 {
		t.Fatalf("router.calls = %d, want 1", router.calls)
	}
	if len(prober.calls) != 1 {
		t.Fatalf("prober.calls = %v, want 1 call", prober.calls)
	}

	var found *podman.ContainerInfo
	for _, c := range rt.containers {
		if c.Name == "bp-blog-3" {
			cc := c
			found = &cc
		}
	}
	if found == nil || found.State != "running" {
		t.Fatalf("bp-blog-3 = %+v, want a running container", found)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.ImageRef != dep1.ImageRef {
		t.Fatalf("app.ImageRef = %q, want %q", gotApp.ImageRef, dep1.ImageRef)
	}
}

// TestRollbackPullsWhenRegistryImageAbsent proves that when the rollback
// target is a registry-sourced deployment ("image" source, not a
// "localhost/..." build tag) whose image is no longer present locally,
// Rollback pulls it before rolling out rather than failing outright.
func TestRollbackPullsWhenRegistryImageAbsent(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, ops := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep1, err := eng.Deploy(ctx, app, "nginx:v1")
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v2"); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	// Simulate the v1 image having since been pruned/removed from the
	// local image store (e.g. an out-of-band `podman image prune`).
	delete(rt.images, "nginx:v1")
	*ops = nil

	dep3, err := eng.Rollback(ctx, app, dep1.Number)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if dep3.ImageRef != "nginx:v1" {
		t.Fatalf("dep3.ImageRef = %q, want nginx:v1", dep3.ImageRef)
	}
	if dep3.TriggerKind != "rollback" {
		t.Fatalf("dep3.TriggerKind = %q, want rollback", dep3.TriggerKind)
	}

	found := false
	for _, op := range *ops {
		if op == "pull:nginx:v1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ops = %v, want a pull:nginx:v1 op (image was absent locally)", *ops)
	}
}

// TestRollbackTargetNotFound proves rolling back to a deployment number
// that doesn't exist for the app returns ErrRollbackTargetNotFound.
func TestRollbackTargetNotFound(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.Rollback(ctx, app, 99)
	if !errors.Is(err, ErrRollbackTargetNotFound) {
		t.Fatalf("err = %v, want ErrRollbackTargetNotFound", err)
	}
	if dep != nil {
		t.Fatalf("dep = %+v, want nil", dep)
	}
}

// TestRollbackTargetUnhealthy proves rolling back to a deployment whose
// Status isn't "healthy" (e.g. it failed) returns ErrRollbackTargetUnhealthy
// without creating any new deployment row.
func TestRollbackTargetUnhealthy(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	failedDep, err := st.CreateDeployment(app.ID, "nginx:bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(failedDep.ID, "failed", "boom"); err != nil {
		t.Fatal(err)
	}

	dep, err := eng.Rollback(ctx, app, failedDep.Number)
	if !errors.Is(err, ErrRollbackTargetUnhealthy) {
		t.Fatalf("err = %v, want ErrRollbackTargetUnhealthy", err)
	}
	if dep != nil {
		t.Fatalf("dep = %+v, want nil", dep)
	}

	deps, err := st.ListDeployments(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("deployments = %+v, want still just the one failed deployment (no rollback row created)", deps)
	}
}

// TestRollbackImageMissingLocalBuiltTag proves that when the rollback
// target is a locally-built tag ("localhost/basepod/<slug>:<n>") that no
// longer exists in the local image store (pruned by retention or removed
// out-of-band), Rollback fails with the typed ErrRollbackImageMissing
// rather than attempting (and failing) a pull — a localhost/ tag can never
// be fetched from a registry — and creates no new deployment row.
func TestRollbackImageMissingLocalBuiltTag(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentFull(app.ID, "localhost/basepod/blog:1", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	// rt.images has no entry for this tag — it's "gone".

	got, err := eng.Rollback(ctx, app, dep.Number)
	if !errors.Is(err, ErrRollbackImageMissing) {
		t.Fatalf("err = %v, want ErrRollbackImageMissing", err)
	}
	if got != nil {
		t.Fatalf("dep = %+v, want nil", got)
	}

	deps, err := st.ListDeployments(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("deployments = %+v, want still just the one deployment (no rollback row created)", deps)
	}
}

// TestPruneBuiltImagesKeepsTop5NumericTagsIgnoresNonNumeric proves
// pruneBuiltImages keeps the 5 highest-numbered "localhost/basepod/<slug>:*"
// tags, removes the rest, and leaves any non-numeric tag (e.g. a stray
// "latest") untouched entirely — it's not part of the numbered ordering at
// all.
func TestPruneBuiltImagesKeepsTop5NumericTagsIgnoresNonNumeric(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	for n := 1; n <= 7; n++ {
		rt.images[fmt.Sprintf("localhost/basepod/blog:%d", n)] = true
	}
	rt.images["localhost/basepod/blog:latest"] = true
	rt.images["localhost/basepod/other:9"] = true // different app, must never be touched

	// currentImageRef deliberately doesn't match any seeded tag, so this
	// test exercises pure numeric-ranking behavior without the
	// current-image protection (covered separately by
	// TestPruneBuiltImagesProtectsCurrentlyRunningImage) changing which
	// tags survive.
	eng.pruneBuiltImages(ctx, "blog", "localhost/basepod/blog:not-a-seeded-tag")

	sort.Strings(rt.removedImages)
	wantRemoved := []string{"localhost/basepod/blog:1", "localhost/basepod/blog:2"}
	if !reflect.DeepEqual(rt.removedImages, wantRemoved) {
		t.Fatalf("removedImages = %v, want %v", rt.removedImages, wantRemoved)
	}

	for n := 3; n <= 7; n++ {
		ref := fmt.Sprintf("localhost/basepod/blog:%d", n)
		if !rt.images[ref] {
			t.Errorf("%s should have been kept (top 5), was removed", ref)
		}
	}
	if !rt.images["localhost/basepod/blog:latest"] {
		t.Error("localhost/basepod/blog:latest should never be touched (not a numeric tag)")
	}
	if !rt.images["localhost/basepod/other:9"] {
		t.Error("localhost/basepod/other:9 should never be touched (different app's tag)")
	}
}

// TestPruneBuiltImagesNoopUnderRetentionLimit proves pruneBuiltImages does
// nothing (no RemoveImage calls) when there are retainBuiltImages or fewer
// numbered tags.
func TestPruneBuiltImagesNoopUnderRetentionLimit(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	for n := 1; n <= retainBuiltImages; n++ {
		rt.images[fmt.Sprintf("localhost/basepod/blog:%d", n)] = true
	}

	eng.pruneBuiltImages(ctx, "blog", "localhost/basepod/blog:not-a-seeded-tag")

	if len(rt.removedImages) != 0 {
		t.Fatalf("removedImages = %v, want none", rt.removedImages)
	}
}

// TestDeployBuildTriggersRetentionAfterSuccess is an integration-level
// regression test proving pruneBuiltImages is actually wired into
// runRollout's success path: with more than retainBuiltImages built tags
// already present in the local image store (as if left over from before
// retention ever ran), a single successful DeployBuild prunes the oldest
// down to retainBuiltImages automatically — the caller doesn't have to
// invoke retention itself.
func TestDeployBuildTriggersRetentionAfterSuccess(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	buildRt := &fakeBuildRuntime{}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-seed 6 older built tags, deliberately numbered well above the
	// deployment number this DeployBuild call will actually produce
	// (which will be 1, for a fresh app's first deployment) — the
	// pruning mechanism this test exercises only cares about a tag's
	// numeric suffix, not real deployment numbering, and choosing
	// non-colliding numbers avoids the just-built tag's own retention
	// protection (see TestPruneBuiltImagesProtectsCurrentlyRunningImage)
	// from being the thing keeping any of these old tags alive.
	for _, n := range []int{10, 11, 12, 13, 14, 15} {
		rt.images[fmt.Sprintf("localhost/basepod/blog:%d", n)] = true
	}

	if _, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder); err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}

	if len(rt.removedImages) != 1 {
		t.Fatalf("removedImages = %v, want exactly 1 (retention pruned automatically after the successful rollout)", rt.removedImages)
	}
	if rt.removedImages[0] != "localhost/basepod/blog:10" {
		t.Fatalf("removedImages = %v, want [localhost/basepod/blog:10] (the oldest)", rt.removedImages)
	}
	if len(rt.images) != retainBuiltImages {
		t.Fatalf("images remaining = %d (%v), want exactly %d", len(rt.images), rt.images, retainBuiltImages)
	}
}

// TestPruneBuiltImagesProtectsCurrentlyRunningImage is a regression test
// for a review finding: pruneBuiltImages used to rank purely by numeric
// tag, so after a Rollback to an old tag with more than retainBuiltImages
// newer tags still on disk, the just-rolled-back-to (now currently
// running) image could fall outside the numeric top-5 and get handed to
// RemoveImage — even though a live container references it. Protection
// must key off the actual current image, not tag recency.
func TestPruneBuiltImagesProtectsCurrentlyRunningImage(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	// 7 healthy deployments, tags 1-7: tag 1 is the old one about to be
	// rolled back to; tags 2-7 are newer builds still present on disk —
	// tags 3-7 are the numeric top-5, so a naive "keep top 5" prune would
	// remove both 1 and 2, even after rolling back to 1.
	for n := 1; n <= 7; n++ {
		ref := fmt.Sprintf("localhost/basepod/blog:%d", n)
		rt.images[ref] = true
		dep, err := st.CreateDeploymentFull(app.ID, ref, "tarball", "api")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
			t.Fatal(err)
		}
	}

	got, err := eng.Rollback(ctx, app, 1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got.ImageRef != "localhost/basepod/blog:1" {
		t.Fatalf("got.ImageRef = %q, want localhost/basepod/blog:1", got.ImageRef)
	}

	for _, removed := range rt.removedImages {
		if removed == "localhost/basepod/blog:1" {
			t.Fatalf("removedImages = %v, must never include the just-rolled-back-to (currently running) image", rt.removedImages)
		}
	}
	if !rt.images["localhost/basepod/blog:1"] {
		t.Fatal("localhost/basepod/blog:1 should still be present after retention (it's the current image)")
	}
	// Pin the exact outcome down, not just "tag 1 survived": the
	// protected set is {current} ∪ {top-5 by number} = {1,3,4,5,6,7}, so
	// tag 2 is the only one actually outside it.
	if len(rt.removedImages) != 1 || rt.removedImages[0] != "localhost/basepod/blog:2" {
		t.Fatalf("removedImages = %v, want exactly [localhost/basepod/blog:2]", rt.removedImages)
	}
}

// TestPruneBuiltImagesProtectsCurrentImageEvenWithoutAContainer isolates
// pruneBuiltImages's own protected-set logic from fakeRuntime.RemoveImage's
// separate in-use-container safety net (see that method's doc comment):
// no container exists at all here, so if currentImageRef still survives
// retention, it can only be because pruneBuiltImages itself excluded it —
// not because the fake happened to reject an in-use removal. This is what
// actually pins down the production fix for the review finding
// TestPruneBuiltImagesProtectsCurrentlyRunningImage exercises end-to-end;
// without it, that other test would pass even if the protected-set logic
// were accidentally removed, purely by riding on the fake's in-use check.
func TestPruneBuiltImagesProtectsCurrentImageEvenWithoutAContainer(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	for n := 1; n <= 7; n++ {
		rt.images[fmt.Sprintf("localhost/basepod/blog:%d", n)] = true
	}
	eng.pruneBuiltImages(ctx, "blog", "localhost/basepod/blog:1")

	for _, removed := range rt.removedImages {
		if removed == "localhost/basepod/blog:1" {
			t.Fatalf("removedImages = %v, must never include currentImageRef even without a container in-use rejection to fall back on", rt.removedImages)
		}
	}
	if len(rt.removedImages) != 1 || rt.removedImages[0] != "localhost/basepod/blog:2" {
		t.Fatalf("removedImages = %v, want exactly [localhost/basepod/blog:2]", rt.removedImages)
	}
}

// TestDeployBuildRemovesOrphanImageWhenRolloutFails proves that when a
// build succeeds (producing a local image tag) but the rollout that
// follows it fails (e.g. a failed health probe), the now-unreachable
// freshly-built tag is removed rather than left as a permanent orphan —
// runRollout's own retention (pruneBuiltImages) only runs on a
// *successful* rollout, so without this cleanup a string of failed builds
// would leave one dead tag behind per attempt, forever. The old, still
// healthy container from a prior deploy must be left completely
// untouched.
func TestDeployBuildRemovesOrphanImageWhenRolloutFails(t *testing.T) {
	st := openStore(t)
	eng, rt, _, prober, _ := newTestEngine(t, st)
	buildRt := &fakeBuildRuntime{}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	prober.failUpstream = "bp-blog-2:80"

	dep, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err == nil {
		t.Fatal("expected an error from the failed probe")
	}
	if dep.Status != "failed" {
		t.Fatalf("dep.Status = %q, want failed", dep.Status)
	}
	wantTag := "localhost/basepod/blog:2"
	if dep.ImageRef != wantTag {
		t.Fatalf("dep.ImageRef = %q, want %q", dep.ImageRef, wantTag)
	}

	found := false
	for _, removed := range rt.removedImages {
		if removed == wantTag {
			found = true
		}
	}
	if !found {
		t.Fatalf("removedImages = %v, want it to include the orphaned build tag %q", rt.removedImages, wantTag)
	}

	var oldID string
	for id, c := range rt.containers {
		if c.Name == "bp-blog-1" {
			oldID = id
		}
		if c.Name == "bp-blog-2" {
			t.Errorf("bp-blog-2 (the failed new container) should have been removed by fail()'s own cleanup, still present: %+v", c)
		}
	}
	if oldID == "" {
		t.Fatal("bp-blog-1 (old, healthy) should still exist")
	}
	if got := rt.containers[oldID]; got.State != "running" {
		t.Errorf("bp-blog-1 state = %q, want still running (untouched)", got.State)
	}
}
