# BasePod — Approved Design (2026-08-05)

## One-liner

An open-source, self-hosted PaaS — CapRover's experience, but rootless Podman
instead of Docker Swarm and Caddy instead of nginx. Single Go binary, runs on
macOS (Intel + Apple Silicon) and any Linux VPS.

## Approved decisions

| Decision | Choice |
|---|---|
| Control plane language | Go (single static binary per platform) |
| Container runtime | Podman ≥ 5.x, rootless-first, via Unix socket + Go bindings |
| Reverse proxy / TLS | Caddy ≥ 2.x, driven through its admin API (JSON config, atomic reloads) |
| App supervision (Linux) | Quadlet (systemd) units |
| App supervision (macOS) | BasePod-supervised containers inside `podman machine` (no systemd) |
| State | Embedded SQLite (pure-Go driver, no CGO) |
| Dashboard | Vue 3 + Vite + Nuxt UI, built to static files, embedded in the Go binary |
| Deploy paths | CLI tarball deploy, git push/webhook, image name via UI, compose files |
| Scope | Single-server first; multi-server (agent mode) designed up front, built in a later phase |
| Extensibility | Plugin system: lifecycle hooks, OCI-image plugins with manifest, one-click app templates |
| Teams | Users, teams, roles (owner/admin/member/viewer), API tokens, audit log |
| License / visibility | Apache-2.0, public GitHub repo |
| Distribution | GoReleaser: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64; Homebrew tap; curl install script |

## Architecture summary

One `basepod` binary:

- `basepod server` — control-plane daemon: REST API + embedded dashboard,
  Podman socket client, Caddy manager, build pipeline, SQLite state.
- `basepod <cmd>` — CLI client (login, deploy, apps, logs, …) hitting the same
  API locally or remotely.

## Phases

0. Foundation: repo, CI, installer, daemon skeleton
1. Runtime: Podman + Caddy management, SQLite, single-admin auth
2. Apps: create/deploy/scale/delete, domains + auto-HTTPS, env vars, volumes, logs
3. Teams: users, teams, RBAC, tokens, audit log
4. Git & catalog: webhook deploys, compose, one-click apps
5. Plugins: hooks, OCI plugins, template catalog
6. Multi-server: agent mode

## Plan documents

Detailed plans live in `docs/plan/01…11`. This spec is the decision record;
the plan docs are the source of truth for implementation detail.
