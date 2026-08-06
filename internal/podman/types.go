package podman

import "errors"

// ErrNotFound is returned by Client methods when the requested libpod
// resource (container, network, image, ...) does not exist.
var ErrNotFound = errors.New("podman: not found")

// CreateSpec describes a container to create via the libpod SpecGenerator
// (POST /libpod/containers/create). Only the fields BasePod needs are
// exposed here; Client.CreateContainer maps this onto the wire format.
//
// MemoryLimitBytes, CPUQuota, and PidsLimit are resource limits (audit
// finding H2: app containers previously ran with none at all, so a hostile
// third-party image could fork-bomb or balloon memory and take down the
// control plane and every neighbor app). Each is independently optional:
// its zero value means "unlimited" and CreateContainer omits the
// corresponding field from the wire request entirely rather than sending
// an explicit zero, since libpod's own zero value for these fields already
// means unlimited — sending it explicitly would be a no-op, not a
// tightening, so omitting is just the honest way to say "no limit" (and
// keeps a caller that never sets these, e.g. internal/caddy.Manager's own
// CreateSpec values, byte-for-byte unaffected). CPUQuota is expressed in
// CPU cores (e.g. 1.5), not raw microseconds — CreateContainer converts it
// to libpod's quota/period pair using a fixed 100ms period.
//
// Every container created through CreateContainer also gets fixed
// hardening applied unconditionally (not just for CreateSpec values that
// set a limit): the no-new-privileges flag and a NET_RAW capability drop.
// This isn't a CreateSpec field because it is not configurable — see
// CreateContainer's doc comment for why it's applied at the client.go
// mapping layer instead.
type CreateSpec struct {
	Name           string
	Image          string
	Labels         map[string]string
	Env            map[string]string
	Command        []string // optional override of the image's default command
	NetworkName    string
	NetworkAliases []string
	PortMappings   []PortMapping
	Mounts         []BindMount
	// NamedVolumes are libpod named volumes (as opposed to Mounts's host
	// bind mounts) mounted into the container — v0.5 Task 6's substrate
	// for compose's stateful services (SQLite/Postgres-style apps). See
	// NamedVolume's doc comment for the wire-format verification trail.
	NamedVolumes  []NamedVolume
	RestartPolicy string // "always" | "no"

	// MemoryLimitBytes caps the container's memory (libpod
	// resource_limits.memory.limit). 0 = unlimited.
	MemoryLimitBytes int64
	// CPUQuota caps the container's CPU allotment in cores (e.g. 1.5 =
	// one and a half CPUs), mapped onto libpod resource_limits.cpu.quota
	// with resource_limits.cpu.period fixed at cpuPeriodMicros (see
	// CreateContainer). 0 = unlimited.
	CPUQuota float64
	// PidsLimit caps the number of processes/threads the container can
	// create (libpod resource_limits.pids.limit) — the direct mitigation
	// for a fork-bomb. 0 = unlimited.
	PidsLimit int64
}

// ContainerInfo is a summarized view of a libpod container, as returned by
// InspectContainer and ListContainers. Image, Ports, and Mounts are
// populated by InspectContainer only (ListContainers's libpod endpoint
// doesn't carry any of them) and are the zero value there.
type ContainerInfo struct {
	ID     string
	Name   string
	State  string // "running" | "exited" | ...
	Labels map[string]string
	Image  string        // human-readable image ref (libpod inspect's "ImageName"), e.g. "docker.io/library/caddy:2.10-alpine"
	Ports  []PortMapping // parsed from inspect's HostConfig.PortBindings; TCP only (v0.3)
	Mounts []BindMount   // parsed from inspect's top-level "Mounts"; bind mounts only (v0.3), ReadOnly always false (unused by callers — see caddy.driftReason)
}

// NetworkInfo is a summarized view of a libpod network, as returned by
// InspectNetwork.
type NetworkInfo struct {
	Name       string
	DNSEnabled bool
	Gateway    string // first subnet's gateway; "" if the network has none
}

