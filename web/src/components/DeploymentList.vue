<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'
import type { TableColumn } from '@nuxt/ui'

import { api, ApiError, type Deployment } from '../lib/api'
import { shortSha } from '../lib/gitFormat'
import { relativeTime } from '../lib/relativeTime'
import StatusBadge from './StatusBadge.vue'
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
// healthy row's number.
const currentHealthyNumber = computed(() => rows.value.find((d) => d.status === 'healthy')?.number ?? null)

function canRollBackTo(deployment: Deployment) {
  return deployment.status === 'healthy' && deployment.number !== currentHealthyNumber.value
}

const columns: TableColumn<Deployment>[] = [
  { accessorKey: 'number', header: '#' },
  { accessorKey: 'image', header: 'Image' },
  { accessorKey: 'status', header: 'Status' },
  { accessorKey: 'started_at', header: 'Started' },
  { accessorKey: 'error', header: 'Error' },
  { id: 'actions', header: '' },
]

// Deployment errors can be long stack-trace-ish strings; truncate by
// default and let the row expand in place on click (also has a native
// title tooltip for a no-click hover preview).
const ERROR_TRUNCATE_AT = 72
const expanded = ref<Set<number>>(new Set())

function toggle(number: number) {
  const next = new Set(expanded.value)
  if (next.has(number)) {
    next.delete(number)
  } else {
    next.add(number)
  }
  expanded.value = next
}

function isTruncated(error: string) {
  return error.length > ERROR_TRUNCATE_AT
}

function displayError(deployment: Deployment) {
  if (!isTruncated(deployment.error) || expanded.value.has(deployment.number)) {
    return deployment.error
  }
  return `${deployment.error.slice(0, ERROR_TRUNCATE_AT)}…`
}

// Build-log drawer: keyed by TanStack's row-id (see getRowId below, set
// to the deployment number) rather than row.toggleExpanded(), so opening
// one row's drawer collapses any other — running more than one build-log
// SSE stream at once per app isn't worth the complexity. A plain object
// (not a Set) because UTable's `expanded` v-model uses TanStack's
// ExpandedState shape (Record<string, boolean>).
const expandedState = ref<Record<string, boolean>>({})

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

// Which row's confirm popover is open, and which target deployment number
// a rollback is currently in flight for (kept separate from the popover's
// own open state so the popover can close immediately on confirm while the
// button keeps its loading state until the request settles — mirrors
// DomainsPanel's delete confirm).
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
  <UTable
    v-model:expanded="expandedState"
    :data="rows"
    :columns="columns"
    :get-row-id="(d: Deployment) => String(d.number)"
    empty="No deployments yet."
    class="w-full"
  >
    <template #number-cell="{ row }">
      <span class="font-mono text-sm text-content-secondary">#{{ row.original.number }}</span>
    </template>

    <template #image-cell="{ row }">
      <div class="flex flex-col gap-0.5">
        <span class="font-mono text-xs text-content-secondary">{{ row.original.image }}</span>
        <span class="text-[11px] text-content-muted">
          {{ row.original.source }} · {{ row.original.trigger }}
          <template v-if="row.original.source === 'git' && row.original.git_sha">
            · <span class="font-mono">{{ shortSha(row.original.git_sha) }}</span>
          </template>
        </span>
      </div>
    </template>

    <template #status-cell="{ row }">
      <StatusBadge :status="row.original.status" />
    </template>

    <template #started_at-cell="{ row }">
      <span class="text-xs text-content-secondary" :title="row.original.started_at">
        {{ relativeTime(row.original.started_at) }}
      </span>
    </template>

    <template #error-cell="{ row }">
      <button
        v-if="row.original.error"
        type="button"
        class="max-w-md text-left text-xs text-status-error hover:underline"
        :title="row.original.error"
        @click="toggle(row.original.number)"
      >
        {{ displayError(row.original) }}
      </button>
      <span v-else class="text-xs text-content-muted">—</span>
    </template>

    <template #actions-cell="{ row }">
      <div class="flex items-center justify-end gap-1.5">
        <UButton
          v-if="row.original.has_build_log"
          size="xs"
          color="neutral"
          variant="ghost"
          :icon="row.getIsExpanded() ? 'i-lucide-chevron-up' : 'i-lucide-terminal'"
          @click="toggleBuildLog(row.original.number)"
        >
          Build log
        </UButton>

        <UPopover
          v-if="canRollBackTo(row.original)"
          :open="confirmOpenNumber === row.original.number"
          @update:open="(v: boolean) => (confirmOpenNumber = v ? row.original.number : null)"
        >
          <UButton
            size="xs"
            color="neutral"
            variant="ghost"
            icon="i-lucide-history"
            :loading="rollbackMutation.isPending.value && rollingBackToNumber === row.original.number"
            :disabled="rollbackMutation.isPending.value"
          >
            Roll back
          </UButton>
          <template #content>
            <div class="flex flex-col gap-2 p-3">
              <p class="text-xs text-content-secondary">
                Roll back to deployment <span class="font-mono">#{{ row.original.number }}</span>?
              </p>
              <p class="max-w-64 text-xs text-content-muted">This redeploys the image from that deployment as a new deployment.</p>
              <div class="flex justify-end gap-2">
                <UButton size="xs" color="neutral" variant="ghost" @click="confirmOpenNumber = null">Cancel</UButton>
                <UButton
                  size="xs"
                  color="warning"
                  :loading="rollbackMutation.isPending.value && rollingBackToNumber === row.original.number"
                  @click="confirmRollback(row.original.number)"
                >
                  Roll back
                </UButton>
              </div>
            </div>
          </template>
        </UPopover>
      </div>
    </template>

    <template #expanded="{ row }">
      <BuildLogPanel :slug="slug" :deployment="row.original" />
    </template>
  </UTable>
</template>
