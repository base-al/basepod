package caddy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/podman"
)

// var _ Runtime = (*podman.Client)(nil) documents (and enforces at compile
// time) that *podman.Client satisfies the Runtime interface the manager
// consumes.
var _ Runtime = (*podman.Client)(nil)

// testInstanceID is the instance id every test in this file builds its
// Manager against, unless a test specifically needs a different one (see
// TestEnsureRefusesForeignInstanceCaddy) — arbitrary, just needs to be
// stable and non-empty.
const testInstanceID = "test-instance-aaa"

// fakeRuntime is a test double for Runtime: it records the ordered
// sequence of calls made against it and returns canned results.
type fakeRuntime struct {
	calls []string

	inspectInfo *podman.ContainerInfo
	inspectErr  error

	createSpec podman.CreateSpec
	createID   string
	createErr  error

	startErr   error
	pullErr    error
	networkErr error
	stopErr    error
	removeErr  error

	// archResult is ImageArchitecture's canned answer; "" means "report
	// whatever the current host's runtime.GOARCH is" (i.e. always match a
	// Manager's default expectedArch), so tests that don't care about the
	// arch check never need to set this. Tests proving the mismatch path
	// set it to a value that deliberately differs from the Manager's
	// expectedArch (itself overridden directly, since manager_test.go is
	// package caddy).
	archResult string
	archErr    error
}

func (f *fakeRuntime) EnsureNetwork(ctx context.Context, name, instanceID string) error {
	f.calls = append(f.calls, "EnsureNetwork")
	return f.networkErr
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string) error {
	f.calls = append(f.calls, "PullImage")
	return f.pullErr
}

func (f *fakeRuntime) ImageArchitecture(ctx context.Context, ref string) (string, error) {
	f.calls = append(f.calls, "ImageArchitecture")
	if f.archErr != nil {
		return "", f.archErr
	}
	if f.archResult == "" {
		return runtime.GOARCH, nil
	}
	return f.archResult, nil
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, spec podman.CreateSpec) (string, error) {
	f.calls = append(f.calls, "CreateContainer")
	f.createSpec = spec
	if f.createErr != nil {
		return "", f.createErr
	}
	id := f.createID
	if id == "" {
		id = "new-container-id"
	}
	return id, nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id string) error {
	f.calls = append(f.calls, "StartContainer")
	return f.startErr
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, nameOrID string) (*podman.ContainerInfo, error) {
	f.calls = append(f.calls, "InspectContainer")
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	return f.inspectInfo, nil
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id string, timeoutSec int) error {
	f.calls = append(f.calls, "StopContainer")
	return f.stopErr
}

func (f *fakeRuntime) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.calls = append(f.calls, "RemoveContainer")
	return f.removeErr
}

// fakeExecer is a test double for Execer: it records the argv of every
// invocation. If errSequence is set, each call pops and returns the next
// entry (nil entries count as success); once exhausted, err (which may
// itself be nil) is returned for every subsequent call.
type fakeExecer struct {
	calls       [][]string
	err         error
	errSequence []error
}

func (f *fakeExecer) Exec(ctx context.Context, container string, cmd ...string) error {
	args := append([]string{container}, cmd...)
	f.calls = append(f.calls, args)
	if len(f.errSequence) > 0 {
		err := f.errSequence[0]
		f.errSequence = f.errSequence[1:]
		return err
	}
	return f.err
}

// TestDashboardSockDial proves the Caddy dial string built for the
// dashboard's unix socket follows the same "unix/" + absolute-path
// pattern as AdminSocket, giving the exact form Caddy's docs (and this
// package's own admin-socket usage) expect: "unix//run/basepod/api.sock".
func TestDashboardSockDial(t *testing.T) {
	want := "unix//run/basepod/api.sock"
	if got := DashboardSockDial(); got != want {
		t.Errorf("DashboardSockDial() = %q, want %q", got, want)
	}
}

func TestEnsureCreatesWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectErr: podman.ErrNotFound}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "PullImage", "ImageArchitecture", "CreateContainer", "StartContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantCalls)
	}

	spec := fr.createSpec
	if spec.Name != ContainerName {
		t.Errorf("spec.Name = %q, want %q", spec.Name, ContainerName)
	}
	if spec.Image != PinnedImage {
		t.Errorf("spec.Image = %q, want %q", spec.Image, PinnedImage)
	}
	if spec.RestartPolicy != "always" {
		t.Errorf("spec.RestartPolicy = %q, want %q", spec.RestartPolicy, "always")
	}
	if spec.NetworkName != NetworkName {
		t.Errorf("spec.NetworkName = %q, want %q", spec.NetworkName, NetworkName)
	}
	// issue #10: bp-caddy must carry this instance's id so a second BasePod
	// instance sharing this Podman socket can recognize it as not-mine.
	if spec.Labels["basepod.instance"] != testInstanceID {
		t.Errorf("spec.Labels[basepod.instance] = %q, want %q", spec.Labels["basepod.instance"], testInstanceID)
	}
	// Command must create /var/run/caddy (AdminSocket's parent dir, which
	// the image doesn't ship) on the container's own filesystem before
	// handing off to caddy: see the comment on this Command in manager.go
	// for why that directory can't be a bind mount.
	wantCommand := []string{"sh", "-c", "mkdir -p /var/run/caddy && exec caddy run --config /etc/caddy/current.json"}
	if !reflect.DeepEqual(spec.Command, wantCommand) {
		t.Errorf("spec.Command = %v, want %v", spec.Command, wantCommand)
	}

	dataDir := filepath.Join(filepath.Dir(configDir), "caddy-data")
	sockDir := filepath.Join(filepath.Dir(configDir), "caddy-sock")
	// Mount sources are resolved to their real (symlink-free) path (see
	// manager.go's create): on macOS, t.TempDir() lives under /var/folders,
	// itself a symlink to /private/var/folders, so the resolved path
	// differs from configDir/dataDir/sockDir as constructed above.
	wantConfigSrc, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(configDir): %v", err)
	}
	wantDataSrc, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(dataDir): %v", err)
	}
	wantSockSrc, err := filepath.EvalSymlinks(sockDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(sockDir): %v", err)
	}
	wantMounts := []podman.BindMount{
		{Source: wantConfigSrc, Dest: "/etc/caddy"},
		{Source: wantDataSrc, Dest: "/data"},
		{Source: wantSockSrc, Dest: DashboardSockMountDest},
	}
	if !reflect.DeepEqual(spec.Mounts, wantMounts) {
		t.Errorf("spec.Mounts = %+v, want %+v", spec.Mounts, wantMounts)
	}

	wantPorts := []podman.PortMapping{
		{ContainerPort: 80, HostPort: 8080},
		{ContainerPort: 443, HostPort: 8443},
	}
	if !reflect.DeepEqual(spec.PortMappings, wantPorts) {
		t.Errorf("spec.PortMappings = %+v, want %+v", spec.PortMappings, wantPorts)
	}

	if fi, err := os.Stat(dataDir); err != nil || !fi.IsDir() {
		t.Errorf("caddy-data dir not created at %s: %v", dataDir, err)
	}
	if fi, err := os.Stat(sockDir); err != nil || !fi.IsDir() {
		t.Errorf("caddy-sock dir not created at %s: %v", sockDir, err)
	} else if perm := fi.Mode().Perm(); perm&0o007 != 0 {
		// The umask in effect may strip bits MkdirAll(0o750) requested, so
		// this only asserts the property that actually matters (no
		// "other" access), rather than an exact 0750 match that could be
		// legitimately narrower under a stricter umask.
		t.Errorf("caddy-sock dir mode = %o, want no world-accessible bits", perm)
	}

	got, err := os.ReadFile(filepath.Join(configDir, "current.json"))
	if err != nil {
		t.Fatalf("reading current.json: %v", err)
	}
	if !strings.Contains(string(got), "unix//var/run/caddy/admin.sock") {
		t.Errorf("current.json missing admin socket, got: %s", got)
	}
}

// matchingPorts is the {80,443} -> {8080,8443} port set every drift test
// in this file builds its Manager against (NewManager(..., 8080, 8443)),
// so a fakeRuntime.inspectInfo carrying these plus Image: PinnedImage
// represents an existing container with no drift.
var matchingPorts = []podman.PortMapping{
	{ContainerPort: 80, HostPort: 8080},
	{ContainerPort: 443, HostPort: 8443},
}

