<script setup lang="ts">
// AppDetail's Domains tab: the generated hostname (always routed, not
// editable) plus custom domains backed by
// GET/POST/DELETE /apps/{slug}/domains (internal/api/domains.go).
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, ApiError } from '../lib/api'

const props = defineProps<{ slug: string }>()

const toast = useToast()
const queryClient = useQueryClient()

// Same query key AppDetail's header uses for the generated-domain link —
// sharing it means an add/delete here also keeps that header in sync for
// free, and there's only ever one fetch for this app's domains in flight.
const slugRef = computed(() => props.slug)
const domainsQuery = useQuery({
  queryKey: ['app-domains', slugRef],
  queryFn: () => api.getDomains(props.slug),
})

const generated = computed(() => domainsQuery.data.value?.generated ?? '')
const custom = computed(() => domainsQuery.data.value?.custom ?? [])

function invalidate() {
  return queryClient.invalidateQueries({ queryKey: ['app-domains', props.slug] })
}

async function copyGenerated() {
  if (!generated.value) return
  try {
    await navigator.clipboard.writeText(generated.value)
    toast.add({ title: 'Copied', description: generated.value, color: 'success', icon: 'i-lucide-check' })
  } catch {
    toast.add({ title: 'Could not copy', color: 'error', icon: 'i-lucide-alert-circle' })
  }
}

// Mirrors internal/api/domains.go's hostnamePattern exactly.
const HOSTNAME_PATTERN = /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$/

const newHostname = ref('')
const addError = ref('')

const hostnameValid = computed(() => HOSTNAME_PATTERN.test(newHostname.value))

function onHostnameInput(value: string) {
  // Lowercase-forced: the API itself lowercases before validating, but
  // forcing it client-side too keeps what's typed and what's validated
  // visibly the same thing rather than silently diverging on submit.
  newHostname.value = value.toLowerCase()
  addError.value = ''
}

const addMutation = useMutation({
  mutationFn: () => api.addDomain(props.slug, newHostname.value),
  onSuccess: async () => {
    addError.value = ''
    newHostname.value = ''
    await invalidate()
    toast.add({
      title: 'Domain added',
      description: 'HTTPS is issued on first request to it — this can take a few seconds.',
      color: 'success',
      icon: 'i-lucide-check',
    })
  },
  onError: (err) => {
    // 422 validation / 409 domain_exists / 502 routes_failed — surfaced
    // verbatim, inline under the input, rather than reworded.
    addError.value = err instanceof ApiError ? err.message : 'Could not add domain — try again'
  },
})

function submitAdd() {
  if (!hostnameValid.value || addMutation.isPending.value) return
  addMutation.mutate()
}

const confirmOpenId = ref<number | null>(null)
const deletingId = ref<number | null>(null)

