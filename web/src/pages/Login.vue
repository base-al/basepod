<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'

import { useAuthStore } from '../stores/auth'
import { ApiError } from '../lib/api'
import BasepodWordmark from '../components/BasepodWordmark.vue'

const auth = useAuthStore()
const router = useRouter()

const email = ref('')
const password = ref('')
const errorMessage = ref('')

// Only these two API error codes are documented for /auth/login; anything
// else falls back to the server's own message.
const ERROR_COPY: Record<string, string> = {
  invalid_credentials: 'Invalid email or password',
  rate_limited: 'Too many attempts — wait a minute',
}

async function onSubmit() {
  if (auth.pending) return
  errorMessage.value = ''
  try {
    await auth.login(email.value, password.value)
    await router.push({ name: 'apps' })
  } catch (err) {
    errorMessage.value =
      err instanceof ApiError ? (ERROR_COPY[err.code] ?? err.message) : 'Something went wrong — try again'
  }
}
</script>

<template>
  <div class="login-grid flex min-h-screen flex-col items-center justify-center gap-8 bg-surface px-4">
    <div class="flex flex-col items-center gap-2 text-center">
      <BasepodWordmark :height="26" />
      <p class="max-w-xs text-sm text-content-secondary">Your server. Your data. No vendor.</p>
    </div>

    <UCard variant="subtle" class="w-full max-w-sm" :ui="{ root: 'ring-line' }">
      <form class="flex flex-col gap-4" novalidate @submit.prevent="onSubmit">
        <UFormField label="Email" name="email">
          <UInput
            v-model="email"
            type="email"
            autocomplete="username"
            placeholder="you@example.com"
            class="w-full"
            :disabled="auth.pending"
            required
          />
        </UFormField>

        <UFormField label="Password" name="password">
          <UInput
            v-model="password"
            type="password"
            autocomplete="current-password"
            placeholder="••••••••"
            class="w-full"
            :disabled="auth.pending"
            required
          />
        </UFormField>

        <UAlert
          v-if="errorMessage"
          color="error"
          variant="subtle"
          :title="errorMessage"
          icon="i-lucide-alert-circle"
        />

        <UButton type="submit" color="primary" block class="tap44" :loading="auth.pending" :disabled="auth.pending">
          Sign in
        </UButton>
      </form>
    </UCard>
  </div>
</template>
