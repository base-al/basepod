# BasePod IA reorg — app detail page + settings

Branch: `feat/ia-reorg` (off `origin/main`). Scope: `web/**` only — no Go
touched. Palette/identity (theme.ts, main.css) untouched, per the user's
explicit feedback that the visual system is approved and this is an IA
problem, not a restyle.

## Status

Done. `npm run build`, `npm run type-check`, and `npm test` all pass
clean; `web/dist` served statically with mocked API fixtures and clicked
through every new route/section in a real (Chrome via Playwright)
browser at 390×844 and 768×1024, both themes — zero console/page errors.
Pushed; CI result recorded below once the run lands.

## The new IA

```
/apps/:slug                              AppDetail — 4 top-level sections
├── Overview (default)                   state + primary actions, no click required
│   ├── Facts card                       status, public URL, image, port, internal hostname, deployment count
│   ├── Live resources card              live CPU sparkline + %, memory used/limit (new — see below)
│   ├── Recent deployments card          last 3, status + relative time, "View all" → Deployments
│   └── Quick actions card               Deploy, Deploy new image…
├── Deployments                          full history, rollback, build-log drawers (DeploymentList — unchanged)
├── Logs                                 live container log stream, given more vertical room (LogViewer — unchanged internals)
└── Configuration                        accordion, one section open at a time
    ├── Environment                      EnvEditor — unchanged
    ├── Domains                          DomainsPanel — unchanged
    ├── Git                              GitPanel — unchanged
    ├── Resource limits                  ResourceLimitsPanel — unchanged
    └── Danger zone                      Delete app (consolidated — see below)

/settings                                redirects → /settings/account
/settings/account                        ChangePasswordForm + SessionsPanel — unchanged, just moved
/settings/instance                       NEW — version, podman health, dashboard/root domain, backup guidance
/settings/users                          NEW — honest "coming soon" placeholder, no fake rows
```

Grouping rationale, one line each:

- **Overview is default and self-sufficient** — status, live resources, URL,
  recent outcomes, and the deploy actions all sit above the fold with zero
  clicks, because "is this healthy, what's running, what changed last" is
  the single most common reason to open this page.
- **Deployments stays its own top-level tab, not folded into Overview or
  Configuration** — it's an audit/rollback surface with real depth (a
  table, build-log drawers, a distinct action), not a glance; Overview's
  own mini-list covers the glance case and links out to it.
