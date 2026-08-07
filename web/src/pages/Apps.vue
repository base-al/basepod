<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, type AllStatsSample, type App } from '../lib/api'
import { formatLimitsSummary } from '../lib/formatLimits'
import { connect, type SseConnection, type SseState } from '../lib/sse'
import { formatCpuPercent, formatMemUsedLive, pushStatsSample, type AppStatsBySlug } from '../lib/statsBuffer'
import { statusStyles } from '../theme'
import AppShell from '../components/AppShell.vue'
import StatusBadge from '../components/StatusBadge.vue'
import ImageRef from '../components/ImageRef.vue'
import ConfirmDanger from '../components/ConfirmDanger.vue'
import CpuSparkline from '../components/CpuSparkline.vue'

// POLL_INTERVAL_MS: apps' deploy status changes over time (created ->
// deploying -> running/error) without any push channel in this milestone,
// so this polls rather than fetches once.
const POLL_INTERVAL_MS = 5000

const toast = useToast()
const queryClient = useQueryClient()

const appsQuery = useQuery({
  queryKey: ['apps'],
  queryFn: () => api.listApps(),
  refetchInterval: POLL_INTERVAL_MS,
})

const apps = computed(() => appsQuery.data.value ?? [])

// --- Live CPU/memory sparklines (batch stats stream) ----------------------
//
// One SSE connection for the whole page (GET /api/v1/stats via lib/sse.ts's
// connect(), scope "all_stats") rather than one per row — that's the whole
// point of the batch route (see internal/api/allstats.go): opening N
// EventSources for N apps would multiply connections, podman-side
// goroutines, and reconnect/backoff logic by however many apps are
// deployed. Each `event: stats` message carries a `slug` (see
// lib/api.ts's AllStatsSample) that routes it to the right row via
// lib/statsBuffer.ts's rolling window, keyed by slug — apps that aren't
// currently running simply never appear as a key (see AllStatsProvider's
// RunningAppContainers doc comment server-side), which is exactly why a
// row for a stopped/deploying-with-no-container-yet app just shows the
// seed state below rather than anything needing special-casing here.
const statsBySlug = ref<AppStatsBySlug>({})

// statsStreamState mirrors lib/sse.ts's SseState. Only 'closed' (a
// DEFINITIVE failure — see connect()'s doc comment: a dead session, or a
// run of failures confirmed not to be a live-but-flaky session) makes the
// numbers below fall back to "unavailable" instead of showing (possibly a
// little stale, mid-reconnect) last-known values — a transient
// 'reconnecting' blip isn't reason enough to blank out real numbers that
// are still probably close to right.
const statsStreamState = ref<SseState>('connecting')

let statsConnection: SseConnection | null = null

onMounted(() => {
  statsConnection = connect(
    '/api/v1/stats',
    { scope: 'all_stats', slug: '' },
    {
      events: ['stats'],
      onEvent: (_name, dataJSON) => {
        // A malformed event payload is silently dropped (that row's
        // numbers simply don't update this tick) rather than thrown or
        // logged — degrading honestly means never spamming the console
        // over a single skipped sample, and JSON.parse on server-sent
        // data is the one place here that can actually throw.
        try {
          const sample = JSON.parse(dataJSON) as AllStatsSample
          statsBySlug.value = pushStatsSample(statsBySlug.value, sample)
        } catch {
          // ignored — see above.
        }
      },
      onStateChange: (state) => {
        statsStreamState.value = state
      },
    },
  )
})

onBeforeUnmount(() => {
  statsConnection?.close()
  statsConnection = null
})

/** This row's recent CPU-percent history for CpuSparkline — an empty
 * array once the stream is confirmed dead (statsStreamState 'closed'),
 * so the sparkline falls back to its own flat/neutral seed state instead
 * of freezing on a last-known shape that can no longer be trusted live. */
function cpuHistory(slug: string): number[] {
  if (statsStreamState.value === 'closed') return []
  return statsBySlug.value[slug]?.cpuHistory ?? []
}

/** "12%" once a sample has arrived for this app, "—" before the first one
 * lands OR once the stream is confirmed dead — never a stale-looking
 * number presented as current. */