// wantMounts computes the desired bind-mount set (config, data, and
// socket dirs, all resolved through EvalSymlinks exactly as
// manager.go's resolvedMountSources does) for a Manager rooted at
// configDir, so a "no drift" test can build a fakeRuntime.inspectInfo
// whose Mounts matches it. It also creates the three directories (via
// MkdirAll, mirroring resolvedMountSources), since EvalSymlinks requires
// them to exist.
func wantMounts(t *testing.T, configDir string) []podman.BindMount {
	t.Helper()
	dataDir := filepath.Join(filepath.Dir(configDir), "caddy-data")
	sockDir := filepath.Join(filepath.Dir(configDir), "caddy-sock")
	for _, d := range []string{configDir, dataDir, sockDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	configSrc, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(configDir): %v", err)
	}
	dataSrc, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(dataDir): %v", err)
	}
	sockSrc, err := filepath.EvalSymlinks(sockDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(sockDir): %v", err)
	}
	return []podman.BindMount{
		{Source: configSrc, Dest: "/etc/caddy"},
		{Source: dataSrc, Dest: "/data"},
		{Source: sockSrc, Dest: DashboardSockMountDest},
	}
}

func TestEnsureStartsWhenStopped(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "exited",
		Image: PinnedImage, Ports: matchingPorts, Mounts: wantMounts(t, configDir),
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "StartContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantCalls)
	}
}

func TestEnsureNoopWhenRunning(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: PinnedImage, Ports: matchingPorts, Mounts: wantMounts(t, configDir),
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantCalls)
	}
}

// TestEnsureAdoptsUnlabeledCaddy proves a pre-v0.5 bp-caddy container —
// carrying no basepod.instance label at all, the exact shape a real
// upgrade from v0.4.2 (no instance labeling) to this fix finds on its
// first boot (issue #10) — is adopted rather than refused or recreated:
// with no other drift, Ensure treats it exactly like TestEnsureNoopWhenRunning
// treats one of its own (matching image/ports/mounts, already running),
// leaving it running untouched. It only ever picks up this instance's own
// label the next time a real drift condition (e.g. an image bump) forces
// an actual recreate — see create()'s Labels.
func TestEnsureAdoptsUnlabeledCaddy(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: PinnedImage, Ports: matchingPorts, Mounts: wantMounts(t, configDir),
		// Deliberately no Labels at all — the pre-instance-id shape.
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (adoption must not stop/remove/recreate)", fr.calls, wantCalls)
	}
}

// TestEnsureRefusesForeignInstanceCaddy proves a bp-caddy container
// labeled with a DIFFERENT instance's id — the shape a second BasePod
// daemon sharing this Podman socket produces once both are on this fix —
// makes Ensure fail loudly rather than recreating it: no pull, no
// stop/remove/create, nothing but the read-only EnsureNetwork+
// InspectContainer calls already made before the ownership check runs.
// This is the fix for the exact incident issue #10 reports (a second
// BasePod instance recreating the first's bp-caddy under its own config).
func TestEnsureRefusesForeignInstanceCaddy(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: PinnedImage, Ports: matchingPorts, Mounts: wantMounts(t, configDir),
		Labels: map[string]string{"basepod.managed": "true", "basepod.instance": "some-other-instance"},
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	err := mgr.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure should refuse to touch a foreign-instance bp-caddy, got nil error")
	}
	if !strings.Contains(err.Error(), "different BasePod instance") {
		t.Errorf("error = %q, want it to explain the instance conflict", err.Error())
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (must never pull/stop/remove/create a foreign instance's bp-caddy)", fr.calls, wantCalls)
	}
}

// wantRecreateCalls is the call sequence Ensure makes when it decides an
// existing bp-caddy container has drifted: pull+arch-verify the image
// BEFORE touching the old container, then stop+remove it, then the same
// pull+arch-verify+create+start sequence as a first-run create() — the
// image step is unconditional (see pullImage's doc comment for why it's no
// longer gated on an existence check), so it runs a second time here even
// though the first pull just confirmed the image is present and correct.
var wantRecreateCalls = []string{
	"EnsureNetwork", "InspectContainer", "PullImage", "ImageArchitecture",
	"StopContainer", "RemoveContainer", "PullImage", "ImageArchitecture", "CreateContainer", "StartContainer",
}

