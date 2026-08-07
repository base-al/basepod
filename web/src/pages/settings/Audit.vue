<script setup lang="ts">
// Settings -> Audit: a read-only log of who did what, backed by GET
// /audit (internal/api's handler, api/openapi.yaml's `audit:read`
// capability — admin floor, same as Users.vue's own gate). Same
// meQuery-driven access-denied treatment as Users.vue rather than a
// shared component: the two pages' gating logic is identical in shape
// but not meaningfully large enough to be worth abstracting out from
// two call sites.
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'

import { api, ApiError } from '../../lib/api'
import { relativeTime } from '../../lib/relativeTime'
import { ROLE_LABELS, isAdminOrAbove } from '../../lib/roles'
import SettingsShell from '../../components/SettingsShell.vue'

const meQuery = useQuery({ queryKey: ['auth-me'], queryFn: () => api.me() })
const currentRole = computed(() => meQuery.data.value?.role ?? null)
const canView = computed(() => currentRole.value !== null && isAdminOrAbove(currentRole.value))

const auditQuery = useQuery({
  queryKey: ['audit'],
  queryFn: () => api.listAudit(),
  enabled: canView,
})

const entries = computed(() => auditQuery.data.value ?? [])

const forbidden = computed(
  () => auditQuery.isError.value && auditQuery.error.value instanceof ApiError && auditQuery.error.value.status === 403,
)
const loadError = computed(() => auditQuery.isError.value && !forbidden.value)
</script>

<template>
  <SettingsShell>
    <div class="mb-6">
      <h2 class="font-mono text-lg font-semibold tracking-tight text-content-primary">Audit</h2>
      <p class="mt-1 text-sm text-content-secondary">The most recent actions taken on this instance, newest first.</p>
    </div>

    <div v-if="meQuery.isPending.value" class="flex items-center justify-center rounded-lg border border-line py-16 text-sm text-content-muted">
      Loading…
    </div>

    <UAlert
      v-else-if="meQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't verify your access"
      description="Check that the BasePod server is running and reachable, then reload."
    />

    <div
      v-else-if="!canView"
      class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-24 text-center"
    >
      <UIcon name="i-lucide-lock" class="h-8 w-8 text-content-muted" aria-hidden="true" />
      <p class="text-sm font-medium text-content-secondary">You don't have access to this page</p>
      <p class="max-w-sm text-sm text-content-muted">
        Viewing the audit log requires the admin role or above.
        <template v-if="currentRole">Your current role is <span class="font-mono text-content-secondary">{{ ROLE_LABELS[currentRole] }}</span>.</template>
        Ask an owner or admin if you need access.
      </p>
    </div>

    <template v-else>
      <UAlert
        v-if="forbidden"
        color="error"
        variant="subtle"
        title="You don't have access to this page"
        description="Your role no longer permits viewing the audit log — sign out and back in, or ask an owner to check your role."
      />

      <UAlert
        v-else-if="loadError"
        color="error"
        variant="subtle"
        title="Couldn't load the audit log"
        :description="auditQuery.error.value instanceof ApiError ? auditQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
      />

      <div v-else-if="auditQuery.isPending.value" class="flex flex-col gap-3" aria-busy="true" aria-label="Loading audit log">
        <div v-for="n in 4" :key="n" class="flex items-center gap-4 rounded-lg border border-line px-4 py-4 sm:px-5">
          <div class="h-3.5 w-32 animate-pulse rounded bg-line" />
          <div class="hidden h-3.5 flex-1 animate-pulse rounded bg-line lg:block" />
          <div class="h-3.5 w-20 animate-pulse rounded bg-line" />
        </div>
      </div>

      <div
        v-else-if="entries.length === 0"
        class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-24 text-center"
      >
        <UIcon name="i-lucide-scroll-text" class="h-8 w-8 text-content-muted" aria-hidden="true" />
        <p class="text-sm font-medium text-content-secondary">Nothing recorded yet</p>
        <p class="max-w-sm text-sm text-content-muted">Deploys, app changes, and user/role changes will show up here as they happen.</p>
      </div>

      <template v-else>
        <!-- Wide desktop only (lg: — see Users.vue's own doc comment on
             why this settings-shell content area needs a taller
             breakpoint than Apps.vue's md: before a table fits). -->
        <div class="hidden overflow-hidden rounded-lg border border-line lg:block">
          <div
            class="grid grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)_auto] items-center gap-4 border-b border-line bg-surface-elevated/40 px-5 py-2.5 text-xs font-medium uppercase tracking-wide text-content-muted"
          >
            <span>Who</span>
            <span>Action</span>
            <span>Target</span>
            <span>When</span>
          </div>

          <div v-for="entry in entries" :key="entry.id" class="grid grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)_auto] items-center gap-4 border-b border-line px-5 py-3 last:border-b-0">
            <span class="truncate font-mono text-xs text-content-secondary">{{ entry.actor_email || 'system' }}</span>
            <div class="min-w-0">
              <span class="block truncate font-mono text-xs font-medium text-content-primary">{{ entry.action }}</span>
              <span v-if="entry.detail" class="block truncate text-xs text-content-muted">{{ entry.detail }}</span>
            </div>
            <span class="truncate font-mono text-xs text-content-secondary">{{ entry.target || '—' }}</span>
            <span class="font-mono text-xs whitespace-nowrap text-content-muted" :title="entry.created_at">{{ relativeTime(entry.created_at) }}</span>
          </div>
        </div>

        <!-- Phone through tablet (< lg): cards, same reasoning as
             Users.vue. -->
        <div class="flex flex-col gap-3 lg:hidden">
          <div v-for="entry in entries" :key="entry.id" class="flex flex-col gap-2 rounded-lg border border-line p-4">
            <div class="flex items-center justify-between gap-3">
              <span class="truncate font-mono text-xs font-medium text-content-primary">{{ entry.action }}</span>
              <span class="shrink-0 font-mono text-xs text-content-muted" :title="entry.created_at">{{ relativeTime(entry.created_at) }}</span>
            </div>
            <p v-if="entry.detail" class="text-xs text-content-secondary">{{ entry.detail }}</p>
            <div class="flex items-center justify-between text-xs text-content-muted">
              <span class="font-mono">{{ entry.actor_email || 'system' }}</span>
              <span v-if="entry.target" class="truncate font-mono">→ {{ entry.target }}</span>
            </div>
          </div>
        </div>
      </template>
    </template>
  </SettingsShell>
</template>
