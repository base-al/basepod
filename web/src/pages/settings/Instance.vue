<script setup lang="ts">
// Settings -> Instance: the facts about THIS server, not this operator's
// own account — version, container-runtime health, the domains involved,
// and how to back the instance up. New page (IA reorg); everything on it
// is either real data already exposed by GET /system (shared with
// AppShell's own health chip — same ['system'] query key, so this page
// causes no extra request) or an honest static fact/explanation. Nothing
// here is invented: the API has no endpoint yet for root_domain or a
// Caddy-specific health signal (see the notes below), so those sections
// say so plainly rather than fake a value — the same rule Users.vue
// follows for its "coming soon" placeholder.
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'

import { api } from '../../lib/api'
import { formatVersion } from '../../lib/version'
import SettingsShell from '../../components/SettingsShell.vue'

// Same key AppShell's health chip polls — sharing it means this page
// causes zero extra network traffic, and the two views can never disagree
// about the current podman status mid-poll-cycle.
const SYSTEM_POLL_INTERVAL_MS = 5000

const systemQuery = useQuery({
  queryKey: ['system'],
  queryFn: () => api.system(),
  refetchInterval: SYSTEM_POLL_INTERVAL_MS,
})

const podmanOk = computed(() => systemQuery.data.value?.podman === 'ok')

// The host this dashboard was actually loaded from — real, observed data
// (not a guess or a stand-in), and exactly what "dashboard domain" means
// in practice: whatever hostname/port got you here.
const dashboardHost = computed(() => window.location.host)
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

      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h3 class="text-sm font-medium text-content-secondary">Version &amp; runtime</h3>
        </template>

        <dl class="grid grid-cols-1 gap-x-6 gap-y-4 sm:grid-cols-3">
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
            <dt class="text-xs text-content-muted">Apps deployed</dt>
            <dd class="mt-1 font-mono text-sm tabular-nums text-content-secondary">
              {{ systemQuery.data.value?.apps ?? '—' }}
            </dd>
          </div>
        </dl>

        <p class="mt-4 text-xs text-content-muted">
          Caddy — BasePod's managed reverse proxy — doesn't have a separate health signal in the API yet; it runs as its
          own container, so the Podman status above is the closest live indicator of whether it's reachable at all.
        </p>
      </UCard>

      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h3 class="text-sm font-medium text-content-secondary">Domains</h3>
        </template>

        <dl class="grid grid-cols-1 gap-x-6 gap-y-4 sm:grid-cols-2">
          <div class="min-w-0">
            <dt class="text-xs text-content-muted">Dashboard domain</dt>
            <dd class="mt-1 truncate font-mono text-sm text-content-secondary" :title="dashboardHost">{{ dashboardHost }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="text-xs text-content-muted">Root domain</dt>
            <dd class="mt-1 text-sm text-content-muted">
              Configured at setup, not yet exposed here as a single value — visible as the suffix of any app's generated
              domain (open an app → Configuration → Domains).
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
