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
import { parseDotEnv, type ParsedEnvEntry } from '../lib/envparse'

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
  /** The key this row had at last server sync, or null if it was added
   * client-side and has never been saved. Distinguishes "blank key on a
   * row that isn't backed by anything yet" (fine, just dropped from the
   * save payload) from "blank key on a row that IS backed by a stored
   * var" (must be a blocking error — silently dropping it from the PUT
   * payload would delete that var on save with no warning). */
  originalKey: string | null
  /** True if, at last server sync, THIS row's stored value was a secret
   * — independent of the current isSecret/masked toggle state. Used to
   * require a fresh typed value when the user demotes a masked secret to
   * public, instead of silently sealing an empty public value over it. */
  hadStoredSecret: boolean
}

let nextId = 0
const rows = ref<EnvRow[]>([])
// Snapshot of the last loaded/saved state, in the same *effective payload*
// shape a save would submit — used for dirty tracking. Comparing effective
// payloads (not raw row/UI state) means UI-only churn that doesn't change
// what would actually be saved — clicking "Replace value" and abandoning
// it, adding then not filling in a blank row — doesn't falsely read as
// unsaved changes.
let savedSnapshot: EnvVar[] = []

/** The exact wire entry a row would contribute to a PUT payload. A still-
 * masked row always submits value:"" — the API's keep-on-empty-secret
 * rule preserves whatever is already sealed for that key. */
function toPayloadEntry(r: EnvRow): EnvVar {
  return { key: r.key.trim(), value: r.masked ? '' : r.value, is_secret: r.isSecret }
}

/** Rows with no key and no value are untouched "Add row" placeholders —
 * dropped rather than submitted or counted toward dirty state. */
function effectivePayload(list: EnvRow[]): EnvVar[] {
  return list.filter((r) => r.key.trim() !== '' || r.value !== '').map(toPayloadEntry)
}

