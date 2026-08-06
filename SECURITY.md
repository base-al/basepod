# Security Policy

BasePod is a self-hosted PaaS control plane (rootless Podman + Caddy) —
see [`docs/plan/01.overview-and-architecture.md`](docs/plan/01.overview-and-architecture.md)
for the full architecture. This document covers which versions get
security fixes, how to report a vulnerability, the threat model this
project is designed against, and known limitations that are still open.

## Supported Versions

BasePod is pre-1.0 and moves fast; only the most recent tagged release
line receives security fixes. Older tags are not backported to.

| Version | Supported          |
| ------- | ------------------ |
| 0.4.x   | :white_check_mark: |
| 0.3.x   | :x:                |
| < 0.3   | :x:                |

If you're running an older release, please upgrade before reporting an
issue that a newer release may have already fixed.

## Reporting a Vulnerability

Please report suspected security vulnerabilities privately via **GitHub
Security Advisories** on this repository:

<https://github.com/base-al/basepod/security/advisories/new>

Do not open a public issue for a suspected vulnerability. Include:

- The affected version/commit.
- Steps to reproduce (a minimal `basepod.yaml`/`Containerfile` or request
  sequence, where applicable).
- The impact you believe it has (e.g. privilege escalation, data
  exposure, denial of service).

We'll acknowledge new reports and aim to follow up with a remediation
timeline once triaged. As a small, single-maintainer-team project there's
no formal SLA yet, but security reports are prioritized over feature work.

## Scope & Threat Model

BasePod's control plane is designed to be operated by **one trusted
administrator per instance** — there is no team/RBAC model yet (see
[`docs/plan/08.teams-and-rbac.md`](docs/plan/08.teams-and-rbac.md) for
the planned direction), so anyone holding a valid session token has full
control over every app on the box. Protecting that session token (and
the admin credential that mints it) is the primary security boundary
today.

Key design choices this threat model relies on:

- **Rootless Podman.** BasePod never requires root, and containers it
  manages run without elevated host privileges — a compromised app
  container should not translate into host compromise via a privileged
  container runtime.
- **Single-admin auth.** The REST API (`/api/v1`) requires a bearer
  session token for every route except login; there is currently one
  administrator account per instance (created via `basepod setup`).
- **Dashboard exposure model.** The web dashboard is reachable either via
  a loopback-only HTTP listener (`cfg.Listen`, default
  `127.0.0.1:<port>`) or, if configured, a hostname routed through Caddy
  to the control plane's dashboard API over a Unix socket that only the
  `bp-caddy` container can reach — never a raw TCP listener on the shared
  app network, which every app container could otherwise probe.
- **App isolation via the container runtime.** Apps are isolated from
  each other and from the control plane by Podman's own container
  boundary, not by anything BasePod adds on top; a container escape is
  out of this project's scope to defend against (that's Podman's/the
  kernel's job).
- **Build contexts are untrusted input.** An uploaded tarball build
  context runs arbitrary `RUN` steps during `podman build`; BasePod's job
  is to bound the *blast radius* of a hostile or buggy Containerfile
  (disk use, log size — see the build-log cap below) rather than to
  sandbox the build itself beyond what Podman already provides.

Out of scope: attacks that require the attacker already holding a valid
admin session token or shell access to the host, and vulnerabilities in
Podman, Caddy, or the Go standard library itself (please report those
upstream — we track and pull in fixes via `govulncheck` in CI and the
Go toolchain/dependency bump process, but the vulnerability itself isn't
ours to fix).

## Known Limitations

BasePod has an internal security audit process that tracks findings by
severity; the items below were still open as of this writing (v0.4 is
the release actively closing out most of them — see
[`docs/superpowers/plans/2026-08-06-v0.4-security-and-zero-config.md`](docs/superpowers/plans/2026-08-06-v0.4-security-and-zero-config.md)
for the in-progress remediation plan). None of these require anything
beyond a valid admin session to exploit further than that session
already grants, but they're worth knowing about:

- **Container resource limits.** Deployed containers may not yet have
  memory/CPU/PID limits enforced by default on every release — a runaway
  app process can still exhaust host resources. Track the resource-limit
  work in the plan doc above.
- **Per-client rate limiting behind the reverse proxy.** Login
  rate-limiting keys off the connecting socket's address; behind Caddy,
  every request arrives from the same upstream hop unless
  `X-Forwarded-For` handling is fully wired up and trusted only on the
  listener Caddy actually connects through.
- **Stream authentication.** Build-log and container-log SSE streams may
  still accept the same long-lived session token used everywhere else,
  rather than a short-lived, single-purpose stream token — a leaked log
  URL (e.g. in a proxy access log) is more sensitive than it needs to be
  until that lands.
- **Security response headers.** Baseline hardening headers (CSP,
  `X-Content-Type-Options`, `X-Frame-Options`, HSTS on the dashboard
  route) may not be applied uniformly across every response yet.
- **Listen-address guard.** `listen` in the config file currently accepts
  any bind address without an explicit opt-in guard against
  accidentally binding a non-loopback interface directly (bypassing the
  Caddy-fronted dashboard route entirely); until that guard lands,
  operators should double-check `listen` stays loopback-only unless they
  specifically intend otherwise.
- **npm dev-dependency advisory.** `web`'s `esbuild` dev dependency has a
  known low-severity advisory (arbitrary file read via the *development*
  server only — [GHSA-g7r4-m6w7-qqqr](https://github.com/advisories/GHSA-g7r4-m6w7-qqqr));
  it does not affect the built dashboard bundle BasePod actually ships,
  and CI's `npm audit --audit-level=high` deliberately doesn't fail the
  build over it. Track the fixed `esbuild`/Vite line and take it once a
  non-breaking upgrade is available.
- **Miscellaneous low-severity items.** A handful of smaller hardening
  items — SSE stream count caps, custom-domain count/length caps, login
  timing against an unknown email, resolving `podman` via a configurable
  absolute path, and binding environment-variable encryption to the
  owning app's ID — are tracked in the plan doc above and may not all be
  closed out yet depending on when you're reading this.

This list is best-effort and updated as fixes land; it is not a
substitute for checking release notes or the plan doc for the current
state of any specific item.
