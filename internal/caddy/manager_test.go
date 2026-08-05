package caddy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
// invocation.
type fakeExecer struct {
	calls [][]string
	err   error
}

func (f *fakeExecer) Exec(ctx context.Context, container string, cmd ...string) error {
	args := append([]string{container}, cmd...)
	f.calls = append(f.calls, args)
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

	dataDir := filepath.Join(filepath.Dir(configDir), "caddy-data")
	wantMounts := []podman.BindMount{
		{Source: configDir, Dest: "/etc/caddy"},
		{Source: dataDir, Dest: "/data"},
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

	routes := []AppRoute{{Slug: "blog", Hostname: "blog.apps.example.com", Upstream: "bp-blog:8080"}}
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
