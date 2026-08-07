<script setup lang="ts">
// Settings' own layout: AppShell (the app-wide rail/top-bar/bottom-bar)
// plus a second-level nav for the three settings sub-pages (IA reorg —
// Settings used to be one flat page; it's now Account/Instance/Users,
// each a real route under /settings/*, see router.ts).
//
// Two structurally different sub-nav treatments, same reasoning as
// AppShell's own header split: desktop gets a narrow side column (enough
// horizontal room that a vertical list reads naturally, and it sits next
// to content the way the concept's own console rail does); phone gets a
// row of wrapping pills, NOT a second horizontally-scrolling strip — this
// page already lives inside AppShell's outer chrome, and a scroll-strip
// nested inside another scroll-strip is exactly the pattern the IA reorg
// was asked to avoid. Three items comfortably wrap to one line at 390px,
// so `flex-wrap` alone is enough; there's no need for a select/dropdown.
import { RouterLink, useRoute } from 'vue-router'

import AppShell from './AppShell.vue'

const route = useRoute()

const items = [
  { label: 'Account', icon: 'i-lucide-user-round', to: { name: 'settings-account' } },
  { label: 'Instance', icon: 'i-lucide-server', to: { name: 'settings-instance' } },
  { label: 'Users', icon: 'i-lucide-users', to: { name: 'settings-users' } },
] as const

function isActive(name: string) {
  return route.name === name
}
</script>

<template>
  <AppShell max-width="5xl">
    <div class="mb-6">
      <h1 class="font-mono text-2xl font-semibold tracking-tight text-content-primary">Settings</h1>
      <p class="mt-1 text-sm text-content-secondary">Account, instance, and access.</p>
    </div>

    <div class="md:flex md:items-start md:gap-8">
      <!-- Desktop / tablet: narrow side column, sits alongside content. -->
      <nav class="hidden shrink-0 flex-col gap-1 md:flex md:w-44" aria-label="Settings sections">
        <RouterLink
          v-for="item in items"
          :key="item.label"
          :to="item.to"
          class="tap44 flex items-center gap-2.5 rounded-md border-l-2 px-3 py-2 text-sm font-medium transition-colors duration-180 ease-out-snap focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
          :class="
            isActive(item.to.name)
              ? 'border-accent bg-accent/10 text-content-primary'
              : 'border-transparent text-content-secondary hover:bg-surface-elevated/60 hover:text-content-primary'
          "
          :aria-current="isActive(item.to.name) ? 'page' : undefined"
        >
          <UIcon :name="item.icon" class="h-4 w-4 shrink-0" aria-hidden="true" />
          {{ item.label }}
        </RouterLink>
      </nav>

      <!-- Phone / narrow tablet: wrapping pill row — never a second
           horizontally-scrolling strip nested inside AppShell's own
           chrome. -->
      <nav class="mb-6 flex flex-wrap gap-2 md:hidden" aria-label="Settings sections">
        <RouterLink
          v-for="item in items"
          :key="item.label"
          :to="item.to"
          class="tap44 flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-sm font-medium transition-colors duration-180 ease-out-snap focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
          :class="
            isActive(item.to.name)
              ? 'border-accent bg-accent/10 text-content-primary'
              : 'border-line text-content-secondary hover:bg-surface-elevated/60 hover:text-content-primary'
          "
          :aria-current="isActive(item.to.name) ? 'page' : undefined"
        >
          <UIcon :name="item.icon" class="h-4 w-4 shrink-0" aria-hidden="true" />
          {{ item.label }}
        </RouterLink>
      </nav>

      <div class="min-w-0 flex-1">
        <slot />
      </div>
    </div>
  </AppShell>
</template>
