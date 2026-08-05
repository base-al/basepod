# BasePod

**A self-hosted PaaS built on Podman and Caddy.** Push code — or an
image, a git repo, or a compose file — and get a running app with
automatic HTTPS. Like CapRover, but rootless, daemonless, and shipped as
a single Go binary.

> **Status: v0.1 shipped, v0.2 (dashboard) is the current milestone.**
> The detailed design lives in [`docs/plan/`](docs/plan/) — start with
> [01 — Overview & Architecture](docs/plan/01.overview-and-architecture.md).

## Quickstart

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

### Dashboard

Once the server is running, open **http://localhost:3080** and sign in
with the admin email/password from `basepod setup` above. From there you
can create an app from an image, deploy it, and manage its env vars,
custom domains, and live logs — everything the API subsection below
does, without curl.

> Screenshots are deferred until the UI settles a bit further; see
> [docs/plan/07 — Web Dashboard](docs/plan/07.dashboard.md) for what's
> shipped in this milestone versus deferred to later ones.

### API

Everything the dashboard does is also available directly over the REST
API — useful for scripting, CI, or just poking at BasePod without a
browser:

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
(plus env vars, custom domains, and log streaming) scripted end-to-end
(also run in CI on every push/PR).

## Known issues

- If the first `basepod server` boot fails because port 80/443 is
  already taken, a created-but-never-started `bp-caddy` container with
  the old port mapping can be left behind. Remedy: `podman rm -f
  bp-caddy`, then set `BASEPOD_HTTP_PORT`/`BASEPOD_HTTPS_PORT` to free
  ports and restart.

## Contributing

The dashboard (`web/`) is a Vite/Vue app that gets embedded into the
`basepod` binary at build time (`web/embed.go`). Useful targets:

```bash
make ui     # build the dashboard into web/dist
make build  # ui, then compile the basepod binary
make dev    # instructions for running server + dashboard with hot-reload
make test   # go test ./...
```

Only a placeholder `web/dist/index.html` ("BasePod dashboard not built —
run make ui") is committed, so `go build ./...` works without Node
installed — `.gitignore` excludes the rest of `web/dist/`. **Never commit
a real `make ui` build**; CI builds the dashboard fresh on every run.

## Why BasePod

- **Rootless Podman** instead of a privileged Docker daemon; apps are
  supervised by systemd (Quadlet) and survive control-plane restarts.
- **Caddy** at the edge: automatic HTTPS, on-demand TLS for customer
  domains, zero-downtime config swaps via its admin API.
- **One binary** — installs on any Linux VPS and on macOS (Intel &
  Apple Silicon), with SQLite embedded. No external dependencies.
- **Web dashboard** — a Vue SPA embedded in the same binary: apps,
  new-app wizard, live logs, env vars, and custom domains, no separate
  frontend deployment.
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