// TestEnsureRecreatesWhenStateCreated proves a container stuck in the
// "created" state (made but never started successfully — the known
// stale-port-mapping issue's original symptom) is stopped, removed, and
// recreated, even though its image and ports otherwise match exactly.
func TestEnsureRecreatesWhenStateCreated(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "created",
		Image: PinnedImage, Ports: matchingPorts,
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !reflect.DeepEqual(fr.calls, wantRecreateCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantRecreateCalls)
	}
	if fr.createSpec.Name != ContainerName {
		t.Errorf("recreated spec.Name = %q, want %q", fr.createSpec.Name, ContainerName)
	}
}

// TestEnsureRecreatesOnImageMismatch proves a running container whose
// image no longer matches PinnedImage (e.g. after a basepod upgrade bumps
// the Caddy version, or its digest) is recreated rather than left running
// the stale image.
func TestEnsureRecreatesOnImageMismatch(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: "docker.io/library/caddy:2.9-alpine", Ports: matchingPorts,
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !reflect.DeepEqual(fr.calls, wantRecreateCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantRecreateCalls)
	}
}

// TestEnsureRecreatesOnPortMismatch proves a running container whose
// inspected host ports don't match the currently configured
// httpPort/httpsPort (the known stale-port-mapping issue: e.g. basepod
// was reconfigured to a new HTTP port but bp-caddy was never recreated)
// is recreated with the current port mapping.
func TestEnsureRecreatesOnPortMismatch(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: PinnedImage,
		Ports: []podman.PortMapping{
			{ContainerPort: 80, HostPort: 9090}, // stale: mgr wants 8080
			{ContainerPort: 443, HostPort: 8443},
		},
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !reflect.DeepEqual(fr.calls, wantRecreateCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantRecreateCalls)
	}
	wantPorts := []podman.PortMapping{
		{ContainerPort: 80, HostPort: 8080},
		{ContainerPort: 443, HostPort: 8443},
	}
	if !reflect.DeepEqual(fr.createSpec.PortMappings, wantPorts) {
		t.Errorf("recreated spec.PortMappings = %+v, want %+v", fr.createSpec.PortMappings, wantPorts)
	}
}

// TestEnsureNoopWhenAllMatch is the drift matrix's negative case: a
// running container whose state, image, ports, and mounts (regardless of
// declaration order) all match the desired spec exactly must not be
// touched at all.
func TestEnsureNoopWhenAllMatch(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	mounts := wantMounts(t, configDir)
	// Deliberately reversed order vs. desiredMounts() too, to prove that
	// comparison is set-based, not positional, exactly like ports below.
	reversedMounts := []podman.BindMount{mounts[2], mounts[1], mounts[0]}

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: PinnedImage,
		// Deliberately reversed order vs. desiredPorts() to prove the
		// comparison is set-based, not positional.
		Ports: []podman.PortMapping{
			{ContainerPort: 443, HostPort: 8443},
			{ContainerPort: 80, HostPort: 8080},
		},
		Mounts: reversedMounts,
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	wantCalls := []string{"EnsureNetwork", "InspectContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (no drift — must not stop/remove/recreate)", fr.calls, wantCalls)
	}
}

// TestEnsureRecreatesOnMountMismatch proves a running container whose
// inspected bind mounts don't match the currently desired set is
// recreated with the correct mounts — the case that matters most in
// practice: a bp-caddy container created by a pre-dashboard BasePod build
// (missing the caddy-sock mount entirely) must be recreated on upgrade so
// it picks up the new mount, rather than running forever without it.
func TestEnsureRecreatesOnMountMismatch(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{
		ID: "abc123", Name: ContainerName, State: "running",
		Image: PinnedImage, Ports: matchingPorts,
		// No caddy-sock mount at all — as an old (pre-dashboard) bp-caddy
		// container would report.
		Mounts: []podman.BindMount{
			{Source: "/old/config", Dest: "/etc/caddy"},
			{Source: "/old/data", Dest: "/data"},
		},
	}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !reflect.DeepEqual(fr.calls, wantRecreateCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantRecreateCalls)
	}

	want := wantMounts(t, configDir)
	if !reflect.DeepEqual(fr.createSpec.Mounts, want) {
		t.Errorf("recreated spec.Mounts = %+v, want %+v", fr.createSpec.Mounts, want)
	}
}

