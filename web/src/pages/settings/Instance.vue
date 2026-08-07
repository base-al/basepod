<script setup lang="ts">
// Settings -> Instance: the facts about THIS server, not this operator's
// own account — version, container-runtime health, Caddy's own health,
// the domains involved, and how to back the instance up. Every value
// here comes from GET /system (shared with AppShell's own health chip —
// same ['system'] query key, so this page causes no extra request), now
// including root_domain, an honest dashboard_domain (never a
// configured-but-inactive hostname — see DASHBOARD_DOMAIN_SENTINELS),
// and a real caddy health signal (issue #16) — closing the gap the
// previous cut of this page left, where those two facts had no backing
// API and it said so plainly rather than fake a value.
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'

import { api, DASHBOARD_DOMAIN_SENTINELS } from '../../lib/api'
import { formatVersion } from '../../lib/version'
import SettingsShell from '../../components/SettingsShell.vue'

// Same key AppShell's health chip polls — sharing it means this page
// causes zero extra network traffic, and the two views can never disagree
// about the current podman/caddy status mid-poll-cycle.
const SYSTEM_POLL_INTERVAL_MS = 5000

const systemQuery = useQuery({
  queryKey: ['system'],
  queryFn: () => api.system(),
  refetchInterval: SYSTEM_POLL_INTERVAL_MS,
})

const podmanOk = computed(() => systemQuery.data.value?.podman === 'ok')

const caddyStatus = computed(() => systemQuery.data.value?.caddy)
const caddyOk = computed(() => caddyStatus.value === 'ok')
// "unknown" (no check wired server-side) is distinct from an actual
// failure: neither a green "ok" dot (that would be a lie) nor the same
// red treatment as a confirmed-broken Caddy (that would overstate what's
// known) is right for it — it gets its own muted/neutral rendering.
const caddyUnknown = computed(() => caddyStatus.value === undefined || caddyStatus.value === 'unknown')
const caddyDown = computed(() => !caddyOk.value && !caddyUnknown.value)

// The host this dashboard was actually loaded from — real, observed data,
// used only as a practical "here's how to reach it instead" hint when the
// dashboard route itself is disabled or unbound (see dashboardState
// below), never presented as if it WERE the configured dashboard domain.
const dashboardHost = computed(() => window.location.host)

// dashboardState classifies dashboard_domain's three possible shapes (see
// SystemInfo's doc comment in lib/api.ts): a live hostname ("active"), or
// one of the two non-hostname sentinels the API reports instead of ever
// echoing a configured-but-inactive value.
const dashboardState = computed<'active' | 'disabled' | 'unbound' | undefined>(() => {
  const domain = systemQuery.data.value?.dashboard_domain
  if (domain === undefined) return undefined
  if (domain === DASHBOARD_DOMAIN_SENTINELS.disabled) return 'disabled'
  if (domain === DASHBOARD_DOMAIN_SENTINELS.unbound) return 'unbound'
  return 'active'
})
</script>

