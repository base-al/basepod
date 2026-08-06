<script setup lang="ts">
// Settings -> Account: change-password form backed by POST /auth/password
// (internal/api/auth.go's handleChangePassword). A successful change
// revokes every OTHER session server-side while leaving this browser's own
// session alive — so unlike DomainsPanel/ResourceLimitsPanel this mutation
// also invalidates the sessions query, so SessionsPanel (rendered
// alongside this on the same page) immediately reflects the now-empty
// "other sessions" list without a manual refresh.
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, ApiError } from '../lib/api'

const toast = useToast()
const queryClient = useQueryClient()

const currentPassword = ref('')
const newPassword = ref('')
const confirmPassword = ref('')

const MIN_PASSWORD_LENGTH = 8

// Client-side checks mirror the server's own rules (see
// internal/server/setup.go's --admin-password validation, matched exactly
// by handleChangePassword) so a doomed submission is caught before the
// round trip — the server's response is still what's shown on a 401/422,
// never reworded.
const tooShort = computed(() => newPassword.value.length > 0 && newPassword.value.length < MIN_PASSWORD_LENGTH)
const mismatched = computed(() => confirmPassword.value.length > 0 && newPassword.value !== confirmPassword.value)
const canSubmit = computed(
  () =>
    currentPassword.value.length > 0 &&
    newPassword.value.length >= MIN_PASSWORD_LENGTH &&
    newPassword.value === confirmPassword.value,
)

const serverError = ref('')

function resetFields() {
  currentPassword.value = ''
  newPassword.value = ''
  confirmPassword.value = ''
}

const changeMutation = useMutation({
  mutationFn: () => api.changePassword({ current_password: currentPassword.value, new_password: newPassword.value }),
  onSuccess: () => {
    serverError.value = ''
    resetFields()
    void queryClient.invalidateQueries({ queryKey: ['sessions'] })
    toast.add({
      title: 'Password changed',
      description: 'Every other session has been signed out.',
      color: 'success',
      icon: 'i-lucide-check',
    })
  },
  onError: (err) => {
    // 401 invalid_credentials / 422 validation — surfaced verbatim, not
    // reworded, so the operator sees exactly what the server rejected.
    serverError.value = err instanceof ApiError ? err.message : 'Could not change password — try again'
  },
})

function submit() {
  if (!canSubmit.value || changeMutation.isPending.value) return
  serverError.value = ''
  changeMutation.mutate()
}
</script>

<template>
  <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
    <template #header>
      <h2 class="text-sm font-medium text-slate-400">Change password</h2>
    </template>

    <form class="flex flex-col gap-4" novalidate @submit.prevent="submit">
      <UAlert v-if="serverError" color="error" variant="subtle" :title="serverError" icon="i-lucide-alert-circle" />

      <UFormField label="Current password" name="current-password">
        <UInput
          v-model="currentPassword"
          type="password"
          autocomplete="current-password"
          placeholder="••••••••"
          class="w-full"
          :disabled="changeMutation.isPending.value"
          required
        />
      </UFormField>

      <UFormField label="New password" name="new-password">
        <UInput
          v-model="newPassword"
          type="password"
          autocomplete="new-password"
          placeholder="••••••••"
          class="w-full"
          :color="tooShort ? 'error' : undefined"
          :disabled="changeMutation.isPending.value"
          required
        />
        <p v-if="tooShort" class="mt-1 text-xs text-red-400">Must be at least {{ MIN_PASSWORD_LENGTH }} characters.</p>
      </UFormField>

      <UFormField label="Confirm new password" name="confirm-password">
        <UInput
          v-model="confirmPassword"
          type="password"
          autocomplete="new-password"
          placeholder="••••••••"
          class="w-full"
          :color="mismatched ? 'error' : undefined"
          :disabled="changeMutation.isPending.value"
          required
        />
        <p v-if="mismatched" class="mt-1 text-xs text-red-400">Passwords don't match.</p>
      </UFormField>

      <div class="flex justify-end">
        <UButton
          type="submit"
          color="primary"
          icon="i-lucide-key-round"
          :loading="changeMutation.isPending.value"
          :disabled="!canSubmit || changeMutation.isPending.value"
        >
          Change password
        </UButton>
      </div>
    </form>
  </UCard>
</template>
