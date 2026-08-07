<script setup lang="ts">
// Persistent app chrome shared by every authenticated page (Apps,
// Settings, AppDetail): wordmark, primary nav, system health, theme
// toggle, and logout. Before this component existed each page rolled its
// own <header>, so /settings — the only place to change the admin
// password or revoke a session — had no way back to the rest of the app
// short of typing a URL. Centralizing this also means the health chip's
// version/podman polling (and its query cache) is shared across pages
// instead of re-mounted per page.
//
// Two structurally different layouts, not one shrinking down to the
// other: a fixed left rail on desktop (matching the approved identity
// concept's console layout), and a compact top bar + bottom tab bar on
// phone — a rail that just gets narrower stops being usable well before
// phone width, and a bottom bar keeps primary navigation reachable with
// a thumb. Settings ALWAYS renders with its own visible text label in
// both layouts, never as an icon alone — v0.4.2 shipped a bug where an
// icon-only Settings gear silently failed to render and made the
// account screen unreachable with no visible sign anything was even
// there. That's policy now, not a one-off fix.
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'

import { api } from '../lib/api'
import { formatVersion } from '../lib/version'
import { useAuthStore } from '../stores/auth'
import { useColorMode } from '../composables/useColorMode'
import BasepodWordmark from './BasepodWordmark.vue'
import BasepodMark from './BasepodMark.vue'

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
  { label: 'Apps', icon: 'i-lucide-boxes', to: { name: 'apps' }, matches: ['apps', 'new-app', 'app-detail'] },
  {
    label: 'Settings',
    icon: 'i-lucide-settings',
    to: { name: 'settings-account' },
    // Every settings sub-route (IA reorg) still lights up this one rail/
    // bottom-bar item — the sub-nav inside SettingsShell.vue is what
    // distinguishes Account/Instance/Users from here.
    matches: ['settings-account', 'settings-instance', 'settings-users'],
  },
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
  <div class="min-h-screen bg-surface md:flex">
    <!-- Desktop / tablet: fixed left rail. Sunken so it visually recedes
         behind the raised content it frames, matching the approved
         concept's console layout. -->
    <aside class="hidden shrink-0 md:flex md:w-56 md:flex-col md:border-r md:border-line md:bg-surface-sunken">
      <RouterLink
        :to="{ name: 'apps' }"
        aria-label="BasePod — home"
        class="flex items-center px-5 py-5 transition-opacity duration-120 ease-out-snap hover:opacity-85 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
      >
        <BasepodWordmark :height="18" aria-hidden="true" />
      </RouterLink>

      <nav class="flex flex-col px-2 pointer-coarse:gap-1" aria-label="Primary">
        <RouterLink
          v-for="item in navItems"
          :key="item.label"
          :to="item.to"
          class="tap44 flex items-center gap-2.5 rounded-md border-l-2 px-3 py-2 text-sm font-medium transition-colors duration-180 ease-out-snap focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
          :class="
            isActive(item.matches)
              ? 'border-accent bg-accent/10 text-content-primary'
              : 'border-transparent text-content-secondary hover:bg-surface-elevated/60 hover:text-content-primary'
          "
          :aria-current="isActive(item.matches) ? 'page' : undefined"
        >
          <UIcon :name="item.icon" class="h-4 w-4 shrink-0" aria-hidden="true" />
          {{ item.label }}
        </RouterLink>
      </nav>

      <div class="mt-auto flex flex-col gap-3 border-t border-line px-4 py-4">
        <div
          class="flex items-center gap-2 font-mono text-xs text-content-secondary"
          :title="systemQuery.data.value ? `podman: ${systemQuery.data.value.podman}` : undefined"
        >
          <span
            class="h-1.5 w-1.5 shrink-0 rounded-full"
            :class="podmanOk ? 'bg-moss-400' : 'bg-crimson-400'"
            aria-hidden="true"
          />
          <template v-if="systemQuery.data.value">
            <span class="text-content-primary">{{ formatVersion(systemQuery.data.value.version) }}</span>
            <span v-if="!podmanOk" class="text-status-error">{{ systemQuery.data.value.podman }}</span>
          </template>
          <span v-else-if="systemQuery.isError.value" class="text-status-error">unreachable</span>
          <span v-else class="text-content-muted">checking…</span>
        </div>

        <div class="tap-row flex items-center gap-1">
          <UButton
            color="neutral"
            variant="ghost"
            square
            size="sm"
            class="tap44"
            :icon="mode === 'dark' ? 'i-lucide-sun' : 'i-lucide-moon'"
            :aria-label="mode === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'"
            @click="toggle"
          />
          <UButton color="neutral" variant="ghost" size="sm" class="tap44" icon="i-lucide-log-out" @click="handleLogout">
            Logout
          </UButton>
        </div>
      </div>
    </aside>

    <!-- Phone / narrow tablet: compact top bar (identity + health +
         quick actions) — navigation itself lives in the bottom tab bar
         below, not here, so it survives the keyboard opening over a
         form higher up the page. -->
    <header
      class="sticky top-0 z-20 flex h-14 items-center gap-2 border-b border-line bg-surface/95 px-4 backdrop-blur md:hidden"
      style="padding-top: env(safe-area-inset-top)"
    >
      <RouterLink
        :to="{ name: 'apps' }"
        aria-label="BasePod — home"
        class="tap44 flex shrink-0 items-center gap-2 rounded-md py-1 transition-opacity duration-120 ease-out-snap hover:opacity-85 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
      >
        <BasepodMark :size="22" :tone="podmanOk ? 'accent' : 'error'" aria-hidden="true" />
        <span class="font-mono text-sm font-semibold text-content-primary">basepod</span>
      </RouterLink>

      <span
        class="ml-1 h-1.5 w-1.5 shrink-0 rounded-full"
        :class="podmanOk ? 'bg-moss-400' : 'bg-crimson-400'"
        :aria-label="podmanOk ? 'Podman: ok' : 'Podman: unreachable'"
        role="img"
      />

      <div class="tap-row ml-auto flex items-center gap-1">
        <UButton
          color="neutral"
          variant="ghost"
          square
          size="sm"
          class="tap44"
          :icon="mode === 'dark' ? 'i-lucide-sun' : 'i-lucide-moon'"
          :aria-label="mode === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'"
          @click="toggle"
        />
        <UButton
          color="neutral"
          variant="ghost"
          square
          size="sm"
          class="tap44"
          icon="i-lucide-log-out"
          aria-label="Logout"
          @click="handleLogout"
        />
      </div>
    </header>

    <div class="flex min-h-screen flex-1 flex-col md:min-h-0">
      <main class="mx-auto w-full flex-1 px-4 py-6 pb-24 sm:px-6 sm:py-8 md:pb-8" :class="maxWidthClass">
        <slot />
      </main>
    </div>

    <!-- Phone / narrow tablet: bottom tab bar. Every item keeps a
         visible text label — never icon-only — see the note at the top
         of this file for why that's a hard rule here, not a preference. -->
    <nav
      class="pb-safe fixed inset-x-0 bottom-0 z-20 flex border-t border-line bg-surface-elevated md:hidden"
      aria-label="Primary"
    >
      <RouterLink
        v-for="item in navItems"
        :key="item.label"
        :to="item.to"
        class="flex min-h-[56px] flex-1 flex-col items-center justify-center gap-1 text-xs font-medium transition-colors duration-180 ease-out-snap focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent"
        :class="isActive(item.matches) ? 'text-accent' : 'text-content-secondary'"
        :aria-current="isActive(item.matches) ? 'page' : undefined"
      >
        <UIcon :name="item.icon" class="h-5 w-5" aria-hidden="true" />
        {{ item.label }}
      </RouterLink>
    </nav>
  </div>
</template>
