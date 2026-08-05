package caddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/base-al/basepod/internal/podman"
)

// Image is the Caddy image the manager pulls and runs.
const Image = "docker.io/library/caddy:2.10-alpine"

// ContainerName is the name of the Caddy container the manager manages.
const ContainerName = "bp-caddy"

// NetworkName is the shared podman network Caddy and every app container
// join.
const NetworkName = "basepod"

// DashboardSockMountDest is the container-side path the caddy-sock host
// directory (Manager.SockDir()) is bind-mounted onto (see create and
// desiredMounts) — where BasePod's own dashboard API's unix socket
// (DashboardSockName, created host-side by internal/server) shows up
// inside the bp-caddy container. Deliberately a directory mount, not a
// single-file mount: only bp-caddy gets it (no app container does), which
// is what actually restricts the dashboard API to Caddy — unlike v0.3's
// first cut of this feature, which listened on the "basepod" network's
// gateway address and so was reachable by every container on that
// network.
const DashboardSockMountDest = "/run/basepod"

// DashboardSockName is the dashboard API unix socket's file name (not
// path) inside DashboardSockMountDest — shared between internal/server
// (which creates the real socket file, host-side, at
// SockDir()/DashboardSockName) and DashboardSockDial (the Caddy dial
// string built from it): both describe the same file, viewed from two
// different mount namespaces, and must agree on its name.
const DashboardSockName = "api.sock"

// DashboardSockDial returns the Caddy JSON reverse_proxy upstream "dial"
// string for the dashboard's unix socket, exactly as Caddy resolves it
// inside the bp-caddy container. Caddy's dial syntax for a unix socket is
// "unix/" followed by the absolute path (see AdminSocket for the same
// pattern applied to Caddy's own admin API socket), giving
// "unix//run/basepod/api.sock".
func DashboardSockDial() string {
	return "unix/" + DashboardSockMountDest + "/" + DashboardSockName
}