const deleteMutation = useMutation({
  mutationFn: (id: number) => api.deleteDomain(props.slug, id),
  onSuccess: async () => {
    confirmOpenId.value = null
    deletingId.value = null
    await invalidate()
    toast.add({ title: 'Domain removed', color: 'success', icon: 'i-lucide-check' })
  },
  onError: async (err) => {
    // No optimistic removal happened, so the row was never pulled from
    // the list — but on a 502 routes_failed the server re-adds the
    // domain under a NEW id (see handleDeleteDomain's rollback in
    // internal/api/domains.go), which the stale cached row doesn't know
    // about. Refetch so any retry acts on the current id instead of a
    // now-404ing one.
    confirmOpenId.value = null
    deletingId.value = null
    await invalidate()
    toast.add({
      title: 'Could not remove domain',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

function confirmDelete(id: number) {
  deletingId.value = id
  deleteMutation.mutate(id)
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <UAlert
      v-if="domainsQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't load domains"
      :description="domainsQuery.error.value instanceof ApiError ? domainsQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
    />

    <div v-else-if="domainsQuery.isPending.value" class="flex items-center justify-center rounded-lg border border-line py-16 text-sm text-content-muted">
      Loading domains…
    </div>

    <template v-else>
      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h2 class="text-sm font-medium text-content-secondary">Generated domain</h2>
        </template>

        <div class="flex flex-wrap items-center gap-2">
          <span class="min-w-0 flex-1 truncate font-mono text-sm text-content-primary">{{ generated }}</span>
          <UBadge color="neutral" variant="subtle">Managed</UBadge>
          <UButton size="sm" color="neutral" variant="ghost" square icon="i-lucide-copy" aria-label="Copy hostname" @click="copyGenerated" />
          <UButton
            size="sm"
            color="neutral"
            variant="ghost"
            square
            icon="i-lucide-external-link"
            :to="`https://${generated}`"
            target="_blank"
            aria-label="Open in new tab"
          />
        </div>
        <p class="mt-2 text-xs text-content-muted">Always routes to this app — it can't be edited or removed.</p>
      </UCard>

      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <template #header>
          <h2 class="text-sm font-medium text-content-secondary">Custom domains</h2>
        </template>

        <div class="flex flex-col gap-4">
          <UAlert
            color="info"
            variant="subtle"
            icon="i-lucide-info"
            title="Point an A/AAAA record for this hostname at your server before adding — HTTPS is issued on first request."
          />

          <form class="flex flex-wrap items-start gap-2" novalidate @submit.prevent="submitAdd">
            <div class="min-w-0 flex-1">
              <UInput
                :model-value="newHostname"
                placeholder="app.example.com"
                autocomplete="off"
                autocapitalize="off"
                spellcheck="false"
                class="w-full font-mono"
                :color="addError ? 'error' : undefined"
                :disabled="addMutation.isPending.value"
                @update:model-value="(v: string | number) => onHostnameInput(String(v))"
              />
              <p v-if="newHostname && !hostnameValid" class="mt-1 text-xs text-status-error">
                Must be a valid lowercase hostname, e.g. app.example.com.
              </p>
              <p v-else-if="addError" class="mt-1 text-xs text-status-error">{{ addError }}</p>
            </div>
            <UButton type="submit" color="primary" variant="soft" icon="i-lucide-plus" :loading="addMutation.isPending.value" :disabled="!hostnameValid || addMutation.isPending.value">
              Add domain
            </UButton>
          </form>

          <div v-if="!custom.length" class="rounded-lg border border-line py-8 text-center text-sm text-content-muted">
            No custom domains yet.
          </div>

          <ul v-else class="flex flex-col gap-2">
            <li v-for="domain in custom" :key="domain.id" class="flex items-center gap-2 rounded-lg border border-line px-3 py-2">
              <span class="min-w-0 flex-1 truncate font-mono text-sm text-content-primary">{{ domain.hostname }}</span>
              <UButton
                size="sm"
                color="neutral"
                variant="ghost"
                square
                icon="i-lucide-external-link"
                :to="`https://${domain.hostname}`"
                target="_blank"
                :aria-label="`Open ${domain.hostname}`"
              />
              <UPopover :open="confirmOpenId === domain.id" @update:open="(v: boolean) => (confirmOpenId = v ? domain.id : null)">
                <UButton size="sm" color="error" variant="ghost" square icon="i-lucide-trash-2" :aria-label="`Delete ${domain.hostname}`" />
                <template #content>
                  <div class="flex flex-col gap-2 p-3">
                    <p class="text-xs text-content-secondary">Remove <span class="font-mono">{{ domain.hostname }}</span>?</p>
                    <div class="flex justify-end gap-2">
                      <UButton size="xs" color="neutral" variant="ghost" @click="confirmOpenId = null">Cancel</UButton>
                      <UButton
                        size="xs"
                        color="error"
                        :loading="deleteMutation.isPending.value && deletingId === domain.id"
                        @click="confirmDelete(domain.id)"
                      >
                        Remove
                      </UButton>
                    </div>
                  </div>
                </template>
              </UPopover>
            </li>
          </ul>
        </div>
      </UCard>
    </template>
  </div>
</template>