function cpuText(slug: string): string {
  if (statsStreamState.value === 'closed') return '—'
  const latest = statsBySlug.value[slug]?.latest
  return latest ? formatCpuPercent(latest.cpu_percent) : '—'
}

/** "41 / 512 MB" (used / this app's configured limit) once a sample has
 * arrived, "—" otherwise — mirrors cpuText's degrade-honestly rule. */
function memText(app: App): string {
  if (statsStreamState.value === 'closed') return '—'
  const latest = statsBySlug.value[app.slug]?.latest
  return latest ? formatMemUsedLive(latest.mem_used_bytes, app.memory_limit_mb) : '—'
}

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
// deployment history — only GET /apps/{slug} does. Because there's no
// domain link here to begin with, an internal (v0.5 Task 8) service is
// marked distinct with an "Internal" badge instead — see AppRow's
// Internal badge below.

interface AppGroup {
  key: string
  /** null = the standalone bucket (apps with no compose_project — the
   * common case today, and the ONLY group that ever existed before v0.5
   * Task 8). Non-null names a compose project's group header. */
  project: string | null
  apps: App[]
}

// v0.5 Task 10: apps created by a compose apply carry compose_project
// (see lib/api.ts's App.compose_project) — group them under their
// project so the list reads as "here's the stack that got deployed
// together", not N unrelated rows that happen to share a name prefix.
// Apps with no compose_project fall into one "standalone" bucket that
// renders with NO header at all when it's the only group present (see
// showGroupHeaders below) — an instance with no compose apps looks
// exactly like it always has.
const groups = computed<AppGroup[]>(() => {
  const byProject = new Map<string, App[]>()
  const standalone: App[] = []
  for (const app of apps.value) {
    if (app.compose_project) {
      const list = byProject.get(app.compose_project) ?? []
      list.push(app)
      byProject.set(app.compose_project, list)
    } else {
      standalone.push(app)
    }
  }

  const projectGroups: AppGroup[] = [...byProject.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([project, list]) => ({
      key: `project:${project}`,
      project,
      apps: [...list].sort((a, b) => a.compose_service.localeCompare(b.compose_service)),
    }))

  const standaloneGroup: AppGroup = { key: 'standalone', project: null, apps: standalone }

  // Standalone apps lead when there ARE compose project groups (they're
  // the catch-all "everything else" bucket, shown first as the most
  // numerous/familiar case); with no compose apps at all, this is the
  // only group and its position is moot.
  return projectGroups.length ? [standaloneGroup, ...projectGroups] : [standaloneGroup]
})

const showGroupHeaders = computed(() => groups.value.some((g) => g.project !== null))

function groupHeading(group: AppGroup): string {
  return group.project ?? 'Standalone'
}

// --- Delete project convenience (v0.5 Task 10) ----------------------------
//
// Project grouping is a display convenience, not a lifecycle object —
// internal/api/compose.go's own doc comment is explicit that BasePod
// never deletes a compose app automatically. This button is the UI's
// answer to "I actually want the whole stack gone": it issues one DELETE
// per app in the group (the same individual endpoint the app-detail page
// uses), behind a single confirm that names every app it's about to
// remove, rather than inventing a project-level delete BasePod's API
// doesn't have.

const deleteProjectTarget = ref<AppGroup | null>(null)

