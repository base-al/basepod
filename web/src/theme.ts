/**
 * BasePod visual system.
 *
 * BasePod is the anti-SaaS: a self-hosted PaaS you run on your own VPS —
 * rootless Podman instead of a privileged Docker daemon, Caddy for
 * automatic HTTPS, one Go binary with SQLite embedded. No vendor, no
 * cloud console, your server. The audience deliberately chose not to use
 * Heroku/Vercel. The visual language answers to that: owned, precise,
 * slightly hand-built — closer to a well-made CLI or terminal UI than to
 * a generic admin template.
 *
 * This system follows the user-approved concept at
 * docs/design/identity-concept.html (commit 505c382) — copper on warm
 * graphite, deliberately not another neon-green-on-black self-hosting
 * tool, mono-led type, a mark derived from the existing wordmark's own
 * letterforms. This file is the single source of truth for the
 * *meaning* behind the tokens; src/assets/css/main.css is where they
 * become actual CSS variables and Tailwind theme entries (colors,
 * radii, fonts, motion). Keep this module free of Vue/DOM imports — it's
 * read by both runtime code and vite.config.ts (build-time Nuxt UI color
 * wiring).
 *
 * ── Palette rationale ────────────────────────────────────────────────
 * Five hand-tuned ramps, matching the approved concept's own token
 * names (in parens):
 *
 *   ink (ground/raised/sunken/ink/ink-2/ink-3)  neutral graphite with a
 *                                 deliberate WARM bias — not the cool
 *                                 blue-gray every "terminal" self-hosting
 *                                 tool defaults to.
 *   copper (copper/copper-soft)   the brand accent. Burnt-orange: metal,
 *                                 hardware you own, sitting in a rack
 *                                 you chose — not the blue/green/purple
 *                                 every other dev-tool dashboard reaches
 *                                 for. Kept out of the status vocabulary
 *                                 below so "this is a BasePod action" (a
 *                                 button, a link, the mark) never gets
 *                                 confused with "this app is running".
 *   moss (steady)                 running / success.
 *   ember (moving)                 deploying / in-progress.
 *   crimson (broken)               error.
 *
 * Every text/background pairing actually used in the app was measured
 * against the WCAG relative-luminance formula and hits ≥4.5:1 (AA,
 * normal text); non-text UI boundaries (input/button borders) hit
 * ≥3:1 (WCAG 1.4.11). Several of the concept's exact hex values were
 * nudged (documented inline in main.css) where the original fell just
 * short of 4.5:1 for its actual use as text — the token moved, not the
 * rule. See design-identity-report.md for the full before/after table.
 * Purely decorative dividers (table row rules) are intentionally
 * low-contrast — spacing and hover states carry that boundary too, so
 * they're exempt from the non-text minimum by design, not oversight.
 *
 * ── Type ─────────────────────────────────────────────────────────────
 * Mono-led: JetBrains Mono for headings, labels, and every technical
 * value (slugs, image refs, hostnames, ports, tokens, deployment
 * numbers — numerics get `tabular-nums` so columns line up). Running
 * prose (descriptions, help text) stays in the platform's own
 * system-sans stack — no webfont needed for it at all. That leaves
 * exactly one face to self-host rather than two: JetBrains Mono is
 * OFL-1.1 and bundled by Vite, so it's served same-origin. The
 * dashboard runs under `default-src 'self'`; a CDN font (Google Fonts,
 * etc.) would silently 404 under that policy. Only the Latin subset of
 * each weight is imported (see main.css) — the product has no i18n
 * today.
 *
 * ── Spacing ──────────────────────────────────────────────────────────
 * Tailwind's stock 4px-based spacing scale is kept as-is (it's already
 * a deliberate, well-tested rhythm) rather than replaced outright. The
 * app's own convention within it: 8–12px for in-row gaps (icon-to-
 * label, badge padding), 16px for card padding and stacked-field gaps,
 * 24px for the space between distinct sections of a page, 32–48px for
 * page-level margins.
 *
 * ── Radii ────────────────────────────────────────────────────────────
 * Small and squared-off (3/4/6/8px, see main.css) — matching the
 * concept's own console mock, and the wordmark's crisp geometric
 * character better than a soft, pill-happy admin-template radius would.
 *
 * ── Motion ───────────────────────────────────────────────────────────
 * Three durations — fast 120ms (hover/active feedback), base 180ms
 * (color/background transitions), slow 280ms (anything that changes
 * layout: tab panels, drawers) — applied as bare Tailwind numerics
 * (`duration-120` etc.) at each call site, since v4 has no named-theme
 * namespace for duration the way it does for color/radius/easing. Two
 * easings *are* real theme tokens (`ease-out-snap` / `ease-in-soft`,
 * see main.css): a "settles without overshoot" curve for things
 * appearing (cubic-bezier(0.16,1,0.3,1)) and a plain fade for things
 * leaving. `prefers-reduced-motion: reduce` collapses all of it to
 * near-zero globally (see main.css) — including the deploying-status
 * pulse.
 */

