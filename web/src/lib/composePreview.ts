// Pure shaping helpers for ComposePreview.vue's dry-run plan display —
// kept dependency-free so the (admittedly small) per-service formatting
// logic is unit-testable without mounting the component.
import type { ComposePlan, ComposeService } from './api'

/** How a service's routing shows up in its preview card: "internal" (no
 * Caddy route, no public URL — the compose semantics for a service with
 * no `expose:`) or "port <n>" for a routed one. */
export function servicePortLabel(service: ComposeService): string {
  return service.internal ? 'internal' : `port ${service.port}`
}

/** Human label for a service's plan action — "create" for a fresh app,
 * "update" for a re-apply against an existing (project, service). Falls
 * back to the raw string for forward-compat with an action value this
 * build doesn't know about yet, rather than rendering nothing. */
export function serviceActionLabel(action: string): string {
  switch (action) {
    case 'create':
      return 'Create'
    case 'update':
      return 'Update'
    default:
      return action
  }
}

/** Total warning count across a plan: its own top-level warnings (parser/
 * plan-level, not tied to one service) plus every service's own —
 * ComposePreview.vue uses this to decide whether to render the "N
 * warnings" summary chip at all. */
export function planWarningCount(plan: ComposePlan): number {
  const serviceWarnings = plan.services.reduce((sum, s) => sum + (s.warnings?.length ?? 0), 0)
  return (plan.warnings?.length ?? 0) + serviceWarnings
}

/** True when applying this plan would leave the project with orphaned
 * apps — services that existed from a prior apply but have no
 * corresponding entry in the file being applied now. BasePod never
 * deletes them automatically (see internal/api/compose.go's package doc
 * comment) — ComposePreview.vue uses this to decide whether to render
 * the "still running, delete explicitly" callout. */
export function hasOrphans(plan: ComposePlan): boolean {
  return (plan.orphans?.length ?? 0) > 0
}
