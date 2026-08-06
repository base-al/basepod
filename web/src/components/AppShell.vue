<script setup lang="ts">
// Persistent app chrome shared by every authenticated page (Apps,
// Settings, AppDetail): wordmark, primary nav, system health chip, theme
// toggle, and logout. Before this component existed each page rolled its
// own <header>, so /settings — the only place to change the admin
// password or revoke a session — had no way back to the rest of the app
// short of typing a URL. Centralizing this also means the health chip's
// version/podman polling (and its query cache) is shared across pages
// instead of re-mounted per page.
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'

import { api } from '../lib/api'
import { formatVersion } from '../lib/version'
import { useAuthStore } from '../stores/auth'
import { useColorMode } from '../composables/useColorMode'

const props = withDefaults(defineProps<{ maxWidth?: '3xl' | '5xl' | '6xl' | '7xl' }>(), { maxWidth: '6xl' })

// Tailwind's build-time scanner needs each candidate class to appear as a
// literal string somewhere in source — `max-w-${maxWidth}` would never be
// generated. This table gives it that literal per option instead.
const MAX_WIDTH_CLASS = {
  '3xl': 'max-w-3xl',
  '5xl': 'max-w-5xl',
  '6xl': 'max-w-6xl',
  '7xl': 'max-w-7xl',
} as const

const maxWidthClass = computed(() => MAX_WIDTH_CLASS[props.maxWidth])

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()
const { mode, toggle } = useColorMode()

// Every page that mounts AppShell shares this one query (same key), so
// the 5s poll and its cached result are shared across navigation rather
// than restarted per page.
const SYSTEM_POLL_INTERVAL_MS = 5000

const systemQuery = useQuery({
  queryKey: ['system'],
  queryFn: () => api.system(),
  refetchInterval: SYSTEM_POLL_INTERVAL_MS,
})

const podmanOk = computed(() => systemQuery.data.value?.podman === 'ok')

const navItems = [
  { label: 'Apps', to: { name: 'apps' }, matches: ['apps', 'new-app', 'app-detail'] },
  { label: 'Settings', to: { name: 'settings' }, matches: ['settings'] },
] as const

function isActive(matches: readonly string[]) {
  return typeof route.name === 'string' && matches.includes(route.name)
}

async function handleLogout() {
  await auth.logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <div class="min-h-screen bg-slate-950">
    <header class="sticky top-0 z-20 border-b border-slate-800 bg-slate-950/90 backdrop-blur">
      <div class="mx-auto flex h-14 max-w-7xl items-center gap-2 px-4 sm:gap-6 sm:px-6">
        <RouterLink
          :to="{ name: 'apps' }"
          class="flex shrink-0 items-center gap-2 rounded-md py-1 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400"
        >
          <span class="h-2.5 w-2.5 rounded-full bg-emerald-400" aria-hidden="true" />
          <span class="text-base font-semibold tracking-tight text-slate-100">BasePod</span>
        </RouterLink>

        <nav class="flex items-center gap-1" aria-label="Primary">
          <RouterLink
            v-for="item in navItems"
            :key="item.label"
            :to="item.to"
            class="rounded-md px-2.5 py-1.5 text-sm font-medium transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400 sm:px-3"
            :class="
              isActive(item.matches)
                ? 'bg-slate-800/80 text-slate-100'
                : 'text-slate-400 hover:bg-slate-900 hover:text-slate-200'
            "
            :aria-current="isActive(item.matches) ? 'page' : undefined"
          >
            {{ item.label }}
          </RouterLink>
        </nav>

        <div class="ml-auto flex items-center gap-1.5 sm:gap-2">
          <div
            class="hidden items-center gap-2 rounded-full border border-slate-800 bg-slate-900/60 px-3 py-1 text-xs text-slate-400 sm:flex"
            :title="systemQuery.data.value ? `podman: ${systemQuery.data.value.podman}` : undefined"
          >
            <span
              class="h-1.5 w-1.5 shrink-0 rounded-full"
              :class="podmanOk ? 'bg-emerald-400' : 'bg-red-400'"
              aria-hidden="true"
            />
            <template v-if="systemQuery.data.value">
              <span class="font-mono text-slate-300">{{ formatVersion(systemQuery.data.value.version) }}</span>
              <span v-if="!podmanOk" class="text-red-400">{{ systemQuery.data.value.podman }}</span>
            </template>
            <span v-else-if="systemQuery.isError.value" class="text-red-400">unreachable</span>
            <span v-else class="text-slate-500">checking…</span>
          </div>

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
            <span class="hidden sm:inline">Logout</span>
          </UButton>
        </div>
      </div>
    </header>

    <main class="mx-auto px-4 py-6 sm:px-6 sm:py-8" :class="maxWidthClass">
      <slot />
    </main>
  </div>
</template>
