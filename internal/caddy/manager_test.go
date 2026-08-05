package caddy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/podman"
)

// var _ Runtime = (*podman.Client)(nil) documents (and enforces at compile
// time) that *podman.Client satisfies the Runtime interface the manager
// consumes.
var _ Runtime = (*podman.Client)(nil)

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
}

func (f *fakeRuntime) EnsureNetwork(ctx context.Context, name string) error {
	f.calls = append(f.calls, "EnsureNetwork")
	return f.networkErr
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string) error {
	f.calls = append(f.calls, "PullImage")
	return f.pullErr
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

func TestEnsureCreatesWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectErr: podman.ErrNotFound}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer", "PullImage", "CreateContainer", "StartContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantCalls)
	}

	spec := fr.createSpec
	if spec.Name != ContainerName {
		t.Errorf("spec.Name = %q, want %q", spec.Name, ContainerName)
	}
	if spec.Image != Image {
		t.Errorf("spec.Image = %q, want %q", spec.Image, Image)
	}
	if spec.RestartPolicy != "always" {
		t.Errorf("spec.RestartPolicy = %q, want %q", spec.RestartPolicy, "always")
	}
	if spec.NetworkName != NetworkName {
		t.Errorf("spec.NetworkName = %q, want %q", spec.NetworkName, NetworkName)
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
	// Mount sources are resolved to their real (symlink-free) path (see
	// manager.go's create): on macOS, t.TempDir() lives under /var/folders,
	// itself a symlink to /private/var/folders, so the resolved path
	// differs from configDir/dataDir as constructed above.
	wantConfigSrc, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(configDir): %v", err)
	}
	wantDataSrc, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(dataDir): %v", err)
	}
	wantMounts := []podman.BindMount{
		{Source: wantConfigSrc, Dest: "/etc/caddy"},
		{Source: wantDataSrc, Dest: "/data"},
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

	got, err := os.ReadFile(filepath.Join(configDir, "current.json"))
	if err != nil {
		t.Fatalf("reading current.json: %v", err)
	}
	if !strings.Contains(string(got), "unix//var/run/caddy/admin.sock") {
		t.Errorf("current.json missing admin socket, got: %s", got)
	}
}

func TestEnsureStartsWhenStopped(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "caddy-config")

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{ID: "abc123", Name: ContainerName, State: "exited"}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443)

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

	fr := &fakeRuntime{inspectInfo: &podman.ContainerInfo{ID: "abc123", Name: ContainerName, State: "running"}}
	fe := &fakeExecer{}
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443)

	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantCalls := []string{"EnsureNetwork", "InspectContainer"}
	if !reflect.DeepEqual(fr.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fr.calls, wantCalls)
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
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443)

	routes := []AppRoute{{Slug: "blog", Hostnames: []string{"blog.apps.example.com"}, Upstream: "bp-blog:8080"}}
	if err := mgr.Apply(context.Background(), routes); err != nil {
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
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443)

	if err := mgr.Apply(context.Background(), nil); err != nil {
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
	mgr := NewManager(fr, fe.Exec, configDir, 8080, 8443)

	err := mgr.Apply(context.Background(), nil)
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
