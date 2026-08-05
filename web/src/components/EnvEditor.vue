<script setup lang="ts">
// AppDetail's Environment tab: a row editor over an app's env var set,
// backed by GET/PUT /apps/{slug}/env (internal/api/env.go).
//
// Key invariant driving the row model: GET always reports value:"" for
// secret entries (their plaintext is never sent to the client once
// sealed) — so a row loaded from the server with is_secret=true starts
// "masked" (rendered as dots, value not editable) until the user
// explicitly clicks "Replace value". Saving a masked row sends value:""
// with is_secret:true, which is exactly the API's keep-on-empty-secret
// rule — no extra bookkeeping needed to preserve an untouched secret.
import { computed, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, ApiError, type EnvVar } from '../lib/api'

const props = defineProps<{ slug: string; redeployRequired: boolean; deploying: boolean }>()

const emit = defineEmits<{
  'update:redeployRequired': [value: boolean]
  redeploy: []
}>()

const toast = useToast()
const queryClient = useQueryClient()

// Mirrors internal/api/env.go's envKeyPattern exactly.
const KEY_PATTERN = /^[A-Z_][A-Z0-9_]{0,63}$/

interface EnvRow {
  id: number
  key: string
  value: string
  isSecret: boolean
  /** true = this row's value came from the server as a sealed secret and
   * hasn't been "replaced" yet — render dots + an affordance instead of
   * an editable input. */
  masked: boolean
}

let nextId = 0
const rows = ref<EnvRow[]>([])
// Deep-ish snapshot of the last loaded/saved state, used for dirty
// tracking — compared structurally (key/value/isSecret/masked), not by
// reference, since rows.value is mutated in place.
let savedSnapshot: Omit<EnvRow, 'id'>[] = []

function comparable(list: EnvRow[]): Omit<EnvRow, 'id'>[] {
  return list.map(({ key, value, isSecret, masked }) => ({ key, value, isSecret, masked }))
}

function loadFromServer(data: EnvVar[]) {
  rows.value = data.map((v) => ({
    id: nextId++,
    key: v.key,
    value: v.value,
    isSecret: v.is_secret,
    masked: v.is_secret,
  }))
  savedSnapshot = comparable(rows.value)
}

// vue-query deeply unwraps refs inside queryKey (MaybeRefDeep) — wrap the
// plain string prop in a computed so a slug change (were this component
// ever reused across apps without remounting) is tracked reactively,
// mirroring AppDetail.vue's own queryKey pattern.
const slugRef = computed(() => props.slug)

const envQuery = useQuery({
  queryKey: ['app-env', slugRef],
  queryFn: () => api.getEnv(props.slug),
})

// Only sync from the server once — a background refetch (e.g. window
// refocus) landing mid-edit must never clobber unsaved row edits. Saves
// re-sync explicitly via loadFromServer() in saveMutation's onSuccess.
let initialized = false
watch(
  envQuery.data,
  (data) => {
    if (data && !initialized) {
      initialized = true
      loadFromServer(data)
    }
  },
  { immediate: true },
)

const bulkMode = ref(false)
const bulkText = ref('')
let bulkSnapshot = ''

function envRowsToText(list: EnvRow[]): string {
  return list
    .filter((r) => !r.isSecret)
    .map((r) => `${r.key}=${r.value}`)
    .join('\n')
}

function enterBulkMode() {
  bulkText.value = envRowsToText(rows.value)
  bulkSnapshot = bulkText.value
  bulkMode.value = true
}

function cancelBulkMode() {
  bulkMode.value = false
  bulkText.value = ''
}

/** Parses .env-format text: blank lines and lines whose first non-blank
 * character is '#' are ignored; every other line splits on the FIRST '='
 * (so values may themselves contain '='); both key and value are
 * trimmed. Lines with no '=' at all are skipped (nothing sane to do with
 * them). Later duplicate keys within the text win over earlier ones,
 * matching typical .env tooling. Surrounding quotes in values are left
 * as-is — this parser doesn't attempt shell-style quote stripping. */
function parseEnvText(text: string): Map<string, string> {
  const out = new Map<string, string>()
  for (const rawLine of text.split('\n')) {
    const line = rawLine.trim()
    if (!line || line.startsWith('#')) continue
    const eq = line.indexOf('=')
    if (eq === -1) continue
    const key = line.slice(0, eq).trim()
    const value = line.slice(eq + 1).trim()
    if (!key) continue
    out.set(key, value)
  }
  return out
}