// Runtime is the podman capability the manager needs (satisfied by
// *podman.Client).
type Runtime interface {
	EnsureNetwork(ctx context.Context, name string) error
	PullImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, spec podman.CreateSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeoutSec int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
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

// SockDir is the host directory bind-mounted into the Caddy container at
// DashboardSockMountDest (/run/basepod), shared with internal/server: it
// creates the dashboard API's actual unix socket file there
// (SockDir()/DashboardSockName), and this bind mount is what makes it
// visible to (only) the bp-caddy container. A sibling of configDir, like
// dataDir, for the same reason: no app's own config/data bind mount
// should ever double as an entry into this directory.
func (m *Manager) SockDir() string {
	return filepath.Join(filepath.Dir(m.configDir), "caddy-sock")
}

// driftStopTimeout bounds how long Ensure waits for a drifted bp-caddy
// container to stop gracefully before recreating it — short, since a
// drifted container (wrong image/ports, or stuck in "created") is being
// discarded outright rather than handed off traffic cleanly.
const driftStopTimeout = 5

// Ensure makes sure the shared network and the bp-caddy container both
// exist and are running, creating and starting the container (with an
// initial empty-routes config) on first run.
//
// If bp-caddy already exists, Ensure first checks for drift against the
// spec create would build today — a different image, different host port
// mappings, or a container stuck in the "created" state (meaning it was
// made but never successfully started, e.g. because its port mapping lost
// a bind race on a previous boot) — and if any of those differ, stops and
// removes the old container and recreates it fresh, rather than trying to
// reconcile it in place. Otherwise it just starts the container if it
// isn't already running.
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

	desiredMounts, err := m.desiredMounts()
	if err != nil {
		return fmt.Errorf("caddy: resolve desired mounts: %w", err)
	}
	if reason := driftReason(info, m.desiredPorts(), desiredMounts); reason != "" {
		log.Printf("caddy: %s has drifted (%s) — recreating", ContainerName, reason)
		if err := m.rt.StopContainer(ctx, info.ID, driftStopTimeout); err != nil {
			return fmt.Errorf("caddy: stop drifted %s: %w", ContainerName, err)
		}
		if err := m.rt.RemoveContainer(ctx, info.ID, true); err != nil {
			return fmt.Errorf("caddy: remove drifted %s: %w", ContainerName, err)
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

// desiredPorts is the port mapping set create() builds — the same
// {80,443} -> {httpPort,httpsPort} mapping Ensure compares an existing
// container's inspected ports against for drift.
func (m *Manager) desiredPorts() []podman.PortMapping {
	return []podman.PortMapping{
		{ContainerPort: 80, HostPort: uint16(m.httpPort)},
		{ContainerPort: 443, HostPort: uint16(m.httpsPort)},
	}
}

// resolvedMountSources ensures configDir, dataDir, and SockDir all exist
// (MkdirAll) and returns their real (symlink-free) host paths — see
// create's comment on why bind-mount sources must be resolved through
// EvalSymlinks under podman machine (macOS). This is the single source of
// truth both create (building a fresh container's Mounts) and
// desiredMounts (what Ensure's drift check compares an existing
// container's inspected mounts against) call through, so the two can
// never compute a different "desired" mount set from each other.
func (m *Manager) resolvedMountSources() (configSrc, dataSrc, sockSrc string, err error) {
	if err := os.MkdirAll(m.configDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("caddy: create config dir %q: %w", m.configDir, err)
	}
	dataDir := m.dataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("caddy: create data dir %q: %w", dataDir, err)
	}
	sockDir := m.SockDir()
	// 0o750, tighter than config/data's 0o755: this directory exists
	// solely to carry the dashboard API's unix socket into (only) the
	// bp-caddy container — see DashboardSockMountDest's doc comment.
	if err := os.MkdirAll(sockDir, 0o750); err != nil {
		return "", "", "", fmt.Errorf("caddy: create socket dir %q: %w", sockDir, err)
	}

	configSrc, err = filepath.EvalSymlinks(m.configDir)
	if err != nil {
		return "", "", "", fmt.Errorf("caddy: resolve config dir %q: %w", m.configDir, err)
	}
	dataSrc, err = filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", "", "", fmt.Errorf("caddy: resolve data dir %q: %w", dataDir, err)
	}
	sockSrc, err = filepath.EvalSymlinks(sockDir)
	if err != nil {
		return "", "", "", fmt.Errorf("caddy: resolve socket dir %q: %w", sockDir, err)
	}
	return configSrc, dataSrc, sockSrc, nil
}

// desiredMounts is the bind-mount set create() builds — the same set
// Ensure compares an existing container's inspected mounts against for
// drift (see resolvedMountSources).
func (m *Manager) desiredMounts() ([]podman.BindMount, error) {
	configSrc, dataSrc, sockSrc, err := m.resolvedMountSources()
	if err != nil {
		return nil, err
	}
	return []podman.BindMount{
		{Source: configSrc, Dest: "/etc/caddy"},
		{Source: dataSrc, Dest: "/data"},
		{Source: sockSrc, Dest: DashboardSockMountDest},
	}, nil
}

// driftReason reports why an existing bp-caddy container no longer
// matches what Ensure would create today, or "" if it matches. Checked in
// order: a container stuck in "created" (made but never started
// successfully — reconciling it in place isn't meaningfully different
// from recreating it, and recreating is simpler to reason about); an
// image mismatch (a newer basepod build changed the pinned Caddy image);
// a port-mapping mismatch (the known stale-port-mapping issue: a previous
// boot's ports don't match the currently configured httpPort/httpsPort);
// then a bind-mount set mismatch — added so a container created by an
// older BasePod build (before the dashboard's caddy-sock mount existed)
// gets recreated on upgrade and picks up the new mount, rather than
// running forever without it.
func driftReason(info *podman.ContainerInfo, desiredPorts []podman.PortMapping, desiredMounts []podman.BindMount) string {
	switch {
	case info.State == "created":
		return "state=created (previous boot never started it successfully)"
	case info.Image != Image:
		return fmt.Sprintf("image %s != desired %s", info.Image, Image)
	case !portsEqual(info.Ports, desiredPorts):
		return fmt.Sprintf("ports %v != desired %v", info.Ports, desiredPorts)
	case !mountsEqual(info.Mounts, desiredMounts):
		return fmt.Sprintf("mounts %v != desired %v", info.Mounts, desiredMounts)
	default:
		return ""
	}
}

// mountsEqual reports whether a and b contain the same {Source,Dest}
// pairs, ignoring order and ignoring ReadOnly (InspectContainer never
// populates it — see parseBindMounts).
func mountsEqual(a, b []podman.BindMount) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct{ source, dest string }
	counts := make(map[key]int, len(a))
	for _, m := range a {
		counts[key{m.Source, m.Dest}]++
	}
	for _, m := range b {
		k := key{m.Source, m.Dest}
		if counts[k] == 0 {
			return false
		}
		counts[k]--
	}
	return true
}

