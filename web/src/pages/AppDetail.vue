<script setup lang="ts">
// IA reorg (see .superpowers/sdd/ia-reorg-report.md): this page used to be
// one flat row of 7 equal-weight tabs (Overview, Deployments, Logs,
// Environment, Domains, Git, Settings) — a menu, not a structure. It's now
// 4: Overview (default — everything needed to answer "is this healthy,
// what's running, what changed last" without a click), Deployments (the
// full history + rollback + build logs), Logs (a reading-mode view, given
// more room than a cramped tab panel), and Configuration (Environment,
// Domains, Git, resource limits, and the app's danger zone — a related
// family of "set it up" surfaces grouped behind one level, since none of
// them is something an operator reaches for while firefighting). The old
// per-app "Settings" tab is gone as a *label* — its two panels
// (facts + resource limits) live on, ResourceLimitsPanel inside
// Configuration and its facts folded into Overview's own Facts card
// (the two were near-duplicates; Overview already has the facts).
//
// Every heavily-used child component (DeploymentList, LogViewer,
// EnvEditor, DomainsPanel, GitPanel, ResourceLimitsPanel) is unchanged
// internally — moved and re-framed into the new grouping, not rewritten.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'
import type { AccordionItem, TabsItem } from '@nuxt/ui'

import { api, ApiError, BASE_URL, type AllStatsSample, type AppStatsSample } from '../lib/api'
import { isPendingPlaceholder } from '../lib/pendingImage'
import { connect, type SseConnection, type SseState } from '../lib/sse'
import { pushStatsSample, formatCpuPercent, formatMemUsedLive, type AppStatsBySlug } from '../lib/statsBuffer'
import { relativeTime } from '../lib/relativeTime'
import AppShell from '../components/AppShell.vue'
import StatusBadge from '../components/StatusBadge.vue'
import ImageRef from '../components/ImageRef.vue'
import CpuSparkline from '../components/CpuSparkline.vue'
import DeploymentList from '../components/DeploymentList.vue'
import ConfirmDanger from '../components/ConfirmDanger.vue'
import LogViewer from '../components/LogViewer.vue'
import EnvEditor from '../components/EnvEditor.vue'
import DomainsPanel from '../components/DomainsPanel.vue'
import ResourceLimitsPanel from '../components/ResourceLimitsPanel.vue'
import GitPanel from '../components/GitPanel.vue'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const queryClient = useQueryClient()

const slug = computed(() => route.params.slug as string)

// Poll fast while a deploy is in flight (so the UI catches "running"/
// "error" promptly once it lands) and slow otherwise — same polling
// posture as Apps.vue, just status-conditional here.
const POLL_FAST_MS = 5000
const POLL_SLOW_MS = 15000

// vue-query deeply unwraps refs inside queryKey (MaybeRefDeep), so `slug`
// here just needs to be reactive, not manually wrapped in computed().
const appQuery = useQuery({
  queryKey: ['app', slug],
  queryFn: () => api.getApp(slug.value),
  refetchInterval: (query) => (query.state.data?.status === 'deploying' ? POLL_FAST_MS : POLL_SLOW_MS),
})

// The generated domain is derived from slug + a server-side root domain
// that doesn't change at runtime, so one fetch (no polling) is enough.
// This shares its query key with DomainsPanel's own domains fetch — an
// add/delete there invalidates the same cache entry, keeping this
// header's (and Overview's Facts card's) generated-domain link in sync
// for free.
const domainsQuery = useQuery({
  queryKey: ['app-domains', slug],
  queryFn: () => api.getDomains(slug.value),
})

const app = computed(() => appQuery.data.value)
const generatedDomain = computed(() => domainsQuery.data.value?.generated)

// True once a PUT to /env reports X-Basepod-Redeploy-Required: true, and
// cleared once a redeploy actually lands — drives EnvEditor's own
// persistent banner (passed down as a prop) and the small dot shown on
// both the outer Configuration tab and the Environment accordion header
// below (kept at this level since the tab/accordion state itself is owned
// here, not by EnvEditor).
const envRedeployRequired = ref(false)
const envEditorRef = ref<InstanceType<typeof EnvEditor> | null>(null)