// TestEnsureDriftRecreatePullFailureLeavesOldContainer proves the ordering
// fix at the heart of this change: if the image needs pulling and the pull
// fails (registry blip, rate limit), the old drifted container must never
// be stopped or removed — StopContainer/RemoveContainer must not appear in
// the call log at all — so a transient pull failure never leaves the host
// with zero ingress. Ensure must surface the pull error.
func TestEnsureDriftRecreatePullFailureLeavesOldContainer(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	pullErr := errors.New("registry: rate limited")
	fr := &fakeRuntime{
		inspectInfo: &podman.ContainerInfo{
			ID: "abc123", Name: ContainerName, State: "created",
			Image: PinnedImage, Ports: matchingPorts,
		},
		pullErr: pullErr,
	}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	err := mgr.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure: expected error, got nil")
	}
	if !errors.Is(err, pullErr) {
		t.Fatalf("Ensure error = %v, want wrapping %v", err, pullErr)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "PullImage"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (old container must never be stopped/removed on pull failure, and a failed pull must short-circuit before the arch check)", fr.calls, wantCalls)
	}
}

// TestEnsureDriftRecreateAlwaysPulls proves the drift-recreate path calls
// PullImage even when the image is already present locally — replacing
// v0.3's first cut, which gated the pull on an ImageExists check and so
// could skip re-resolving the image manifest entirely. That skip is the
// production bug this change fixes: on an arm64 host with a wrong-arch
// (amd64) copy of the image already cached (e.g. left behind by an earlier
// `podman build --platform linux/amd64`), ImageExists reports true, the
// pull that would have re-resolved the manifest for the host arch never
// happens, and bp-caddy starts under emulation and crashes instantly. The
// pull must happen (and, per pullImage, be arch-verified) before the old
// container is stopped/removed, exactly like a genuinely missing image.
func TestEnsureDriftRecreateAlwaysPulls(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{
		inspectInfo: &podman.ContainerInfo{
			ID: "abc123", Name: ContainerName, State: "created",
			Image: PinnedImage, Ports: matchingPorts,
		},
	}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !reflect.DeepEqual(fr.calls, wantRecreateCalls) {
		t.Fatalf("calls = %v, want %v (PullImage must run before StopContainer/RemoveContainer)", fr.calls, wantRecreateCalls)
	}
}

// TestEnsureCreateAlwaysPulls covers the same "always pull, never gated on
// existence" fix on the plain first-run create() path (no existing
// container at all), not just drift-recreate.
func TestEnsureCreateAlwaysPulls(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectErr: podman.ErrNotFound}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "PullImage", "ImageArchitecture", "CreateContainer", "StartContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantCalls)
	}
}

// TestEnsureDriftRecreateArchMismatchLeavesOldContainer proves the
// defensive arch check added alongside the always-pull fix: if the pulled
// image's architecture doesn't match the Manager's expectedArch, Ensure
// returns an actionable error (naming both architectures and the fix
// command) and — critically — never stops or removes the still-running old
// container, exactly like a pull failure. expectedArch is overridden
// directly here (this file is package caddy) to deterministically force a
// mismatch regardless of which architecture the test happens to run on.
func TestEnsureDriftRecreateArchMismatchLeavesOldContainer(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{
		inspectInfo: &podman.ContainerInfo{
			ID: "abc123", Name: ContainerName, State: "created",
			Image: PinnedImage, Ports: matchingPorts,
		},
		archResult: "amd64",
	}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)
	mgr.expectedArch = "arm64"

	err := mgr.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure: expected error, got nil")
	}
	for _, want := range []string{"amd64", "arm64", "podman pull --arch arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Ensure error = %q, want it to contain %q", err.Error(), want)
		}
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "PullImage", "ImageArchitecture"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (old container must never be stopped/removed on an arch mismatch)", fr.calls, wantCalls)
	}
}

