<script setup lang="ts">
import { computed } from 'vue'

// Middle-truncates long image refs (registry/namespace/repo@sha256:...)
// so the meaningful tail (tag/digest) stays visible instead of just the
// registry prefix. A flexbox split (shrinking head + fixed-width tail)
// gives a true middle ellipsis, unlike the CSS direction:rtl trick.
const props = withDefaults(defineProps<{ value: string; tailChars?: number }>(), {
  tailChars: 18,
})

const parts = computed(() => {
  const { value, tailChars } = props
  if (value.length <= tailChars + 3) {
    return { head: value, tail: '' }
  }
  return { head: value.slice(0, value.length - tailChars), tail: value.slice(-tailChars) }
})
</script>

<template>
  <span class="flex min-w-0 max-w-full font-mono text-xs text-slate-400" :title="value">
    <span class="truncate">{{ parts.head }}</span>
    <span class="shrink-0">{{ parts.tail }}</span>
  </span>
</template>
