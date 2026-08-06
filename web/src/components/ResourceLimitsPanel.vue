<script setup lang="ts">
// AppDetail's Settings tab: an edit form over an app's container resource
// limits (audit finding H2), backed by GET /apps/{slug} (for the current
// values — shares AppDetail's own ['app', slug] query cache, so this
// component causes no extra fetch) and PATCH /apps/{slug}
// (internal/api/apps.go's handlePatchApp).
//
// Each of the three limits has its own "Unlimited" toggle rather than
// requiring the admin to type 0 — 0 is the API's real "unlimited" value
// (see podman.CreateSpec's doc comment: a zero limit is omitted from the
// container spec entirely, not sent as a literal zero), so the toggle is
// just a friendlier way to reach that same value.
import { computed, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, ApiError, type PatchAppRequest } from '../lib/api'

const props = defineProps<{ slug: string }>()

const toast = useToast()
const queryClient = useQueryClient()

// Mirrors internal/api/apps.go's validateMemoryLimit / validateCPULimit /
// validatePidsLimit bounds exactly.
const MIN_MEMORY_MB = 64
const MAX_MEMORY_MB = 262144
const MIN_CPU = 0.1
const MAX_CPU = 64
const MIN_PIDS = 16
const MAX_PIDS = 65536

// Shares AppDetail's own query cache — this panel neither triggers an
// extra fetch nor needs its own polling; PATCH's onSuccess below
// invalidates the same key so every consumer (this panel, the header,
// Overview) sees the new values together.
const slugRef = computed(() => props.slug)
const appQuery = useQuery({
  queryKey: ['app', slugRef],
  queryFn: () => api.getApp(props.slug),
})

interface FormState {
  memoryMB: number
  memoryUnlimited: boolean
  cpu: number
  cpuUnlimited: boolean
  pids: number
  pidsUnlimited: boolean
}

// Last value typed for a field before its "Unlimited" toggle was turned
// on, restored if the admin turns it back off — so toggling Unlimited on
// and back off doesn't lose whatever number was there.
const lastTyped = { memoryMB: 512, cpu: 1, pids: 512 }

function toFormState(memoryMB: number, cpu: number, pids: number): FormState {
  return {
    memoryMB: memoryMB || lastTyped.memoryMB,
    memoryUnlimited: memoryMB === 0,
    cpu: cpu || lastTyped.cpu,
    cpuUnlimited: cpu === 0,
    pids: pids || lastTyped.pids,
    pidsUnlimited: pids === 0,
  }
}

const form = ref<FormState | null>(null)
let savedSnapshot: { memoryMB: number; cpu: number; pids: number } | null = null

// Only sync from the server once — matches EnvEditor's pattern: a
// background refetch (e.g. window refocus) landing mid-edit must never
// clobber unsaved changes. A successful save re-syncs explicitly.
let initialized = false
watch(
  appQuery.data,
  (data) => {
    if (data && !initialized) {
      initialized = true
      form.value = toFormState(data.memory_limit_mb, data.cpu_limit, data.pids_limit)
      savedSnapshot = { memoryMB: data.memory_limit_mb, cpu: data.cpu_limit, pids: data.pids_limit }
    }
  },
  { immediate: true },
)

/** The effective value a field would submit: 0 when its Unlimited toggle
 * is on, regardless of whatever number is still sitting in the (disabled)
 * input beside it. */
function effective(f: FormState): { memoryMB: number; cpu: number; pids: number } {
  return {
    memoryMB: f.memoryUnlimited ? 0 : f.memoryMB,
    cpu: f.cpuUnlimited ? 0 : f.cpu,
    pids: f.pidsUnlimited ? 0 : f.pids,
  }
}

function fieldError(unlimited: boolean, value: number, min: number, max: number, label: string): string {
  if (unlimited) return ''
  if (!Number.isFinite(value)) return `Enter a number, or mark ${label} unlimited`
  if (value < min || value > max) return `${label} must be between ${min} and ${max}, or unlimited`
  return ''
}

const memoryError = computed(() => (form.value ? fieldError(form.value.memoryUnlimited, form.value.memoryMB, MIN_MEMORY_MB, MAX_MEMORY_MB, 'Memory') : ''))
const cpuError = computed(() => (form.value ? fieldError(form.value.cpuUnlimited, form.value.cpu, MIN_CPU, MAX_CPU, 'CPU') : ''))
const pidsError = computed(() => (form.value ? fieldError(form.value.pidsUnlimited, form.value.pids, MIN_PIDS, MAX_PIDS, 'Process') : ''))
const hasErrors = computed(() => !!(memoryError.value || cpuError.value || pidsError.value))

const isDirty = computed(() => {
  if (!form.value || !savedSnapshot) return false
  const eff = effective(form.value)
  return eff.memoryMB !== savedSnapshot.memoryMB || eff.cpu !== savedSnapshot.cpu || eff.pids !== savedSnapshot.pids
})

function onToggleUnlimited(field: 'memory' | 'cpu' | 'pids', value: boolean) {
  if (!form.value) return
  if (!value) {
    // Turning Unlimited off: restore whatever was last typed rather than
    // leaving the input at 0 (a value that would itself fail validation).
    if (field === 'memory' && form.value.memoryMB === 0) form.value.memoryMB = lastTyped.memoryMB
    if (field === 'cpu' && form.value.cpu === 0) form.value.cpu = lastTyped.cpu
    if (field === 'pids' && form.value.pids === 0) form.value.pids = lastTyped.pids
  }
  if (field === 'memory') form.value.memoryUnlimited = value
  if (field === 'cpu') form.value.cpuUnlimited = value
  if (field === 'pids') form.value.pidsUnlimited = value
}

