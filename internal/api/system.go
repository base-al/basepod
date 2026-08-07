package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/base-al/basepod/internal/caddy"
)

// CaddyHealthChecker reports whether Caddy is actually reachable — nil
// means "healthy", any other error wraps one of caddy.ErrCaddyNotRunning
// or caddy.ErrCaddyAdminUnreachable (see their doc comments). Implemented
// by *caddy.Manager in production (its Health method); a fake in tests
// that need to script every one of the three states handleSystem's
// "caddy" field can report.
type CaddyHealthChecker interface {
	Health(ctx context.Context) error
}

// SystemInfo bundles the instance-level facts GET /system reports beyond
// podman reachability and app count: the root domain apps deploy under,
// the dashboard route's domain (as actually in effect, not just as
// stored — see DashboardDomain's doc comment), and a Caddy health
// prober. These all come from the same place at boot —
// internal/server.Run, right after it resolves rootDomain,
// dashboardDomain, and dashboardRoute — and travel together as a single
// api.New parameter rather than three more positional ones.
type SystemInfo struct {
	// RootDomain is the root domain this instance deploys apps under
	// (the "root_domain" setting, or cfg.RootDomain — see
	// internal/server.Run). The server refuses to boot without one, so
	// this is always non-empty in production.
	RootDomain string

	// DashboardDomain is what handleSystem's "dashboard_domain" field
	// reports verbatim: either the live hostname Caddy is actually
	// proxying to this instance's own API right now, or one of two
	// sentinel strings — "off" (the operator set the dashboard_domain
	// setting to "off") or "unbound" (a domain is configured, but the
	// dashboard's unix-socket listener failed to bind at boot — expected
	// on macOS, where podman machine's virtiofs bind mounts don't carry
	// unix sockets across the VM boundary; see
	// internal/server.prepareDashboardListener's doc comment) — that are
	// never valid hostnames themselves, so a client can tell "this is a
	// live domain" from "this is a status" by construction. This mirrors
	// the dashboard_domain SETTING's own pre-existing "off" sentinel
	// (internal/server.resolveDashboardDomain) rather than inventing a
	// second, parallel vocabulary or a separate status field.
	//
	// Never the stored setting's raw value when it isn't actually in
	// effect: reporting a configured-but-unbound hostname here would
	// claim the dashboard is reachable at that domain when it demonstrably
	// isn't (issue #16).
	DashboardDomain string

	// CaddyHealth reports Caddy's own reachability. nil disables the
	// check (e.g. a test that doesn't care about the "caddy" field),
	// which handleSystem reports as "unknown" — an honest "not checked",
	// never a fabricated "ok". Production always passes a *caddy.Manager
	// (which never returns a nil interface value with a nil underlying
	// pointer, since internal/server.Run always has one it built for
	// mgr.Ensure earlier in Run).
	CaddyHealth CaddyHealthChecker
}

type systemResponse struct {
	Version         string `json:"version"`
	Podman          string `json:"podman"`
	Apps            int    `json:"apps"`
	RootDomain      string `json:"root_domain"`
	DashboardDomain string `json:"dashboard_domain"`
	Caddy           string `json:"caddy"`
}

// handleSystem reports the running version, container runtime
// reachability, total app count, the root and dashboard domains, and a
// real Caddy health signal — replacing the dashboard's previous circular
// stand-in ("the dashboard loaded at all", which proves nothing when the
// dashboard route itself is disabled or unbound) with an actual probe of
// Caddy's admin API (see caddy.Manager.Health).
//
// A podman ping failure is reported to the caller as the fixed string
// "error" (audit finding L5): the underlying error can carry internal
// infrastructure detail — e.g. the unix socket path podman.Client dials
// (see internal/podman.socketPath) — that has no business reaching an
// API client. The actual detail is logged server-side instead, where an
// operator diagnosing "podman: error" in the dashboard can find it.
//
// The "caddy" field follows the same shape/redaction convention, extended
// to distinguish the three states an operator needs told apart (issue
// #16 — a stopped container, a running container whose admin API is
// unreachable, and healthy, are three different problems): "ok", "error:
// container not running", "error: admin unreachable", or the fixed
// string "error" as a fallback for any other failure shape, with full
// error detail logged server-side exactly like podman's.
func (a *api) handleSystem(w http.ResponseWriter, r *http.Request) {
	podmanStatus := "ok"
	if err := a.ping(r.Context()); err != nil {
		log.Printf("api: podman ping failed: %v", err)
		podmanStatus = "error"
	}

	apps, err := a.st.ListApps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list apps")
		return
	}

	writeJSON(w, http.StatusOK, systemResponse{
		Version:         a.version,
		Podman:          podmanStatus,
		Apps:            len(apps),
		RootDomain:      a.sysInfo.RootDomain,
		DashboardDomain: a.sysInfo.DashboardDomain,
		Caddy:           a.caddyHealthStatus(r.Context()),
	})
}

// caddyHealthStatus runs a.sysInfo.CaddyHealth (if wired) and maps its
// result onto the redacted, categorical string handleSystem's "caddy"
// field reports — see handleSystem's doc comment for the shape/redaction
// rationale shared with "podman".
func (a *api) caddyHealthStatus(ctx context.Context) string {
	if a.sysInfo.CaddyHealth == nil {
		return "unknown"
	}
	err := a.sysInfo.CaddyHealth.Health(ctx)
	if err == nil {
		return "ok"
	}
	log.Printf("api: caddy health check failed: %v", err)
	switch {
	case errors.Is(err, caddy.ErrCaddyNotRunning):
		return "error: container not running"
	case errors.Is(err, caddy.ErrCaddyAdminUnreachable):
		return "error: admin unreachable"
	default:
		return "error"
	}
}
