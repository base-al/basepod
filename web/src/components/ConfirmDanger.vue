<script setup lang="ts">
import { ref, watch } from 'vue'

// Generic type-to-confirm modal for destructive actions. The destructive
// button stays disabled until the user types `confirmText` (the app slug,
// for the Delete-app use case) exactly — a deliberate speed bump against
// misclicks, since the action it guards is irreversible.
const props = withDefaults(
  defineProps<{
    open: boolean
    confirmText: string
    title: string
    description?: string
    confirmLabel?: string
    loading?: boolean
  }>(),
  {
    description: undefined,
    confirmLabel: 'Delete',
    loading: false,
  },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  confirm: []
}>()

const typed = ref('')

// Reset the typed confirmation whenever the modal opens/closes so a stale
// match from a previous open can't leave the button armed.
watch(
  () => props.open,
  () => {
    typed.value = ''
  },
)

function onConfirm() {
  if (typed.value !== props.confirmText || props.loading) return
  emit('confirm')
}
</script>

<template>
  <UModal :open="open" :title="title" :dismissible="!loading" @update:open="(value: boolean) => emit('update:open', value)">
    <template #body>
      <div class="flex flex-col gap-4">
        <p v-if="description" class="text-sm text-content-secondary">{{ description }}</p>
        <p class="text-sm text-content-secondary">
          Type <span class="font-mono text-content-primary">{{ confirmText }}</span> to confirm.
        </p>
        <UInput
          v-model="typed"
          autocomplete="off"
          autocapitalize="off"
          spellcheck="false"
          class="w-full font-mono"
          :placeholder="confirmText"
          :disabled="loading"
          @keyup.enter="onConfirm"
        />
      </div>
    </template>

    <template #footer>
      <div class="tap-row flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" class="tap44" :disabled="loading" @click="emit('update:open', false)">
          Cancel
        </UButton>
        <UButton color="error" class="tap44" :disabled="typed !== confirmText" :loading="loading" @click="onConfirm">
          {{ confirmLabel }}
        </UButton>
      </div>
    </template>
  </UModal>
</template>
