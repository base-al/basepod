<script setup lang="ts">
// AppDetail's Git tab: connect/manage push-to-deploy (v0.5 Tasks 4/5/9),
// backed by GET/PUT/DELETE /apps/{slug}/git and GET
// /apps/{slug}/git/deliveries (internal/api/git.go).
//
// One deliberate deviation from the milestone brief, made to match the
// REAL shipped API rather than the plan's original sketch: the webhook
// secret is NOT "shown once on creation" — internal/api/git.go's
// gitSourceResponse returns it in plaintext from every GET, by design
// (see that file's doc comment: the operator may need to re-paste it
// into a forge's webhook settings more than once). This panel reflects
// that honestly — the secret is visible here whenever this tab is open —
// rather than pretending a retrieval story the server doesn't implement.
import { computed, reactive, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'
import type { AccordionItem, TableColumn } from '@nuxt/ui'

import { api, ApiError, type GitDelivery, type GitDeliveryStatus } from '../lib/api'
import { formatGitRef, shortSha } from '../lib/gitFormat'
import { relativeTime } from '../lib/relativeTime'
import { gitDeliveryStatusStyles } from '../theme'
import ConfirmDanger from './ConfirmDanger.vue'

const props = defineProps<{ slug: string }>()

const emit = defineEmits<{
  /** Fired once a manual "Deploy now" successfully queues a deployment —
   * AppDetail.vue switches to the Deployments tab and auto-expands this
   * deployment's build-log drawer, the same way NewApp.vue's upload flow
   * does, so the build streams live without an extra click. */
  deployed: [deploymentNumber: number]
}>()

const toast = useToast()
const queryClient = useQueryClient()

const slugRef = computed(() => props.slug)

// retry: false — a 404 "git_not_connected" here is the ordinary steady
// state for an app with no repo connected yet, not a transient failure
// worth retrying three times before the connect form even renders.
const gitQuery = useQuery({
  queryKey: ['app-git', slugRef],
  queryFn: () => api.getGitSource(props.slug),
  retry: false,
})

const notConnected = computed(
  () => gitQuery.isError.value && gitQuery.error.value instanceof ApiError && gitQuery.error.value.code === 'git_not_connected',
)
const loadError = computed(() => gitQuery.isError.value && !notConnected.value)
const gitSource = computed(() => gitQuery.data.value ?? null)
const connected = computed(() => gitSource.value !== null)

function invalidateGit() {
  return queryClient.invalidateQueries({ queryKey: ['app-git', props.slug] })
}

// --- Connect / edit form -------------------------------------------------

// Shown when nothing is connected yet, or while editing an existing
// connection's URL/branch/token (toggled via editing.value below). Token
// is always typed fresh here — an existing token never round-trips (see
// GitSource's doc comment), so this field starts blank either way, with
// its placeholder communicating whether leaving it blank keeps an
// existing token or just means "no token".
const form = reactive({ url: '', branch: 'main', token: '' })
const editing = ref(false)
const formError = ref('')

function startEdit() {
  if (!gitSource.value) return
  form.url = gitSource.value.url
  form.branch = gitSource.value.branch
  form.token = ''
  formError.value = ''
  editing.value = true
}

function cancelEdit() {
  editing.value = false
  formError.value = ''
}

const URL_PATTERN = /^https:\/\/.+/

const urlValid = computed(() => URL_PATTERN.test(form.url.trim()))
const branchValid = computed(() => form.branch.trim().length > 0)
const formValid = computed(() => urlValid.value && branchValid.value)

const connectMutation = useMutation({
  mutationFn: () =>
    api.putGitSource(props.slug, {
      url: form.url.trim(),
      branch: form.branch.trim(),
      token: form.token,
    }),
  onSuccess: async () => {
    formError.value = ''
    editing.value = false
    form.token = ''
    await invalidateGit()
    toast.add({ title: 'Repo connected', description: form.url, color: 'success', icon: 'i-lucide-git-branch' })
  },
  onError: (err) => {
    formError.value = err instanceof ApiError ? err.message : 'Could not save — try again'
  },
})

function submitForm() {
  if (!formValid.value || connectMutation.isPending.value) return
  formError.value = ''
  connectMutation.mutate()
}

// --- Rotate secret ---------------------------------------------------

const rotateConfirmOpen = ref(false)

const rotateMutation = useMutation({
  mutationFn: () => {
    if (!gitSource.value) throw new Error('no git source')
    return api.putGitSource(props.slug, {
      url: gitSource.value.url,
      branch: gitSource.value.branch,
      token: '',
      rotate_secret: true,
    })
  },
  onSuccess: async () => {
    rotateConfirmOpen.value = false
    await invalidateGit()
    toast.add({
      title: 'Webhook secret rotated',
      description: 'Update the secret in your forge’s webhook settings with the new value below.',
      color: 'success',
      icon: 'i-lucide-key',
    })
  },
  onError: (err) => {
    rotateConfirmOpen.value = false
    toast.add({
      title: 'Could not rotate secret',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

// --- Disconnect --------------------------------------------------------

const disconnectOpen = ref(false)

const disconnectMutation = useMutation({
  mutationFn: () => api.deleteGitSource(props.slug),
  onSuccess: async () => {
    disconnectOpen.value = false
    await invalidateGit()
    toast.add({ title: 'Repo disconnected', color: 'success', icon: 'i-lucide-unlink' })
  },
  onError: (err) => {
    disconnectOpen.value = false
    toast.add({
      title: 'Could not disconnect',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

// --- Copy helpers --------------------------------------------------------

async function copyText(value: string, label: string) {
  if (!value) return
  try {
    await navigator.clipboard.writeText(value)
    toast.add({ title: 'Copied', description: label, color: 'success', icon: 'i-lucide-check' })
  } catch {
    toast.add({ title: 'Could not copy', color: 'error', icon: 'i-lucide-alert-circle' })
  }
}

// --- Deploy now ----------------------------------------------------------

const deployError = ref('')

const deployMutation = useMutation({
  mutationFn: () => api.deployGit(props.slug),
  onSuccess: (deployment) => {
    deployError.value = ''
    emit('deployed', deployment.number)
  },
  onError: (err) => {
    deployError.value = err instanceof ApiError ? err.message : 'Deploy failed — try again'
  },
})

function deployNow() {
  if (deployMutation.isPending.value) return
  deployError.value = ''
  deployMutation.mutate()
}

// --- Deliveries ----------------------------------------------------------

// No polling loop (deliveries are low-traffic — plan's own call) — just a
// manual refresh button, fetched once the repo is connected.
const deliveriesQuery = useQuery({
  queryKey: ['app-git-deliveries', slugRef],
  queryFn: () => api.listGitDeliveries(props.slug),
  enabled: connected,
})

const deliveries = computed(() => deliveriesQuery.data.value ?? [])

function refreshDeliveries() {
  void deliveriesQuery.refetch()
}

// A repo just connected has no deliveries query result yet (enabled
// flips true right as `connected` does) — trigger one fetch immediately
// rather than waiting for the next unrelated re-render.
watch(connected, (isConnected) => {
  if (isConnected) void deliveriesQuery.refetch()
})

// Template-slot typing note: indexing gitDeliveryStatusStyles directly
// with `row.original.status` inside the #status-cell slot below type-
// checks fine under `vue-tsc --noEmit` but resolves to `any` under the
// stricter project-reference build (`vue-tsc -b`, what `npm run build`
// actually runs) — the slot's row type doesn't infer through cleanly
// there. A small typed wrapper sidesteps it either way.
function deliveryStatusStyle(status: GitDeliveryStatus) {
  return gitDeliveryStatusStyles[status]
}

const deliveryColumns: TableColumn<GitDelivery>[] = [
  { accessorKey: 'received_at', header: 'When' },
  { accessorKey: 'ref', header: 'Branch' },
  { accessorKey: 'commit_sha', header: 'Commit' },
  { accessorKey: 'status', header: 'Outcome' },
  { id: 'deployment', header: '' },
]

// --- Provider hints --------------------------------------------------------

const providerHintItems: (AccordionItem & { slot: string })[] = [
  { label: 'GitHub', slot: 'github', icon: 'i-lucide-github' },
  { label: 'GitLab', slot: 'gitlab', icon: 'i-lucide-gitlab' },
  { label: 'Gitea', slot: 'gitea', icon: 'i-lucide-server' },
]

const detectedAccordionValue = computed(() => {
  switch (gitSource.value?.provider) {
    case 'github':
      return ['0']
    case 'gitlab':
      return ['1']
    case 'gitea':
      return ['2']
    default:
      return ['0']
  }
})
</script>

<template>
  <div class="flex flex-col gap-6">
    <UAlert
      v-if="loadError"
      color="error"
      variant="subtle"
      title="Couldn't load git connection"
      :description="gitQuery.error.value instanceof ApiError ? gitQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
    />

    <div v-else-if="gitQuery.isPending.value" class="flex items-center justify-center rounded-lg border border-slate-800 py-16 text-sm text-slate-500">
      Loading git connection…
    </div>

    <template v-else>
      <!-- Connect form: shown when nothing is connected, or while editing
           an existing connection (editing.value, toggled from the facts
           card below). -->
      <UCard v-if="!connected || editing" variant="subtle" :ui="{ root: 'ring-slate-800' }">
        <template #header>
          <h2 class="text-sm font-medium text-slate-400">{{ connected ? 'Edit connection' : 'Connect a repo' }}</h2>
        </template>

        <form class="flex flex-col gap-4" novalidate @submit.prevent="submitForm">
          <UFormField label="Repository URL" name="url">
            <UInput v-model="form.url" placeholder="https://github.com/org/repo.git" class="w-full font-mono" :disabled="connectMutation.isPending.value" autofocus />
            <template #hint>
              <span v-if="form.url && !urlValid" class="text-xs text-red-400">Must be an https:// URL.</span>
            </template>
          </UFormField>

          <UFormField label="Branch" name="branch">
            <UInput v-model="form.branch" placeholder="main" class="w-full font-mono" :disabled="connectMutation.isPending.value" />
          </UFormField>

          <UFormField label="Access token" name="token">
            <UInput
              v-model="form.token"
              type="password"
              autocomplete="off"
              :placeholder="connected && gitSource?.token === 'set' ? 'leave blank to keep the stored token' : 'optional — required for private repos'"
              class="w-full font-mono"
              :disabled="connectMutation.isPending.value"
            />
            <template #hint>
              <span class="text-xs text-slate-500">Write-only — never shown again once saved, same as a secret env value.</span>
            </template>
          </UFormField>

          <UAlert v-if="formError" color="error" variant="subtle" :title="formError" icon="i-lucide-alert-circle" />

          <div class="flex justify-end gap-2">
            <UButton v-if="connected" color="neutral" variant="ghost" :disabled="connectMutation.isPending.value" @click="cancelEdit">
              Cancel
            </UButton>
            <UButton type="submit" color="primary" icon="i-lucide-git-branch" :loading="connectMutation.isPending.value" :disabled="!formValid || connectMutation.isPending.value">
              {{ connected ? 'Save changes' : 'Connect repo' }}
            </UButton>
          </div>
        </form>
      </UCard>

      <template v-else-if="gitSource">
        <!-- Connection facts -->
        <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
          <template #header>
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-medium text-slate-400">Connected repo</h2>
              <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-pencil" @click="startEdit">Edit</UButton>
            </div>
          </template>

          <dl class="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
            <div class="col-span-2 min-w-0 sm:col-span-1">
              <dt class="text-xs text-slate-500">Repository</dt>
              <dd class="mt-1 truncate font-mono text-sm text-slate-200" :title="gitSource.url">{{ gitSource.url }}</dd>
            </div>
            <div>
              <dt class="text-xs text-slate-500">Branch</dt>
              <dd class="mt-1 font-mono text-sm text-slate-300">{{ gitSource.branch }}</dd>
            </div>
            <div>
              <dt class="text-xs text-slate-500">Access token</dt>
              <dd class="mt-1 text-sm text-slate-300">
                <UBadge :color="gitSource.token === 'set' ? 'success' : 'neutral'" variant="subtle">
                  {{ gitSource.token === 'set' ? 'Set' : 'Not set (public repo)' }}
                </UBadge>
              </dd>
            </div>
          </dl>
        </UCard>

        <!-- Webhook setup -->
        <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
          <template #header>
            <h2 class="text-sm font-medium text-slate-400">Webhook</h2>
          </template>

          <div class="flex flex-col gap-4">
            <UAlert
              v-if="gitSource.warnings?.length"
              color="warning"
              variant="subtle"
              icon="i-lucide-alert-triangle"
              :title="gitSource.warnings[0]"
            />

            <div>
              <p class="mb-1.5 text-xs text-slate-500">Payload URL — paste this into your forge's webhook settings.</p>
              <div class="flex items-center gap-2">
                <span class="min-w-0 flex-1 truncate rounded-md border border-slate-800 bg-slate-900/50 px-3 py-1.5 font-mono text-sm text-slate-200">
                  {{ gitSource.webhook_url }}
                </span>
                <UButton size="sm" color="neutral" variant="ghost" square icon="i-lucide-copy" aria-label="Copy webhook URL" @click="copyText(gitSource.webhook_url, 'Webhook URL')" />
              </div>
            </div>

            <div>
              <p class="mb-1.5 text-xs text-slate-500">
                Secret — visible on this page whenever you view it (BasePod doesn't hide it after first connect, since you
                may need to paste it again). Treat this tab as sensitive.
              </p>
              <div class="flex items-center gap-2">
                <span class="min-w-0 flex-1 truncate rounded-md border border-slate-800 bg-slate-900/50 px-3 py-1.5 font-mono text-sm text-slate-200">
                  {{ gitSource.secret }}
                </span>
                <UButton size="sm" color="neutral" variant="ghost" square icon="i-lucide-copy" aria-label="Copy secret" @click="copyText(gitSource.secret, 'Webhook secret')" />
                <UPopover :open="rotateConfirmOpen" @update:open="(v: boolean) => (rotateConfirmOpen = v)">
                  <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-rotate-cw">Rotate</UButton>
                  <template #content>
                    <div class="flex max-w-64 flex-col gap-2 p-3">
                      <p class="text-xs text-slate-300">
                        Rotate the webhook secret? Any webhook already configured on your forge will need updating with the
                        new value.
                      </p>
                      <div class="flex justify-end gap-2">
                        <UButton size="xs" color="neutral" variant="ghost" @click="rotateConfirmOpen = false">Cancel</UButton>
                        <UButton size="xs" color="warning" :loading="rotateMutation.isPending.value" @click="rotateMutation.mutate()">Rotate</UButton>
                      </div>
                    </div>
                  </template>
                </UPopover>
              </div>
            </div>

            <UAccordion :items="providerHintItems" :default-value="detectedAccordionValue" type="multiple">
              <template #github>
                <p class="pb-2 text-xs text-slate-400">
                  Settings → Webhooks → Add webhook. Payload URL: the URL above. Content type:
                  <span class="font-mono">application/json</span>. Secret: the value above. Trigger on: just the push event.
                </p>
              </template>
              <template #gitlab>
                <p class="pb-2 text-xs text-slate-400">
                  Settings → Webhooks → Add new webhook. URL: the URL above. Secret token: the value above. Trigger on push
                  events.
                </p>
              </template>
              <template #gitea>
                <p class="pb-2 text-xs text-slate-400">
                  Settings → Webhooks → Add Webhook (Gitea/Gogs). Target URL: the URL above. Secret: the value above.
                  Trigger on push events.
                </p>
              </template>
            </UAccordion>
          </div>
        </UCard>

        <!-- Deploy now -->
        <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
          <template #header>
            <h2 class="text-sm font-medium text-slate-400">Manual deploy</h2>
          </template>

          <div class="flex flex-col gap-3">
            <UAlert v-if="deployError" color="error" variant="subtle" title="Deploy failed" :description="deployError" icon="i-lucide-alert-circle" />
            <div class="flex items-center justify-between gap-4">
              <p class="text-sm text-slate-400">
                Clones <span class="font-mono text-slate-300">{{ gitSource.branch }}</span> right now and deploys it, without
                waiting for a push.
              </p>
              <UButton color="primary" variant="soft" icon="i-lucide-rocket" :loading="deployMutation.isPending.value" :disabled="deployMutation.isPending.value" @click="deployNow">
                Deploy now
              </UButton>
            </div>
          </div>
        </UCard>

        <!-- Deliveries -->
        <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
          <template #header>
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-medium text-slate-400">Recent deliveries</h2>
              <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-refresh-cw" :loading="deliveriesQuery.isFetching.value" @click="refreshDeliveries">
                Refresh
              </UButton>
            </div>
          </template>

          <UAlert
            v-if="deliveriesQuery.isError.value"
            color="error"
            variant="subtle"
            title="Couldn't load deliveries"
            :description="deliveriesQuery.error.value instanceof ApiError ? deliveriesQuery.error.value.message : 'Something went wrong — try again.'"
          />

          <UTable v-else :data="deliveries" :columns="deliveryColumns" empty="No deliveries yet — push to the configured branch, or use Deploy now above." class="w-full">
            <template #received_at-cell="{ row }">
              <span class="text-xs text-slate-400" :title="row.original.received_at">{{ relativeTime(row.original.received_at) }}</span>
            </template>

            <template #ref-cell="{ row }">
              <span class="font-mono text-xs text-slate-300">{{ formatGitRef(row.original.ref) || '—' }}</span>
            </template>

            <template #commit_sha-cell="{ row }">
              <span v-if="row.original.commit_sha" class="font-mono text-xs text-slate-400" :title="row.original.commit_sha">
                {{ shortSha(row.original.commit_sha) }}
              </span>
              <span v-else class="text-xs text-slate-600">—</span>
            </template>

            <template #status-cell="{ row }">
              <div class="flex flex-col gap-1">
                <UBadge :color="deliveryStatusStyle(row.original.status).color" variant="subtle" class="w-fit items-center gap-1.5">
                  <span
                    class="h-1.5 w-1.5 shrink-0 rounded-full"
                    :class="[deliveryStatusStyle(row.original.status).dotClass, deliveryStatusStyle(row.original.status).pulse && 'status-pulse']"
                    aria-hidden="true"
                  />
                  {{ deliveryStatusStyle(row.original.status).label }}
                </UBadge>
                <span v-if="row.original.detail" class="max-w-xs text-xs text-slate-500">{{ row.original.detail }}</span>
              </div>
            </template>

            <template #deployment-cell="{ row }">
              <span v-if="row.original.deployment_number" class="font-mono text-xs text-slate-400">
                #{{ row.original.deployment_number }}
              </span>
            </template>
          </UTable>
        </UCard>

        <!-- Danger zone -->
        <UCard variant="subtle" :ui="{ root: 'ring-red-900/60' }">
          <template #header>
            <h2 class="text-sm font-medium text-red-400">Danger zone</h2>
          </template>

          <div class="flex items-center justify-between gap-4">
            <p class="text-sm text-slate-400">
              Disconnects this repo: the webhook stops accepting pushes and the stored secret/token are deleted. The app
              itself and its deployment history are untouched.
            </p>
            <UButton color="error" variant="soft" icon="i-lucide-unlink" @click="disconnectOpen = true">Disconnect</UButton>
          </div>
        </UCard>
      </template>
    </template>

    <ConfirmDanger
      :open="disconnectOpen"
      :confirm-text="slug"
      title="Disconnect this repo?"
      description="The webhook stops accepting pushes and the stored secret/token are deleted. This app and its deployment history are untouched."
      confirm-label="Disconnect"
      :loading="disconnectMutation.isPending.value"
      @update:open="(value) => (disconnectOpen = value)"
      @confirm="disconnectMutation.mutate()"
    />
  </div>
</template>
