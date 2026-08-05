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
	// ImageExists reports whether ref is present in the local image
	// store — used by Rollback to decide whether a target deployment's
	// image needs pulling (registry refs) or is simply gone
	// (localhost/... build tags, which can never be re-fetched).
	ImageExists(ctx context.Context, ref string) (bool, error)
	// RemoveImage removes an image by reference — used by the deploy
	// engine's built-image retention (see pruneBuiltImages).
	RemoveImage(ctx context.Context, ref string, force bool) error
	// ListImageTags returns every locally-present image tag matching
	// "<repoPrefix>:*" — used by pruneBuiltImages to enumerate an app's
	// built tags.
	ListImageTags(ctx context.Context, repoPrefix string) ([]string, error)
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

// Router applies a full route set (plus an optional dashboard route — see
// caddy.DashboardRoute) to the reverse proxy (satisfied by *caddy.Manager).
type Router interface {
	Apply(ctx context.Context, routes []caddy.AppRoute, dashboard *caddy.DashboardRoute) error
}

// Compile-time checks that the production implementations satisfy the
// seams the engine consumes.
var (
	_ Runtime = (*podman.Client)(nil)
	_ Router  = (*caddy.Manager)(nil)
)
