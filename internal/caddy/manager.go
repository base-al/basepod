package caddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/base-al/basepod/internal/podman"
)

// Image is the Caddy image the manager pulls and runs.
const Image = "docker.io/library/caddy:2.10-alpine"

// ContainerName is the name of the Caddy container the manager manages.
const ContainerName = "bp-caddy"

// NetworkName is the shared podman network Caddy and every app container
// join.
const NetworkName = "basepod"

// Runtime is the podman capability the manager needs (satisfied by
// *podman.Client).
type Runtime interface {
	EnsureNetwork(ctx context.Context, name string) error
	PullImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, spec podman.CreateSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	InspectContainer(ctx context.Context, nameOrID string) (*podman.ContainerInfo, error)
}

// Execer runs a command in a container; the production implementation
// (PodmanExec) shells out to `podman exec`.
type Execer func(ctx context.Context, container string, cmd ...string) error

// Manager bootstraps and drives the bp-caddy container: it ensures the
// container exists and is running, and applies rendered route
// configuration by writing current.json and triggering a reload.
type Manager struct {
	rt        Runtime
	exec      Execer
	configDir string
	httpPort  int
	httpsPort int
}

// NewManager builds a Manager. configDir is the host directory bind-mounted
// into the Caddy container at /etc/caddy; it must be an absolute path.
// httpPort and httpsPort are the host ports mapped to the container's 80
// and 443.
func NewManager(rt Runtime, exec Execer, configDir string, httpPort, httpsPort int) *Manager {
	return &Manager{
		rt:        rt,
		exec:      exec,
		configDir: configDir,
		httpPort:  httpPort,
		httpsPort: httpsPort,
	}
}

// configPath is the path to the live Caddy config file inside configDir.
func (m *Manager) configPath() string {
	return filepath.Join(m.configDir, "current.json")
}

// dataDir is the host directory bind-mounted into the Caddy container at
// /data, so TLS certificates survive container recreation. It is a sibling
// of configDir rather than a child so an app's config bind mount never
// exposes Caddy's certificate storage.
func (m *Manager) dataDir() string {
	return filepath.Join(filepath.Dir(m.configDir), "caddy-data")
}

// Ensure makes sure the shared network and the bp-caddy container both
// exist and are running, creating and starting the container (with an
// initial empty-routes config) on first run.
func (m *Manager) Ensure(ctx context.Context) error {
	if err := m.rt.EnsureNetwork(ctx, NetworkName); err != nil {
		return fmt.Errorf("caddy: ensure network %q: %w", NetworkName, err)
	}

	info, err := m.rt.InspectContainer(ctx, ContainerName)
	if err != nil {
		if !errors.Is(err, podman.ErrNotFound) {
			return fmt.Errorf("caddy: inspect %s: %w", ContainerName, err)
		}
		return m.create(ctx)
	}

	if info.State != "running" {
		if err := m.rt.StartContainer(ctx, info.ID); err != nil {
			return fmt.Errorf("caddy: start %s: %w", ContainerName, err)
		}
	}
	return nil
}

// create writes the initial (empty-routes) config, pulls the Caddy image,
// and creates and starts the bp-caddy container. Called only when
// InspectContainer reports the container doesn't exist yet.
func (m *Manager) create(ctx context.Context) error {
	if err := os.MkdirAll(m.configDir, 0o755); err != nil {
		return fmt.Errorf("caddy: create config dir %q: %w", m.configDir, err)
	}
	dataDir := m.dataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("caddy: create data dir %q: %w", dataDir, err)
	}

	initial, err := Render(nil)
	if err != nil {
		return fmt.Errorf("caddy: render initial config: %w", err)
	}
	if err := writeFileAtomic(m.configPath(), initial); err != nil {
		return fmt.Errorf("caddy: write initial config: %w", err)
	}

	if err := m.rt.PullImage(ctx, Image); err != nil {
		return fmt.Errorf("caddy: pull %s: %w", Image, err)
	}

	spec := podman.CreateSpec{
		Name:        ContainerName,
		Image:       Image,
		Labels:      map[string]string{"basepod.managed": "true"},
		Command:     []string{"caddy", "run", "--config", "/etc/caddy/current.json"},
		NetworkName: NetworkName,
		PortMappings: []podman.PortMapping{
			{ContainerPort: 80, HostPort: uint16(m.httpPort)},
			{ContainerPort: 443, HostPort: uint16(m.httpsPort)},
		},
		Mounts: []podman.BindMount{
			{Source: m.configDir, Dest: "/etc/caddy"},
			{Source: dataDir, Dest: "/data"},
		},
		RestartPolicy: "always",
	}

	id, err := m.rt.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("caddy: create container: %w", err)
	}
	if err := m.rt.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("caddy: start container: %w", err)
	}
	return nil
}

// Apply renders routes into current.json (written atomically) and tells
// the running Caddy instance to reload it over its Unix-socket admin API.
func (m *Manager) Apply(ctx context.Context, routes []AppRoute) error {
	data, err := Render(routes)
	if err != nil {
		return fmt.Errorf("caddy: render config: %w", err)
	}
	if err := writeFileAtomic(m.configPath(), data); err != nil {
		return fmt.Errorf("caddy: write config: %w", err)
	}
	if err := m.exec(ctx, ContainerName, "caddy", "reload", "--config", "/etc/caddy/current.json", "--address", AdminSocket); err != nil {
		return fmt.Errorf("caddy: reload: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path via a temp-file-then-rename so a
// concurrent reader (Caddy reloading) never observes a partial write.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PodmanExec is the production Execer: it shells out to `podman exec`
// rather than using the libpod exec API, since that API requires stream
// hijacking and shelling out is five lines, used only for reloads.
func PodmanExec(ctx context.Context, container string, cmd ...string) error {
	args := append([]string{"exec", container}, cmd...)
	c := exec.CommandContext(ctx, "podman", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("podman exec: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
