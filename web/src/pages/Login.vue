<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'

import { useAuthStore } from '../stores/auth'
import { ApiError } from '../lib/api'

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
  <div class="flex min-h-screen items-center justify-center bg-slate-950 px-4">
    <UCard variant="subtle" class="w-full max-w-sm" :ui="{ root: 'ring-slate-800' }">
      <template #header>
        <div class="flex items-center gap-2 py-1">
          <span class="h-2.5 w-2.5 rounded-full bg-emerald-400" aria-hidden="true" />
          <span class="text-lg font-semibold tracking-tight text-slate-100">BasePod</span>
        </div>
      </template>

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

        <UButton type="submit" color="primary" block :loading="auth.pending" :disabled="auth.pending">
          Sign in
        </UButton>
      </form>
    </UCard>
  </div>
</template>