// TestEnsureCreateArchMismatch covers the same arch-mismatch fix on the
// plain first-run create() path: Ensure must fail with the actionable
// error rather than creating a container from a wrong-arch image.
func TestEnsureCreateArchMismatch(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectErr: podman.ErrNotFound, archResult: "amd64"}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)
	mgr.expectedArch = "arm64"

	err := mgr.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure: expected error, got nil")
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "PullImage", "ImageArchitecture"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (no CreateContainer/StartContainer on an arch mismatch)", fr.calls, wantCalls)
	}
}

func TestApplyWritesAndReloads(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRuntime{}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	routes := []AppRoute{{Slug: "blog", Hostnames: []string{"blog.apps.example.com"}, Upstream: "bp-blog:8080"}}
	if err := mgr.Apply(context.Background(), routes, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(configDir, "current.json"))
	if err != nil {
		t.Fatalf("reading current.json: %v", err)
	}
	if !strings.Contains(string(got), "blog.apps.example.com") {
		t.Errorf("current.json missing route hostname, got: %s", got)
	}

	wantCalls := [][]string{
		{ContainerName, "caddy", "reload", "--config", "/etc/caddy/current.json", "--address", AdminSocket},
	}
	if !reflect.DeepEqual(fe.calls, wantCalls) {
		t.Fatalf("exec calls = %v, want %v", fe.calls, wantCalls)
	}
}

// TestApplyWritesDashboardRoute proves a non-nil dashboard argument lands
// in the written current.json alongside the app routes.
func TestApplyWritesDashboardRoute(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRuntime{}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	routes := []AppRoute{{Slug: "blog", Hostnames: []string{"blog.apps.example.com"}, Upstream: "bp-blog:8080"}}
	dashboard := &DashboardRoute{Hostname: "basepod.apps.example.com", Upstream: DashboardSockDial()}
	if err := mgr.Apply(context.Background(), routes, dashboard); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(configDir, "current.json"))
	if err != nil {
		t.Fatalf("reading current.json: %v", err)
	}
	if !strings.Contains(string(got), "basepod.apps.example.com") {
		t.Errorf("current.json missing dashboard hostname, got: %s", got)
	}
	if !strings.Contains(string(got), DashboardSockDial()) {
		t.Errorf("current.json missing dashboard upstream, got: %s", got)
	}
}

// TestApplyRetriesReloadOnTransientFailure covers the retry added after a
// real, reproducible failure on podman machine (macOS): the reload exec
// can transiently report the just-written config file missing (a
// virtiofs bind-mount visibility race), most reliably right after other
// containers on the same podman machine were just stopped/removed (as in
// deploy.Engine.RemoveApp). Reload is idempotent, so Apply retries it a
// bounded number of times rather than failing on the first error.
func TestApplyRetriesReloadOnTransientFailure(t *testing.T) {
	orig := reloadBackoff
	reloadBackoff = time.Millisecond
	t.Cleanup(func() { reloadBackoff = orig })

	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRuntime{}
	transient := errors.New(`podman exec: exit status 1: Error: reading config from file: open /etc/caddy/current.json: no such file or directory`)
	fe := &fakeExecer{errSequence: []error{transient, transient, nil}}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	if err := mgr.Apply(context.Background(), nil, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fe.calls) != 3 {
		t.Fatalf("exec called %d times, want 3 (2 failures + 1 success)", len(fe.calls))
	}
}

// TestApplyFailsAfterExhaustingReloadRetries covers the case where the
// reload exec keeps failing: Apply must give up after reloadAttempts
// tries and surface the last error, rather than retrying forever.
func TestApplyFailsAfterExhaustingReloadRetries(t *testing.T) {
	orig := reloadBackoff
	reloadBackoff = time.Millisecond
	t.Cleanup(func() { reloadBackoff = orig })

	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRuntime{}
	persistent := errors.New("permanent failure")
	fe := &fakeExecer{err: persistent}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443, testInstanceID)

	err := mgr.Apply(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Apply: expected error, got nil")
	}
	if !errors.Is(err, persistent) {
		t.Fatalf("Apply error = %v, want wrapping %v", err, persistent)
	}
	if len(fe.calls) != reloadAttempts {
		t.Fatalf("exec called %d times, want %d", len(fe.calls), reloadAttempts)
	}
}

