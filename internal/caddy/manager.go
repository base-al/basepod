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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/base-al/basepod/internal/podman"
)

// caddyRepo is the Caddy image repository, shared by Image (the
// human-readable, tag-qualified form used in logs/errors) and PinnedImage
// (the digest-qualified form the manager actually pulls and runs).
const caddyRepo = "docker.io/library/caddy"

// Image is the Caddy image tag the manager tracks — kept purely as a
// readable identity for logs and error messages: what actually gets
// pulled/created is PinnedImage, below.
const Image = caddyRepo + ":2.10-alpine"

// CaddyImageDigest pins Image to a specific manifest-list digest, so a
// compromised or force-moved registry tag can never silently swap the
// image BasePod runs as its reverse proxy: PullImage/CreateContainer
// reference PinnedImage (Image's repo + this digest), not the mutable
// tag, so what's pulled and run is exactly the bytes verified below,
// every time.
//
// Verified against the live registry on 2026-08-06 via the Docker
// Registry HTTP API v2 (no skopeo/docker available in that environment):
//
//	TOKEN=$(curl -s 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/caddy:pull' | jq -r .token)
//	curl -sI -H "Authorization: Bearer $TOKEN" \
//	  -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
//	  https://registry-1.docker.io/v2/library/caddy/manifests/2.10-alpine | grep -i docker-content-digest
//
// which returned the docker-content-digest header below for the
// "2.10-alpine" tag's manifest INDEX (not a single-platform manifest) —
// confirmed multi-arch (amd64, arm/v6, arm/v7, arm64/v8, ppc64le,
// riscv64, s390x), matching pullImage's own per-arch verification, so
// pinning it preserves normal multi-arch resolution rather than locking
// every host to one platform's manifest.
//
// Bump procedure: when Image's tag changes, re-run the command above
// against the new tag, update Image and CaddyImageDigest together in the
// same commit, and refresh this comment's date/digest.
const CaddyImageDigest = "sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d"

// PinnedImage is the digest-qualified image reference ("<repo>@<digest>")
// pullImage actually pulls and create actually runs — see
// CaddyImageDigest's doc comment for why this, not Image, is what's used
// at runtime.
const PinnedImage = caddyRepo + "@" + CaddyImageDigest

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
	EnsureNetwork(ctx context.Context, name, instanceID string) error
	PullImage(ctx context.Context, ref string) error
	// ImageArchitecture reports the architecture recorded in ref's image
	// manifest/config — used by Manager to catch a wrong-arch image that
	// slipped into the local store (see pullImage).
	ImageArchitecture(ctx context.Context, ref string) (string, error)
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

	// instanceID is this BasePod instance's stable id (see
	// internal/instanceid.LoadOrCreate), stamped as basepod.instance=<id>
	// on the basepod network and on bp-caddy itself (see create) — issue
	// #10. Ensure uses it to tell "my bp-caddy" (recreate on drift, same
	// as before instance ids existed) apart from "a different BasePod
	// instance's bp-caddy, sharing this Podman socket" (never touched —
	// see Ensure's doc comment).
	instanceID string

	// expectedArch is the architecture pullImage requires the PinnedImage
	// ref to resolve to after pulling — defaults to runtime.GOARCH (see
	// NewManager) and is only ever overridden by tests. runtime.GOARCH is
	// correct here even under podman machine on macOS: BasePod itself runs
	// on the host (darwin/arm64 on Apple Silicon) while containers run
	// inside the podman-machine Linux VM, but that VM's architecture always
	// matches the host's — an arm64 Mac gets an arm64 VM, never an
	// emulated amd64 one — so the host process's own GOARCH is the right
	// value to compare a container image's architecture against on both
	// bare Linux and macOS/Apple-Silicon.
	expectedArch string

	// healthMu guards healthCachedAt/healthCachedErr below, and — since
	// Health holds it across the ENTIRE check, not just the map/field
	// writes — also single-flights concurrent Health callers into one
	// probe: see Health's doc comment for why (measured production cost:
	// ~200-250ms of podman-exec per call, times every open dashboard
	// tab's 5s poll of GET /system, was ~4.4% of a core sustained —
	// issue #16 review feedback).
	healthMu sync.Mutex
	// healthCachedAt is the wall-clock time (per now, below) of the last
	// completed probe; the zero Time means "never probed yet" (always
	// stale). healthCachedErr is that probe's result, reported verbatim
	// by every Health call that lands within caddyHealthCacheTTL of it.
	healthCachedAt  time.Time
	healthCachedErr error

	// now is Health's clock — a field (not a bare time.Now() call) so
	// TestManagerHealthCaching can advance time deterministically past
	// caddyHealthCacheTTL without a real sleep. Defaults to time.Now in
	// NewManager; only ever overridden by tests.
	now func() time.Time
}