const generalError = ref('')

/** Maps the form's effective values onto the PATCH request's wire field
 * names (internal/api/apps.go's patchAppRequest). */
function toPatchRequest(f: FormState): PatchAppRequest {
  const eff = effective(f)
  return { memory_limit_mb: eff.memoryMB, cpu_limit: eff.cpu, pids_limit: eff.pids }
}

const saveMutation = useMutation({
  mutationFn: () => {
    if (!form.value) throw new Error('form not loaded')
    return api.patchApp(props.slug, toPatchRequest(form.value))
  },
  onSuccess: (updated) => {
    generalError.value = ''
    form.value = toFormState(updated.memory_limit_mb, updated.cpu_limit, updated.pids_limit)
    savedSnapshot = { memoryMB: updated.memory_limit_mb, cpu: updated.cpu_limit, pids: updated.pids_limit }
    void queryClient.invalidateQueries({ queryKey: ['app', props.slug] })
    toast.add({ title: 'Resource limits saved', description: 'Applies on the next deploy.', color: 'success', icon: 'i-lucide-check' })
  },
  onError: (err) => {
    generalError.value = err instanceof ApiError ? err.message : 'Could not save resource limits — try again'
  },
})

function save() {
  if (hasErrors.value || !isDirty.value || saveMutation.isPending.value) return
  generalError.value = ''
  saveMutation.mutate()
}
</script>

<template>
  <UCard variant="subtle" :ui="{ root: 'ring-slate-800' }">
    <template #header>
      <h2 class="text-sm font-medium text-slate-400">Resource limits</h2>
    </template>

    <UAlert
      v-if="appQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't load resource limits"
      :description="appQuery.error.value instanceof ApiError ? appQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
    />

    <div v-else-if="!form" class="flex items-center justify-center py-10 text-sm text-slate-500">Loading…</div>

    <div v-else class="flex flex-col gap-5">
      <p class="text-xs text-slate-500">
        Caps applied to every container this app deploys — memory, CPU, and max processes. A hostile or misbehaving
        image can't balloon memory or fork-bomb past these. Changes take effect on the app's <em>next</em> deploy.
      </p>

      <UAlert v-if="generalError" color="error" variant="subtle" title="Couldn't save" :description="generalError" icon="i-lucide-alert-circle" />

      <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <div class="flex flex-col gap-1.5">
          <label class="text-xs text-slate-500">Memory (MiB)</label>
          <UInput
            :model-value="form.memoryMB"
            type="number"
            :min="MIN_MEMORY_MB"
            :max="MAX_MEMORY_MB"
            :disabled="form.memoryUnlimited || saveMutation.isPending.value"
            :color="memoryError ? 'error' : undefined"
            @update:model-value="(v: string | number) => { form!.memoryMB = Number(v); lastTyped.memoryMB = Number(v) }"
          />
          <USwitch :model-value="form.memoryUnlimited" label="Unlimited" @update:model-value="(v: boolean) => onToggleUnlimited('memory', v)" />
          <p v-if="memoryError" class="text-xs text-red-400">{{ memoryError }}</p>
        </div>

        <div class="flex flex-col gap-1.5">
          <label class="text-xs text-slate-500">CPU (cores)</label>
          <UInput
            :model-value="form.cpu"
            type="number"
            step="0.1"
            :min="MIN_CPU"
            :max="MAX_CPU"
            :disabled="form.cpuUnlimited || saveMutation.isPending.value"
            :color="cpuError ? 'error' : undefined"
            @update:model-value="(v: string | number) => { form!.cpu = Number(v); lastTyped.cpu = Number(v) }"
          />
          <USwitch :model-value="form.cpuUnlimited" label="Unlimited" @update:model-value="(v: boolean) => onToggleUnlimited('cpu', v)" />
          <p v-if="cpuError" class="text-xs text-red-400">{{ cpuError }}</p>
        </div>

        <div class="flex flex-col gap-1.5">
          <label class="text-xs text-slate-500">Max processes</label>
          <UInput
            :model-value="form.pids"
            type="number"
            :min="MIN_PIDS"
            :max="MAX_PIDS"
            :disabled="form.pidsUnlimited || saveMutation.isPending.value"
            :color="pidsError ? 'error' : undefined"
            @update:model-value="(v: string | number) => { form!.pids = Number(v); lastTyped.pids = Number(v) }"
          />
          <USwitch :model-value="form.pidsUnlimited" label="Unlimited" @update:model-value="(v: boolean) => onToggleUnlimited('pids', v)" />
          <p v-if="pidsError" class="text-xs text-red-400">{{ pidsError }}</p>
        </div>
      </div>

      <div class="flex justify-end">
        <UButton
          color="primary"
          icon="i-lucide-save"
          :loading="saveMutation.isPending.value"
          :disabled="hasErrors || !isDirty || saveMutation.isPending.value"
          @click="save"
        >
          Save limits
        </UButton>
      </div>
    </div>
  </UCard>
</template>