// TestManagerHealth is table-driven over the three states an operator
// needs distinguished (issue #16): bp-caddy not running at all (missing,
// or inspected but not in the "running" state), running but its admin
// API unreachable over its own unix socket, and genuinely healthy. Each
// case asserts BOTH the returned error (nil for healthy; wrapping the
// named sentinel otherwise, via errors.Is) and — for the two failure
// cases — that Health actually short-circuits or proceeds as intended:
// "not running" must never reach the exec probe at all (asserting
// fe.calls stays empty), while "admin unreachable" must have actually
// attempted it (fe.calls non-empty) — the check the task's instructions
// call out explicitly: a test that can't tell "the probe ran and failed"
// apart from "the probe was never wired up" isn't proving the check is
// real.
func TestManagerHealth(t *testing.T) {
	cases := []struct {
		name string

		inspectInfo *podman.ContainerInfo
		inspectErr  error
		execErr     error

		wantErr      error // nil means Health must return nil
		wantExecCall bool
	}{
		{
			name:         "healthy: running and admin answers",
			inspectInfo:  &podman.ContainerInfo{State: "running"},
			wantErr:      nil,
			wantExecCall: true,
		},
		{
			name:         "container missing entirely",
			inspectErr:   podman.ErrNotFound,
			wantErr:      ErrCaddyNotRunning,
			wantExecCall: false,
		},
		{
			name:         "container inspected but not running",
			inspectInfo:  &podman.ContainerInfo{State: "exited"},
			wantErr:      ErrCaddyNotRunning,
			wantExecCall: false,
		},
		{
			name:         "container running but admin unreachable",
			inspectInfo:  &podman.ContainerInfo{State: "running"},
			execErr:      errors.New(`podman exec: exit status 7: curl: (7) Failed to connect to localhost port 80: Connection refused`),
			wantErr:      ErrCaddyAdminUnreachable,
			wantExecCall: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRuntime{inspectInfo: tc.inspectInfo, inspectErr: tc.inspectErr}
			fe := &fakeExecer{err: tc.execErr}
			mgr := NewManager(fr, fe.Exec, t.TempDir(), 8080, 8443, testInstanceID)

			err := mgr.Health(context.Background())

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Health() = %v, want nil", err)
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Health() = %v, want it to wrap %v", err, tc.wantErr)
				}
			}

			gotExecCall := len(fe.calls) > 0
			if gotExecCall != tc.wantExecCall {
				t.Fatalf("exec called = %v (calls: %v), want %v", gotExecCall, fe.calls, tc.wantExecCall)
			}
			if tc.wantExecCall {
				call := fe.calls[0]
				if call[0] != ContainerName {
					t.Errorf("exec container = %q, want %q", call[0], ContainerName)
				}
				argv := strings.Join(call, " ")
				if !strings.Contains(argv, "curl") || !strings.Contains(argv, AdminSocketPath) {
					t.Errorf("exec argv = %v, want it to run curl against %q", call, AdminSocketPath)
				}
			}
		})
	}
}

