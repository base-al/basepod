# BasePod

**A self-hosted PaaS built on Podman and Caddy.** Push code — or an
image, a git repo, or a compose file — and get a running app with
automatic HTTPS. Like CapRover, but rootless, daemonless, and shipped as
a single Go binary.

> **Status: planning.** The detailed design lives in
> [`docs/plan/`](docs/plan/) — start with
> [01 — Overview & Architecture](docs/plan/01.overview-and-architecture.md).

## Quickstart

> **v0.1 is API-only.** There's no CLI deploy command or web dashboard
> yet — the walking skeleton proves the control plane end-to-end over
> its REST API. `docs/plan/` describes where the rest is headed.

### Prerequisites

- **Go** 1.26+ (to build from source)
- **Podman**, with a reachable socket:
  - macOS: `podman machine init && podman machine start`
  - Linux (rootless): `systemctl --user enable --now podman.socket`
- **curl** and **jq** (for the flow below)

### Build from source

```bash
go build -o basepod ./cmd/basepod
```

### First-run setup

```bash
./basepod setup \
  --config ~/.config/basepod/config.yaml \
  --data-dir ~/.local/share/basepod \
  --root-domain apps.localhost \
  --admin-email admin@example.com \
  --admin-password change-me-please
```

### Run the control plane

```bash
./basepod server --config ~/.config/basepod/config.yaml
```

By default the API listens on `127.0.0.1:3080`, and BasePod's own
Caddy container serves apps on ports 80/443 (override with
`BASEPOD_HTTP_PORT`/`BASEPOD_HTTPS_PORT` if those clash on your
machine).

### Deploy an app via curl

```bash
TOKEN=$(curl -s localhost:3080/api/v1/auth/login \
  -d '{"email":"admin@example.com","password":"change-me-please"}' | jq -r .token)

curl -s -H "Authorization: Bearer $TOKEN" localhost:3080/api/v1/apps \
  -d '{"name":"hello","image":"docker.io/traefik/whoami:latest","port":80}'

curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  localhost:3080/api/v1/apps/hello/deploy

curl -sk --resolve hello.apps.localhost:443:127.0.0.1 \
  https://hello.apps.localhost/
```

That last request is served over HTTPS (BasePod's internal CA) by the
`hello` app's container, routed automatically by Caddy. Tear it down
with `curl -X DELETE -H "Authorization: Bearer $TOKEN"
localhost:3080/api/v1/apps/hello`.

See [`scripts/e2e-local.sh`](scripts/e2e-local.sh) for this whole flow
scripted end-to-end (also run in CI on every push/PR).

## Known issues (v0.1)

- If the first `basepod server` boot fails because port 80/443 is
  already taken, a created-but-never-started `bp-caddy` container with
  the old port mapping can be left behind. Remedy: `podman rm -f
  bp-caddy`, then set `BASEPOD_HTTP_PORT`/`BASEPOD_HTTPS_PORT` to free
  ports and restart.

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
