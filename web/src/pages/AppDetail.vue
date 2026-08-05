<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'
import type { TabsItem } from '@nuxt/ui'

import { api, ApiError } from '../lib/api'
import StatusBadge from '../components/StatusBadge.vue'
import ImageRef from '../components/ImageRef.vue'
import DeploymentList from '../components/DeploymentList.vue'
import ConfirmDanger from '../components/ConfirmDanger.vue'
import LogViewer from '../components/LogViewer.vue'
import EnvEditor from '../components/EnvEditor.vue'
import DomainsPanel from '../components/DomainsPanel.vue'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const queryClient = useQueryClient()

const slug = computed(() => route.params.slug as string)

// Poll fast while a deploy is in flight (so the UI catches "running"/
// "error" promptly once it lands) and slow otherwise — same polling
// posture as Apps.vue, just status-conditional here.
const POLL_FAST_MS = 5000
const POLL_SLOW_MS = 15000

// vue-query deeply unwraps refs inside queryKey (MaybeRefDeep), so `slug`
// here just needs to be reactive, not manually wrapped in computed().
const appQuery = useQuery({
  queryKey: ['app', slug],
  queryFn: () => api.getApp(slug.value),
  refetchInterval: (query) => (query.state.data?.status === 'deploying' ? POLL_FAST_MS : POLL_SLOW_MS),
})

// The generated domain is derived from slug + a server-side root domain
// that doesn't change at runtime, so one fetch (no polling) is enough.
// This shares its query key with DomainsPanel's own domains fetch — an
// add/delete there invalidates the same cache entry, keeping this
// header's generated-domain link in sync for free.
const domainsQuery = useQuery({
  queryKey: ['app-domains', slug],
  queryFn: () => api.getDomains(slug.value),
})

const app = computed(() => appQuery.data.value)
const generatedDomain = computed(() => domainsQuery.data.value?.generated)

// True once a PUT to /env reports X-Basepod-Redeploy-Required: true, and
// cleared once a redeploy actually lands — drives both EnvEditor's own
// persistent banner (passed down as a prop) and the small dot on the
// Environment tab label below (kept at this level since the tab bar
// itself is owned here, not by EnvEditor).
const envRedeployRequired = ref(false)

// UTabs' modelValue type is `string | number` (generic over its items),
// which is wider than a closed literal union — keep this a plain string
// ref so v-model assignment type-checks; the tab panels below just
// compare it against the item values used in tabItems.
const activeTab = ref('overview')
const envEditorRef = ref<InstanceType<typeof EnvEditor> | null>(null)

const tabItems = computed<TabsItem[]>(() => [
  { label: 'Overview', value: 'overview' },
  { label: 'Deployments', value: 'deployments' },
  { label: 'Logs', value: 'logs' },
  {
    label: 'Environment',
    value: 'environment',
    badge: envRedeployRequired.value ? { color: 'warning', variant: 'solid', class: 'h-2 w-2 rounded-full p-0 ring-0' } : undefined,
  },
  { label: 'Domains', value: 'domains' },
  { label: 'Settings', value: 'settings' },
])

// Switching tabs is intercepted (rather than bound via plain v-model) so
// leaving the Environment tab with unsaved edits can be guarded with a
// confirm — UTabs' v-model would otherwise just swap panels immediately
// with no chance to ask first.
function onTabChange(value: string | number) {
  if (activeTab.value === 'environment' && value !== 'environment' && envEditorRef.value?.isDirty) {
    if (!window.confirm('You have unsaved environment variable changes. Leave without saving?')) {
      return
    }
  }
  activeTab.value = String(value)
}

const deployError = ref('')

const deployMutation = useMutation({
  mutationFn: (image?: string) => api.deploy(slug.value, image),
  onSuccess: () => {
    deployError.value = ''
    // A landed deploy picks up whatever env vars are currently stored,
    // so any pending "redeploy to apply" state is now resolved.
    envRedeployRequired.value = false
    void queryClient.invalidateQueries({ queryKey: ['app', slug.value] })
  },
  onError: (err) => {
    deployError.value = err instanceof ApiError ? err.message : 'Deploy failed — try again'
    // The server has already flipped the app's status (e.g. to "error")
    // by the time this rejects — refetch so the badge/facts reflect that
    // immediately instead of showing the stale pre-deploy status for up
    // to POLL_SLOW_MS.
    void queryClient.invalidateQueries({ queryKey: ['app', slug.value] })
  },
})

// True while *this* client is waiting on a deploy it triggered, or while
// polling shows one is underway (e.g. triggered from another tab, or the
// auto-deploy fired by NewApp.vue on creation). Either condition disables
// deploy actions and forces the amber "Deploying" badge.
const isDeploying = computed(() => deployMutation.isPending.value || app.value?.status === 'deploying')
const displayStatus = computed(() => (isDeploying.value ? 'deploying' : (app.value?.status ?? 'created')))