// TestManagerHealthCaching proves Health's caching (issue #16 review
// feedback: an uncached podman-exec probe on every GET /system poll
// measured ~200-250ms and ~4.4% of a CPU core sustained per open
// dashboard tab in production): a call within caddyHealthCacheTTL of the
// last probe returns the cached result without re-probing; a call after
// the TTL has elapsed re-probes. Drives Health's clock via mgr.now
// (overridden here) rather than a real sleep, so this test doesn't
// itself take caddyHealthCacheTTL to run.
func TestManagerHealthCaching(t *testing.T) {
	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{State: "running"}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, t.TempDir(), 8080, 8443, testInstanceID)

	now := time.Now()
	mgr.now = func() time.Time { return now }

	if err := mgr.Health(context.Background()); err != nil {
		t.Fatalf("Health (first call): %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("exec called %d time(s) after first Health call, want 1", len(fe.calls))
	}

	// Still within the TTL (clock hasn't moved): must be a cache hit, not
	// a second probe.
	if err := mgr.Health(context.Background()); err != nil {
		t.Fatalf("Health (cached call): %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("exec called %d time(s) after a cached Health call, want still 1 (cache hit, no re-probe)", len(fe.calls))
	}
	if len(fr.calls) != 1 {
		t.Fatalf("InspectContainer called %d time(s) after a cached Health call, want still 1 (cache hit skips the whole probe)", len(fr.calls))
	}

	// Advance past the TTL: the next call must re-probe.
	now = now.Add(caddyHealthCacheTTL + time.Second)
	if err := mgr.Health(context.Background()); err != nil {
		t.Fatalf("Health (after TTL expiry): %v", err)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("exec called %d time(s) after TTL expiry, want 2 (re-probed)", len(fe.calls))
	}
}

// TestManagerHealthCancelledProbeIsNotCached proves a probe that failed
// only because its caller's context was cancelled does not poison the
// cache. handleSystem passes the HTTP request's context and the dashboard
// polls /system every 5s from every page, so a tab closing mid-probe
// cancels one routinely — caching that would report a false "admin
// unreachable" to every later caller for the rest of the TTL.
func TestManagerHealthCancelledProbeIsNotCached(t *testing.T) {
	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{State: "running"}}

	// First call: the exec fails the way a cancelled podman-exec does, and
	// the context is already cancelled by the time Health inspects it.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	var execCalls int32
	exec := func(ctx context.Context, container string, cmd ...string) error {
		atomic.AddInt32(&execCalls, 1)
		if ctx == cancelledCtx {
			cancel()
			return context.Canceled
		}
		return nil
	}
	mgr := NewManager(fr, exec, t.TempDir(), 8080, 8443, testInstanceID)
	now := time.Now()
	mgr.now = func() time.Time { return now }

	if err := mgr.Health(cancelledCtx); err == nil {
		t.Fatal("Health with a cancelled context: got nil error, want the probe failure")
	}

	// Same instant, so the TTL has definitely not expired: if the
	// cancelled result had been cached, this would return it without
	// re-probing. It must probe again instead, and report healthy.
	if err := mgr.Health(context.Background()); err != nil {
		t.Fatalf("Health after a cancelled probe: %v, want nil (cancelled result must not be cached)", err)
	}
	if got := atomic.LoadInt32(&execCalls); got != 2 {
		t.Fatalf("exec called %d time(s), want 2 (the cancelled probe must not have been cached)", got)
	}
}

// TestManagerHealthConcurrentRequestsShareOneProbe proves Health
// single-flights concurrent callers landing during a cache-miss window
// into exactly one real probe (issue #16 review feedback's "make sure
// concurrent /system requests don't each fire their own probe"), rather
// than each firing its own podman-exec. Deterministic, no sleep-based
// race: the fake exec signals (via closing `entered`) the instant it's
// actually running, so the second Health call is only started once the
// first is confirmed to be inside the probe — meaning the second call is
// guaranteed to block on Health's mutex, not race to also start probing.
func TestManagerHealthConcurrentRequestsShareOneProbe(t *testing.T) {
	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{State: "running"}}

	var execCalls int32
	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	exec := func(ctx context.Context, container string, cmd ...string) error {
		atomic.AddInt32(&execCalls, 1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}
	mgr := NewManager(fr, exec, t.TempDir(), 8080, 8443, testInstanceID)

	var wg sync.WaitGroup
	results := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		results <- mgr.Health(context.Background())
	}()

	<-entered // the first call is now inside the (locked) probe, blocked on release

	wg.Add(1)
	go func() {
		defer wg.Done()
		results <- mgr.Health(context.Background())
	}()

	close(release) // let both proceed: the second only ever gets past the mutex after the first finishes and caches
	wg.Wait()
	close(results)

	for err := range results {
		if err != nil {
			t.Errorf("Health() = %v, want nil", err)
		}
	}
	if got := atomic.LoadInt32(&execCalls); got != 1 {
		t.Fatalf("exec called %d time(s) for 2 concurrent Health calls, want 1 (single-flighted)", got)
	}
}
