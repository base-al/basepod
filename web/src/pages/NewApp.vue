<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useMutation } from '@tanstack/vue-query'

import { api, ApiError, type ComposePlan } from '../lib/api'
import { slugify } from '../lib/slugify'
import ComposePreview from '../components/ComposePreview.vue'

const router = useRouter()

const name = ref('')
const image = ref('')
const port = ref<number | null>(null)

// Source picker: "image" is the original flow (registry pull via the
// existing Image field below); "upload" instead POSTs a pre-made
// .tar.gz build context to /apps/{slug}/deploy/tarball (see
// internal/api/apps.go's handleDeployTarball) once the app itself
// exists; "compose" (v0.5 Task 10) posts a .tar.gz carrying compose.yaml
// (plus any per-service build contexts) to /api/v1/compose/up — one
// upload becomes N apps, one per service, so it skips the Name/Image/Port
// fields below entirely (see the compose-mode template branch). There's
// no client-side tar step for either upload path — packaging a whole
// local folder in the browser is too heavy for this UI; the `basepod
// deploy` / `basepod compose up` CLIs cover that directly, see the hint
// text near each file picker.
type SourceMode = 'image' | 'upload' | 'compose'
const sourceMode = ref<SourceMode>('image')
const sourceItems: { label: string; value: SourceMode }[] = [
  { label: 'Image', value: 'image' },
  { label: 'Upload build context (.tar.gz)', value: 'upload' },
  { label: 'Compose (.tar.gz)', value: 'compose' },
]

const uploadFile = ref<File | null>(null)
const uploading = ref(false)
// null while the create-app call is in flight (nothing to measure yet —
// rendered as an indeterminate bar), then 0..1 once the upload itself
// starts, updated from XMLHttpRequest's upload progress events (fetch
// exposes no such events, which is why api.uploadTarball uses XHR
// instead — see lib/api.ts).
const uploadProgress = ref<number | null>(null)
// Set once handleCreateApp succeeds for the upload flow, so a retry
// after a failed *upload* (the app already exists at that point) doesn't
// re-POST /apps and collide with itself via app_exists. Cleared whenever
// the name or source changes, since either invalidates which app (if
// any) that earlier create-app call actually made.
const createdAppSlug = ref<string | null>(null)
watch([name, sourceMode], () => {
  createdAppSlug.value = null
})

// Leaving compose mode (e.g. to switch to Image/Upload and back) drops
// any dry-run preview already fetched — coming back to Compose always
// starts from a fresh upload rather than silently reusing a stale plan
// for a file that's no longer selected.
watch(sourceMode, (mode) => {
  if (mode !== 'compose') {
    composePhase.value = 'idle'
    composePlan.value = null
    composeError.value = ''
  }
})

// Mirrors internal/api/apps.go's slugPattern exactly: must start with a
// letter and contain only lowercase letters/digits/hyphens, max 32 chars.
// slugify() itself (imported above) mirrors the Go function of the same
// name — lowercase, spaces -> hyphens, nothing else stripped.
const SLUG_PATTERN = /^[a-z][a-z0-9-]{0,31}$/

const slug = computed(() => slugify(name.value))
const slugValid = computed(() => slug.value.length > 0 && SLUG_PATTERN.test(slug.value))

const portValid = computed(() => port.value !== null && Number.isInteger(port.value) && port.value >= 1 && port.value <= 65535)
const imageValid = computed(() => image.value.trim().length > 0)

const formValid = computed(() => {
  if (sourceMode.value === 'compose') return composeProjectValid.value && composeFile.value !== null
  if (!slugValid.value || !portValid.value) return false
  return sourceMode.value === 'image' ? imageValid.value : uploadFile.value !== null
})

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

// True while either the image-source mutation, the upload-source flow, or
// the compose-source flow below is in flight — drives the shared
// disabled/loading state for fields and the submit/cancel buttons
// regardless of which source is selected.
const submitting = computed(() => {
  if (sourceMode.value === 'image') return createMutation.isPending.value
  if (sourceMode.value === 'compose') return composePhase.value === 'dry-running' || composePhase.value === 'applying'
  return uploading.value
})