const deleteProjectMutation = useMutation({
  mutationFn: async (group: AppGroup) => {
    const results = await Promise.allSettled(group.apps.map((app) => api.deleteApp(app.slug)))
    const failed = group.apps.filter((_, i) => results[i]!.status === 'rejected')
    if (failed.length) {
      throw new Error(`Failed to delete: ${failed.map((a) => a.slug).join(', ')}`)
    }
  },
  onSuccess: async (_data, group) => {
    deleteProjectTarget.value = null
    await queryClient.invalidateQueries({ queryKey: ['apps'] })
    toast.add({ title: 'Project deleted', description: group.project ?? undefined, color: 'success', icon: 'i-lucide-trash-2' })
  },
  onError: async (err) => {
    // Some deletes may have already succeeded (Promise.allSettled ran
    // every one regardless) — refetch so the list reflects whichever
    // apps are actually gone rather than staying stale until the next
    // poll tick.
    deleteProjectTarget.value = null
    await queryClient.invalidateQueries({ queryKey: ['apps'] })
    toast.add({
      title: 'Could not delete every app in this project',
      description: err instanceof Error ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})
</script>

<template>
  <AppShell max-width="7xl">
    <div class="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 class="font-mono text-2xl font-semibold tracking-tight text-content-primary">Apps</h1>
        <p class="mt-1 text-sm text-content-secondary">
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
        class="flex items-center gap-4 rounded-lg border border-line px-4 py-4 sm:px-5"
      >
        <div class="h-3.5 w-28 animate-pulse rounded bg-line" />
        <div class="h-5 w-20 animate-pulse rounded-full bg-line" />
        <div class="hidden h-3.5 flex-1 animate-pulse rounded bg-line md:block" />
        <div class="hidden h-3.5 w-12 animate-pulse rounded bg-line sm:block" />
        <div class="hidden h-3.5 w-24 animate-pulse rounded bg-line lg:block" />
      </div>
    </div>

    <div
      v-else-if="apps.length === 0"
      class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-24 text-center"
    >
      <UIcon name="i-lucide-rocket" class="h-8 w-8 text-content-muted" aria-hidden="true" />
      <p class="text-sm font-medium text-content-secondary">No apps deployed yet</p>
      <p class="max-w-sm text-sm text-content-muted">
        Deploy a container image or upload a build context to get your first app running.
      </p>
      <UButton to="/apps/new" color="primary" icon="i-lucide-plus" class="mt-1">New app</UButton>
    </div>

    <template v-else>
      <div v-for="group in groups" :key="group.key" class="mb-6 last:mb-0">
        <div v-if="showGroupHeaders" class="mb-2 flex items-center justify-between gap-2">
          <div class="flex items-center gap-2">
            <UIcon
              :name="group.project ? 'i-lucide-boxes' : 'i-lucide-box'"
              class="h-3.5 w-3.5 text-content-muted"
              aria-hidden="true"
            />
            <h2 class="font-mono text-xs font-medium tracking-wide text-content-secondary uppercase">{{ groupHeading(group) }}</h2>
            <span class="text-xs text-content-muted">{{ group.apps.length }}</span>
          </div>
          <UButton
            v-if="group.project"
            size="xs"
            color="error"
            variant="ghost"
            icon="i-lucide-trash-2"
            @click="deleteProjectTarget = group"
          >
            Delete project
          </UButton>
        </div>

        <!-- Desktop / tablet: dense grid-row table. Every row gets the same
             border treatment (the old UI had exactly one stray <hr> between
             two rows) and the whole row is a link, not just the slug. -->
        <div class="hidden overflow-hidden rounded-lg border border-line md:block">
          <div
            class="grid grid-cols-[minmax(0,1.1fr)_auto_auto_auto_minmax(0,1.3fr)_auto_auto] items-center gap-4 border-b border-line bg-surface-elevated/40 px-5 py-2.5 text-xs font-medium uppercase tracking-wide text-content-muted"
          >
            <span>App</span>
            <span>Status</span>
            <span>CPU</span>
            <span>Mem</span>
            <span>Image</span>
            <span>Port</span>
            <span>Limits</span>
          </div>

          <RouterLink
            v-for="app in group.apps"
            :key="app.slug"
            :to="{ name: 'app-detail', params: { slug: app.slug } }"
            :aria-label="`Open app ${app.slug}, ${statusStyles[app.status].label}${app.internal ? ', internal' : ''}`"
            class="grid grid-cols-[minmax(0,1.1fr)_auto_auto_auto_minmax(0,1.3fr)_auto_auto] items-center gap-4 border-b border-line px-5 py-3.5 transition-colors last:border-b-0 hover:bg-surface-elevated/60 focus-visible:bg-surface-elevated/60 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent"
          >
            <span class="truncate font-mono text-sm font-medium text-content-primary">
              {{ app.compose_project ? app.compose_service : app.slug }}
            </span>
            <div class="flex items-center gap-1.5">
              <StatusBadge :status="app.status" />
              <UBadge v-if="app.internal" color="neutral" variant="subtle" size="sm" class="items-center gap-1">
                <UIcon name="i-lucide-lock" class="h-2.5 w-2.5" aria-hidden="true" />
                Internal
              </UBadge>
            </div>
            <div class="flex items-center gap-2">
              <CpuSparkline :samples="cpuHistory(app.slug)" :status="app.status" />
              <span class="font-mono text-xs tabular-nums text-content-secondary">{{ cpuText(app.slug) }}</span>
            </div>
            <span class="whitespace-nowrap font-mono text-xs tabular-nums text-content-secondary">{{ memText(app) }}</span>
            <ImageRef :value="app.image" class="min-w-0" />
            <span class="font-mono text-sm text-content-secondary tabular-nums">{{ app.internal ? '—' : app.port }}</span>
            <span class="whitespace-nowrap font-mono text-xs tabular-nums text-content-secondary">
              {{ formatLimitsSummary(app.memory_limit_mb, app.cpu_limit) }}
            </span>
          </RouterLink>
        </div>

        <!-- Narrow screens: the same rows collapse into cards instead of
             forcing a horizontal scroll on a five-column table. -->
        <div class="flex flex-col gap-3 md:hidden">
          <RouterLink
            v-for="app in group.apps"
            :key="app.slug"
            :to="{ name: 'app-detail', params: { slug: app.slug } }"
            :aria-label="`Open app ${app.slug}, ${statusStyles[app.status].label}${app.internal ? ', internal' : ''}`"
            class="flex flex-col gap-3 rounded-lg border border-line p-4 transition-colors hover:bg-surface-elevated/60 focus-visible:bg-surface-elevated/60 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent"
          >
            <div class="flex items-center justify-between gap-3">
              <span class="truncate font-mono text-sm font-medium text-content-primary">
                {{ app.compose_project ? app.compose_service : app.slug }}
              </span>
              <div class="flex items-center gap-1.5">
                <StatusBadge :status="app.status" />
                <UBadge v-if="app.internal" color="neutral" variant="subtle" size="sm" class="items-center gap-1">
                  <UIcon name="i-lucide-lock" class="h-2.5 w-2.5" aria-hidden="true" />
                  Internal
                </UBadge>
              </div>
            </div>
            <ImageRef :value="app.image" />
            <div class="flex items-center justify-between text-xs text-content-muted">
              <span class="font-mono">{{ app.internal ? 'internal' : `port ${app.port}` }}</span>
              <span class="font-mono">{{ formatLimitsSummary(app.memory_limit_mb, app.cpu_limit) }}</span>
            </div>
            <!-- Live CPU/memory, numbers only — no sparkline graphic here.
                 At card width (~390px) the row above already carries two
                 text segments (port, limits); a third element squeezed in
                 next to the state chip would either force a wrap or shrink
                 the spark's ~60px trend line down to a few illegible
                 pixels. The numbers are the actual value-add (an operator
                 checking "is this thing busy" from their phone cares about
                 the current reading, not the shape of the last 100s) so
                 they're what's kept; the trend line stays a desktop-table
                 affordance (see CpuSparkline.vue above). -->
            <div class="flex items-center justify-between text-xs text-content-muted">
              <span class="font-mono">cpu {{ cpuText(app.slug) }}</span>
              <span class="font-mono">mem {{ memText(app) }}</span>
            </div>
          </RouterLink>
        </div>
      </div>
    </template>

    <ConfirmDanger
      v-if="deleteProjectTarget"
      :open="deleteProjectTarget !== null"
      :confirm-text="deleteProjectTarget.project ?? ''"
      title="Delete this project?"
      :description="`Deletes every app in this project — ${deleteProjectTarget.apps.map((a) => a.slug).join(', ')}. Stops and removes their containers and routes, and deletes their deployment history. This cannot be undone.`"
      confirm-label="Delete project"
      :loading="deleteProjectMutation.isPending.value"
      @update:open="(value) => (deleteProjectTarget = value ? deleteProjectTarget : null)"
      @confirm="deleteProjectTarget && deleteProjectMutation.mutate(deleteProjectTarget)"
    />
  </AppShell>
</template>