<template>
  <SettingsShell>
    <div class="mb-6">
      <h2 class="font-mono text-lg font-semibold tracking-tight text-content-primary">Instance</h2>
      <p class="mt-1 text-sm text-content-secondary">This server: version, health, domains, and backup.</p>
    </div>

    <div class="flex flex-col gap-6">
      <UAlert
        v-if="systemQuery.isError.value"
        color="error"
        variant="subtle"
        title="Couldn't load instance info"
        description="Check that the BasePod server is running and reachable."
      />

      <!-- Caddy fronts every app's public URL — if it's down, every app is
           unreachable regardless of its own container health, so this gets
           its own prominent, impossible-to-miss alert rather than only the
           small status dot below. -->
      <UAlert
        v-if="caddyDown"
        color="error"
        variant="subtle"
        title="Caddy is unreachable"
        :description="`${caddyStatus} — every app's public URL is served through Caddy, so none of them are reachable right now even if their containers are otherwise fine.`"
      />

      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h3 class="text-sm font-medium text-content-secondary">Version &amp; runtime</h3>
        </template>

        <dl class="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-4">
          <div>
            <dt class="text-xs text-content-muted">BasePod version</dt>
            <dd class="mt-1 font-mono text-sm text-content-secondary">
              {{ systemQuery.data.value ? formatVersion(systemQuery.data.value.version) : '—' }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-content-muted">Podman</dt>
            <dd class="mt-1 flex items-center gap-1.5">
              <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="podmanOk ? 'bg-moss-400' : 'bg-crimson-400'" aria-hidden="true" />
              <span class="font-mono text-sm" :class="podmanOk ? 'text-content-secondary' : 'text-status-error'">
                {{ systemQuery.data.value?.podman ?? 'checking…' }}
              </span>
            </dd>
          </div>
          <div>
            <dt class="text-xs text-content-muted">Caddy</dt>
            <dd class="mt-1 flex items-center gap-1.5">
              <span
                class="h-1.5 w-1.5 shrink-0 rounded-full"
                :class="caddyUnknown ? 'bg-content-muted' : caddyOk ? 'bg-moss-400' : 'bg-crimson-400'"
                aria-hidden="true"
              />
              <span
                class="font-mono text-sm"
                :class="caddyUnknown ? 'text-content-muted' : caddyOk ? 'text-content-secondary' : 'text-status-error'"
              >
                {{ caddyStatus ?? 'checking…' }}
              </span>
            </dd>
          </div>
          <div>
            <dt class="text-xs text-content-muted">Apps deployed</dt>
            <dd class="mt-1 font-mono text-sm tabular-nums text-content-secondary">
              {{ systemQuery.data.value?.apps ?? '—' }}
            </dd>
          </div>
        </dl>

        <p class="mt-4 text-xs text-content-muted">
          Caddy's status above comes from asking it directly, over its admin API — not inferred from whether this
          page happened to load.
        </p>
      </UCard>

      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h3 class="text-sm font-medium text-content-secondary">Domains</h3>
        </template>

        <dl class="grid grid-cols-1 gap-x-6 gap-y-4 sm:grid-cols-2">
          <div class="min-w-0">
            <dt class="text-xs text-content-muted">Dashboard domain</dt>
            <dd class="mt-1 min-w-0">
              <span
                v-if="dashboardState === 'active'"
                class="block truncate font-mono text-sm text-content-secondary"
                :title="systemQuery.data.value?.dashboard_domain"
              >
                {{ systemQuery.data.value?.dashboard_domain }}
              </span>
              <span v-else-if="dashboardState === 'disabled'" class="text-sm text-content-muted">
                Disabled — the dashboard_domain setting is set to "off".
              </span>
              <span v-else-if="dashboardState === 'unbound'" class="text-sm text-content-muted">
                Configured, but not active on this platform (the dashboard's socket listener failed to bind at boot —
                expected on macOS). Reachable at
                <span class="font-mono text-content-secondary">{{ dashboardHost }}</span>
                directly instead.
              </span>
              <span v-else class="text-sm text-content-muted">—</span>
            </dd>
          </div>
          <div class="min-w-0">
            <dt class="text-xs text-content-muted">Root domain</dt>
            <dd
              class="mt-1 truncate font-mono text-sm text-content-secondary"
              :title="systemQuery.data.value?.root_domain"
            >
              {{ systemQuery.data.value?.root_domain ?? '—' }}
            </dd>
          </div>
        </dl>
      </UCard>

      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h3 class="text-sm font-medium text-content-secondary">Backup</h3>
        </template>

        <p class="mb-3 text-sm text-content-secondary">
          Back up these three files together (all under the server's configured data directory) to preserve this
          instance:
        </p>
        <ul class="flex flex-col gap-2">
          <li class="flex items-start gap-2 text-sm text-content-secondary">
            <span class="mt-1 h-1 w-1 shrink-0 rounded-full bg-content-muted" aria-hidden="true" />
            <span><span class="font-mono text-content-primary">basepod.db</span> — the SQLite database</span>
          </li>
          <li class="flex items-start gap-2 text-sm text-content-secondary">
            <span class="mt-1 h-1 w-1 shrink-0 rounded-full bg-content-muted" aria-hidden="true" />
            <span>
              <span class="font-mono text-content-primary">secret.key</span> — env-var encryption key. Losing this file
              makes stored environment variables unrecoverable and apps undeployable until re-entered.
            </span>
          </li>
          <li class="flex items-start gap-2 text-sm text-content-secondary">
            <span class="mt-1 h-1 w-1 shrink-0 rounded-full bg-content-muted" aria-hidden="true" />
            <span><span class="font-mono text-content-primary">caddy-data/</span> — Caddy's TLS certificates</span>
          </li>
        </ul>
        <p class="mt-3 text-xs text-content-muted">Restore by copying all three to the same locations in the new instance.</p>
      </UCard>
    </div>
  </SettingsShell>
</template>