// --- Top-level section + Configuration sub-section, reflected in the URL
// ------------------------------------------------------------------------
// `?tab=` and (within Configuration) `&section=` make the current view
// shareable and back-button-able, the same way `?buildLog=` already was —
// UTabs' modelValue type is `string | number` (generic over its items),
// wider than a closed literal union, so these stay plain strings rather
// than a narrowed type (mirrors the project's own note on this exact
// widening elsewhere in this file, and GitPanel's on the stricter
// `vue-tsc -b` project-reference build).
const VALID_TABS = ['overview', 'deployments', 'logs', 'configuration']
const VALID_SECTIONS = ['environment', 'domains', 'git', 'limits', 'danger']

function readQueryTab(): string {
  const value = route.query.tab
  return typeof value === 'string' && VALID_TABS.includes(value) ? value : 'overview'
}

function readQuerySection(): string {
  const value = route.query.section
  return typeof value === 'string' && VALID_SECTIONS.includes(value) ? value : 'environment'
}

const activeTab = ref(readQueryTab())
const openSection = ref(readQuerySection())

/** Rewrites the URL's query to exactly what the current tab/section need
 * — omitting `tab` entirely for the (default) Overview case and `section`
 * whenever it's the (default) Environment case, so the plain
 * `/apps/<slug>` URL keeps working as "Overview" rather than growing
 * query noise for the common path. Called after every interactive
 * tab/section change; the one-shot `?buildLog=`/`?autodeploy=` handling in
 * onMounted below manages its own replace() separately (matches its
 * pre-reorg behavior exactly). */
function syncQuery() {
  const query: Record<string, string> = {}
  if (activeTab.value !== 'overview') query.tab = activeTab.value
  if (activeTab.value === 'configuration' && openSection.value !== 'environment') query.section = openSection.value
  void router.replace({ name: 'app-detail', params: { slug: slug.value }, query })
}

const tabItems = computed<TabsItem[]>(() => [
  { label: 'Overview', value: 'overview' },
  { label: 'Deployments', value: 'deployments' },
  { label: 'Logs', value: 'logs' },
  {
    label: 'Configuration',
    value: 'configuration',
    badge: envRedeployRequired.value ? { color: 'warning', variant: 'solid', class: 'h-2 w-2 rounded-full p-0 ring-0' } : undefined,
  },
])

// Switching away from Configuration while its Environment section has
// unsaved edits is intercepted with a confirm — mirrors switching the
// accordion's own open section away from Environment below. Both paths
// share this one check so leaving unsaved env edits behind is never
// possible via either route out of the Environment section.
function hasUnsavedEnvEdits(): boolean {
  return activeTab.value === 'configuration' && openSection.value === 'environment' && !!envEditorRef.value?.isDirty
}

function confirmLeaveUnsavedEnv(): boolean {
  return window.confirm('You have unsaved environment variable changes. Leave without saving?')
}

function onTabChange(value: string | number) {
  const next = String(value)
  if (next === activeTab.value) return
  if (hasUnsavedEnvEdits() && !confirmLeaveUnsavedEnv()) return
  activeTab.value = next
  syncQuery()
}

// The tab strip's `list` is a manually horizontally-scrolling row (see
// the `:ui` override below — UTabs itself has no built-in "keep the
// active trigger in view" behavior). A tab change driven by a click
// already means the trigger the user tapped is on screen, but the two
// programmatic changes below (the ?buildLog= handoff from NewApp.vue's
// upload flow, and GitPanel's "Deploy now" handoff) can jump straight to
// a tab the user never touched — scroll it into view explicitly so it's
// never left off the edge of a phone-width strip.
const tabsRoot = ref<{ $el?: HTMLElement } | null>(null)

watch(activeTab, async () => {
  await nextTick()
  const el = tabsRoot.value?.$el
  const active = el?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]')
  active?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
})

