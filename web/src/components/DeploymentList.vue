<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, ApiError, type Deployment } from '../lib/api'
import { canRollBackTo, currentHealthyNumber, displayError, isErrorTruncated } from '../lib/deploymentDisplay'
import { shortSha } from '../lib/gitFormat'
import { relativeTime } from '../lib/relativeTime'
import StatusBadge from './StatusBadge.vue'
import ImageRef from './ImageRef.vue'
import BuildLogPanel from './BuildLogPanel.vue'

const props = defineProps<{
  slug: string
  deployments: Deployment[]
  /** A deployment number to auto-expand the build-log drawer for on
   * mount/change — set by AppDetail.vue right after NewApp.vue's upload
   * flow navigates here, so the freshly-created build's log is visible
   * immediately instead of requiring an extra click to find it. Applied
   * once per value change, not held open against the user's own later
   * collapse/expand actions. */
  autoExpandNumber?: number | null
}>()

const toast = useToast()
const queryClient = useQueryClient()

// The API already returns deployments newest-first (ORDER BY number DESC
// in internal/store.ListDeployments), but sort defensively rather than
// assume server ordering forever.
const rows = computed(() => [...props.deployments].sort((a, b) => b.number - a.number))

// The single healthy deployment currently serving traffic — every other
// healthy row gets a "Roll back" action, this one doesn't (rolling back
// to the current live deployment would be a no-op deploy of the same
// image). rows is already sorted newest-first, so this is just the first
// healthy row's number. Eligibility rule itself lives in
// lib/deploymentDisplay.ts, shared verbatim between the desktop and
// mobile layouts below (and unit-tested there) rather than duplicated.
const currentHealthy = computed(() => currentHealthyNumber(rows.value))

function rollbackEligible(deployment: Deployment) {
  return canRollBackTo(deployment, currentHealthy.value)
}

// --- Responsive layout: table (>= md) vs cards (< phone/tablet) ----------
//
// Deployments render inside AppDetail.vue's Deployments tab, which sits
// in AppShell same as Apps.vue — no nested side-nav on top of AppShell's
// own rail (unlike SettingsShell, which adds a second nav column and is
// why settings/Users.vue only collapses to cards at `lg`). That's the
// same container shape Apps.vue's own app-list table already solved at
// `md` (768px): AppShell's rail turns on at exactly that breakpoint too,
// and Apps.vue's wider 7-column table already fits what's left. Verified
// in a real browser at 768px/414px/390px (see issue #14's verification
// notes) — this table has fewer columns than Apps.vue's, so if that one
// clears `md`, this one comfortably does too.
//
// UNLIKE Apps.vue/Users.vue, this component can't just render both
// layouts always and hide one with CSS: the build-log drawer
// (BuildLogPanel below) opens a live SSE stream or fetches the log on
// mount, and only one deployment is ever expanded at a time. If both the
// table and the card list stayed mounted (one merely `hidden` via CSS),
// toggling a drawer open would mount BuildLogPanel twice — once visible,
// once invisible-but-still-fetching/streaming — doubling load on the
// server for zero benefit. So the layout choice here is driven by a real
// `matchMedia` check against the same breakpoint the CSS uses, and only
// the layout that's actually visible ever mounts, exclusively via v-if
// rather than v-show/hidden.
const DESKTOP_QUERY = '(min-width: 768px)' // Tailwind's `md`

const isDesktopLayout = ref(typeof window !== 'undefined' ? window.matchMedia(DESKTOP_QUERY).matches : true)
let mediaQuery: MediaQueryList | null = null

function handleMediaChange(event: MediaQueryListEvent) {
  isDesktopLayout.value = event.matches
}

onMounted(() => {
  mediaQuery = window.matchMedia(DESKTOP_QUERY)
  isDesktopLayout.value = mediaQuery.matches
  mediaQuery.addEventListener('change', handleMediaChange)
})

onUnmounted(() => {
  mediaQuery?.removeEventListener('change', handleMediaChange)
  mediaQuery = null
})

