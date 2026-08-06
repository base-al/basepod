// Formats an app's container resource limits (memory_limit_mb, cpu_limit
// — see lib/api.ts's App interface) for display in the apps list and
// anywhere else that needs a compact read-only summary. 0 means
// unlimited on the wire (see ResourceLimitsPanel.vue's own doc comment
// on this exact convention) — shown as the word "Unlimited" rather than
// a bare "0", which would read as "no memory at all".

export function formatMemoryLimit(memoryMb: number): string {
  if (memoryMb === 0) return 'Unlimited'
  if (memoryMb >= 1024 && memoryMb % 1024 === 0) return `${memoryMb / 1024} GiB`
  return `${memoryMb} MiB`
}

export function formatCpuLimit(cpuLimit: number): string {
  if (cpuLimit === 0) return 'Unlimited'
  return `${cpuLimit} vCPU`
}

/** Combined "512 MiB · 1 vCPU" summary for a dense single-column display
 * (the apps list). Falls back to just "Unlimited" once, not twice, when
 * both limits are unset. */
export function formatLimitsSummary(memoryMb: number, cpuLimit: number): string {
  if (memoryMb === 0 && cpuLimit === 0) return 'Unlimited'
  return `${formatMemoryLimit(memoryMb)} · ${formatCpuLimit(cpuLimit)}`
}