/** Semantic color -> Tailwind color name mapping handed to the @nuxt/ui
 * Vite plugin. `primary` is the brand accent (copper) — kept distinct
 * from `success` (moss) so a primary action button and a "running"
 * badge never share a hue. */
export const uiColors = {
  primary: 'copper',
  secondary: 'ink',
  success: 'moss',
  warning: 'ember',
  error: 'crimson',
  info: 'copper',
  neutral: 'ink',
} as const

/** ui-monospace stack used for headings, labels, and every technical
 * value: slugs, image refs, hostnames, ports, tokens. JetBrains Mono
 * first (self-hosted, see main.css), falling back to the platform's
 * own monospace stack if the webfont hasn't loaded yet. */
export const fontMono =
  "'JetBrains Mono', ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace"

/** Running-prose stack — system sans only, no self-hosted face. See the
 * "Type" section above for why the concept deliberately needs none. */
export const fontSans =
  "-apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif"

export type AppStatus = 'created' | 'deploying' | 'running' | 'stopped' | 'error'

/** A single deployment's own status, distinct from (and narrower than)
 * AppStatus — see internal/deploy/deploy.go: a deployment lands in
 * "deploying" -> "healthy" (success) or "failed" (error), while the
 * *app's* status separately tracks "running"/"error" based on what's
 * actually alive after that deploy resolves. */
export type DeploymentStatus = 'deploying' | 'healthy' | 'failed'

/** Union covering every status string StatusBadge might be asked to
 * render, whether it came from an App or a Deployment. */
export type Status = AppStatus | DeploymentStatus

/** Badge styling per status, per the design language's color semantics.
 * Keyed by the union above — AppStatus and DeploymentStatus share the
 * "deploying" member, so it appears once. Dot classes reference the
 * custom moss/ember/crimson/ink ramps (see main.css's @theme block),
 * not Tailwind's stock emerald/amber/red/slate. */
export const statusStyles: Record<
  Status,
  { label: string; color: 'neutral' | 'warning' | 'success' | 'error'; dotClass: string; pulse: boolean }
> = {
  created: { label: 'Created', color: 'neutral', dotClass: 'bg-ink-300', pulse: false },
  deploying: { label: 'Deploying', color: 'warning', dotClass: 'bg-ember-400', pulse: true },
  running: { label: 'Running', color: 'success', dotClass: 'bg-moss-400', pulse: false },
  stopped: { label: 'Stopped', color: 'neutral', dotClass: 'bg-ink-300', pulse: false },
  error: { label: 'Error', color: 'error', dotClass: 'bg-crimson-400', pulse: false },
  healthy: { label: 'Healthy', color: 'success', dotClass: 'bg-moss-400', pulse: false },
  failed: { label: 'Failed', color: 'error', dotClass: 'bg-crimson-400', pulse: false },
}

/** v0.5's GitDeliveryStatus (see lib/api.ts) — every reason a webhook push
 * did or didn't trigger a deploy. Styled separately from `statusStyles`
 * above rather than folded into the same union: a delivery's status is
 * about "what a push resulted in", not "is this thing currently running",
 * and several of its members (ignored_branch, coalesced) have no
 * equivalent app/deployment state at all. */
export const gitDeliveryStatusStyles: Record<
  | 'deployed'
  | 'ignored_branch'
  | 'ignored_event'
  | 'invalid_signature'
  | 'rate_limited'
  | 'coalesced'
  | 'error',
  { label: string; color: 'neutral' | 'warning' | 'success' | 'error'; dotClass: string; pulse: boolean }
> = {
  deployed: { label: 'Deployed', color: 'success', dotClass: 'bg-moss-400', pulse: false },
  ignored_branch: { label: 'Ignored — wrong branch', color: 'neutral', dotClass: 'bg-ink-300', pulse: false },
  ignored_event: { label: 'Ignored — not a push', color: 'neutral', dotClass: 'bg-ink-300', pulse: false },
  invalid_signature: { label: 'Rejected — bad signature', color: 'error', dotClass: 'bg-crimson-400', pulse: false },
  rate_limited: { label: 'Rate limited', color: 'warning', dotClass: 'bg-ember-400', pulse: false },
  coalesced: { label: 'Queued', color: 'warning', dotClass: 'bg-ember-400', pulse: true },
  error: { label: 'Error', color: 'error', dotClass: 'bg-crimson-400', pulse: false },
}

/** localStorage keys used across the app — centralized to avoid typos/drift. */
export const storageKeys = {
  token: 'basepod.token',
  colorMode: 'basepod.color-mode',
} as const