// PortMapping maps a container port to a host port. It is marshaled
// directly into the libpod SpecGenerator's "portmappings" field.
type PortMapping struct {
	ContainerPort uint16 `json:"container_port"`
	HostPort      uint16 `json:"host_port,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

// BindMount describes a host bind mount into the container.
type BindMount struct {
	Source   string
	Dest     string
	ReadOnly bool
}

// NamedVolume describes a libpod named volume mounted into the container
// — as opposed to BindMount's host bind mount. Name is the actual libpod
// volume name (already fully derived by the caller — internal/deploy's
// VolumeName, "bp-<slug>-<volume-name>" — not the app's short logical
// name); Dest is the absolute path inside the container it's mounted at.
//
// Wire-format verification (v0.5 Task 6, following the same drill v0.4's
// resource limits used — see specGen's doc comment): confirmed against
// podman v5.7.1's actual source at the exact commit (f845d14e) the local
// dev daemon (podman machine, server API version 5.7.1) reports itself
// built from — pkg/specgen/specgen.go's ContainerStorageConfig.Volumes
// (`json:"volumes,omitempty"`, type []*NamedVolume) and
// pkg/specgen/volumes.go's NamedVolume struct itself: Name/Dest/Options/
// IsAnonymous/SubPath, all string/[]string/bool with NO json tags at
// all — meaning the wire keys are the bare Go field names ("Name",
// "Dest"), not the lowercase forms the SpecGenerator's OTHER embedded
// structs use. ContainerStorageConfig is anonymously embedded in
// SpecGenerator (alongside ContainerBasicConfig, ContainerSecurityConfig,
// etc.), so its Volumes field flattens onto the top-level request body as
// "volumes" — the same flattening pattern this package's ResourceLimits
// field already relies on (see specGen's doc comment). Cross-checked live
// against the local socket: created a container with
// {"volumes":[{"Name":"...","Dest":"/data"}]} and confirmed via
// GET .../json that libpod auto-created and mounted the named volume at
// the given destination — see the v0.5 Task 6 report for the full
// request/response trail.
type NamedVolume struct {
	Name string
	Dest string
}

// specGen is the subset of libpod's SpecGenerator that CreateContainer
// sends to POST /libpod/containers/create. Field names/casing follow the
// libpod API exactly (see docs/plan/03 and the podman API reference).
//
// ResourceLimits/CapDrop/NoNewPrivileges field names were verified against
// podman v5.7.1's actual source (matching the version of the live daemon
// this was developed against — see the v0.4 Task 2 report for the full
// verification trail): pkg/specgen/specgen.go's
// ContainerResourceConfig.ResourceLimits
// (`json:"resource_limits,omitempty"`, type *spec.LinuxResources) and
// ContainerSecurityConfig.CapDrop (`json:"cap_drop,omitempty"`,
// []string)/NoNewPrivileges (`json:"no_new_privileges,omitempty"`, *bool).
// resourceLimits/linuxMemory/linuxCPU/linuxPids below hand-roll just the
// subset of opencontainers/runtime-spec v1.2.1's LinuxResources this
// client sets, rather than importing that module, matching how namespace/
// perNetworkOptions/specMount already hand-roll their own libpod wire
// subsets instead of depending on podman's or runtime-spec's Go packages.
type specGen struct {
	Name         string                       `json:"name"`
	Image        string                       `json:"image"`
	Labels       map[string]string            `json:"labels,omitempty"`
	Env          map[string]string            `json:"env,omitempty"`
	Command      []string                     `json:"command,omitempty"`
	Networks     map[string]perNetworkOptions `json:"Networks,omitempty"`
	NetNS        *namespace                   `json:"netns,omitempty"`
	PortMappings []PortMapping                `json:"portmappings,omitempty"`
	Mounts       []specMount                  `json:"mounts,omitempty"`
	// Volumes maps onto ContainerStorageConfig.Volumes when flattened —
	// see NamedVolume's doc comment for the wire-format verification
	// trail (v0.5 Task 6).
	Volumes         []namedVolume   `json:"volumes,omitempty"`
	RestartPolicy   string          `json:"restart_policy,omitempty"`
	ResourceLimits  *resourceLimits `json:"resource_limits,omitempty"`
	CapDrop         []string        `json:"cap_drop,omitempty"`
	NoNewPrivileges *bool           `json:"no_new_privileges,omitempty"`
}

// resourceLimits mirrors the subset of OCI runtime-spec's LinuxResources
// (github.com/opencontainers/runtime-spec v1.2.1, specs-go/config.go — the
// version podman v5.7.1 itself depends on) that CreateContainer populates:
// memory, cpu, and pids limits. Each sub-object is a pointer left nil (and
// so omitted) when CreateSpec's corresponding field is 0 — see
// CreateContainer.
type resourceLimits struct {
	Memory *linuxMemory `json:"memory,omitempty"`
	CPU    *linuxCPU    `json:"cpu,omitempty"`
	Pids   *linuxPids   `json:"pids,omitempty"`
}

// linuxMemory mirrors runtime-spec's LinuxMemory, restricted to the one
// field CreateContainer sets. Limit is *int64 (matching runtime-spec
// exactly) so a present-but-zero value is still distinguishable from
// absent, though CreateContainer never constructs one with a zero Limit —
// see resourceLimits's doc comment.
type linuxMemory struct {
	Limit *int64 `json:"limit,omitempty"`
}

// linuxCPU mirrors runtime-spec's LinuxCPU, restricted to Quota/Period —
// the hardcap pair CreateContainer sets from CreateSpec.CPUQuota (cores)
// via cpuPeriodMicros. Quota is *int64, Period is *uint64, matching
// runtime-spec exactly.
type linuxCPU struct {
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`
}

