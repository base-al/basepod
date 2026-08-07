// A per-app rolling window of recent CPU-percent samples plus the most
// recent full sample — Apps.vue's landing spot for `event: stats`
// messages off the batch-stats stream (GET /api/v1/stats, see
// lib/sse.ts's connect() and lib/api.ts's AllStatsSample for the wire
// shape). Kept as plain, framework-free functions over a plain object
// (not a class, not a Vue reactive() wrapper) so the rolling-window logic
// is covered by an ordinary vitest unit test — this repo has no
// component-rendering test harness (see lib/sparkline.ts's identical
// reasoning). Apps.vue owns wrapping the result in ref()/reactive()
// itself.

import type { AllStatsSample } from './api'

/** How many recent cpu_percent samples each app's sparkline keeps. At
 * podman's own ~5s sampling interval (not a rate this client controls —
 * see internal/api/allstats.go) that's roughly 100s of recent history:
 * enough to show a meaningful trend without the SVG polyline getting so
 * dense it stops reading as a shape. */
export const SPARKLINE_WINDOW = 20

export interface AppStatsEntry {
  /** Oldest-first rolling window of recent cpu_percent readings, capped
   * at SPARKLINE_WINDOW — see pushStatsSample. */
  cpuHistory: number[]
  /** The most recently received full sample for this app, or null before
   * the first one has arrived (the "seed" state — see Apps.vue and
   * CpuSparkline.vue, both of which treat an empty/null state as "no data
   * yet", not zero). */
  latest: AllStatsSample | null
}

export type AppStatsBySlug = Record<string, AppStatsEntry>

/** The entry every app starts with before any SSE data has arrived for
 * it — an empty history (sparklinePoints' own <2-samples case renders
 * this as a flat midline, not a misleading zero) and no "latest" numbers
 * to show. */
export function emptyStatsEntry(): AppStatsEntry {
  return { cpuHistory: [], latest: null }
}

/**
 * Returns a NEW AppStatsBySlug with `sample`'s app entry updated: its
 * cpu_percent pushed onto that app's rolling window (capped at
 * SPARKLINE_WINDOW, dropping the oldest reading once full) and `latest`
 * replaced with the full sample. An app slug not yet present in `bySlug`
 * gets a fresh entry (see emptyStatsEntry) rather than being dropped —
 * the batch stream can start emitting for an app the client hasn't seen
 * a sample for yet at any point in its connection lifetime (e.g. it just
 * finished deploying).
 *
 * Immutable: returns a new top-level object and a new per-app entry
 * rather than mutating bySlug or its nested arrays in place, so a plain
 * `!==` check (or Vue's own reactive-object diffing) sees a real change.
 */
export function pushStatsSample(bySlug: AppStatsBySlug, sample: AllStatsSample): AppStatsBySlug {
  const prev = bySlug[sample.slug] ?? emptyStatsEntry()
  const cpuHistory = [...prev.cpuHistory, sample.cpu_percent].slice(-SPARKLINE_WINDOW)
  return { ...bySlug, [sample.slug]: { cpuHistory, latest: sample } }
}

/** Formats a live sample's cpu_percent for display (e.g. "12%", "104%"
 * for a container busy across more than one core — see AllStatsSample's
 * doc comment on the 0-100-per-core scale). Rounded to the nearest whole
 * percent: sub-percent precision isn't meaningful at a ~5s sampling
 * interval and would just make the apps-list column noisier. */
export function formatCpuPercent(cpuPercent: number): string {
  return `${Math.round(cpuPercent)}%`
}

/** Formats a live sample's mem_used_bytes against an app's CONFIGURED
 * limit (memory_limit_mb — see lib/api.ts's App interface, the same
 * field formatLimits.ts's formatMemoryLimit renders elsewhere in this
 * UI), e.g. "41 / 512 MB". Deliberately NOT the live sample's own
 * mem_limit_bytes: an unlimited app's live limit reads as the host's
 * total memory (cgroups' own "no limit" convention) — an ever-different,
 * host-specific number that would read as a real ceiling when it isn't
 * one, right next to "Unlimited" everywhere else this UI already shows
 * for the same app. limitMb of 0 (this app's own "no limit" convention)
 * renders as just the used amount, with no misleading "/ 0 MB". */
export function formatMemUsedLive(usedBytes: number, limitMb: number): string {
  const usedMb = Math.round(usedBytes / (1024 * 1024))
  if (limitMb === 0) return `${usedMb} MB`
  return `${usedMb} / ${limitMb} MB`
}