// NewApp.vue navigates here with ?autodeploy=1 right after creating the
// app, rather than triggering the deploy itself and racing its own
// unmount — this page owns the deploy mutation, so the pending/error UI
// below (amber badge, error callout) covers the very first deploy too.
onMounted(() => {
  if (route.query.autodeploy === '1') {
    void router.replace({ name: 'app-detail', params: { slug: slug.value }, query: {} })
    deployMutation.mutate(undefined)
  }
})

function deployLatest() {
  if (isDeploying.value) return
  deployMutation.mutate(undefined)
}

const newImageOpen = ref(false)
const newImageValue = ref('')

function openNewImage() {
  newImageValue.value = app.value?.image ?? ''
  newImageOpen.value = true
}

function cancelNewImage() {
  newImageOpen.value = false
  newImageValue.value = ''
}

function confirmNewImage() {
  const image = newImageValue.value.trim()
  if (!image || isDeploying.value) return
  newImageOpen.value = false
  deployMutation.mutate(image)
}

const deleteModalOpen = ref(false)

const deleteMutation = useMutation({
  mutationFn: () => api.deleteApp(slug.value),
  onSuccess: () => {
    deleteModalOpen.value = false
    toast.add({ title: 'App deleted', description: slug.value, color: 'success', icon: 'i-lucide-trash-2' })
    void router.push({ name: 'apps' })
  },
  onError: (err) => {
    // 502 remove_failed: the app stays (server left the row in place so a
    // retry has something to act on) — surface the real error and let the
    // user try again rather than silently pretending it succeeded.
    deleteModalOpen.value = false
    toast.add({
      title: 'Could not delete app',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})
</script>

<template>
  <div class="min-h-screen bg-slate-950">
    <header class="sticky top-0 z-10 border-b border-slate-800 bg-slate-950/90 backdrop-blur">
      <div class="mx-auto flex max-w-5xl items-center gap-3 px-6 py-3">
        <UButton to="/" color="neutral" variant="ghost" square size="sm" icon="i-lucide-arrow-left" aria-label="Back to apps" />

        <template v-if="app">
          <span class="font-mono text-base font-semibold tracking-tight text-slate-100">{{ app.slug }}</span>
          <StatusBadge :status="displayStatus" />
          <a
            v-if="generatedDomain"
            :href="`https://${generatedDomain}`"
            target="_blank"
            rel="noopener noreferrer"
            class="flex items-center gap-1 font-mono text-xs text-emerald-400 hover:underline"
          >
            {{ generatedDomain }}
            <UIcon name="i-lucide-external-link" class="h-3 w-3" />
          </a>
          <ImageRef :value="app.image" class="ml-auto max-w-xs" />
        </template>
        <span v-else-if="appQuery.isPending.value" class="text-sm text-slate-500">Loading…</span>
      </div>
    </header>

    <main class="mx-auto max-w-5xl px-6 py-8">
      <UAlert
        v-if="appQuery.isError.value"
        color="error"
        variant="subtle"
        title="Couldn't load app"
        :description="appQuery.error.value instanceof ApiError ? appQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
      />

      <div
        v-else-if="appQuery.isPending.value"
        class="flex items-center justify-center rounded-lg border border-slate-800 py-24 text-sm text-slate-500"
      >
        Loading app…
      </div>

      <template v-else-if="app">
        <UTabs :items="tabItems" :model-value="activeTab" :content="false" variant="link" class="mb-6" @update:model-value="onTabChange" />

        <div v-if="activeTab === 'overview'" class="flex flex-col gap-6">
          <UAlert v-if="deployError" color="error" variant="subtle" title="Deploy failed" :description="deployError" icon="i-lucide-alert-circle" />

          <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
            <template #header>
              <h2 class="text-sm font-medium text-slate-400">Facts</h2>
            </template>

            <dl class="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
              <div>
                <dt class="text-xs text-slate-500">Status</dt>
                <dd class="mt-1"><StatusBadge :status="displayStatus" /></dd>
              </div>
              <div class="min-w-0">
                <dt class="text-xs text-slate-500">Image</dt>
                <dd class="mt-1"><ImageRef :value="app.image" /></dd>
              </div>
              <div>
                <dt class="text-xs text-slate-500">Port</dt>
                <dd class="mt-1 font-mono text-sm text-slate-300">{{ app.port }}</dd>
              </div>
              <div>
                <dt class="text-xs text-slate-500">Internal hostname</dt>
                <dd class="mt-1 font-mono text-sm text-slate-300">bp-{{ app.slug }}</dd>
              </div>
              <div>
                <dt class="text-xs text-slate-500">Deployments</dt>
                <dd class="mt-1 text-sm text-slate-300">{{ app.deployments.length }}</dd>
              </div>
            </dl>
          </UCard>

          <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
            <template #header>
              <h2 class="text-sm font-medium text-slate-400">Quick actions</h2>
            </template>

            <div class="flex flex-col gap-4">
              <div class="flex flex-wrap items-center gap-2">
                <UButton
                  color="primary"
                  variant="soft"
                  icon="i-lucide-rocket"
                  :loading="deployMutation.isPending.value"
                  :disabled="isDeploying"
                  @click="deployLatest"
                >
                  Deploy
                </UButton>

                <UButton
                  v-if="!newImageOpen"
                  color="neutral"
                  variant="ghost"
                  icon="i-lucide-package-plus"
                  :disabled="isDeploying"
                  @click="openNewImage"
                >
                  Deploy new image…
                </UButton>

                <UButton color="error" variant="ghost" icon="i-lucide-trash-2" :disabled="isDeploying" @click="deleteModalOpen = true">
                  Delete
                </UButton>
              </div>

              <div v-if="newImageOpen" class="flex flex-wrap items-center gap-2 rounded-lg border border-slate-800 p-3">
                <UInput
                  v-model="newImageValue"
                  placeholder="docker.io/library/nginx:alpine"
                  class="min-w-64 flex-1 font-mono"
                  :disabled="isDeploying"
                  autofocus
                  @keyup.enter="confirmNewImage"
                />
                <UButton color="primary" size="sm" :disabled="!newImageValue.trim() || isDeploying" @click="confirmNewImage">
                  Deploy image
                </UButton>
                <UButton color="neutral" variant="ghost" size="sm" :disabled="isDeploying" @click="cancelNewImage">Cancel</UButton>
              </div>
            </div>
          </UCard>
        </div>

        <div v-else-if="activeTab === 'deployments'">
          <DeploymentList :slug="slug" :deployments="app.deployments" />
        </div>

        <!-- v-if (not v-show): the log stream must actually tear down —
             not just hide — when the tab is left, so LogViewer's
             onUnmounted call to sse.ts's close() runs and the EventSource
             (and its reconnect loop) doesn't keep running in the
             background for a tab the user can no longer see. -->
        <div v-else-if="activeTab === 'logs'">
          <LogViewer :slug="slug" @deploy-hint="activeTab = 'overview'" />
        </div>

        <div v-else-if="activeTab === 'environment'">
          <EnvEditor
            ref="envEditorRef"
            :slug="slug"
            :redeploy-required="envRedeployRequired"
            :deploying="isDeploying"
            @update:redeploy-required="(value) => (envRedeployRequired = value)"
            @redeploy="deployLatest"
          />
        </div>

        <div v-else-if="activeTab === 'domains'">
          <DomainsPanel :slug="slug" />
        </div>

        <div v-else-if="activeTab === 'settings'" class="flex flex-col gap-6">
          <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
            <template #header>
              <h2 class="text-sm font-medium text-slate-400">App facts</h2>
            </template>

            <dl class="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
              <div>
                <dt class="text-xs text-slate-500">Slug</dt>
                <dd class="mt-1 font-mono text-sm text-slate-300">{{ app.slug }}</dd>
              </div>
              <div class="min-w-0">
                <dt class="text-xs text-slate-500">Image</dt>
                <dd class="mt-1"><ImageRef :value="app.image" /></dd>
              </div>
              <div>
                <dt class="text-xs text-slate-500">Port</dt>
                <dd class="mt-1 font-mono text-sm text-slate-300">{{ app.port }}</dd>
              </div>
              <div>
                <dt class="text-xs text-slate-500">Internal hostname</dt>
                <dd class="mt-1 font-mono text-sm text-slate-300">bp-{{ app.slug }}</dd>
              </div>
            </dl>
          </UCard>

          <UCard variant="subtle" :ui="{ root: 'ring-red-900/60' }">
            <template #header>
              <h2 class="text-sm font-medium text-red-400">Danger zone</h2>
            </template>

            <div class="flex items-center justify-between gap-4">
              <p class="text-sm text-slate-400">
                Stops and removes this app's containers and routes, and deletes its deployment history. This cannot be
                undone.
              </p>
              <UButton color="error" variant="soft" icon="i-lucide-trash-2" :disabled="isDeploying" @click="deleteModalOpen = true">
                Delete app
              </UButton>
            </div>
          </UCard>
        </div>
      </template>
    </main>

    <ConfirmDanger
      v-if="app"
      :open="deleteModalOpen"
      :confirm-text="app.slug"
      title="Delete this app?"
      description="This stops and removes its containers and routes, and deletes its deployment history. This cannot be undone."
      confirm-label="Delete app"
      :loading="deleteMutation.isPending.value"
      @update:open="(value) => (deleteModalOpen = value)"
      @confirm="deleteMutation.mutate()"
    />
  </div>
</template>
