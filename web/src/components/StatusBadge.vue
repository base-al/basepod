<script setup lang="ts">
import { statusStyles, type Status } from '../theme'

// Accepts both AppStatus (Apps.vue list, AppDetail header/overview) and
// DeploymentStatus (AppDetail's deployments table) — see theme.ts for why
// these are separate-but-overlapping enums.
const props = defineProps<{ status: Status }>()
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
