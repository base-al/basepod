<script setup lang="ts">
import { computed } from 'vue'
import { useRouter } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import type { TableColumn } from '@nuxt/ui'

import { api, type App } from '../lib/api'
import { useAuthStore } from '../stores/auth'
import { useColorMode } from '../composables/useColorMode'
import StatusBadge from '../components/StatusBadge.vue'
import ImageRef from '../components/ImageRef.vue'

const auth = useAuthStore()
const router = useRouter()
const { mode, toggle } = useColorMode()

// POLL_INTERVAL_MS: apps' deploy status changes over time (created ->
// deploying -> running/error) without any push channel in this milestone,
// so both queries poll rather than fetch once.
const POLL_INTERVAL_MS = 5000

const appsQuery = useQuery({
  queryKey: ['apps'],
  queryFn: () => api.listApps(),
  refetchInterval: POLL_INTERVAL_MS,
})

const systemQuery = useQuery({
  queryKey: ['system'],
  queryFn: () => api.system(),
  refetchInterval: POLL_INTERVAL_MS,
})

const apps = computed(() => appsQuery.data.value ?? [])
const podmanOk = computed(() => systemQuery.data.value?.podman === 'ok')

const columns: TableColumn<App>[] = [
  { accessorKey: 'slug', header: 'Slug' },
  { accessorKey: 'status', header: 'Status' },
  { accessorKey: 'image', header: 'Image' },
  { accessorKey: 'port', header: 'Port' },
]

async function handleLogout() {
  await auth.logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <div class="min-h-screen bg-slate-950">
    <header class="sticky top-0 z-10 border-b border-slate-800 bg-slate-950/90 backdrop-blur">
      <div class="mx-auto flex max-w-6xl items-center justify-between gap-4 px-6 py-3">
        <div class="flex items-center gap-2">
          <span class="h-2.5 w-2.5 rounded-full bg-emerald-400" aria-hidden="true" />
          <span class="text-base font-semibold tracking-tight text-slate-100">BasePod</span>
        </div>

        <div class="flex items-center gap-3">
          <div
            class="flex items-center gap-2 rounded-full border border-slate-800 bg-slate-900/60 px-3 py-1 text-xs text-slate-400"
          >
            <span
              class="h-1.5 w-1.5 shrink-0 rounded-full"
              :class="podmanOk ? 'bg-emerald-400' : 'bg-red-400'"
              aria-hidden="true"
            />
            <template v-if="systemQuery.data.value">
              <span class="font-mono text-slate-300">v{{ systemQuery.data.value.version }}</span>
              <span v-if="!podmanOk" class="text-red-400">{{ systemQuery.data.value.podman }}</span>
            </template>
            <span v-else-if="systemQuery.isError.value" class="text-red-400">unreachable</span>
            <span v-else>checking…</span>
          </div>

          <UButton to="/apps/new" color="primary" variant="soft" icon="i-lucide-plus" size="sm">
            New app
          </UButton>

          <UButton
            color="neutral"
            variant="ghost"
            square
            size="sm"
            :icon="mode === 'dark' ? 'i-lucide-sun' : 'i-lucide-moon'"
            :aria-label="mode === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'"
            @click="toggle"
          />

          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-log-out" @click="handleLogout">
            Logout
          </UButton>
        </div>
      </div>
    </header>

    <main class="mx-auto max-w-6xl px-6 py-8">
      <div class="mb-4 flex items-center justify-between">
        <h1 class="text-sm font-medium text-slate-400">Apps</h1>
      </div>

      <UAlert
        v-if="appsQuery.isError.value"
        color="error"
        variant="subtle"
        title="Couldn't load apps"
        description="Check that the BasePod server is running and reachable."
        class="mb-4"
      />

      <div
        v-if="appsQuery.isPending.value"
        class="flex items-center justify-center rounded-lg border border-slate-800 py-24 text-sm text-slate-500"
      >
        Loading apps…
      </div>

      <div
        v-else-if="apps.length === 0"
        class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-slate-800 py-24 text-center"
      >
        <p class="text-sm text-slate-400">No apps deployed yet.</p>
        <UButton to="/apps/new" color="primary" icon="i-lucide-plus">New app</UButton>
      </div>

      <UTable v-else :data="apps" :columns="columns" class="w-full">
        <template #slug-cell="{ row }">
          <span class="font-mono text-sm text-slate-200">{{ row.original.slug }}</span>
        </template>

        <template #status-cell="{ row }">
          <StatusBadge :status="row.original.status" />
        </template>

        <template #image-cell="{ row }">
          <ImageRef :value="row.original.image" class="max-w-xs" />
        </template>

        <template #port-cell="{ row }">
          <span class="font-mono text-sm text-slate-400">{{ row.original.port }}</span>
        </template>
      </UTable>
    </main>
  </div>
</template>
