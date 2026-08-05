// Tiny relative-time formatter for deployment timestamps
// (internal/api/apps.go's deploymentResponse.started_at/finished_at, both
// RFC3339 strings). No dayjs/date-fns dependency — this is the one place
// in the dashboard that needs it, and the rules are simple enough to keep
// inline.

/** Formats an RFC3339 timestamp as a short "N <unit> ago" string relative
 * to now. Returns "" for an empty/unparseable input so callers can render
 * nothing rather than a bogus date (matches the "no data, don't fabricate
 * it" convention used elsewhere in this file's callers). */
export function relativeTime(iso: string): string {
  if (!iso) return ''
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''

  const deltaSeconds = Math.round((Date.now() - then) / 1000)
  if (deltaSeconds < 5) return 'just now'

  const units: [string, number][] = [
    ['year', 31536000],
    ['month', 2592000],
    ['week', 604800],
    ['day', 86400],
    ['hour', 3600],
    ['minute', 60],
    ['second', 1],
  ]

  const future = deltaSeconds < 0
  const abs = Math.abs(deltaSeconds)
  for (const [unit, secondsPerUnit] of units) {
    const value = Math.floor(abs / secondsPerUnit)
    if (value >= 1) {
      const plural = value === 1 ? unit : `${unit}s`
      return future ? `in ${value} ${plural}` : `${value} ${plural} ago`
    }
  }
  return 'just now'
}
