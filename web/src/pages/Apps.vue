<script setup lang="ts">
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'

import { api } from '../lib/api'
import { formatLimitsSummary } from '../lib/formatLimits'
import { statusStyles } from '../theme'
import AppShell from '../components/AppShell.vue'
import StatusBadge from '../components/StatusBadge.vue'
import ImageRef from '../components/ImageRef.vue'

// POLL_INTERVAL_MS: apps' deploy status changes over time (created ->
// deploying -> running/error) without any push channel in this milestone,
// so this polls rather than fetches once.
const POLL_INTERVAL_MS = 5000

const appsQuery = useQuery({
  queryKey: ['apps'],
  queryFn: () => api.listApps(),
  refetchInterval: POLL_INTERVAL_MS,
})

const apps = computed(() => appsQuery.data.value ?? [])

// NOTE ON SCOPE: the apps list intentionally does NOT show each app's
// public URL. The generated hostname (<slug>.<root_domain>) requires
// root_domain, which the API only exposes per-app via
// GET /apps/{slug}/domains (see internal/api/domains.go) — there is no
// system-wide field for it (internal/api/system.go's systemResponse has
// only version/podman/apps). Fetching it here would mean one extra
// request per app in the list (N+1), which this pass was told to avoid.
// AppDetail.vue already shows the generated domain as a clickable link
// via its own single per-app fetch. Same reasoning for last-deployment
// info: GET /apps returns App[], not AppDetail[], so it carries no
// deployment history — only GET /apps/{slug} does.
</script>

<template>
  <AppShell max-width="7xl">
    <div class="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-100">Apps</h1>
        <p class="mt-1 text-sm text-slate-400">
          <template v-if="appsQuery.data.value">{{ apps.length }} app{{ apps.length === 1 ? '' : 's' }} deployed</template>
          <template v-else>&nbsp;</template>
        </p>
      </div>

      <UButton to="/apps/new" color="primary" variant="soft" icon="i-lucide-plus" size="sm">New app</UButton>
    </div>

    <UAlert
      v-if="appsQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't load apps"
      description="Check that the BasePod server is running and reachable."
      class="mb-4"
    />

    <!-- Loading skeleton: mirrors the shape of the real rows so the list
         doesn't visibly jump once data lands. -->
    <div v-else-if="appsQuery.isPending.value" class="flex flex-col gap-3" aria-busy="true" aria-label="Loading apps">
      <div
        v-for="n in 4"
        :key="n"
        class="flex items-center gap-4 rounded-lg border border-slate-800 px-4 py-4 sm:px-5"
      >
        <div class="h-3.5 w-28 animate-pulse rounded bg-slate-800" />
        <div class="h-5 w-20 animate-pulse rounded-full bg-slate-800" />
        <div class="hidden h-3.5 flex-1 animate-pulse rounded bg-slate-800 md:block" />
        <div class="hidden h-3.5 w-12 animate-pulse rounded bg-slate-800 sm:block" />
        <div class="hidden h-3.5 w-24 animate-pulse rounded bg-slate-800 lg:block" />
      </div>
    </div>

    <div
      v-else-if="apps.length === 0"
      class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-slate-800 py-24 text-center"
    >
      <UIcon name="i-lucide-rocket" class="h-8 w-8 text-slate-600" aria-hidden="true" />
      <p class="text-sm font-medium text-slate-300">No apps deployed yet</p>
      <p class="max-w-sm text-sm text-slate-500">
        Deploy a container image or upload a build context to get your first app running.
      </p>
      <UButton to="/apps/new" color="primary" icon="i-lucide-plus" class="mt-1">New app</UButton>
    </div>

    <template v-else>
      <!-- Desktop / tablet: dense grid-row table. Every row gets the same
           border treatment (the old UI had exactly one stray <hr> between
           two rows) and the whole row is a link, not just the slug. -->
      <div class="hidden overflow-hidden rounded-lg border border-slate-800 md:block">
        <div
          class="grid grid-cols-[minmax(0,1.1fr)_auto_minmax(0,1.6fr)_auto_auto] items-center gap-4 border-b border-slate-800 bg-slate-900/40 px-5 py-2.5 text-xs font-medium uppercase tracking-wide text-slate-500"
        >
          <span>App</span>
          <span>Status</span>
          <span>Image</span>
          <span>Port</span>
          <span>Limits</span>
        </div>

        <RouterLink
          v-for="app in apps"
          :key="app.slug"
          :to="{ name: 'app-detail', params: { slug: app.slug } }"
          :aria-label="`Open app ${app.slug}, ${statusStyles[app.status].label}`"
          class="grid grid-cols-[minmax(0,1.1fr)_auto_minmax(0,1.6fr)_auto_auto] items-center gap-4 border-b border-slate-800 px-5 py-3.5 transition-colors last:border-b-0 hover:bg-slate-900/60 focus-visible:bg-slate-900/60 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-emerald-400"
        >
          <span class="truncate font-mono text-sm font-medium text-slate-100">{{ app.slug }}</span>
          <StatusBadge :status="app.status" />
          <ImageRef :value="app.image" class="min-w-0" />
          <span class="font-mono text-sm text-slate-400">{{ app.port }}</span>
          <span class="whitespace-nowrap font-mono text-xs text-slate-400">
            {{ formatLimitsSummary(app.memory_limit_mb, app.cpu_limit) }}
          </span>
        </RouterLink>
      </div>

      <!-- Narrow screens: the same rows collapse into cards instead of
           forcing a horizontal scroll on a five-column table. -->
      <div class="flex flex-col gap-3 md:hidden">
        <RouterLink
          v-for="app in apps"
          :key="app.slug"
          :to="{ name: 'app-detail', params: { slug: app.slug } }"
          :aria-label="`Open app ${app.slug}, ${statusStyles[app.status].label}`"
          class="flex flex-col gap-3 rounded-lg border border-slate-800 p-4 transition-colors hover:bg-slate-900/60 focus-visible:bg-slate-900/60 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-emerald-400"
        >
          <div class="flex items-center justify-between gap-3">
            <span class="truncate font-mono text-sm font-medium text-slate-100">{{ app.slug }}</span>
            <StatusBadge :status="app.status" />
          </div>
          <ImageRef :value="app.image" />
          <div class="flex items-center justify-between text-xs text-slate-500">
            <span class="font-mono">port {{ app.port }}</span>
            <span class="font-mono">{{ formatLimitsSummary(app.memory_limit_mb, app.cpu_limit) }}</span>
          </div>
        </RouterLink>
      </div>
    </template>
  </AppShell>
</template>