// --- Configuration: Environment / Domains / Git / Resource limits /
// Danger zone, grouped behind one accordion rather than four more flat
// tabs — a related "set it up" family, not something reached for while
// checking state or firefighting. `type="single"` with no `collapsible`
// keeps exactly one section open at a time (never fully closed), so
// there's always a clear "you are here".
const configItems: (AccordionItem & { value: string; slot: string })[] = [
  { value: 'environment', label: 'Environment', icon: 'i-lucide-variable', slot: 'environment' },
  { value: 'domains', label: 'Domains', icon: 'i-lucide-globe', slot: 'domains' },
  { value: 'git', label: 'Git', icon: 'i-lucide-git-branch', slot: 'git' },
  { value: 'limits', label: 'Resource limits', icon: 'i-lucide-gauge', slot: 'limits' },
  { value: 'danger', label: 'Danger zone', icon: 'i-lucide-alert-triangle', slot: 'danger' },
]

function onSectionChange(value: string | string[] | undefined) {
  const next = Array.isArray(value) ? value[0] : value
  if (!next || next === openSection.value) return
  if (hasUnsavedEnvEdits() && !confirmLeaveUnsavedEnv()) return
  openSection.value = next
  syncQuery()
}

const deployError = ref('')

const deployMutation = useMutation({
  mutationFn: (image?: string) => api.deploy(slug.value, image),
  onSuccess: () => {
    deployError.value = ''
    // A landed deploy picks up whatever env vars are currently stored,
    // so any pending "redeploy to apply" state is now resolved.
    envRedeployRequired.value = false
    void queryClient.invalidateQueries({ queryKey: ['app', slug.value] })
  },
  onError: (err) => {
    deployError.value = err instanceof ApiError ? err.message : 'Deploy failed — try again'
    // The server has already flipped the app's status (e.g. to "error")
    // by the time this rejects — refetch so the badge/facts reflect that
    // immediately instead of showing the stale pre-deploy status for up
    // to POLL_SLOW_MS.
    void queryClient.invalidateQueries({ queryKey: ['app', slug.value] })
  },
})

// True while *this* client is waiting on a deploy it triggered, or while
// polling shows one is underway (e.g. triggered from another tab, or the
// auto-deploy fired by NewApp.vue on creation). Either condition disables
// deploy actions and forces the amber "Deploying" badge.
const isDeploying = computed(() => deployMutation.isPending.value || app.value?.status === 'deploying')
const displayStatus = computed(() => (isDeploying.value ? 'deploying' : (app.value?.status ?? 'created')))

// True when this app was created for an upload build that never landed
// (see lib/pendingImage.ts) — it's still sitting on the honest-but-fake
// "localhost/basepod/<slug>:pending" image NewApp.vue sets before the
// real tarball deploy runs. Plain "Deploy" (redeploy the app's *current*
// image) would just try, and fail, to pull that nonexistent tag, so it's
// disabled in that state; "Deploy new image…" and "Delete" (in
// Configuration's danger zone) stay enabled since both are real ways out
// of it.
const hasPendingPlaceholder = computed(() => (app.value ? isPendingPlaceholder(app.value.image) : false))

// Set by the ?buildLog=<number> query param below, right after
// NewApp.vue's upload-source flow lands here — passed down to
// DeploymentList so it opens that deployment's build-log drawer without
// requiring an extra click to find the build that was just kicked off.
const autoExpandDeploymentNumber = ref<number | null>(null)

// NewApp.vue navigates here with ?autodeploy=1 right after creating the
// app (image source), rather than triggering the deploy itself and
// racing its own unmount — this page owns the deploy mutation, so the
// pending/error UI below (amber badge, error callout) covers the very
// first deploy too. The upload source instead navigates with
// ?buildLog=<number> (it already triggered its own tarball deploy before
// navigating) — the two query shapes are mutually exclusive, one per
// source picker branch in NewApp.vue. Both replace the query outright
// (matching pre-reorg behavior) rather than going through syncQuery() —
// they're one-shot handoffs, not a tab/section the user navigated to.
onMounted(() => {
  if (route.query.autodeploy === '1') {
    void router.replace({ name: 'app-detail', params: { slug: slug.value }, query: {} })
    deployMutation.mutate(undefined)
    return
  }

  if (typeof route.query.buildLog === 'string') {
    const number = Number(route.query.buildLog)
    if (Number.isInteger(number)) {
      autoExpandDeploymentNumber.value = number
      activeTab.value = 'deployments'
    }
    void router.replace({ name: 'app-detail', params: { slug: slug.value }, query: {} })
  }
})

