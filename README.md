# BasePod

**A self-hosted PaaS built on Podman and Caddy.** Push code — or an
image, a git repo, or a compose file — and get a running app with
automatic HTTPS. Like CapRover, but rootless, daemonless, and shipped as
a single Go binary.

> **Status: planning.** The detailed design lives in
> [`docs/plan/`](docs/plan/) — start with
> [01 — Overview & Architecture](docs/plan/01.overview-and-architecture.md).

## Why BasePod

- **Rootless Podman** instead of a privileged Docker daemon; apps are
  supervised by systemd (Quadlet) and survive control-plane restarts.
- **Caddy** at the edge: automatic HTTPS, on-demand TLS for customer
  domains, zero-downtime config swaps via its admin API.
- **One binary** — installs on any Linux VPS and on macOS (Intel &
  Apple Silicon), with SQLite embedded. No external dependencies.
- **Teams & RBAC** — users, teams, roles, scoped API tokens, audit log.
- **Extensible** — lifecycle webhooks, one-click app templates, and
  OCI-image plugins.

## Plan

| # | Document |
|---|---|
| 01 | [Overview & Architecture](docs/plan/01.overview-and-architecture.md) |
| 02 | [Installation & Platforms](docs/plan/02.installation-and-platforms.md) |
| 03 | [Podman Integration](docs/plan/03.podman-integration.md) |
| 04 | [Caddy & Networking](docs/plan/04.caddy-and-networking.md) |
| 05 | [Data Model & API](docs/plan/05.data-model-and-api.md) |
| 06 | [Builds, Deployments & CLI](docs/plan/06.builds-and-deployments.md) |
| 07 | [Web Dashboard](docs/plan/07.dashboard.md) |
| 08 | [Teams & RBAC](docs/plan/08.teams-and-rbac.md) |
| 09 | [Plugins & Extensibility](docs/plan/09.plugins-and-extensibility.md) |
| 10 | [Multi-Server](docs/plan/10.multi-server.md) |
| 11 | [Testing, Release & Roadmap](docs/plan/11.testing-release-and-roadmap.md) |

## License

[Apache-2.0](LICENSE)