// NewManager builds a Manager. configDir is the host directory bind-mounted
// into the Caddy container at /etc/caddy; it must be an absolute path.
// httpPort and httpsPort are the host ports mapped to the container's 80
// and 443. instanceID is this BasePod instance's stable id (see
// internal/instanceid.LoadOrCreate) — see the instanceID field's doc
// comment for what it's used for.
func NewManager(rt Runtime, exec Execer, configDir string, httpPort, httpsPort int, instanceID string) *Manager {
	return &Manager{
		rt:           rt,
		exec:         exec,
		configDir:    configDir,
		httpPort:     httpPort,
		httpsPort:    httpsPort,
		instanceID:   instanceID,
		expectedArch: runtime.GOARCH,
		now:          time.Now,
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
// If bp-caddy already exists, Ensure first checks whether it's owned by a
// DIFFERENT BasePod instance (issue #10): a bp-caddy container carrying a
// basepod.instance label that doesn't match m.instanceID belongs to
// another BasePod daemon sharing this same Podman socket, and Ensure
// refuses to touch it at all — no drift check, no start-if-stopped, and
// certainly no stop/remove/recreate — returning a clear, actionable error
// instead (see the instance-mismatch branch below). Two BasePod instances
// racing to own the one shared "bp-caddy" name/proxy is not a supported
// configuration; each instance needs its own Podman socket. A bp-caddy
// with NO basepod.instance label at all (every v0.4.2-and-earlier install,
// before this label existed) is treated as adopted — exactly the same as
// before instance ids existed — since there's no way to tell "unlabeled
// because it predates instance labeling" apart from "unlabeled because it
// belongs to some other, still-unlabeled instance", and refusing to ever
// touch bp-caddy again after every upgrade would defeat the whole point of
// this method. It picks up its own instance's label the next time drift
// (e.g. an image bump) forces a real recreate — see create().
//
// Once ownership is established, Ensure checks for drift against the spec
// create would build today — a different image, different host port
// mappings, or a container stuck in the "created" state (meaning it was
// made but never successfully started, e.g. because its port mapping lost
// a bind race on a previous boot) — and if any of those differ, pulls the
// image fresh, then stops and removes the old container and recreates it
// fresh, rather than trying to reconcile it in place. The pull happens
// before the old container is touched, so a pull failure (registry blip,
// rate limit) leaves the old container running rather than tearing down
// ingress with nothing to replace it. Otherwise it just starts the
// container if it isn't already running.
//
// The pull is unconditional — never gated on an existence check — even
// though the image is very often already present locally: v0.3's first cut
// of this skipped PullImage whenever ImageExists reported the image was
// already cached, which is exactly what let a stale wrong-architecture
// image (e.g. an amd64 docker.io/library/caddy:2.10-alpine pulled once as a
// `podman build --platform linux/amd64` base layer, sitting in an arm64
// Mac's local store) get used unchanged: existence says nothing about which
// architecture's manifest got resolved, but `podman pull` re-resolves the
// manifest for the host architecture every time. pullImage additionally
// double-checks the result via ImageArchitecture as a defensive backstop —
// see its doc comment — for the residual case where the pull itself is a
// no-op against a wrong-arch cache entry (e.g. daemon offline, or a future
// libpod/registry combination that doesn't re-resolve on every call).
func (m *Manager) Ensure(ctx context.Context) error {
	if err := m.rt.EnsureNetwork(ctx, NetworkName, m.instanceID); err != nil {
		return fmt.Errorf("caddy: ensure network %q: %w", NetworkName, err)
	}

	info, err := m.rt.InspectContainer(ctx, ContainerName)
	if err != nil {
		if !errors.Is(err, podman.ErrNotFound) {
			return fmt.Errorf("caddy: inspect %s: %w", ContainerName, err)
		}
		return m.create(ctx)
	}

	if owner, labeled := info.Labels["basepod.instance"]; labeled && owner != m.instanceID {
		return fmt.Errorf(
			"caddy: %s is owned by a different BasePod instance (id %s; this instance is %s) — "+
				"two BasePod instances appear to share this Podman socket, which is not supported; "+
				"refusing to recreate or otherwise touch %s. Point one of the instances at a separate "+
				"Podman socket, or stop the other instance, before starting this one",
			ContainerName, owner, m.instanceID, ContainerName,
		)
	}

	desiredMounts, err := m.desiredMounts()
	if err != nil {
		return fmt.Errorf("caddy: resolve desired mounts: %w", err)
	}
	if reason := driftReason(info, m.desiredPorts(), desiredMounts); reason != "" {
		log.Printf("caddy: %s has drifted (%s) — recreating", ContainerName, reason)
		// Pull (and arch-verify) the image BEFORE tearing down the old
		// container: pullImage can fail (registry blip, rate limit — no
		// local dependency, so no reason it can't, and now also a wrong-arch
		// result — see pullImage's doc comment), and if that happened after
		// StopContainer/RemoveContainer had already run, the old container
		// would already be gone with nothing to replace it, leaving zero
		// ingress until the operator retries. Pulling first means a pull or
		// arch-mismatch failure here leaves the old, still-serving container
		// untouched.
		if err := m.pullImage(ctx); err != nil {
			return err
		}
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
	case info.Image != PinnedImage:
		return fmt.Sprintf("image %s != desired %s", info.Image, PinnedImage)
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

// pullImage unconditionally pulls PinnedImage (Image's repo pinned to
// CaddyImageDigest — see its doc comment) and verifies the result's
// architecture matches expectedArch, returning an actionable error on
// mismatch. Called by create() (so both first install and the end of
// Ensure's drift-recreate branch always pull) and, before that, by
// Ensure's drift-recreate branch itself (so a pull or arch-mismatch
// failure there is caught before the old container is torn down — see
// Ensure's doc comment for why this must never be gated on an existence
// check).
//
// Pulling by digest rather than by Image's mutable tag is what makes the
// pin meaningful: podman's default "pull if missing" policy means a
// CreateContainer referencing the tag would otherwise silently re-resolve
// it (and could pull something other than what was digest-verified here)
// the moment the tag isn't already cached locally under that exact name —
// see create(), which also uses PinnedImage for exactly this reason.
//
// The arch check is a defensive backstop, not the primary fix: `podman
// pull` itself re-resolves the image manifest for the host architecture,
// so in the overwhelmingly common case a wrong-arch image simply can't
// survive a real pull. This still confirms it explicitly and fails loudly
// (naming both architectures and the exact command to fix it) rather than
// silently handing a container spec to CreateContainer that's certain to
// crash the moment it starts (the qemu-emulation "taggedPointerPack
// invalid packing" style failure this whole fix exists to prevent) — cheap
// insurance against any pull path that turns out to be a no-op against a
// stale wrong-arch cache entry (e.g. a daemon that's unreachable but
// reports success, or a future libpod/registry combination that doesn't
// re-resolve on every call).
func (m *Manager) pullImage(ctx context.Context) error {
	if err := m.rt.PullImage(ctx, PinnedImage); err != nil {
		return fmt.Errorf("caddy: pull %s: %w", PinnedImage, err)
	}
	arch, err := m.rt.ImageArchitecture(ctx, PinnedImage)
	if err != nil {
		return fmt.Errorf("caddy: check architecture of %s: %w", PinnedImage, err)
	}
	if arch != m.expectedArch {
		return fmt.Errorf("caddy: %s resolved to architecture %q, but this host needs %q — likely a stale cached image (e.g. pulled earlier as a build's base layer under a different --platform); fix with `podman pull --arch %s %s`, or remove the cached image and retry", PinnedImage, arch, m.expectedArch, m.expectedArch, PinnedImage)
	}
	return nil
}

// create writes the initial (empty-routes) config, unconditionally pulls
// and arch-verifies the Caddy image (see pullImage), and creates and
// starts the bp-caddy container. Called both on first run
// (InspectContainer reports the container doesn't exist yet) and at the
// end of Ensure's drift-recreate branch.
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

	if err := m.pullImage(ctx); err != nil {
		return err
	}

	spec := podman.CreateSpec{
		Name:  ContainerName,
		Image: PinnedImage,
		// basepod.instance (issue #10): stamped so a second BasePod
		// instance sharing this Podman socket can tell this bp-caddy
		// apart from its own and refuse to fight over it (see Ensure's
		// instance-mismatch check above) instead of recreating it under
		// its own config, as happened in the original incident.
		Labels: map[string]string{"basepod.managed": "true", "basepod.instance": m.instanceID},
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

// ErrCaddyNotRunning and ErrCaddyAdminUnreachable are the two distinct
// failure modes Health (below) can report, wrapped into its returned
// error via %w so a caller can tell them apart with errors.Is without
// Health itself deciding how much of the underlying detail — e.g. a raw
// podman-exec error, which can carry infrastructure detail the same way
// a podman ping failure can (see internal/api/system.go's
// handleSystem doc comment, audit finding L5) — is safe to hand an API
// client. That redaction call belongs to the caller, same as it already
// does for podman ping failures.
var (
	// ErrCaddyNotRunning means the bp-caddy container itself isn't up —
	// missing entirely, or inspected but not in the "running" state.
	// This is the container-lifecycle problem: no amount of Caddy config
	// or admin-API state matters if the container isn't running at all.
	ErrCaddyNotRunning = errors.New("caddy: bp-caddy container is not running")
	// ErrCaddyAdminUnreachable means the container is running but its
	// admin API didn't answer over its own unix socket within
	// caddyHealthTimeoutSeconds — Caddy is wedged, still starting, or the
	// admin socket never came up. A different, container-is-fine problem
	// from ErrCaddyNotRunning.
	ErrCaddyAdminUnreachable = errors.New("caddy: admin API did not respond over its unix socket")
)

// caddyHealthTimeoutSeconds bounds how long Health's admin-API probe
// waits for a response, expressed as whole seconds (curl's --max-time
// flag takes a plain number, not a Go time.Duration) since the probe
// runs through m.exec — a podman-exec call — rather than an http.Client
// Health could otherwise hand its own context deadline to directly.
const caddyHealthTimeoutSeconds = "3"

// caddyHealthCacheTTL bounds how long Health serves a cached result
// before re-probing — see Health's doc comment for the production
// measurement this exists to fix (issue #16 review feedback). A
// stale-by-up-to-this-long health chip is an acceptable tradeoff: the
// failure it reports isn't a fast-moving signal (an operator isn't
// racing to notice Caddy going down within single-digit seconds), and
// it's well inside AppShell/Instance.vue's own 5s poll interval — a
// caller polling every 5s still gets a real (if not brand-new) answer
// every single call, just not a freshly-probed one every time.
const caddyHealthCacheTTL = 12 * time.Second

// Health reports whether Caddy is actually up, by asking it — replacing
// the previous, circular signal ("the dashboard loaded at all", which
// proves nothing if the dashboard route itself is disabled or unbound;
// see internal/server's dashboard_domain handling). nil means healthy;
// otherwise the returned error wraps exactly one of ErrCaddyNotRunning or
// ErrCaddyAdminUnreachable — see their doc comments for what each means
// and why an operator needs them told apart (issue #16).
//
// Cached for caddyHealthCacheTTL: GET /system (this method's only
// caller) is polled every 5s by every open dashboard tab
// (AppShell.vue/Instance.vue), and the underlying probe — a podman-exec
// call, checked below — measured ~200-250ms and ~4.4% of a CPU core
// sustained per continuously-open tab on a real production box if run on
// every poll uncached. Health holds healthMu across the ENTIRE check
// (not just the cache read/write), which — beyond guarding the cache
// fields — also single-flights concurrent callers that land during a
// stale window into one real probe: the second and later callers simply
// block on the same mutex until the first's result is cached, rather
// than each firing their own podman-exec (see
// TestManagerHealthConcurrentRequestsShareOneProbe). Acceptable because
// a health probe isn't latency-sensitive the way, say, a deploy is: a
// caller blocking for up to one probe's duration (bounded by
// caddyHealthTimeoutSeconds) behind another in-flight one is a fine
// tradeoff for guaranteeing at most one probe in flight at a time.
//
// The check itself, once the cache is stale, is two steps: first,
// whether the bp-caddy container itself is running at all
// (m.rt.InspectContainer, the same check Ensure already makes — cheap,
// a single podman API call, not re-measured but not the cost this cache
// exists to amortize); only if so, whether its admin API answers over
// its own unix socket (AdminSocketPath) — reusing exactly the path
// Apply already uses for reload (m.exec, a podman-exec call into the
// running container) but running `curl --unix-socket <AdminSocketPath>
// http://localhost/config/` instead of `caddy reload`. GET /config/
// (https://caddyserver.com/docs/api) is a plain read that reports
// Caddy's live config without mutating anything, unlike reload.
//
// curl, not wget (unlike CaddyProber in internal/deploy, which probes an
// app's plain TCP upstream with wget --spider): only curl, of the two,
// supports --unix-socket, which this check needs since Caddy's admin API
// here is reachable exclusively over AdminSocketPath, never a TCP port
// (see render.go's Config.Admin.Listen) — confirmed present in the
// caddy:2.10-alpine image (verified 2026-08-07 via `podman run --rm
// docker.io/library/caddy:2.10-alpine which curl`, alongside busybox
// wget, which lacks unix-socket support). -f treats any HTTP status >=
// 400 as failure (nonzero exit); --max-time bounds how long a wedged
// Caddy process can block this call.
func (m *Manager) Health(ctx context.Context) error {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()

	now := m.now()
	if !m.healthCachedAt.IsZero() && now.Sub(m.healthCachedAt) < caddyHealthCacheTTL {
		return m.healthCachedErr
	}

	err := m.probeHealth(ctx)
	// A probe that failed because the *caller* went away says nothing
	// about Caddy. handleSystem passes the HTTP request's context, and the
	// dashboard polls /system every 5s from every page, so a tab closing
	// or navigating mid-probe cancels one — cheap to hit, given the probe
	// is a ~200ms window. Caching that would report a false "admin
	// unreachable" to every other caller for the rest of the TTL, which
	// reads to an operator as their ingress having just broken. Leave the
	// previous entry alone (stale is better than wrong) and let the next
	// caller with a live context probe again.
	if ctx.Err() != nil {
		return err
	}
	m.healthCachedAt = now
	m.healthCachedErr = err
	return err
}

// probeHealth is Health's uncached body — the actual container-state
// check plus admin-API exec probe, split out so Health itself only
// carries the cache/single-flight bookkeeping (see Health's doc
// comment). Always called with healthMu already held.
func (m *Manager) probeHealth(ctx context.Context) error {
	info, err := m.rt.InspectContainer(ctx, ContainerName)
	if err != nil {
		if errors.Is(err, podman.ErrNotFound) {
			return fmt.Errorf("%w: no such container", ErrCaddyNotRunning)
		}
		// Any other inspect failure (e.g. podman itself unreachable) also
		// means "can't confirm bp-caddy is running" from an operator's
		// point of view — the "podman" field on the same /system response
		// separately surfaces a broken podman connection itself, so this
		// doesn't need its own fourth state to say the same thing twice.
		return fmt.Errorf("%w: inspecting container: %v", ErrCaddyNotRunning, err)
	}
	if info.State != "running" {
		return fmt.Errorf("%w: state=%s", ErrCaddyNotRunning, info.State)
	}

	if err := m.exec(ctx, ContainerName, "curl", "-fsS", "--max-time", caddyHealthTimeoutSeconds,
		"--unix-socket", AdminSocketPath, "http://localhost/config/", "-o", "/dev/null"); err != nil {
		return fmt.Errorf("%w: %v", ErrCaddyAdminUnreachable, err)
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
//
// The binary is resolved via podman.BinPath (audit finding L3) rather
// than the bare "podman" command name, so this never trusts whatever
// $PATH happens to resolve at call time — see BinPath's doc comment for
// the resolution order and the BASEPOD_PODMAN_BIN override.
func PodmanExec(ctx context.Context, container string, cmd ...string) error {
	bin, err := podman.BinPath()
	if err != nil {
		return err
	}
	args := append([]string{"exec", container}, cmd...)
	c := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("podman exec: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
