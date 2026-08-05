<script setup lang="ts">
import { statusStyles, type AppStatus } from '../theme'

const props = defineProps<{ status: AppStatus }>()
</script>

<template>
  <UBadge :color="statusStyles[props.status].color" variant="subtle" class="items-center gap-1.5">
    <span
      class="h-1.5 w-1.5 shrink-0 rounded-full"
      :class="[statusStyles[props.status].dotClass, statusStyles[props.status].pulse && 'status-pulse']"
      aria-hidden="true"
    />
    {{ statusStyles[props.status].label }}
  </UBadge>
</template>