/** Upload-source submit path: create the app (skipped on a retry after
 * the app already exists — see createdAppSlug above), then stream the
 * chosen .tar.gz to /deploy/tarball with live progress, then navigate to
 * the new app with its build-log drawer pre-opened. Image-source
 * validation errors (app_exists, the "name"/"image"/"port"-prefixed 422
 * messages) and tarball-phase errors (invalid_content_type,
 * request_too_large, context_too_large, no_containerfile, bad_path,
 * deploy_failed) both flow through applyApiError — none of the tarball
 * codes match its "app_exists"/"validation" special cases, so they all
 * land in the general error banner, which is exactly where an upload
 * failure belongs (it's not about any single form field). */
async function submitUpload() {
  if (uploading.value || !uploadFile.value) return
  fieldErrors.value = { name: '', image: '', port: '' }
  generalError.value = ''
  uploading.value = true
  uploadProgress.value = null

  try {
    let appSlug = createdAppSlug.value
    if (!appSlug) {
      const placeholderImage = `localhost/basepod/${slug.value}:pending`
      const app = await api.createApp(name.value, placeholderImage, port.value as number)
      appSlug = app.slug
      createdAppSlug.value = app.slug
    }

    uploadProgress.value = 0
    const deployment = await api.uploadTarball(appSlug, uploadFile.value, (fraction) => {
      uploadProgress.value = fraction
    })

    void router.push({ name: 'app-detail', params: { slug: appSlug }, query: { buildLog: String(deployment.number) } })
  } catch (err) {
    if (err instanceof ApiError) {
      applyApiError(err)
    } else {
      generalError.value = 'Something went wrong — try again'
    }
  } finally {
    uploading.value = false
  }
}

// --- Compose source (v0.5 Task 10) ----------------------------------------
//
// Two-step flow, never skippable: a dry run (POST /compose/up?dry_run=1)
// returns the full Plan — services, actions, routing, warnings — and
// changes nothing server-side; only after the operator reviews that and
// clicks ComposePreview's "Confirm & apply" does the real apply
// (dry_run=0) run. Mirrors internal/api/compose.go's own contract exactly
// — this screen never has a reason to apply without having shown a
// preview first.
const composeProject = ref('')
const composeFile = ref<File | null>(null)

// The project field is OPTIONAL — mirrors internal/api/compose.go's
// handleComposeUp exactly: an empty ?project= falls back to the compose
// file's own top-level `name:`, and only 422s if neither is present (a
// server-side error this component surfaces via composeError like any
// other compose error, not something worth duplicating client-side).
// When non-empty, loosely mirrors the slug rules service/project names
// feed into server-side (internal/compose's DNS-label + reserved-name
// checks) closely enough to catch an obviously bad name before a round
// trip; the server has the authoritative validation (e.g. the combined
// `<project>-<service>` 32-char cap).
const PROJECT_PATTERN = /^[a-z][a-z0-9-]{0,31}$/
const composeProjectValid = computed(() => composeProject.value === '' || PROJECT_PATTERN.test(composeProject.value))

type ComposePhase = 'idle' | 'dry-running' | 'preview' | 'applying' | 'applied'
const composePhase = ref<ComposePhase>('idle')
const composePlan = ref<ComposePlan | null>(null)
const composeError = ref('')
const composeUploadProgress = ref<number | null>(null)

async function dryRunCompose() {
  if (!composeProjectValid.value || !composeFile.value) return
  composeError.value = ''
  composePhase.value = 'dry-running'
  composeUploadProgress.value = 0
  try {
    const plan = await api.uploadCompose(composeProject.value, composeFile.value, true, (fraction) => {
      composeUploadProgress.value = fraction
    })
    composePlan.value = plan
    composePhase.value = 'preview'
  } catch (err) {
    composeError.value = err instanceof ApiError ? err.message : 'Something went wrong — try again'
    composePhase.value = 'idle'
  } finally {
    composeUploadProgress.value = null
  }
}