/** Applying bulk text replaces every non-secret row with what the text
 * now says, and keeps existing secret rows untouched UNLESS the user
 * typed that secret's key into the bulk text themselves — doing so is
 * read as "I want to take this key over as a plain value", so it demotes
 * that row to a public one with the given value rather than silently
 * ignoring it. */
function applyBulk() {
  const parsed = parseEnvText(bulkText.value)
  const keptSecrets = rows.value.filter((r) => r.isSecret && !parsed.has(r.key))
  const newPublicRows: EnvRow[] = [...parsed.entries()].map(([key, value]) => ({
    id: nextId++,
    key,
    value,
    isSecret: false,
    masked: false,
  }))
  rows.value = [...keptSecrets, ...newPublicRows]
  bulkMode.value = false
  bulkText.value = ''
}

function addRow() {
  rows.value.push({ id: nextId++, key: '', value: '', isSecret: false, masked: false })
}

function removeRow(id: number) {
  rows.value = rows.value.filter((r) => r.id !== id)
}

function replaceValue(row: EnvRow) {
  row.masked = false
  row.value = ''
}

function onToggleSecret(row: EnvRow, value: boolean) {
  row.isSecret = value
  // Turning secret protection OFF on a still-masked row: we never have
  // the plaintext to reveal, so the only honest move is to drop the mask
  // and make the user re-enter a value (same as clicking "Replace").
  if (!value && row.masked) {
    row.masked = false
    row.value = ''
  }
}

// Per-row key error: empty message means valid. Blank key + blank value
// (an untouched "Add row") is not an error — it's just dropped from the
// save payload rather than forcing the user to delete it manually.
function keyError(row: EnvRow, allRows: EnvRow[]): string {
  const key = row.key.trim()
  if (!key) {
    // A blank key with a blank value is just an untouched "Add row" —
    // silently dropped from the save payload rather than flagged.
    return row.value !== '' ? 'Key is required' : ''
  }
  if (!KEY_PATTERN.test(key)) {
    return 'Must be uppercase letters, digits, underscore; start with a letter or underscore; max 64 chars'
  }
  const dupes = allRows.filter((r) => r.key.trim() === key)
  if (dupes.length > 1) {
    return 'Duplicate key'
  }
  return ''
}

const rowErrors = computed(() => {
  const errs = new Map<number, string>()
  for (const row of rows.value) {
    const err = keyError(row, rows.value)
    if (err) errs.set(row.id, err)
  }
  return errs
})

const hasErrors = computed(() => rowErrors.value.size > 0)

const isRowsDirty = computed(() => JSON.stringify(comparable(rows.value)) !== JSON.stringify(savedSnapshot))
const isBulkDirty = computed(() => bulkMode.value && bulkText.value !== bulkSnapshot)
const isDirty = computed(() => isRowsDirty.value || isBulkDirty.value)

// Exposed so AppDetail can guard switching away from this tab while
// there are unsaved edits.
defineExpose({ isDirty })

function buildPayload(): EnvVar[] {
  return rows.value
    .filter((r) => r.key.trim() !== '' || r.value !== '')
    .map((r) => ({
      key: r.key.trim(),
      // A still-masked row always submits value:"" — the API's
      // keep-on-empty-secret rule preserves whatever is already sealed
      // for that key. A row the user explicitly replaced (masked=false)
      // submits exactly what they typed, even if that's empty (which,
      // for a key with no prior secret, just seals an empty value).
      value: r.masked ? '' : r.value,
      is_secret: r.isSecret,
    }))
}

const generalError = ref('')

const saveMutation = useMutation({
  mutationFn: () => api.putEnv(props.slug, buildPayload()),
  onSuccess: ({ vars, redeployRequired }) => {
    generalError.value = ''
    loadFromServer(vars)
    emit('update:redeployRequired', redeployRequired)
    void queryClient.invalidateQueries({ queryKey: ['app-env', props.slug] })
    toast.add({ title: 'Environment saved', color: 'success', icon: 'i-lucide-check' })
  },
  onError: (err) => {
    generalError.value = err instanceof ApiError ? err.message : 'Could not save environment variables — try again'
  },
})

