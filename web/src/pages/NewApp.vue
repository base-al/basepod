<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useMutation } from '@tanstack/vue-query'

import { api, ApiError } from '../lib/api'

const router = useRouter()

const name = ref('')
const image = ref('')
const port = ref<number | null>(null)

// Mirrors internal/api/apps.go's slugify() + slugPattern exactly: lowercase,
// spaces -> hyphens (nothing else stripped), then must start with a letter
// and contain only lowercase letters/digits/hyphens, max 32 chars.
const SLUG_PATTERN = /^[a-z][a-z0-9-]{0,31}$/
function slugify(value: string) {
  return value.toLowerCase().replaceAll(' ', '-')
}

const slug = computed(() => slugify(name.value))
const slugValid = computed(() => slug.value.length > 0 && SLUG_PATTERN.test(slug.value))

const portValid = computed(() => port.value !== null && Number.isInteger(port.value) && port.value >= 1 && port.value <= 65535)
const imageValid = computed(() => image.value.trim().length > 0)

const formValid = computed(() => slugValid.value && imageValid.value && portValid.value)

const fieldErrors = ref<{ name: string; image: string; port: string }>({ name: '', image: '', port: '' })
const generalError = ref('')

// The API's 422 "validation" message names the field it's about (see
// handleCreateApp in internal/api/apps.go) — route it to that field's
// inline error rather than a generic banner. app_exists (409) is also
// name-shaped (the slug derived from name collided), so it goes there too.
function applyApiError(err: ApiError) {
  fieldErrors.value = { name: '', image: '', port: '' }
  generalError.value = ''

  if (err.code === 'app_exists') {
    fieldErrors.value.name = err.message
    return
  }
  if (err.code === 'validation') {
    if (err.message.startsWith('name')) {
      fieldErrors.value.name = err.message
    } else if (err.message.startsWith('image')) {
      fieldErrors.value.image = err.message
    } else if (err.message.startsWith('port')) {
      fieldErrors.value.port = err.message
    } else {
      generalError.value = err.message
    }
    return
  }
  generalError.value = err.message
}

const createMutation = useMutation({
  mutationFn: () => api.createApp(name.value, image.value.trim(), port.value as number),
  onSuccess: (app) => {
    // Navigate straight to the detail page and hand off the first-deploy
    // trigger to it via ?autodeploy=1 — AppDetail fires the deploy
    // mutation itself on mount so its own pending/error UI (amber badge,
    // error callout) naturally covers this deploy too, including the
    // case where it takes minutes or fails.
    void router.push({ name: 'app-detail', params: { slug: app.slug }, query: { autodeploy: '1' } })
  },
  onError: (err) => {
    if (err instanceof ApiError) {
      applyApiError(err)
    } else {
      generalError.value = 'Something went wrong — try again'
    }
  },
})

function onSubmit() {
  if (!formValid.value || createMutation.isPending.value) return
  fieldErrors.value = { name: '', image: '', port: '' }
  generalError.value = ''
  createMutation.mutate()
}
</script>

<template>
  <div class="min-h-screen bg-slate-950">
    <header class="sticky top-0 z-10 border-b border-slate-800 bg-slate-950/90 backdrop-blur">
      <div class="mx-auto flex max-w-2xl items-center gap-3 px-6 py-3">
        <UButton to="/" color="neutral" variant="ghost" square size="sm" icon="i-lucide-arrow-left" aria-label="Back to apps" />
        <span class="text-base font-semibold tracking-tight text-slate-100">New app</span>
      </div>
    </header>

    <main class="mx-auto max-w-2xl px-6 py-8">
      <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
        <form class="flex flex-col gap-5" novalidate @submit.prevent="onSubmit">
          <UFormField label="Name" name="name" :error="fieldErrors.name || undefined">
            <UInput v-model="name" placeholder="My Blog" class="w-full" :disabled="createMutation.isPending.value" autofocus />
            <template #hint>
              <span class="font-mono text-xs" :class="name && !slugValid ? 'text-red-400' : 'text-slate-500'">
                {{ slug || 'slug' }}
              </span>
            </template>
          </UFormField>
          <p v-if="name && !slugValid" class="-mt-3 text-xs text-slate-500">
            Slug must start with a letter and contain only lowercase letters, digits, and hyphens (max 32 characters).
          </p>

          <UFormField label="Image" name="image" :error="fieldErrors.image || undefined">
            <UInput
              v-model="image"
              placeholder="docker.io/library/nginx:alpine"
              class="w-full font-mono"
              :disabled="createMutation.isPending.value"
            />
          </UFormField>

          <UFormField label="Port" name="port" :error="fieldErrors.port || undefined">
            <UInput
              v-model.number="port"
              type="number"
              :min="1"
              :max="65535"
              placeholder="8080"
              class="w-full"
              :disabled="createMutation.isPending.value"
            />
          </UFormField>

          <UAlert v-if="generalError" color="error" variant="subtle" :title="generalError" icon="i-lucide-alert-circle" />

          <div class="flex justify-end gap-2">
            <UButton to="/" color="neutral" variant="ghost" :disabled="createMutation.isPending.value">Cancel</UButton>
            <UButton
              type="submit"
              color="primary"
              :loading="createMutation.isPending.value"
              :disabled="!formValid || createMutation.isPending.value"
            >
              Create &amp; deploy
            </UButton>
          </div>
        </form>
      </UCard>
    </main>
  </div>
</template>