// linuxPids mirrors runtime-spec's LinuxPids. Unlike LinuxMemory.Limit and
// LinuxCPU.Quota, runtime-spec declares this field as a plain (non-pointer)
// int64 with json tag "limit" (no omitempty) — verified directly against
// opencontainers/runtime-spec v1.2.1's specs-go/config.go. That's harmless
// here: linuxPids is only ever embedded via resourceLimits.Pids, itself a
// pointer left nil (and so the whole "pids" object omitted) whenever
// CreateSpec.PidsLimit is 0 — see CreateContainer.
type linuxPids struct {
	Limit int64 `json:"limit"`
}

// namespace is libpod SpecGenerator's Namespace type, used here only for
// netns. NSMode must be set to "bridge" explicitly whenever Networks is
// non-empty: unlike the `podman` CLI (which infers bridge mode from
// `--network <name>` and fills this in for you), the raw API validates
// Networks against whatever netns mode is already set — and its
// unset/zero value is not bridge mode on every podman version (observed:
// podman 4.9.3 rejects a populated Networks map outright with "networks
// and static ip/mac address can only be used with Bridge mode
// networking" when netns is left unset, even though the exact same
// request works on podman 5.x, which apparently infers it).
type namespace struct {
	NSMode string `json:"nsmode,omitempty"`
}

// perNetworkOptions configures how a container joins a given network
// (libpod SpecGenerator's Networks map value).
type perNetworkOptions struct {
	Aliases []string `json:"aliases,omitempty"`
}

// specMount is a libpod SpecGenerator bind mount entry.
type specMount struct {
	Destination string   `json:"destination"`
	Source      string   `json:"source"`
	Type        string   `json:"type"`              // "bind"
	Options     []string `json:"options,omitempty"` // ["ro"] when ReadOnly
}

// namedVolume is the wire shape of one entry in specGen.Volumes. Mirrors
// libpod's own pkg/specgen.NamedVolume exactly — Name and Dest only (the
// other real fields, Options/IsAnonymous/SubPath, are unused by BasePod
// and left off rather than carried as always-zero placeholders) — with NO
// json tags, deliberately: see NamedVolume's doc comment for why the real
// struct has none either, so the wire keys must be the literal Go field
// names "Name"/"Dest", not lowercased.
type namedVolume struct {
	Name string
	Dest string
}

// networkCreate is the body for POST /libpod/networks/create. DNSEnabled
// must be set explicitly: unlike `podman network create` (whose CLI
// defaults to DNS-enabled), the raw libpod API defaults dns_enabled to
// false, which would leave containers unable to resolve each other by
// name/alias — the mechanism the Caddy manager and deploy engine rely on
// for routing to app containers (e.g. "bp-hello:80").
type networkCreate struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels,omitempty"`
	DNSEnabled bool              `json:"dns_enabled"`
}

// volumeCreate is the body for POST /libpod/volumes/create
// (Client.EnsureVolume). Field names verified against libpod v5.7.1's
// actual source at the exact commit (f845d14e) the local dev daemon
// reports itself built from:
// pkg/api/handlers/libpod/volumes.go's CreateVolume handler decodes the
// request BODY via plain json.Decode into
// entities.VolumeCreateOptions (a type alias for
// pkg/domain/entities/types.VolumeCreateOptions), whose fields (Name,
// Driver, Label, Labels, Options, IgnoreIfExists, UID, GID) carry ONLY
// `schema:"..."` tags — used for that same struct's OTHER binding path,
// decoding query-string parameters, which volumes/create doesn't use for
// the body — and no `json` tags at all, so like NamedVolume the wire keys
// for the body are the bare Go field names. Only Name and Labels are
// populated here (the rest default to their zero value and are omitted).
// Verified live against the local socket: POST'd
// {"Name":"...","Labels":{"basepod.managed":"true",...}}, confirmed via
// GET .../json that the created volume carries exactly those labels, and
// confirmed a container referencing that volume by name in its "volumes"
// field mounts it as-is (labels preserved) rather than silently
// re-creating an unlabeled one — see the v0.5 Task 6 report for the full
// request/response trail.
type volumeCreate struct {
	Name   string            `json:"Name,omitempty"`
	Labels map[string]string `json:"Labels,omitempty"`
}
