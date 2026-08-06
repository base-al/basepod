# BasePod

**A self-hosted PaaS built on Podman and Caddy.** Push code — or an
image, a git repo, or a compose file — and get a running app with
automatic HTTPS. Like CapRover, but rootless, daemonless, and shipped as
a single Go binary.

> **Status: v0.1 and v0.2 shipped; v0.3 (real deploys — tarball builds,
> rollback, CLI core) is complete on this branch, pending release.**
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

> Once an app exists (create it once from the [dashboard](#dashboard) or
> the [API](#api)), day-to-day shipping is `basepod deploy`: `cd` into
> your project and run it — see [CLI](#cli) below for the full
> quick-reference. That's the primary dev flow as of v0.3.

### Backup

Back up **three files together** to preserve your BasePod instance:
- `basepod.db` (the SQLite database in `<data-dir>`)
- `secret.key` (env-var encryption key in `<data-dir>`) — **losing this file makes stored environment variables unrecoverable and apps undeployable until re-entered**
- `<data-dir>/caddy-data` (Caddy's TLS certificates)

Restore by copying all three to the same locations in your new instance.

### Dashboard

Once the server is running, open **http://localhost:3080** and sign in
with the admin email/password from `basepod setup` above. From there you
can create an app from an image, deploy it, and manage its env vars,
custom domains, and live logs — everything the API subsection below
does, without curl.

> Screenshots are deferred until the UI settles a bit further; see
> [docs/plan/07 — Web Dashboard](docs/plan/07.dashboard.md) for what's
> shipped in this milestone versus deferred to later ones.

### Remote access

On a remote box (a VPS, a home server, anything not `localhost`), BasePod
serves the dashboard itself, automatically, over HTTPS — no SSH tunnel
required.

At boot, BasePod binds a second internal listener on a unix socket
(shared with the `bp-caddy` container through its own dedicated bind
mount — no other container, and nothing else on the network, can reach
it), then adds a route for it to Caddy at
**`https://basepod.<root-domain>`** (e.g.
`https://basepod.apps.example.com`), alongside your deployed apps'
routes. The hostname comes from the `dashboard_domain` setting: unset by
default (BasePod computes and stores `basepod.<root-domain>` the first
time it boots), or set it to any hostname you prefer, or to the literal
value `off` to disable the dashboard route entirely.

This works out of the box on **Linux** hosts, rootless Podman included.
On **macOS**, `podman machine`'s virtiofs-shared directories don't carry
unix sockets across the VM boundary, so BasePod logs a warning and
disables the dashboard route there — the dashboard is still reachable
locally at `http://localhost:3080` (see [Dashboard](#dashboard) above),
or remotely via an SSH tunnel as a fallback:

```bash
ssh -L 3080:localhost:3080 user@your-server
# then open http://localhost:3080 locally
```

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

### CLI

The `basepod` binary doubles as a client: same executable, run against
a running server (local or remote) to log in, deploy, and manage apps
without curl.

```bash
# Log in once — saves the server URL + session token as a named context
# in ~/.config/basepod/cli.yaml (override with BASEPOD_CLI_CONFIG).
./basepod login http://localhost:3080 --email admin@example.com

# Deploy from source: tars the given directory (must have a Containerfile
# or Dockerfile at its root, default cwd), uploads it, builds it, and
# rolls it out — the same pipeline POST /deploy/tarball drives above.
./basepod deploy . -a hello

# ...or deploy an existing image instead of building from source.
./basepod deploy -a hello --image ghcr.io/user/app:tag

# Tail logs (-f to follow; omit for a fixed --tail window).
./basepod logs hello -f
./basepod logs hello --tail 200

# Roll back to an earlier deployment's exact image (see `basepod status`
# or the dashboard's history tab for deployment numbers).
./basepod rollback hello 3
```

Other commands: `basepod apps` (list, `--json` for scripting), `basepod
env <app> [set KEY=VALUE... | unset KEY...]`, `basepod status` (system +
every app in one shot), and `basepod context list|use <name>` for
juggling multiple saved servers. Every server-talking command accepts a
one-off `--context <name>` flag.

## Known issues

- Behind the HTTPS dashboard proxy, the login rate limit is currently
  shared across all remote clients (10/min total) — a deliberate
  fail-closed tradeoff for v0.3. Sustained failed logins can temporarily
  lock out remote login; existing sessions are unaffected and `ssh -L
  3080:localhost:3080` + http://localhost:3080 always works. Per-client
  limiting returns in v0.4.

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