function loadFromServer(data: EnvVar[]) {
  rows.value = data.map((v) => ({
    id: nextId++,
    key: v.key,
    value: v.value,
    isSecret: v.is_secret,
    masked: v.is_secret,
    originalKey: v.key,
    hadStoredSecret: v.is_secret,
  }))
  savedSnapshot = effectivePayload(rows.value)
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

// Bulk-apply that would demote an existing secret to a plain value (see
// applyBulk below) is confirmed first, listing exactly which keys — set
// by applyBulk, read/cleared by the confirm modal in the template.
const bulkDemoteConfirmOpen = ref(false)
const bulkDemoteKeys = ref<string[]>([])

function performBulkApply(parsed: ParsedEnvEntry[]) {
  const keptSecrets = rows.value.filter((r) => r.isSecret && !parsed.some((e) => e.key === r.key))
  const newPublicRows: EnvRow[] = parsed.map(({ key, value }) => ({
    id: nextId++,
    key,
    value,
    isSecret: false,
    masked: false,
    originalKey: null,
    hadStoredSecret: false,
  }))
  rows.value = [...keptSecrets, ...newPublicRows]
  bulkMode.value = false
  bulkText.value = ''
  bulkDemoteConfirmOpen.value = false
  bulkDemoteKeys.value = []
}

/** Applying bulk text replaces every non-secret row with what the text
 * now says, and keeps existing secret rows untouched UNLESS the user
 * typed that secret's key into the bulk text themselves — doing so is
 * read as "I want to take this key over as a plain value", so it demotes
 * that row to a public one with the given value rather than silently
 * ignoring it. Any such demotion is confirmed first, by name, since it's
 * otherwise an easy way to lose a secret without meaning to. */
function applyBulk() {
  const parsed = parseDotEnv(bulkText.value)
  const demoted = rows.value.filter((r) => r.isSecret && parsed.some((e) => e.key === r.key)).map((r) => r.key)
  if (demoted.length) {
    bulkDemoteKeys.value = demoted
    bulkDemoteConfirmOpen.value = true
    return
  }
  performBulkApply(parsed)
}

function confirmBulkDemote() {
  performBulkApply(parseDotEnv(bulkText.value))
}

function cancelBulkDemote() {
  bulkDemoteConfirmOpen.value = false
  bulkDemoteKeys.value = []
}

function addRow() {
  rows.value.push({ id: nextId++, key: '', value: '', isSecret: false, masked: false, originalKey: null, hadStoredSecret: false })
}

// Rows backed by a stored var (originalKey !== null) get a small confirm
// before removal — deleting them is a real, save-triggered data loss, not
// just discarding an in-progress row. Never-saved rows remove immediately.
const confirmRemoveId = ref<number | null>(null)

function removeRow(id: number) {
  rows.value = rows.value.filter((r) => r.id !== id)
  if (confirmRemoveId.value === id) confirmRemoveId.value = null
}

function replaceValue(row: EnvRow) {
  row.masked = false
  row.value = ''
}

function onToggleSecret(row: EnvRow, value: boolean) {
  if (value) {
    if (row.hadStoredSecret && !row.masked && row.value === '') {
      // Nothing was typed after a Replace/demote — cleanly restore the
      // masked view of the still-stored secret rather than treating this
      // as "seal a fresh empty secret value" on save.
      row.masked = true
    }
    row.isSecret = true
    return
  }
  row.isSecret = false
  if (row.masked) {
    // We never have the plaintext to reveal, so the only honest move is
    // to drop the mask and make the user re-enter a value (same as
    // clicking "Replace value").
    row.masked = false
    row.value = ''
  }
}

// Per-row key error: empty message means valid.
function keyError(row: EnvRow, allRows: EnvRow[]): string {
  const key = row.key.trim()
  if (!key) {
    if (row.originalKey !== null) {
      // Blanking the key of a row backed by a stored var would silently
      // drop it from the save payload — the server would then delete
      // that var on save with zero warning. Removal must go through the
      // (confirmed) remove button instead.
      return 'Key is required — use the remove button to delete this variable'
    }
    // A blank key with a blank value on a never-saved row is just an
    // untouched "Add row" — silently dropped from the save payload
    // rather than flagged.
    return row.value !== '' ? 'Key is required' : ''
  }
  if (!KEY_PATTERN.test(key)) {
    return 'Must be uppercase letters, digits, underscore; start with a letter or underscore; max 64 chars'
  }
  const dupes = allRows.filter((r) => r.key.trim() === key)
  if (dupes.length > 1) {
    return 'Duplicate key'
  }
  if (row.hadStoredSecret && !row.isSecret && !row.masked && row.value === '') {
    // Demoted a masked secret to public and left the value empty: saving
    // this would seal an empty PUBLIC value over the stored secret,
    // silently destroying it (unlike leaving is_secret:true+value:"",
    // which the API's keep-on-empty-secret rule preserves).
    return 'Enter a value, or re-enable Secret to keep the stored one'
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

const isRowsDirty = computed(() => JSON.stringify(effectivePayload(rows.value)) !== JSON.stringify(savedSnapshot))
const isBulkDirty = computed(() => bulkMode.value && bulkText.value !== bulkSnapshot)
const isDirty = computed(() => isRowsDirty.value || isBulkDirty.value)

// Exposed so AppDetail can guard switching away from this tab while
// there are unsaved edits.
defineExpose({ isDirty })

const generalError = ref('')

const saveMutation = useMutation({
  mutationFn: () => api.putEnv(props.slug, effectivePayload(rows.value)),
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
        <UButton size="sm" color="warning" variant="solid" class="tap44" icon="i-lucide-rocket" :loading="props.deploying" :disabled="props.deploying" @click="emit('redeploy')">
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

    <div v-else-if="envQuery.isPending.value" class="flex items-center justify-center rounded-lg border border-line py-16 text-sm text-content-muted">
      Loading environment…
    </div>

    <template v-else>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <p class="text-xs text-content-muted">
          Secret values are never shown once saved. Leaving a replaced secret's value empty on save keeps the previously
          stored value.
        </p>
        <div class="flex items-center gap-2">
          <UButton
            size="sm"
            color="neutral"
            variant="ghost"
            class="tap44"
            :icon="bulkMode ? 'i-lucide-table' : 'i-lucide-file-text'"
            @click="bulkMode ? cancelBulkMode() : enterBulkMode()"
          >
            {{ bulkMode ? 'Row editor' : 'Bulk edit' }}
          </UButton>
        </div>
      </div>

      <div v-if="bulkMode" class="flex flex-col gap-3 rounded-lg border border-line p-4">
        <p class="text-xs text-content-muted">
          One <span class="font-mono">KEY=VALUE</span> per line. Blank lines and <span class="font-mono">#</span> comments
          are ignored; only the first <span class="font-mono">=</span> splits key from value, so values may contain
          <span class="font-mono">=</span> themselves. Secret values aren't shown here — applying this leaves existing
          secret rows untouched unless you retype their key, which demotes them to a plain value.
        </p>
        <UTextarea v-model="bulkText" :rows="12" class="w-full font-mono text-xs" placeholder="DATABASE_URL=postgres://...&#10;# a comment&#10;FEATURE_FLAG=on" />
        <div class="tap-row flex justify-end gap-2">
          <UButton size="sm" color="neutral" variant="ghost" class="tap44" @click="cancelBulkMode">Cancel</UButton>
          <UButton size="sm" color="primary" variant="soft" class="tap44" @click="applyBulk">Apply</UButton>
        </div>
      </div>

      <div v-else class="flex flex-col gap-2">
        <div v-if="!rows.length" class="rounded-lg border border-line py-10 text-center text-sm text-content-muted">
          No environment variables yet.
        </div>

        <div v-for="row in rows" :key="row.id" class="flex flex-col gap-1.5 rounded-lg border border-line p-3">
          <!-- Below sm, this stacks into three lines (key / value / switch
               +remove) instead of the wrap-happy single row it used to be —
               a masked-value row with a "Replace value" button next to it
               used to wrap key onto its own line and dump value+switch+
               remove onto a second, cramming the two hardest-to-hit
               controls (switch, remove) against each other. `sm:contents`
               on the two inner wrappers below dissolves them at the sm
               breakpoint, so desktop gets back the exact original flat
               single row. -->
          <div class="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
            <UInput
              v-model="row.key"
              placeholder="MY_VAR"
              autocomplete="off"
              autocapitalize="off"
              spellcheck="false"
              class="w-full font-mono sm:w-56"
              :color="rowErrors.get(row.id) ? 'error' : undefined"
            />

            <div class="flex items-center gap-2 sm:contents">
              <template v-if="row.masked">
                <span class="min-w-0 flex-1 select-none rounded-md border border-line bg-surface-elevated/50 px-3 py-1.5 font-mono text-sm tracking-widest text-content-muted">
                  ●●●●●●●●
                </span>
                <UButton size="sm" color="neutral" variant="ghost" class="tap44" icon="i-lucide-pencil" @click="replaceValue(row)">
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
            </div>

            <div class="flex items-center justify-between gap-2 sm:contents">
              <USwitch
                :model-value="row.isSecret"
                label="Secret"
                class="tap44 shrink-0"
                @update:model-value="(v: boolean) => onToggleSecret(row, v)"
              />

              <UButton
                v-if="row.originalKey === null"
                size="sm"
                color="error"
                variant="ghost"
                square
                class="tap44"
                icon="i-lucide-trash-2"
                aria-label="Remove variable"
                @click="removeRow(row.id)"
              />
              <UPopover v-else :open="confirmRemoveId === row.id" @update:open="(v: boolean) => (confirmRemoveId = v ? row.id : null)">
                <UButton size="sm" color="error" variant="ghost" square class="tap44" icon="i-lucide-trash-2" :aria-label="`Remove ${row.originalKey}`" />
                <template #content>
                  <div class="flex flex-col gap-2 p-3">
                    <p class="max-w-56 text-xs text-content-secondary">
                      Remove <span class="font-mono">{{ row.originalKey }}</span>? Its stored value will be deleted on save.
                    </p>
                    <div class="tap-row flex justify-end gap-2">
                      <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="confirmRemoveId = null">Cancel</UButton>
                      <UButton size="xs" color="error" class="tap44" @click="removeRow(row.id)">Remove</UButton>
                    </div>
                  </div>
                </template>
              </UPopover>
            </div>
          </div>
          <p v-if="rowErrors.get(row.id)" class="pl-1 text-xs text-status-error">{{ rowErrors.get(row.id) }}</p>
        </div>

        <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-plus" class="tap44 self-start" @click="addRow">
          Add variable
        </UButton>
      </div>

      <div class="flex justify-end">
        <UButton
          color="primary"
          icon="i-lucide-save"
          class="tap44"
          :loading="saveMutation.isPending.value"
          :disabled="bulkMode || hasErrors || !isRowsDirty || saveMutation.isPending.value"
          @click="save"
        >
          Save
        </UButton>
      </div>
    </template>
  </div>

  <UModal :open="bulkDemoteConfirmOpen" title="Demote secrets to plain values?" @update:open="(v: boolean) => (v ? undefined : cancelBulkDemote())">
    <template #body>
      <div class="flex flex-col gap-3">
        <p class="text-sm text-content-secondary">
          The bulk text includes {{ bulkDemoteKeys.length === 1 ? 'a key' : 'keys' }} currently stored as secrets. Applying
          will replace {{ bulkDemoteKeys.length === 1 ? 'it' : 'them' }} with the plain value typed in the textarea:
        </p>
        <ul class="flex flex-col gap-1">
          <li v-for="key in bulkDemoteKeys" :key="key" class="font-mono text-sm text-status-deploying">{{ key }}</li>
        </ul>
      </div>
    </template>

    <template #footer>
      <div class="tap-row flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" class="tap44" @click="cancelBulkDemote">Cancel</UButton>
        <UButton color="warning" class="tap44" @click="confirmBulkDemote">Demote and apply</UButton>
      </div>
    </template>
  </UModal>
</template>