// GitPanel's manual "Deploy now" (POST .../deploy/git) already queued a
// deployment by the time this fires — switch to the Deployments tab and
// auto-expand its build-log drawer so the build streams live immediately,
// the same handoff NewApp.vue's upload flow does via ?buildLog=.
function onGitDeployed(deploymentNumber: number) {
  autoExpandDeploymentNumber.value = deploymentNumber
  activeTab.value = 'deployments'
  syncQuery()
  void queryClient.invalidateQueries({ queryKey: ['app', slug.value] })
}

function deployLatest() {
  // Same guard as the Deploy button's :disabled — also reached from
  // EnvEditor's "Redeploy" action (Configuration's Environment section),
  // which redeploys the current image too and would hit the same doomed
  // pull against the placeholder.
  if (isDeploying.value || hasPendingPlaceholder.value) return
  deployMutation.mutate(undefined)
}

const newImageOpen = ref(false)
const newImageValue = ref('')

function openNewImage() {
  newImageValue.value = app.value?.image ?? ''
  newImageOpen.value = true
}

function cancelNewImage() {
  newImageOpen.value = false
  newImageValue.value = ''
}

function confirmNewImage() {
  const image = newImageValue.value.trim()
  if (!image || isDeploying.value) return
  newImageOpen.value = false
  deployMutation.mutate(image)
}

const deleteModalOpen = ref(false)