async function applyCompose() {
  if (!composeFile.value || composePhase.value !== 'preview') return
  composeError.value = ''
  composePhase.value = 'applying'
  composeUploadProgress.value = 0
  try {
    const plan = await api.uploadCompose(composeProject.value, composeFile.value, false, (fraction) => {
      composeUploadProgress.value = fraction
    })
    composePlan.value = plan
    composePhase.value = 'applied'
  } catch (err) {
    // The dry-run already showed a valid plan against the same upload, so
    // an apply-time failure here is almost always transient (a store
    // write error, a race with something else) rather than another
    // validation problem — stay on the preview (phase back to 'preview')
    // so the operator can just retry Confirm & apply rather than
    // re-uploading the file and project name from scratch.
    composeError.value = err instanceof ApiError ? err.message : 'Something went wrong — try again'
    composePhase.value = 'preview'
  } finally {
    composeUploadProgress.value = null
  }
}

function resetCompose() {
  composePhase.value = 'idle'
  composePlan.value = null
  composeError.value = ''
  composeFile.value = null
}

function onSubmit() {
  if (!formValid.value || submitting.value) return
  fieldErrors.value = { name: '', image: '', port: '' }
  generalError.value = ''
  if (sourceMode.value === 'image') {
    createMutation.mutate()
  } else if (sourceMode.value === 'compose') {
    void dryRunCompose()
  } else {
    void submitUpload()
  }
}
</script>

