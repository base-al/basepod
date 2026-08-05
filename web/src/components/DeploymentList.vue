<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'
import type { TableColumn } from '@nuxt/ui'

import { api, ApiError, type Deployment } from '../lib/api'
import { relativeTime } from '../lib/relativeTime'
import StatusBadge from './StatusBadge.vue'

const props = defineProps<{ slug: string; deployments: Deployment[] }>()

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
  <UTable :data="rows" :columns="columns" empty="No deployments yet." class="w-full">
    <template #number-cell="{ row }">
      <span class="font-mono text-sm text-slate-400">#{{ row.original.number }}</span>
    </template>

    <template #image-cell="{ row }">
      <div class="flex flex-col gap-0.5">
        <span class="font-mono text-xs text-slate-300">{{ row.original.image }}</span>
        <span class="text-[11px] text-slate-500">{{ row.original.source }} · {{ row.original.trigger }}</span>
      </div>
    </template>

    <template #status-cell="{ row }">
      <StatusBadge :status="row.original.status" />
    </template>

    <template #started_at-cell="{ row }">
      <span class="text-xs text-slate-400" :title="row.original.started_at">
        {{ relativeTime(row.original.started_at) }}
      </span>
    </template>

    <template #error-cell="{ row }">
      <button
        v-if="row.original.error"
        type="button"
        class="max-w-md text-left text-xs text-red-400 hover:underline"
        :title="row.original.error"
        @click="toggle(row.original.number)"
      >
        {{ displayError(row.original) }}
      </button>
      <span v-else class="text-xs text-slate-600">—</span>
    </template>

    <template #actions-cell="{ row }">
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
            <p class="text-xs text-slate-300">
              Roll back to deployment <span class="font-mono">#{{ row.original.number }}</span>?
            </p>
            <p class="max-w-64 text-xs text-slate-500">This redeploys the image from that deployment as a new deployment.</p>
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
    </template>
  </UTable>
</template>
