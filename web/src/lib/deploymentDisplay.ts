// Pure display/eligibility helpers for DeploymentList.vue, extracted so
// the truncation and rollback-eligibility rules are unit-testable without
// mounting the component (and so the mobile-card layout added for issue
// #14 shares the exact same rules the desktop table already used — one
// source of truth for what "long" means and who's eligible to roll back
// to, not two copies that can drift).

import type { Deployment } from './api'

/** Deployment errors can be long stack-trace-ish strings; truncate by
 * default and let the caller expand it in place on click/tap. Matches
 * the length DeploymentList used before it grew a mobile layout. */
export const ERROR_TRUNCATE_AT = 72

export function isErrorTruncated(error: string): boolean {
  return error.length > ERROR_TRUNCATE_AT
}

/** The text to actually render for a deployment's error: the full string
 * once expanded (or if it was never long enough to need truncating),
 * otherwise a truncated-with-ellipsis preview. */
export function displayError(error: string, expanded: boolean): string {
  if (!isErrorTruncated(error) || expanded) return error
  return `${error.slice(0, ERROR_TRUNCATE_AT)}…`
}

/** The single healthy deployment currently serving traffic, given a list
 * already sorted newest-first (as DeploymentList's `rows` is) — the
 * first healthy entry. null if none is healthy (e.g. every deployment
 * failed, or the app has never deployed). */
export function currentHealthyNumber(sortedDeployments: Deployment[]): number | null {
  return sortedDeployments.find((d) => d.status === 'healthy')?.number ?? null
}

/** Whether `deployment` should offer a "Roll back" action: it must itself
 * have deployed healthily, and it must not already be the currently-live
 * deployment (rolling back to the deployment that's already running
 * would just redeploy the same image as a no-op). */
export function canRollBackTo(deployment: Deployment, currentHealthy: number | null): boolean {
  return deployment.status === 'healthy' && deployment.number !== currentHealthy
}
