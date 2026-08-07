<script setup lang="ts">
// Public route (router.ts's `public: true`) — the far end of an invite
// link Users.vue's invite flow builds via lib/inviteUrl.ts's
// buildInviteAcceptUrl. Deliberately reachable with no session: whoever
// clicks this has never necessarily seen BasePod before, so this page
// has to work standalone — same visual treatment as Login.vue (no
// AppShell, no nav, nothing assuming an existing account).
//
// Every failure mode POST /invitations/accept can 404/409/422 on
// (lib/api.ts's acceptInvite) gets its own specific message here rather
// than one generic "something went wrong" — an expired invite and an
// already-used one call for different next steps, and a first-time
// visitor has no other context to fall back on.
import { computed, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '../lib/api'
import { useAuthStore } from '../stores/auth'
import BasepodWordmark from '../components/BasepodWordmark.vue'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// URLSearchParams-decoded already by vue-router; a query value can in
// principle be an array or missing entirely (a hand-edited/truncated
// link) — normalize both to "" so the rest of this file only ever deals
// with a plain string.
const token = computed(() => {
  const raw = route.query.token
  return typeof raw === 'string' ? raw : ''
})

const name = ref('')
const password = ref('')
const confirmPassword = ref('')
const errorMessage = ref('')
const pending = ref(false)

const MIN_PASSWORD_LENGTH = 8

const tooShort = computed(() => password.value.length > 0 && password.value.length < MIN_PASSWORD_LENGTH)
const mismatched = computed(() => confirmPassword.value.length > 0 && password.value !== confirmPassword.value)
const canSubmit = computed(
  () =>
    token.value.length > 0 &&
    name.value.trim().length > 0 &&
    password.value.length >= MIN_PASSWORD_LENGTH &&
    password.value === confirmPassword.value,
)

// One message per documented failure code (lib/api.ts's acceptInvite doc
// comment) — each names what happened AND what to do next, rather than
// a bare restatement of the code.
const ERROR_COPY: Record<string, string> = {
  invite_not_found: "This invite link isn't valid. Check that you copied the whole link, or ask whoever invited you to send a new one.",
  invite_already_used: 'This invite has already been used. If you already finished creating your account, sign in instead.',
  invite_expired: 'This invite has expired. Ask an admin or owner to send you a new one.',
  user_exists: 'An account with this email already exists. Sign in instead.',
}

async function onSubmit() {
  if (!canSubmit.value || pending.value) return
  errorMessage.value = ''
  pending.value = true
  try {
    const res = await api.acceptInvite({ token: token.value, name: name.value.trim(), password: password.value })
    auth.setSession(res)
    await router.push({ name: 'apps' })
  } catch (err) {
    errorMessage.value = err instanceof ApiError ? (ERROR_COPY[err.code] ?? err.message) : 'Something went wrong — try again'
  } finally {
    pending.value = false
  }
}
</script>

<template>
  <div class="login-grid flex min-h-screen flex-col items-center justify-center gap-8 bg-surface px-4">
    <div class="flex flex-col items-center gap-2 text-center">
      <BasepodWordmark :height="26" />
      <p class="max-w-xs text-sm text-content-secondary">Set a name and password to finish creating your account.</p>
    </div>

    <UCard variant="subtle" class="w-full max-w-sm" :ui="{ root: 'ring-line' }">
      <!-- No token in the URL at all — nothing to submit, so don't show
           a form that can only ever fail. -->
      <div v-if="!token" class="flex flex-col items-center gap-3 py-4 text-center">
        <UIcon name="i-lucide-link-2-off" class="h-8 w-8 text-content-muted" aria-hidden="true" />
        <p class="text-sm font-medium text-content-secondary">This link is missing its invite token</p>
        <p class="text-sm text-content-muted">Check that you copied the whole link from your invite, or ask whoever invited you to send it again.</p>
        <UButton :to="{ name: 'login' }" color="neutral" variant="ghost" class="tap44 mt-1">Go to sign in</UButton>
      </div>

      <form v-else class="flex flex-col gap-4" novalidate @submit.prevent="onSubmit">
        <UFormField label="Name" name="name">
          <UInput v-model="name" autocomplete="name" placeholder="Your name" class="w-full" :disabled="pending" required autofocus />
        </UFormField>

        <UFormField label="Password" name="password">
          <UInput
            v-model="password"
            type="password"
            autocomplete="new-password"
            placeholder="••••••••"
            class="w-full"
            :color="tooShort ? 'error' : undefined"
            :disabled="pending"
            required
          />
          <p v-if="tooShort" class="mt-1 text-xs text-status-error">Must be at least {{ MIN_PASSWORD_LENGTH }} characters.</p>
        </UFormField>

        <UFormField label="Confirm password" name="confirm-password">
          <UInput
            v-model="confirmPassword"
            type="password"
            autocomplete="new-password"
            placeholder="••••••••"
            class="w-full"
            :color="mismatched ? 'error' : undefined"
            :disabled="pending"
            required
          />
          <p v-if="mismatched" class="mt-1 text-xs text-status-error">Passwords don't match.</p>
        </UFormField>

        <UAlert v-if="errorMessage" color="error" variant="subtle" :title="errorMessage" icon="i-lucide-alert-circle" />

        <UButton type="submit" color="primary" block class="tap44" :loading="pending" :disabled="!canSubmit || pending">
          Create account and sign in
        </UButton>

        <p class="text-center text-xs text-content-muted">
          Already finished this before?
          <RouterLink :to="{ name: 'login' }" class="text-content-secondary underline underline-offset-2 hover:text-content-primary">Sign in instead</RouterLink>
        </p>
      </form>
    </UCard>
  </div>
</template>