<template>
  <div class="min-h-screen bg-surface">
    <header class="sticky top-0 z-10 border-b border-line bg-surface/90 backdrop-blur">
      <div class="mx-auto flex max-w-2xl items-center gap-3 px-6 py-3" :class="sourceMode === 'compose' && 'max-w-3xl'">
        <UButton to="/" color="neutral" variant="ghost" square size="sm" icon="i-lucide-arrow-left" aria-label="Back to apps" />
        <span class="text-base font-semibold tracking-tight text-content-primary">New app</span>
      </div>
    </header>

    <main class="mx-auto max-w-2xl px-6 py-8" :class="sourceMode === 'compose' && 'max-w-3xl'">
      <UCard variant="subtle" :ui="{ root: 'ring-line' }">
        <!-- Image / upload sources: the original single-app flow. -->
        <form v-if="sourceMode !== 'compose'" class="flex flex-col gap-5" novalidate @submit.prevent="onSubmit">
          <UFormField label="Name" name="name" :error="fieldErrors.name || undefined">
            <UInput v-model="name" placeholder="My Blog" class="w-full" :disabled="submitting" autofocus />
            <template #hint>
              <span class="font-mono text-xs" :class="name && !slugValid ? 'text-status-error' : 'text-content-muted'">
                {{ slug || 'slug' }}
              </span>
            </template>
          </UFormField>
          <p v-if="name && !slugValid" class="-mt-3 text-xs text-content-muted">
            Slug must start with a letter and contain only lowercase letters, digits, and hyphens (max 32 characters).
          </p>

          <UFormField label="Source">
            <URadioGroup v-model="sourceMode" :items="sourceItems" orientation="horizontal" :disabled="submitting" />
          </UFormField>

          <UFormField v-if="sourceMode === 'image'" label="Image" name="image" :error="fieldErrors.image || undefined">
            <UInput
              v-model="image"
              placeholder="docker.io/library/nginx:alpine"
              class="w-full font-mono"
              :disabled="submitting"
            />
          </UFormField>

          <UFormField v-else label="Build context">
            <UFileUpload v-model="uploadFile" accept=".tar.gz,.tgz,application/gzip" :disabled="submitting" icon="i-lucide-archive" />
            <template #hint>
              <span class="text-xs text-content-muted">
                A gzipped tarball with a Containerfile or Dockerfile at its root. Deploying a local folder directly (no
                packaging step)? Use the <span class="font-mono">basepod deploy</span> CLI instead.
              </span>
            </template>
          </UFormField>

          <div v-if="sourceMode === 'upload' && uploading" class="flex flex-col gap-1.5">
            <UProgress :model-value="uploadProgress === null ? null : Math.round(uploadProgress * 100)" :max="100" />
            <span class="text-xs text-content-muted">
              {{ uploadProgress === null ? 'Creating app…' : `Uploading… ${Math.round(uploadProgress * 100)}%` }}
            </span>
          </div>

          <UFormField label="Port" name="port" :error="fieldErrors.port || undefined">
            <UInput
              v-model.number="port"
              type="number"
              :min="1"
              :max="65535"
              placeholder="8080"
              class="w-full"
              :disabled="submitting"
            />
          </UFormField>

          <UAlert v-if="generalError" color="error" variant="subtle" :title="generalError" icon="i-lucide-alert-circle" />

          <div class="flex justify-end gap-2">
            <UButton to="/" color="neutral" variant="ghost" :disabled="submitting">Cancel</UButton>
            <UButton type="submit" color="primary" :loading="submitting" :disabled="!formValid || submitting">
              {{ sourceMode === 'image' ? 'Create & deploy' : 'Upload & deploy' }}
            </UButton>
          </div>
        </form>

        <!-- Compose source (v0.5 Task 10): project name + upload, then a
             mandatory dry-run preview before anything is created — see
             dryRunCompose/applyCompose's doc comments. One upload becomes
             N apps (one per service), so there's no single slug/image/
             port to collect up front — those live per-service in the
             plan below. -->
        <form v-else class="flex flex-col gap-5" novalidate @submit.prevent="onSubmit">
          <UFormField label="Source">
            <URadioGroup v-model="sourceMode" :items="sourceItems" orientation="horizontal" :disabled="submitting" />
          </UFormField>

          <template v-if="composePhase === 'idle' || composePhase === 'dry-running'">
            <UFormField label="Project name" name="project">
              <UInput v-model="composeProject" placeholder="my-stack" class="w-full font-mono" :disabled="submitting" autofocus />
              <template #hint>
                <span class="text-xs" :class="composeProject && !composeProjectValid ? 'text-status-error' : 'text-content-muted'">
                  Groups this file's services on the Apps list; falls back to the file's own top-level
                  <span class="font-mono">name:</span> if left blank.
                </span>
              </template>
            </UFormField>

            <UFormField label="Compose bundle">
              <UFileUpload v-model="composeFile" accept=".tar.gz,.tgz,application/gzip" :disabled="submitting" icon="i-lucide-archive" />
              <template #hint>
                <span class="text-xs text-content-muted">
                  A gzipped tarball with <span class="font-mono">compose.yaml</span> at its root, plus any per-service build
                  contexts it references. Packaging a local folder? Use
                  <span class="font-mono">basepod compose up</span> from the CLI instead — it tars, previews, and applies in
                  one step.
                </span>
              </template>
            </UFormField>

            <div v-if="composePhase === 'dry-running'" class="flex flex-col gap-1.5">
              <UProgress :model-value="composeUploadProgress === null ? null : Math.round(composeUploadProgress * 100)" :max="100" />
              <span class="text-xs text-content-muted">
                {{ composeUploadProgress === null ? 'Parsing…' : `Uploading… ${Math.round(composeUploadProgress * 100)}%` }}
              </span>
            </div>

            <UAlert v-if="composeError" color="error" variant="subtle" :title="composeError" icon="i-lucide-alert-circle" />

            <div class="flex justify-end gap-2">
              <UButton to="/" color="neutral" variant="ghost" :disabled="submitting">Cancel</UButton>
              <UButton type="submit" color="primary" icon="i-lucide-eye" :loading="submitting" :disabled="!formValid || submitting">
                Preview plan
              </UButton>
            </div>
          </template>

          <template v-else-if="composePlan">
            <ComposePreview :plan="composePlan" :confirming="composePhase === 'applying'" @confirm="applyCompose" />

            <UAlert v-if="composeError" color="error" variant="subtle" :title="composeError" icon="i-lucide-alert-circle" />

            <div v-if="composePhase === 'applying'" class="flex flex-col gap-1.5">
              <UProgress :model-value="composeUploadProgress === null ? null : Math.round(composeUploadProgress * 100)" :max="100" />
              <span class="text-xs text-content-muted">Applying…</span>
            </div>

            <div class="flex justify-end gap-2">
              <UButton v-if="composePhase === 'preview'" color="neutral" variant="ghost" :disabled="submitting" @click="resetCompose">
                Back
              </UButton>
              <UButton v-if="composePhase === 'applied'" color="primary" icon="i-lucide-check" to="/">Done</UButton>
            </div>
          </template>
        </form>
      </UCard>
    </main>
  </div>
</template>