function save() {
  if (hasErrors.value || saveMutation.isPending.value) return
  generalError.value = ''
  saveMutation.mutate()
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <UAlert
      v-if="props.redeployRequired"
      color="warning"
      variant="subtle"
      title="Environment changed — redeploy to apply"
      description="Env var changes are saved but don't affect a running container until you redeploy."
      icon="i-lucide-alert-triangle"
    >
      <template #actions>
        <UButton size="sm" color="warning" variant="solid" icon="i-lucide-rocket" :loading="props.deploying" :disabled="props.deploying" @click="emit('redeploy')">
          Redeploy
        </UButton>
      </template>
    </UAlert>

    <UAlert v-if="generalError" color="error" variant="subtle" title="Couldn't save" :description="generalError" icon="i-lucide-alert-circle" />

    <UAlert
      v-if="envQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't load environment variables"
      :description="envQuery.error.value instanceof ApiError ? envQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
    />

    <div v-else-if="envQuery.isPending.value" class="flex items-center justify-center rounded-lg border border-slate-800 py-16 text-sm text-slate-500">
      Loading environment…
    </div>

    <template v-else>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <p class="text-xs text-slate-500">
          Secret values are never shown once saved. Leaving a replaced secret's value empty on save keeps the previously
          stored value.
        </p>
        <div class="flex items-center gap-2">
          <UButton
            size="sm"
            color="neutral"
            variant="ghost"
            :icon="bulkMode ? 'i-lucide-table' : 'i-lucide-file-text'"
            @click="bulkMode ? cancelBulkMode() : enterBulkMode()"
          >
            {{ bulkMode ? 'Row editor' : 'Bulk edit' }}
          </UButton>
        </div>
      </div>

      <div v-if="bulkMode" class="flex flex-col gap-3 rounded-lg border border-slate-800 p-4">
        <p class="text-xs text-slate-500">
          One <span class="font-mono">KEY=VALUE</span> per line. Blank lines and <span class="font-mono">#</span> comments
          are ignored; only the first <span class="font-mono">=</span> splits key from value, so values may contain
          <span class="font-mono">=</span> themselves. Secret values aren't shown here — applying this leaves existing
          secret rows untouched unless you retype their key, which demotes them to a plain value.
        </p>
        <UTextarea v-model="bulkText" :rows="12" class="w-full font-mono text-xs" placeholder="DATABASE_URL=postgres://...&#10;# a comment&#10;FEATURE_FLAG=on" />
        <div class="flex justify-end gap-2">
          <UButton size="sm" color="neutral" variant="ghost" @click="cancelBulkMode">Cancel</UButton>
          <UButton size="sm" color="primary" variant="soft" @click="applyBulk">Apply</UButton>
        </div>
      </div>

      <div v-else class="flex flex-col gap-2">
        <div v-if="!rows.length" class="rounded-lg border border-slate-800 py-10 text-center text-sm text-slate-500">
          No environment variables yet.
        </div>

        <div v-for="row in rows" :key="row.id" class="flex flex-col gap-1.5 rounded-lg border border-slate-800 p-3">
          <div class="flex flex-wrap items-center gap-2 sm:flex-nowrap">
            <UInput
              v-model="row.key"
              placeholder="MY_VAR"
              autocomplete="off"
              autocapitalize="off"
              spellcheck="false"
              class="w-full font-mono sm:w-56"
              :color="rowErrors.get(row.id) ? 'error' : undefined"
            />

            <template v-if="row.masked">
              <span class="min-w-0 flex-1 select-none rounded-md border border-slate-800 bg-slate-900/50 px-3 py-1.5 font-mono text-sm tracking-widest text-slate-500">
                ●●●●●●●●
              </span>
              <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-pencil" @click="replaceValue(row)">
                Replace value
              </UButton>
            </template>
            <UInput
              v-else
              v-model="row.value"
              placeholder="value"
              autocomplete="off"
              spellcheck="false"
              class="w-full min-w-0 flex-1 font-mono"
              :type="row.isSecret ? 'password' : 'text'"
            />

            <USwitch :model-value="row.isSecret" label="Secret" class="shrink-0" @update:model-value="(v: boolean) => onToggleSecret(row, v)" />

            <UButton size="sm" color="error" variant="ghost" square icon="i-lucide-trash-2" aria-label="Remove variable" @click="removeRow(row.id)" />
          </div>
          <p v-if="rowErrors.get(row.id)" class="pl-1 text-xs text-red-400">{{ rowErrors.get(row.id) }}</p>
        </div>

        <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-plus" class="self-start" @click="addRow">
          Add variable
        </UButton>
      </div>

      <div class="flex justify-end">
        <UButton
          color="primary"
          icon="i-lucide-save"
          :loading="saveMutation.isPending.value"
          :disabled="bulkMode || hasErrors || !isRowsDirty || saveMutation.isPending.value"
          @click="save"
        >
          Save
        </UButton>
      </div>
    </template>
  </div>
</template>