- **Logs stays its own top-level tab** — it's the thing reached for in an
  emergency, and doesn't belong buried a click deeper than Deployments.
  Its pane now scales with the viewport (`clamp(32rem, 100dvh-18rem,
  56rem)` — floor pinned to the old fixed height so short screens never
  get *less* room, ceiling so it doesn't balloon) instead of a flat
  512px box.
- **Environment/Domains/Git/Resource limits/Danger zone are one
  "Configuration" family** — none of them is something an operator reaches
  for while checking state or firefighting; they're all "set this app up"
  surfaces. Grouped behind a single accordion (not four more flat tabs)
  so they stop competing for the same visual weight as Overview/
  Deployments/Logs.
- **Danger zone (delete app) is consolidated to exactly one place** — the
  pre-reorg page had it twice (Overview's quick actions *and* the old
  Settings tab). It now lives once, inside Configuration, where every
  other irreversible/structural action already sits.
- **Settings becomes Account/Instance/Users, each a real route** — Account
  is unchanged content; Instance is new (server facts, not operator
  facts — version, health, domains, backup); Users is a placeholder for
  the concurrent backend work, honestly labeled, no fake data.

## Why an accordion, not a second tab strip, for Configuration

The non-negotiable was explicit: don't nest a scrolling tab strip inside
another one. AppDetail's outer 4 items already form one horizontally-
scrolling strip on phone. A second-level tab strip for Configuration's 5
items would have nested exactly that. An accordion (Nuxt UI's
`UAccordion`, `type="single"`) sidesteps the problem by construction — it's
a vertical disclosure list, never horizontally scrolling, and reads
identically on desktop and phone. `?tab=` and `&section=` mirror the
current top-level tab / open accordion section in the URL (the same
one-shot-query-param pattern `?buildLog=` already used on this page), so
e.g. `/apps/hello?tab=configuration&section=git` is a real, shareable,
back-button-safe link — verified by scripted clicks through every
section in a live browser.

## New capability: live CPU/memory on Overview

`GET /apps/{slug}/stats` (`internal/api/stats.go`'s `handleAppStats`) has
existed server-side since v0.5 but no page consumed it. Overview's Live
resources card is the first consumer: it reuses `lib/statsBuffer.ts`'s
exact rolling-window functions (the same ones Apps.vue's batch-stats
sparklines use for the whole list) rather than duplicating that logic,
and `CpuSparkline.vue` unchanged. The one frontend-only addition needed
was `'stats'` on `StreamTokenRequest.scope` in `lib/api.ts` (the server
already accepted that scope; nothing there was a Go change) plus a new
`AppStatsSample` type (`Omit<AllStatsSample, 'slug'>`, matching the
per-app payload's actual shape — it has no `slug` since the route is
already scoped by URL). The stream is only opened while the app's status
is `running` (derived from data this page already polls) rather than
adding a second preflight endpoint — an app that isn't running shows an
honest "No live stats — app isn't running" instead of a stuck spinner.

## Settings → Instance: what's real vs. honestly deferred

- **Version, Podman health** — real, from `GET /system`, sharing
  AppShell's own `['system']` query key (zero extra requests).
- **Dashboard domain** — real: `window.location.host`, i.e. whatever
  hostname actually served this page.
- **Backup guidance** — static text matching README's own Backup section
  verbatim (the three files: `basepod.db`, `secret.key`, `caddy-data/`).
- **Root domain, Caddy-specific health** — the API has no endpoint for
  either (`GET /system` only returns version/podman/apps; root_domain is
  a store setting only ever surfaced per-app via
  `GET /apps/{slug}/domains`). Rather than fake a single value, the page
  says so plainly and points at where root domain *is* visible (any
  app's Domains section) and explains Podman's status is the closest
  live signal for Caddy today. No Go touched to add either — out of
  scope for this branch.

## Heavily-tested components

DeploymentList, BuildLogPanel, EnvEditor, GitPanel, DomainsPanel,
ResourceLimitsPanel, SessionsPanel, ChangePasswordForm: moved to their
new home (Configuration's accordion or Settings/Account), zero internal
changes — same props, same emits, same internal logic. LogViewer got one
intentional reframe (the height clamp above). ComposePreview is
NewApp.vue's own compose-preview UI, not part of either page in scope
here (app detail / settings) — untouched.

This repo has no component-render test harness — only `src/lib/*.test.ts`
(11 files) exist, and none of them touch a `.vue` file — so the 89 tests
were never at risk from moving where a component is *used*; they cover
pure functions (`envparse`, `gitFormat`, `sparkline`, `statsBuffer`,
`sse`, etc.) that weren't touched at all. Verified before and after:
89/89 both times.

## Mobile at 390px, both themes

**App detail — Overview**: back-link/breadcrumb row, then a single
horizontally-scrollable 4-item tab strip (all four fit without scrolling
at 390px in practice), then stacked cards — Facts (2-col grid), Live
resources (sparkline + cpu%/mem on one row), Recent deployments (3 rows,
status badge + relative time), Quick actions (Deploy / Deploy new
image…). Every card is full-width, `.tap44` on every interactive
element, safe-area-respecting bottom tab bar. Dark: warm graphite
ground, copper accent on the URL/links, moss "Running" dot. Light: warm
paper ground, same copper (contrast-nudged per theme.ts), identical
layout — no light-mode-specific breakage observed.

**App detail — Configuration**: the same tab strip, then a single-column
accordion — Environment / Domains / Git / Resource limits / Danger zone
— one open at a time, each a plain vertical disclosure (no horizontal
scroll anywhere on this tab). Confirmed by scripted clicks that every
section opens, updates `&section=`, and renders without error; Danger
zone's "Delete app" card carries the same red-ring/red-heading treatment
the pre-reorg page used.

**App detail — Logs**: controls row (connection chip, Follow, tail-length
select, Pause/Clear/Download) above a pane that now fills most of the
remaining viewport height instead of a flat 512px box — visibly taller
in the 390×844 screenshot than the pre-reorg fixed height, and grows
further at 768px+ where AppShell switches to the desktop rail layout.

**Settings**: `/settings/account`, `/settings/instance`, `/settings/users`
all real routes. At 390px the sub-nav is a wrapping row of three pills
(Account/Instance/Users) — never a second scroll strip — with the active
pill filled copper/accent. At 768px it becomes a narrow left column
inside the content area, alongside AppShell's own desktop rail. Instance
page's three cards (Version & runtime, Domains, Backup) stack cleanly at
390px; Users' empty state matches Apps.vue's own "no apps yet" dashed-
border convention.

`focus-visible` rings (`outline-2 outline-offset-2 outline-accent`) are
applied identically to every new nav element (SettingsShell's two nav
variants, the Configuration accordion's `.tap44` trigger) — same classes
already used by AppShell's own nav links, not reinvented.

## Deep links

- `?buildLog=<n>` — unchanged: still consumed on mount, still switches to
  Deployments and auto-expands that build's log drawer, still strips the
  query afterward.
- `/settings` (the old page's own URL) — now `redirect: { name:
  'settings-account' }` in the router, so anything that bookmarked or
  linked the pre-reorg URL lands on equivalent content instead of a
  broken/blank route.
- New: `?tab=` / `&section=` on `/apps/:slug` reflect the current
  section, verified round-trip (navigate with the query present → correct
  tab/accordion section opens on load) and forward (click through →
  query updates to match) in the scripted browser check.

## Concerns / things I did not do

- **No live backend was exercised.** Verification used a static server
  (`web/dist`) with Playwright driving a real Chrome against *mocked*
  `/api/v1/*` responses (crafted fixtures for `getApp`, domains, env,
  git-not-connected, system, sessions) — this proves the bundle loads,
  routes resolve, and every new surface renders without a JS exception in
  both themes at two widths, but it is not the same as a real BasePod
  server + Podman. CI's own `e2e` job (`scripts/e2e-local.sh`) drives the
  real API and dashboard shell over curl, but only asserts on
  `<div id="app">` and a hashed `/assets/*` path — it has no assertions
  tied to specific tab labels or routes, so it should be unaffected by
  this reorg, but I have not personally watched it pass against this
  branch's actual binary.
- **DeploymentList's table overflows narrow phone width** (the rightmost
  "Status" column extends past 390px in the mobile screenshot) — this is
  pre-existing behavior in a component I deliberately left untouched
  (moved, not rewritten, per the brief); flagging it as an existing gap
  rather than silently leaving it out of the report.
- **A pre-existing `UInput` rendering quirk** (password/text inputs in
  `ChangePasswordForm` and `EnvEditor` render with a plain light
  background even in dark theme) is visible in the screenshots and is
  **not** something this branch introduced — identical in the untouched
  `ChangePasswordForm`, just relocated. Worth a follow-up, out of scope
  here.
- Root domain and a dedicated Caddy health signal are honestly deferred
  on the new Instance page (see above) rather than faked — closing that
  gap needs a small Go-side addition (e.g. exposing `root_domain` on
  `GET /system`) that's out of scope for a web-only branch.
- `web/src/pages/Settings.vue` was removed (`git rm`) in favor of the
  three `web/src/pages/settings/*.vue` pages — fully recoverable from git
  history on this branch; noting it explicitly since deleting a tracked
  file is worth calling out even when it's a normal, git-safe refactor.
