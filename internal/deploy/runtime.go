package deploy

import (
	"context"
	"io"

	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/podman"
)

// Runtime is the podman capability the deploy engine needs (satisfied by
// *podman.Client). It's the seam tests substitute a fake for.
type Runtime interface {
	PullImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, spec podman.CreateSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeoutSec int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, nameOrID string) (*podman.ContainerInfo, error)
	ListContainers(ctx context.Context, labelFilters map[string]string) ([]podman.ContainerInfo, error)
	ContainerLogs(ctx context.Context, nameOrID string, follow bool, tail int) (io.ReadCloser, error)
}

// Prober checks that an upstream ("host:port" reachable on the basepod
// network) is answering.
type Prober func(ctx context.Context, upstream string) error

// Router applies a full route set to the reverse proxy (satisfied by
// *caddy.Manager).
type Router interface {
	Apply(ctx context.Context, routes []caddy.AppRoute) error
}

// Compile-time checks that the production implementations satisfy the
// seams the engine consumes.
var (
	_ Runtime = (*podman.Client)(nil)
	_ Router  = (*caddy.Manager)(nil)
)
