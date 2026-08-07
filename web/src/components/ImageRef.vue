<script setup lang="ts">
import { computed } from 'vue'

import { isPendingPlaceholder } from '../lib/pendingImage'

// Middle-truncates long image refs (registry/namespace/repo@sha256:...)
// so the meaningful tail (tag/digest) stays visible instead of just the
// registry prefix. A flexbox split (shrinking head + fixed-width tail)
// gives a true middle ellipsis, unlike the CSS direction:rtl trick.
const props = withDefaults(defineProps<{ value: string; tailChars?: number }>(), {
  tailChars: 18,
})

// NewApp.vue's upload-source placeholder ("localhost/basepod/<slug>:pending",
// set before the first tarball deploy ever lands — see lib/pendingImage.ts)
// isn't a real, pullable image, so the raw ":pending" tag is just noise to
// a human reading it. Shown muted, with the tag dropped in favor of a
// plain-language suffix instead — see AppDetail.vue for the accompanying
// "Deploy" guard and callout for this same state.
const isPending = computed(() => isPendingPlaceholder(props.value))

const displayValue = computed(() => (isPending.value ? props.value.replace(/:pending$/, '') : props.value))

const parts = computed(() => {
  const value = displayValue.value
  const { tailChars } = props
  if (value.length <= tailChars + 3) {
    return { head: value, tail: '' }
  }
  return { head: value.slice(0, value.length - tailChars), tail: value.slice(-tailChars) }
})
</script>

<template>
  <span
    class="flex min-w-0 max-w-full items-center gap-1.5 font-mono text-xs"
    :class="isPending ? 'text-content-muted' : 'text-content-secondary'"
    :title="value"
  >
    <span class="flex min-w-0 shrink" :class="isPending && 'italic'">
      <span class="truncate">{{ parts.head }}</span>
      <span class="shrink-0">{{ parts.tail }}</span>
    </span>
    <span v-if="isPending" class="shrink-0 not-italic text-status-deploying">(no build yet)</span>
  </span>
</template>