// Deployment errors can be long stack-trace-ish strings; truncate by
// default and let the row/card expand in place on click (also has a
// native title tooltip for a no-click hover preview). Truncation length
// and the actual substring logic live in lib/deploymentDisplay.ts.
const expandedErrors = ref<Set<number>>(new Set())

function toggleError(number: number) {
  const next = new Set(expandedErrors.value)
  if (next.has(number)) {
    next.delete(number)
  } else {
    next.add(number)
  }
  expandedErrors.value = next
}

function errorText(deployment: Deployment): string {
  return displayError(deployment.error, expandedErrors.value.has(deployment.number))
}

// Build-log drawer: at most one open at a time — opening one deployment's
// drawer collapses any other (running more than one build-log SSE stream
// at once per app isn't worth the complexity). Keyed by deployment number
// as a string so both layouts above can check `expandedState[String(n)]`
// directly without needing TanStack's row APIs (this no longer backs a
// UTable — see the layout note above for why the table itself was
// rewritten as a plain grid).
const expandedState = ref<Record<string, boolean>>({})

function isBuildLogOpen(number: number): boolean {
  return Boolean(expandedState.value[String(number)])
}

function toggleBuildLog(number: number) {
  const id = String(number)
  expandedState.value = expandedState.value[id] ? {} : { [id]: true }
}

watch(
  () => props.autoExpandNumber,
  (number) => {
    if (number != null) {
      expandedState.value = { [String(number)]: true }
    }
  },
  { immediate: true },
)

// Which row/card's rollback confirm popover is open, and which target
// deployment number a rollback is currently in flight for (kept separate
// from the popover's own open state so the popover can close immediately
// on confirm while the button keeps its loading state until the request
// settles — mirrors DomainsPanel's delete confirm). Safe to key by plain
// deployment number (not `${layout}:${number}` the way settings/Users.vue
// keys its own duplicated-by-CSS rows) because exactly one layout is ever
// mounted here — see the layout note above.
const confirmOpenNumber = ref<number | null>(null)
const rollingBackToNumber = ref<number | null>(null)

function invalidateApp() {
  return queryClient.invalidateQueries({ queryKey: ['app', props.slug] })
}

