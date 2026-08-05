<script setup lang="ts">
import { computed, ref } from 'vue'
import type { TableColumn } from '@nuxt/ui'

import type { Deployment } from '../lib/api'
import { relativeTime } from '../lib/relativeTime'
import StatusBadge from './StatusBadge.vue'

const props = defineProps<{ deployments: Deployment[] }>()

// The API already returns deployments newest-first (ORDER BY number DESC
// in internal/store.ListDeployments), but sort defensively rather than
// assume server ordering forever.
const rows = computed(() => [...props.deployments].sort((a, b) => b.number - a.number))

const columns: TableColumn<Deployment>[] = [
  { accessorKey: 'number', header: '#' },
  { accessorKey: 'image', header: 'Image' },
  { accessorKey: 'status', header: 'Status' },
  { accessorKey: 'started_at', header: 'Started' },
  { accessorKey: 'error', header: 'Error' },
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
</script>

<template>
  <UTable :data="rows" :columns="columns" empty="No deployments yet." class="w-full">
    <template #number-cell="{ row }">
      <span class="font-mono text-sm text-slate-400">#{{ row.original.number }}</span>
    </template>

    <template #image-cell="{ row }">
      <span class="font-mono text-xs text-slate-300">{{ row.original.image }}</span>
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
  </UTable>
</template>