// portsEqual reports whether a and b contain the same
// {ContainerPort,HostPort} pairs, ignoring order (and ignoring Protocol,
// which InspectContainer never populates — see parsePortBindings).
func portsEqual(a, b []podman.PortMapping) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct{ containerPort, hostPort uint16 }
	counts := make(map[key]int, len(a))
	for _, p := range a {
		counts[key{p.ContainerPort, p.HostPort}]++
	}
	for _, p := range b {
		k := key{p.ContainerPort, p.HostPort}
		if counts[k] == 0 {
			return false
		}
		counts[k]--
	}
	return true
}

// create writes the initial (empty-routes) config, pulls the Caddy image,
// and creates and starts the bp-caddy container. Called only when
// InspectContainer reports the container doesn't exist yet.
func (m *Manager) create(ctx context.Context) error {
	// Resolve bind-mount sources to their real (symlink-free) path (also
	// MkdirAll's config/data/sock dirs into existence — see
	// resolvedMountSources). Podman machine (macOS) shares host
	// directories into its VM by their canonical path (e.g.
	// /private/tmp), not through macOS-only symlinks like /tmp ->
	// /private/tmp; passing the symlinked path straight through as a
	// bind-mount source fails inside the VM with "no such file or
	// directory" even though the path exists on the host.
	configSrc, dataSrc, sockSrc, err := m.resolvedMountSources()
	if err != nil {
		return err
	}

	initial, err := Render(nil, nil)
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
		Name:   ContainerName,
		Image:  Image,
		Labels: map[string]string{"basepod.managed": "true"},
		// The image doesn't ship /var/run/caddy, the parent directory
		// AdminSocket's unix socket binds into, and Caddy can't create
		// it itself ("bind: no such file or directory"). It must live
		// on the container's own filesystem rather than a bind mount:
		// setting the socket's permission bits on a virtiofs-shared
		// directory (as every other mount here is, under podman
		// machine on macOS) fails with EINVAL. `exec` hands off PID 1
		// to caddy once the directory exists.
		Command:     []string{"sh", "-c", "mkdir -p /var/run/caddy && exec caddy run --config /etc/caddy/current.json"},
		NetworkName: NetworkName,
		PortMappings: []podman.PortMapping{
			{ContainerPort: 80, HostPort: uint16(m.httpPort)},
			{ContainerPort: 443, HostPort: uint16(m.httpsPort)},
		},
		Mounts: []podman.BindMount{
			{Source: configSrc, Dest: "/etc/caddy"},
			{Source: dataSrc, Dest: "/data"},
			// Shared with internal/server: it creates the dashboard API's
			// actual unix socket file host-side, at
			// SockDir()/DashboardSockName — this mount is what makes that
			// file (and only that file's directory) visible inside
			// bp-caddy, and bp-caddy alone, unlike the network-wide
			// gateway listener this replaced.
			{Source: sockSrc, Dest: DashboardSockMountDest},
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

// reloadAttempts and reloadBackoff bound the retry below. reloadBackoff is
// a var (not a const) so tests can shrink it to keep the retry path fast.
const reloadAttempts = 5

var reloadBackoff = 100 * time.Millisecond

// Apply renders routes (plus dashboard, if non-nil — see
// caddy.DashboardRoute) into current.json (written atomically) and tells
// the running Caddy instance to reload it over its Unix-socket admin API.
//
// The reload exec is retried up to reloadAttempts times on failure.
// Observed on podman machine's virtiofs-backed bind mounts (macOS): the
// container's view of a file just written+renamed on the host can lag
// behind by a short, bounded window, so `caddy reload` occasionally
// reports /etc/caddy/current.json missing immediately after
// writeFileAtomic has already returned successfully on the host side —
// most reliably reproduced right after this Manager's container-facing
// RemoveApp caller has just stopped/removed other containers on the same
// podman machine. A bare retry a few milliseconds later reliably
// succeeds, and `caddy reload` is idempotent (it just re-reads and
// re-applies the same file), so retrying here is safe.
func (m *Manager) Apply(ctx context.Context, routes []AppRoute, dashboard *DashboardRoute) error {
	data, err := Render(routes, dashboard)
	if err != nil {
		return fmt.Errorf("caddy: render config: %w", err)
	}
	if err := writeFileAtomic(m.configPath(), data); err != nil {
		return fmt.Errorf("caddy: write config: %w", err)
	}

	var reloadErr error
	for attempt := 1; attempt <= reloadAttempts; attempt++ {
		reloadErr = m.exec(ctx, ContainerName, "caddy", "reload", "--config", "/etc/caddy/current.json", "--address", AdminSocket)
		if reloadErr == nil {
			return nil
		}
		if attempt == reloadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("caddy: reload: %w", ctx.Err())
		case <-time.After(reloadBackoff):
		}
	}
	return fmt.Errorf("caddy: reload: %w", reloadErr)
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