const rollbackMutation = useMutation({
  mutationFn: (number: number) => api.rollbackApp(props.slug, number),
  onSuccess: async (deployment) => {
    const rolledBackTo = rollingBackToNumber.value
    confirmOpenNumber.value = null
    rollingBackToNumber.value = null
    await invalidateApp()
    toast.add({
      title: 'Rolled back',
      description: `Deployment #${deployment.number} now runs the image from #${rolledBackTo}.`,
      color: 'success',
      icon: 'i-lucide-history',
    })
  },
  onError: (err) => {
    confirmOpenNumber.value = null
    rollingBackToNumber.value = null
    toast.add({
      title: 'Rollback failed',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

function confirmRollback(number: number) {
  rollingBackToNumber.value = number
  rollbackMutation.mutate(number)
}
</script>

<template>
  <div v-if="rows.length === 0" class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-16 text-center">
    <UIcon name="i-lucide-rocket" class="h-8 w-8 text-content-muted" aria-hidden="true" />
    <p class="text-sm font-medium text-content-secondary">No deployments yet</p>
  </div>

  <!-- Desktop / tablet (>= md): dense grid-row table — see the layout
       note above for why `md` and why this mounts exclusively (never
       alongside the card layout below). -->
  <div v-else-if="isDesktopLayout" class="overflow-hidden rounded-lg border border-line">
    <div
      class="grid grid-cols-[2.75rem_minmax(0,1.6fr)_6.5rem_5.5rem_minmax(0,1fr)_4.5rem] items-center gap-3 border-b border-line bg-surface-elevated/40 px-5 py-2.5 text-xs font-medium uppercase tracking-wide text-content-muted"
    >
      <span>#</span>
      <span>Image</span>
      <span>Status</span>
      <span>Started</span>
      <span>Error</span>
      <span class="text-right">Actions</span>
    </div>

    <template v-for="d in rows" :key="d.number">
      <div class="grid grid-cols-[2.75rem_minmax(0,1.6fr)_6.5rem_5.5rem_minmax(0,1fr)_4.5rem] items-center gap-3 border-b border-line px-5 py-3.5 last:border-b-0">
        <span class="font-mono text-sm text-content-secondary">#{{ d.number }}</span>

        <div class="flex min-w-0 flex-col gap-0.5">
          <ImageRef :value="d.image" />
          <!-- `truncate` (not free-wrapping) so a long source/trigger/sha
               combo never blows out the row's height — with the row
               vertically centered (`items-center` above), a tall wrapped
               cell here would visually crowd the Status/Actions columns
               next to it even though they're in separate grid tracks. -->
          <span class="truncate text-[11px] text-content-muted">
            {{ d.source }} · {{ d.trigger }}
            <template v-if="d.source === 'git' && d.git_sha">
              · <span class="font-mono">{{ shortSha(d.git_sha) }}</span>
            </template>
          </span>
        </div>

        <StatusBadge :status="d.status" />

        <span class="text-xs whitespace-nowrap text-content-secondary" :title="d.started_at">{{ relativeTime(d.started_at) }}</span>

        <button
          v-if="d.error"
          type="button"
          class="tap44 line-clamp-2 rounded-xs text-left text-xs text-status-error hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
          :title="d.error"
          @click="toggleError(d.number)"
        >
          {{ errorText(d) }}
        </button>
        <span v-else class="text-xs text-content-muted">—</span>

        <!-- Icon-only (not the mobile card's full-text buttons) — mirrors
             settings/Users.vue's own desktop action column, which made
             the same call for the same reason: this column's width has
             to share the row with Image/Error, and two full-text buttons
             ("Build log" / "Roll back") don't fit next to a 1024×-wide
             error/image pair once the sidebar rail is also taking space
             (see the layout note up top on why `md` still works here —
             it's the buttons, not the text columns, that needed to
             shrink). Each still carries a real label via aria-label/title,
             not just a bare icon. -->
        <div class="tap-row flex items-center justify-end gap-1">
          <UButton
            v-if="d.has_build_log"
            size="xs"
            color="neutral"
            variant="ghost"
            square
            class="tap44"
            :icon="isBuildLogOpen(d.number) ? 'i-lucide-chevron-up' : 'i-lucide-terminal'"
            :aria-label="isBuildLogOpen(d.number) ? `Collapse build log for deployment #${d.number}` : `View build log for deployment #${d.number}`"
            :title="isBuildLogOpen(d.number) ? 'Collapse build log' : 'Build log'"
            @click="toggleBuildLog(d.number)"
          />

          <UPopover
            v-if="rollbackEligible(d)"
            :open="confirmOpenNumber === d.number"
            @update:open="(v: boolean) => (confirmOpenNumber = v ? d.number : null)"
          >
            <UButton
              size="xs"
              color="neutral"
              variant="ghost"
              square
              class="tap44"
              icon="i-lucide-history"
              :loading="rollbackMutation.isPending.value && rollingBackToNumber === d.number"
              :disabled="rollbackMutation.isPending.value"
              :aria-label="`Roll back to deployment #${d.number}`"
              title="Roll back"
            />
            <template #content>
              <div class="flex flex-col gap-2 p-3">
                <p class="text-xs text-content-secondary">
                  Roll back to deployment <span class="font-mono">#{{ d.number }}</span>?
                </p>
                <p class="max-w-64 text-xs text-content-muted">This redeploys the image from that deployment as a new deployment.</p>
                <div class="tap-row flex justify-end gap-2">
                  <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="confirmOpenNumber = null">Cancel</UButton>
                  <UButton
                    size="xs"
                    color="warning"
                    class="tap44"
                    :loading="rollbackMutation.isPending.value && rollingBackToNumber === d.number"
                    @click="confirmRollback(d.number)"
                  >
                    Roll back
                  </UButton>
                </div>
              </div>
            </template>
          </UPopover>
        </div>
      </div>

      <div v-if="isBuildLogOpen(d.number)" class="border-b border-line bg-surface-sunken/40 px-5 py-3 last:border-b-0">
        <BuildLogPanel :slug="slug" :deployment="d" />
      </div>
    </template>
  </div>

  <!-- Phone / narrow tablet (< md): cards instead of a horizontally-
       scrolling table (issue #14). Number + status lead each card — the
       two things you scan for first — with the error, when present,
       called out in its own tinted block right under the image/meta so
       it can't be missed rather than sitting in a narrow column. -->
  <div v-else class="flex flex-col gap-3">
    <div v-for="d in rows" :key="d.number" class="flex flex-col gap-3 rounded-lg border border-line p-4">
      <div class="flex items-center justify-between gap-3">
        <span class="font-mono text-sm font-medium text-content-primary">#{{ d.number }}</span>
        <StatusBadge :status="d.status" class="shrink-0" />
      </div>

      <ImageRef :value="d.image" />

      <div class="flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-content-muted">
        <span class="font-mono">
          {{ d.source }} · {{ d.trigger }}
          <template v-if="d.source === 'git' && d.git_sha">
            · <span class="font-mono">{{ shortSha(d.git_sha) }}</span>
          </template>
        </span>
        <span class="ml-auto font-mono whitespace-nowrap" :title="d.started_at">{{ relativeTime(d.started_at) }}</span>
      </div>

      <button
        v-if="d.error"
        type="button"
        class="tap44 rounded-md border border-crimson-400/30 bg-status-error-bg px-2.5 py-2 text-left text-xs text-status-error"
        :class="isErrorTruncated(d.error) ? '' : 'cursor-default'"
        :title="d.error"
        @click="isErrorTruncated(d.error) && toggleError(d.number)"
      >
        {{ errorText(d) }}
      </button>

      <div v-if="d.has_build_log || rollbackEligible(d)" class="tap-row flex flex-wrap items-center gap-2 border-t border-line pt-3">
        <UButton
          v-if="d.has_build_log"
          size="xs"
          color="neutral"
          variant="soft"
          class="tap44"
          :icon="isBuildLogOpen(d.number) ? 'i-lucide-chevron-up' : 'i-lucide-terminal'"
          @click="toggleBuildLog(d.number)"
        >
          Build log
        </UButton>

        <UPopover
          v-if="rollbackEligible(d)"
          :open="confirmOpenNumber === d.number"
          @update:open="(v: boolean) => (confirmOpenNumber = v ? d.number : null)"
        >
          <UButton
            size="xs"
            color="warning"
            variant="soft"
            class="tap44"
            icon="i-lucide-history"
            :loading="rollbackMutation.isPending.value && rollingBackToNumber === d.number"
            :disabled="rollbackMutation.isPending.value"
          >
            Roll back
          </UButton>
          <template #content>
            <div class="flex w-64 flex-col gap-2 p-3">
              <p class="text-xs text-content-secondary">
                Roll back to deployment <span class="font-mono">#{{ d.number }}</span>?
              </p>
              <p class="text-xs text-content-muted">This redeploys the image from that deployment as a new deployment.</p>
              <div class="tap-row flex justify-end gap-2">
                <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="confirmOpenNumber = null">Cancel</UButton>
                <UButton
                  size="xs"
                  color="warning"
                  class="tap44"
                  :loading="rollbackMutation.isPending.value && rollingBackToNumber === d.number"
                  @click="confirmRollback(d.number)"
                >
                  Roll back
                </UButton>
              </div>
            </div>
          </template>
        </UPopover>
      </div>

      <div v-if="isBuildLogOpen(d.number)" class="border-t border-line pt-3">
        <BuildLogPanel :slug="slug" :deployment="d" />
      </div>
    </div>
  </div>
</template>