const deleteMutation = useMutation({
  mutationFn: () => api.deleteApp(slug.value),
  onSuccess: () => {
    deleteModalOpen.value = false
    toast.add({ title: 'App deleted', description: slug.value, color: 'success', icon: 'i-lucide-trash-2' })
    void router.push({ name: 'apps' })
  },
  onError: (err) => {
    // 502 remove_failed: the app stays (server left the row in place so a
    // retry has something to act on) — surface the real error and let the
    // user try again rather than silently pretending it succeeded.
    deleteModalOpen.value = false
    toast.add({
      title: 'Could not delete app',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

// --- Overview: recent deployments (top 3, read-only — the full history
// with rollback/build-log lives in the Deployments tab; this is a glance,
// not a duplicate of it) --------------------------------------------------
const recentDeployments = computed(() =>
  app.value ? [...app.value.deployments].sort((a, b) => b.number - a.number).slice(0, 3) : [],
)

function viewAllDeployments() {
  onTabChange('deployments')
}

// --- Overview: live CPU/memory ------------------------------------------
// GET .../apps/{slug}/stats (internal/api/stats.go's handleAppStats) —
// wired server-side since v0.5 but never consumed by a page until this
// reorg's Overview tab (see lib/api.ts's AppStatsSample doc comment).
// Reuses lib/statsBuffer's rolling-window functions (the exact same ones
// Apps.vue's batch-stats sparklines use) rather than duplicating that
// logic for a single app — bySlug here just only ever holds one key.
//
// The route 409s "not_running" for an app with no live container, and
// EventSource can't see that status code (the same problem LogViewer
// solves with a preflight fetch) — rather than add a second preflight
// endpoint just for this card, the stream is only ever opened while
// displayStatus is "running" (already-polled data this page has anyway),
// and torn down the moment it isn't. That means it can lag the *true*
// container state by up to one appQuery poll tick, same as every other
// status-derived UI on this page already does.
const overviewStats = ref<AppStatsBySlug>({})
const statsStreamState = ref<SseState>('connecting')
let statsConnection: SseConnection | null = null

function startStatsStream() {
  if (statsConnection) return
  statsStreamState.value = 'connecting'
  statsConnection = connect(
    `${BASE_URL}/apps/${slug.value}/stats`,
    { scope: 'stats', slug: slug.value },
    {
      events: ['stats'],
      onEvent: (_name, dataJSON) => {
        try {
          const sample = JSON.parse(dataJSON) as AppStatsSample
          const withSlug: AllStatsSample = { ...sample, slug: slug.value }
          overviewStats.value = pushStatsSample(overviewStats.value, withSlug)
        } catch {
          // Malformed frame — drop rather than let it crash the card (same
          // "degrade honestly, don't throw" rule Apps.vue's batch stream
          // follows for the identical failure mode).
        }
      },
      onStateChange: (state) => {
        statsStreamState.value = state
      },
    },
  )
}

function stopStatsStream() {
  statsConnection?.close()
  statsConnection = null
}

watch(
  displayStatus,
  (status) => {
    if (status === 'running') {
      startStatsStream()
    } else {
      stopStatsStream()
      overviewStats.value = {}
    }
  },
  { immediate: true },
)

// A slug change (route-param navigation reusing this instance — same
// caveat LogViewer.vue's own slug watcher documents) must restart the
// stream against the new app rather than keep streaming the old one's
// numbers under the new header.
watch(slug, () => {
  stopStatsStream()
  overviewStats.value = {}
  if (displayStatus.value === 'running') startStatsStream()
})

onBeforeUnmount(stopStatsStream)

const liveStats = computed(() => overviewStats.value[slug.value] ?? null)
const liveCpuHistory = computed(() => liveStats.value?.cpuHistory ?? [])
const liveCpuText = computed(() => (liveStats.value?.latest ? formatCpuPercent(liveStats.value.latest.cpu_percent) : '—'))
const liveMemText = computed(() =>
  liveStats.value?.latest && app.value ? formatMemUsedLive(liveStats.value.latest.mem_used_bytes, app.value.memory_limit_mb) : '—',
)
</script>

<template>
  <AppShell max-width="5xl">
    <div class="mb-6 flex flex-wrap items-center gap-3">
      <RouterLink
        :to="{ name: 'apps' }"
        class="tap44 flex items-center gap-1 rounded-md text-sm text-content-secondary transition-colors hover:text-content-primary focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
      >
        <UIcon name="i-lucide-arrow-left" class="h-4 w-4" aria-hidden="true" />
        Apps
      </RouterLink>
      <span class="text-content-muted" aria-hidden="true">/</span>

      <template v-if="app">
        <span class="font-mono text-base font-semibold tracking-tight text-content-primary">{{ app.slug }}</span>
        <StatusBadge :status="displayStatus" />
        <a
          v-if="generatedDomain"
          :href="`https://${generatedDomain}`"
          target="_blank"
          rel="noopener noreferrer"
          class="tap44 flex items-center gap-1 rounded-sm font-mono text-xs text-accent hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
        >
          {{ generatedDomain }}
          <UIcon name="i-lucide-external-link" class="h-3 w-3" />
        </a>
        <ImageRef :value="app.image" class="ml-auto max-w-xs" />
      </template>
      <span v-else-if="appQuery.isPending.value" class="text-sm text-content-muted">Loading…</span>
    </div>

    <UAlert
        v-if="appQuery.isError.value"
        color="error"
        variant="subtle"
        title="Couldn't load app"
        :description="appQuery.error.value instanceof ApiError ? appQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
      />

      <div
        v-else-if="appQuery.isPending.value"
        class="flex items-center justify-center rounded-lg border border-line py-24 text-sm text-content-muted"
      >
        Loading app…
      </div>

      <template v-else-if="app">
        <!-- variant="link" wraps by default at narrow widths, pushing
             later tabs below the fold instead of keeping every tab
             reachable. Overriding the list slot to a horizontally-
             scrollable single row keeps all four tabs one thumb-swipe
             away on a phone, with the active one always rendered (never
             hidden behind a "more" menu). Four short labels comfortably
             fit without scrolling on most phone widths, but the
             mechanism stays in place regardless. -->
        <!-- pointer-coarse:py-3 gives each trigger a real 44px-tall tap
             target on touch (Tailwind's built-in `pointer: coarse` media
             variant — the same finger-vs-mouse signal as the .tap44 CSS
             utility elsewhere, just expressed as a direct size bump here
             since these sit in a horizontally-scrolling strip: an
             invisible hit-area overlay between adjacent tabs would make
             swipe-to-scroll and tap-to-select ambiguous at the boundary,
             which padding doesn't. Desktop keeps its original compact
             trigger height. -->
        <UTabs
          ref="tabsRoot"
          :items="tabItems"
          :model-value="activeTab"
          :content="false"
          variant="link"
          class="mb-6"
          :ui="{
            list: 'flex-nowrap overflow-x-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
            trigger: 'shrink-0 pointer-coarse:py-3',
          }"
          @update:model-value="onTabChange"
        />

        <!-- ============================== OVERVIEW ============================== -->
        <div v-if="activeTab === 'overview'" class="flex flex-col gap-6">
          <UAlert v-if="hasPendingPlaceholder" color="warning" variant="subtle" title="No successful build yet" icon="i-lucide-alert-triangle">
            <template #actions>
              <UButton size="sm" color="warning" variant="soft" class="tap44" icon="i-lucide-package-plus" @click="openNewImage">
                Deploy new image…
              </UButton>
              <UButton size="sm" color="neutral" variant="ghost" class="tap44" to="/apps/new" icon="i-lucide-upload">
                New app (upload)
              </UButton>
            </template>
            <template #description>
              <p class="text-sm text-content-secondary">
                This app was created for an upload build that never finished, so it has no real image to run —
                deploying now would just fail trying to pull a placeholder tag. Deploy a real image below, delete
                this app (Configuration → Danger zone) and re-upload from
                <span class="font-medium text-content-secondary">New app</span>, or deploy a build context from the
                command line with <span class="font-mono">basepod deploy</span>.
              </p>
            </template>
          </UAlert>

          <UAlert v-if="deployError" color="error" variant="subtle" title="Deploy failed" :description="deployError" icon="i-lucide-alert-circle" />

          <UCard variant="subtle" :ui="{ root: 'ring-line' }">
            <template #header>
              <h2 class="font-mono text-sm font-medium tracking-wide text-content-secondary uppercase">Facts</h2>
            </template>

            <dl class="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
              <div>
                <dt class="text-xs text-content-muted">Status</dt>
                <dd class="mt-1"><StatusBadge :status="displayStatus" /></dd>
              </div>
              <div class="col-span-2 min-w-0 sm:col-span-1">
                <dt class="text-xs text-content-muted">Public URL</dt>
                <dd class="mt-1">
                  <a
                    v-if="generatedDomain"
                    :href="`https://${generatedDomain}`"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="tap44 inline-flex items-center gap-1 rounded-sm font-mono text-sm text-accent hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
                  >
                    {{ generatedDomain }}
                    <UIcon name="i-lucide-external-link" class="h-3 w-3" />
                  </a>
                  <span v-else class="text-sm text-content-muted">—</span>
                </dd>
              </div>
              <div class="min-w-0">
                <dt class="text-xs text-content-muted">Image</dt>
                <dd class="mt-1"><ImageRef :value="app.image" /></dd>
              </div>
              <div>
                <dt class="text-xs text-content-muted">Port</dt>
                <dd class="mt-1 font-mono text-sm tabular-nums text-content-secondary">{{ app.port }}</dd>
              </div>
              <div>
                <dt class="text-xs text-content-muted">Internal hostname</dt>
                <dd class="mt-1 font-mono text-sm text-content-secondary">bp-{{ app.slug }}</dd>
              </div>
              <div>
                <dt class="text-xs text-content-muted">Deployments</dt>
                <dd class="mt-1 text-sm text-content-secondary">{{ app.deployments.length }}</dd>
              </div>
            </dl>
          </UCard>

          <UCard variant="subtle" :ui="{ root: 'ring-line' }">
            <template #header>
              <h2 class="font-mono text-sm font-medium tracking-wide text-content-secondary uppercase">Live resources</h2>
            </template>

            <div v-if="displayStatus !== 'running'" class="flex items-center justify-center py-6 text-sm text-content-muted">
              No live stats — app isn't running.
            </div>
            <div v-else class="flex flex-wrap items-center justify-between gap-4">
              <div class="flex items-center gap-3">
                <CpuSparkline :samples="liveCpuHistory" :status="displayStatus" :width="90" :height="24" />
                <span class="font-mono text-sm tabular-nums text-content-secondary">cpu {{ liveCpuText }}</span>
              </div>
              <span class="font-mono text-sm tabular-nums text-content-secondary">mem {{ liveMemText }}</span>
              <span v-if="statsStreamState === 'closed'" class="text-xs text-status-error">Live stats disconnected</span>
            </div>
          </UCard>

          <UCard variant="subtle" :ui="{ root: 'ring-line' }">
            <template #header>
              <div class="flex items-center justify-between gap-2">
                <h2 class="font-mono text-sm font-medium tracking-wide text-content-secondary uppercase">Recent deployments</h2>
                <UButton
                  v-if="recentDeployments.length"
                  size="xs"
                  color="neutral"
                  variant="ghost"
                  class="tap44"
                  trailing-icon="i-lucide-arrow-right"
                  @click="viewAllDeployments"
                >
                  View all
                </UButton>
              </div>
            </template>

            <div v-if="!recentDeployments.length" class="py-6 text-center text-sm text-content-muted">No deployments yet.</div>
            <div v-else class="flex flex-col">
              <div
                v-for="d in recentDeployments"
                :key="d.number"
                class="flex items-center justify-between gap-3 border-b border-line py-2.5 first:pt-0 last:border-b-0 last:pb-0"
              >
                <div class="flex min-w-0 items-center gap-2.5">
                  <span class="font-mono text-xs text-content-secondary">#{{ d.number }}</span>
                  <StatusBadge :status="d.status" />
                  <span class="truncate font-mono text-xs text-content-muted">{{ d.image }}</span>
                </div>
                <span class="shrink-0 text-xs text-content-muted" :title="d.started_at">{{ relativeTime(d.started_at) }}</span>
              </div>
            </div>
          </UCard>

          <UCard variant="subtle" :ui="{ root: 'ring-line' }">
            <template #header>
              <h2 class="font-mono text-sm font-medium tracking-wide text-content-secondary uppercase">Quick actions</h2>
            </template>

            <div class="flex flex-col gap-4">
              <div class="tap-row flex flex-wrap items-center gap-2">
                <UButton
                  color="primary"
                  variant="soft"
                  class="tap44"
                  icon="i-lucide-rocket"
                  :loading="deployMutation.isPending.value"
                  :disabled="isDeploying || hasPendingPlaceholder"
                  :title="hasPendingPlaceholder ? 'No successful build yet — upload a build context or set an image' : undefined"
                  @click="deployLatest"
                >
                  Deploy
                </UButton>

                <UButton
                  v-if="!newImageOpen"
                  color="neutral"
                  variant="ghost"
                  class="tap44"
                  icon="i-lucide-package-plus"
                  :disabled="isDeploying"
                  @click="openNewImage"
                >
                  Deploy new image…
                </UButton>
              </div>

              <div v-if="newImageOpen" class="tap-row flex flex-wrap items-center gap-2 rounded-lg border border-line p-3">
                <UInput
                  v-model="newImageValue"
                  placeholder="docker.io/library/nginx:alpine"
                  class="min-w-64 flex-1 font-mono"
                  :disabled="isDeploying"
                  autofocus
                  @keyup.enter="confirmNewImage"
                />
                <UButton color="primary" size="sm" class="tap44" :disabled="!newImageValue.trim() || isDeploying" @click="confirmNewImage">
                  Deploy image
                </UButton>
                <UButton color="neutral" variant="ghost" size="sm" class="tap44" :disabled="isDeploying" @click="cancelNewImage">Cancel</UButton>
              </div>
            </div>
          </UCard>
        </div>

        <!-- ============================== DEPLOYMENTS ============================== -->
        <div v-else-if="activeTab === 'deployments'">
          <DeploymentList :slug="slug" :deployments="app.deployments" :auto-expand-number="autoExpandDeploymentNumber" />
        </div>

        <!-- ============================== LOGS ============================== -->
        <!-- v-if (not v-show): the log stream must actually tear down —
             not just hide — when the tab is left, so LogViewer's
             onUnmounted call to sse.ts's close() runs and the EventSource
             (and its reconnect loop) doesn't keep running in the
             background for a tab the user can no longer see. Logs are a
             *reading* mode, not a form to skim past — LogViewer's own
             pane height (see LogViewer.vue) was widened as part of this
             reorg to actually use the screen instead of a fixed 512px
             box, since this is exactly the view an operator reaches for
             mid-incident. -->
        <div v-else-if="activeTab === 'logs'">
          <LogViewer :slug="slug" @deploy-hint="onTabChange('overview')" />
        </div>

        <!-- ============================== CONFIGURATION ============================== -->
        <!-- Environment / Domains / Git / Resource limits / Danger zone:
             a related "set it up" family grouped behind one level rather
             than four more flat tabs (see the top-of-file comment). An
             accordion — not a second tab strip — so mobile never nests a
             horizontally-scrolling strip inside the outer one. -->
        <div v-else-if="activeTab === 'configuration'">
          <UAccordion
            :items="configItems"
            type="single"
            :model-value="openSection"
            :ui="{ trigger: 'tap44 pointer-coarse:py-3' }"
            @update:model-value="onSectionChange"
          >
            <template #default="{ item }">
              <span :class="item.value === 'danger' ? 'text-status-error' : ''">{{ item.label }}</span>
              <span
                v-if="item.value === 'environment' && envRedeployRequired"
                class="ml-1.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-ember-400"
                aria-hidden="true"
              />
            </template>

            <template #environment>
              <EnvEditor
                ref="envEditorRef"
                :slug="slug"
                :redeploy-required="envRedeployRequired"
                :deploying="isDeploying"
                @update:redeploy-required="(value) => (envRedeployRequired = value)"
                @redeploy="deployLatest"
              />
            </template>

            <template #domains>
              <DomainsPanel :slug="slug" />
            </template>

            <template #git>
              <GitPanel :slug="slug" @deployed="onGitDeployed" />
            </template>

            <template #limits>
              <ResourceLimitsPanel :slug="slug" />
            </template>

            <template #danger>
              <UCard variant="subtle" :ui="{ root: 'ring-status-error/35' }">
                <template #header>
                  <h2 class="font-mono text-sm font-medium tracking-wide text-status-error uppercase">Delete app</h2>
                </template>

                <div class="flex flex-wrap items-center justify-between gap-3">
                  <p class="min-w-0 flex-1 text-sm break-words text-content-secondary">
                    Stops and removes this app's containers and routes, and deletes its deployment history. This cannot
                    be undone.
                  </p>
                  <UButton color="error" variant="soft" class="tap44" icon="i-lucide-trash-2" :disabled="isDeploying" @click="deleteModalOpen = true">
                    Delete app
                  </UButton>
                </div>
              </UCard>
            </template>
          </UAccordion>
        </div>
      </template>

    <ConfirmDanger
      v-if="app"
      :open="deleteModalOpen"
      :confirm-text="app.slug"
      title="Delete this app?"
      description="This stops and removes its containers and routes, and deletes its deployment history. This cannot be undone."
      confirm-label="Delete app"
      :loading="deleteMutation.isPending.value"
      @update:open="(value) => (deleteModalOpen = value)"
      @confirm="deleteMutation.mutate()"
    />
  </AppShell>
</template>
