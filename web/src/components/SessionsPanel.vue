<script setup lang="ts">
// Settings -> Account: lists every live session for the current user
// (GET /auth/sessions) with a revoke action per row (DELETE
// /auth/sessions/{id}). Shares the ['sessions'] query key with
// ChangePasswordForm.vue, which invalidates it on a successful password
// change so this list reflects the now-revoked "other sessions" without a
// manual refresh.
import { ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'
import { useRouter } from 'vue-router'
import type { TableColumn } from '@nuxt/ui'

import { api, ApiError, type Session } from '../lib/api'
import { useAuthStore } from '../stores/auth'
import { relativeTime } from '../lib/relativeTime'

const toast = useToast()
const queryClient = useQueryClient()
const auth = useAuthStore()
const router = useRouter()

const sessionsQuery = useQuery({
  queryKey: ['sessions'],
  queryFn: () => api.listSessions(),
})

const columns: TableColumn<Session>[] = [
  { accessorKey: 'created_at', header: 'Created' },
  { accessorKey: 'expires_at', header: 'Expires' },
  { id: 'device', header: '' },
  { id: 'actions', header: '' },
]

function invalidate() {
  return queryClient.invalidateQueries({ queryKey: ['sessions'] })
}

const confirmOpenId = ref<number | null>(null)
const revokingId = ref<number | null>(null)

const revokeMutation = useMutation({
  mutationFn: (session: Session) => api.deleteSession(session.id).then(() => session),
  onSuccess: async (session) => {
    confirmOpenId.value = null
    revokingId.value = null

    // Revoking the session this browser tab is itself using logs the user
    // out immediately — the server has already dropped it, so there's
    // nothing left to invalidate() for; go straight to /login rather than
    // refetching a session list this tab can no longer authenticate for.
    if (session.current) {
      auth.clearSession()
      await router.push({ name: 'login' })
      return
    }

    await invalidate()
    toast.add({ title: 'Session revoked', color: 'success', icon: 'i-lucide-check' })
  },
  onError: (err) => {
    confirmOpenId.value = null
    revokingId.value = null
    toast.add({
      title: 'Could not revoke session',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

function confirmRevoke(session: Session) {
  revokingId.value = session.id
  revokeMutation.mutate(session)
}
</script>

<template>
  <UCard variant="subtle" :ui="{ root: 'ring-line' }">
    <template #header>
      <h2 class="text-sm font-medium text-content-secondary">Sessions</h2>
    </template>

    <UAlert
      v-if="sessionsQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't load sessions"
      :description="sessionsQuery.error.value instanceof ApiError ? sessionsQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
    />

    <div v-else-if="sessionsQuery.isPending.value" class="flex items-center justify-center py-10 text-sm text-content-muted">
      Loading sessions…
    </div>

    <UTable v-else :data="sessionsQuery.data.value ?? []" :columns="columns" empty="No active sessions." class="w-full">
      <template #created_at-cell="{ row }">
        <span class="text-xs text-content-secondary" :title="row.original.created_at">{{ relativeTime(row.original.created_at) }}</span>
      </template>

      <template #expires_at-cell="{ row }">
        <span class="text-xs text-content-secondary" :title="row.original.expires_at">{{ relativeTime(row.original.expires_at) }}</span>
      </template>

      <template #device-cell="{ row }">
        <UBadge v-if="row.original.current" color="primary" variant="subtle">This device</UBadge>
      </template>

      <template #actions-cell="{ row }">
        <div class="flex justify-end">
          <UPopover
            :open="confirmOpenId === row.original.id"
            @update:open="(v: boolean) => (confirmOpenId = v ? row.original.id : null)"
          >
            <UButton size="xs" color="error" variant="ghost" icon="i-lucide-log-out" :disabled="revokeMutation.isPending.value">
              Revoke
            </UButton>
            <template #content>
              <div class="flex flex-col gap-2 p-3">
                <p class="max-w-64 text-xs text-content-secondary">
                  <template v-if="row.original.current">Revoke this session? You'll be signed out immediately.</template>
                  <template v-else>Revoke this session?</template>
                </p>
                <div class="flex justify-end gap-2">
                  <UButton size="xs" color="neutral" variant="ghost" @click="confirmOpenId = null">Cancel</UButton>
                  <UButton
                    size="xs"
                    color="error"
                    :loading="revokeMutation.isPending.value && revokingId === row.original.id"
                    @click="confirmRevoke(row.original)"
                  >
                    Revoke
                  </UButton>
                </div>
              </div>
            </template>
          </UPopover>
        </div>
      </template>
    </UTable>
  </UCard>
</template>
