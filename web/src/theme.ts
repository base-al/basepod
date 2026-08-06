/**
 * BasePod visual system.
 *
 * Design language (binding, see plan's Global Constraints): a dense-but-calm
 * ops dashboard. Dark mode is the default with a light toggle; surfaces are
 * neutral slate; emerald is the accent for healthy states and primary
 * actions; amber flags in-progress/deploying states; red flags errors.
 * Slugs, image refs, and hostnames render in a monospace stack. No
 * drop-shadow "card wall" look, no default component-library template feel.
 *
 * This module is imported from both vite.config.ts (to configure the
 * @nuxt/ui Vite plugin's semantic color mapping at build time) and from
 * runtime code (status badge styling, shared constants) — keep it free of
 * Vue/DOM imports so it stays usable in both contexts.
 */

/** Semantic color -> Tailwind color name mapping handed to the @nuxt/ui Vite plugin. */
export const uiColors = {
  primary: 'emerald',
  secondary: 'slate',
  success: 'emerald',
  warning: 'amber',
  error: 'red',
  info: 'sky',
  neutral: 'slate',
} as const

/** ui-monospace stack used for slugs, image refs, hostnames, and tokens. */
export const fontMono =
  "ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace"

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
 * "deploying" member, so it appears once. */
export const statusStyles: Record<
  Status,
  { label: string; color: 'neutral' | 'warning' | 'success' | 'error'; dotClass: string; pulse: boolean }
> = {
  created: { label: 'Created', color: 'neutral', dotClass: 'bg-slate-400', pulse: false },
  deploying: { label: 'Deploying', color: 'warning', dotClass: 'bg-amber-400', pulse: true },
  running: { label: 'Running', color: 'success', dotClass: 'bg-emerald-400', pulse: false },
  stopped: { label: 'Stopped', color: 'neutral', dotClass: 'bg-slate-400', pulse: false },
  error: { label: 'Error', color: 'error', dotClass: 'bg-red-400', pulse: false },
  healthy: { label: 'Healthy', color: 'success', dotClass: 'bg-emerald-400', pulse: false },
  failed: { label: 'Failed', color: 'error', dotClass: 'bg-red-400', pulse: false },
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
  deployed: { label: 'Deployed', color: 'success', dotClass: 'bg-emerald-400', pulse: false },
  ignored_branch: { label: 'Ignored — wrong branch', color: 'neutral', dotClass: 'bg-slate-400', pulse: false },
  ignored_event: { label: 'Ignored — not a push', color: 'neutral', dotClass: 'bg-slate-400', pulse: false },
  invalid_signature: { label: 'Rejected — bad signature', color: 'error', dotClass: 'bg-red-400', pulse: false },
  rate_limited: { label: 'Rate limited', color: 'warning', dotClass: 'bg-amber-400', pulse: false },
  coalesced: { label: 'Queued', color: 'warning', dotClass: 'bg-amber-400', pulse: true },
  error: { label: 'Error', color: 'error', dotClass: 'bg-red-400', pulse: false },
}

/** localStorage keys used across the app — centralized to avoid typos/drift. */
export const storageKeys = {
  token: 'basepod.token',
  colorMode: 'basepod.color-mode',
} as const
