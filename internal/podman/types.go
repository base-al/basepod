package podman

import "errors"

// ErrNotFound is returned by Client methods when the requested libpod
// resource (container, network, image, ...) does not exist.
var ErrNotFound = errors.New("podman: not found")

// CreateSpec describes a container to create via the libpod SpecGenerator
// (POST /libpod/containers/create). Only the fields BasePod needs are
// exposed here; Client.CreateContainer maps this onto the wire format.
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
	RestartPolicy  string // "always" | "no"
}

// ContainerInfo is a summarized view of a libpod container, as returned by
// InspectContainer and ListContainers.
type ContainerInfo struct {
	ID     string
	Name   string
	State  string // "running" | "exited" | ...
	Labels map[string]string
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

// specGen is the subset of libpod's SpecGenerator that CreateContainer
// sends to POST /libpod/containers/create. Field names/casing follow the
// libpod API exactly (see docs/plan/03 and the podman API reference).
type specGen struct {
	Name          string                       `json:"name"`
	Image         string                       `json:"image"`
	Labels        map[string]string            `json:"labels,omitempty"`
	Env           map[string]string            `json:"env,omitempty"`
	Command       []string                     `json:"command,omitempty"`
	Networks      map[string]perNetworkOptions `json:"Networks,omitempty"`
	NetNS         *namespace                   `json:"netns,omitempty"`
	PortMappings  []PortMapping                `json:"portmappings,omitempty"`
	Mounts        []specMount                  `json:"mounts,omitempty"`
	RestartPolicy string                       `json:"restart_policy,omitempty"`
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
