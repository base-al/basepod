<script setup lang="ts">
import { computed } from 'vue'

import { sparklinePoints, sparklinePolyline } from '../lib/sparkline'
import type { AppStatus } from '../theme'

// Hand-rolled SVG sparkline for the apps-list CPU column (v0.6 batch-
// stats milestone) — deliberately no charting library for a ~60x18px
// trend line with one series and one endpoint dot; see lib/sparkline.ts
// for the actual point math (unit-tested there, since this repo has no
// component-rendering test harness).
//
// Colored by app STATE, not by the data itself — running gets the
// "steady" token, deploying "moving", error "broken" (the design
// concept's own naming for --color-status-running/-deploying/-error,
// docs/design/identity-concept.html section 04's `.spark` mock, ported
// onto this app's actual theme tokens rather than the concept's
// throw-away --steady/--moving/--broken var names). "created"/"stopped"
// — states with no running container, and so never any live CPU data —
// fall back to a muted neutral: still shown as a flat seed line via
// samples.length < 2 (see sparklinePoints), never colored as if it were
// meaningful data.
const props = withDefaults(
  defineProps<{
    samples: number[]
    status: AppStatus
    width?: number
    height?: number
  }>(),
  { width: 62, height: 18 },
)

const strokeColor = computed(() => {
  switch (props.status) {
    case 'running':
      return 'var(--color-status-running)'
    case 'deploying':
      return 'var(--color-status-deploying)'
    case 'error':
      return 'var(--color-status-error)'
    default:
      return 'var(--color-content-muted)'
  }
})

const points = computed(() => sparklinePoints(props.samples, props.width, props.height))
const polyline = computed(() => sparklinePolyline(points.value))
const endpoint = computed(() => points.value[points.value.length - 1]!)
</script>

<template>
  <svg
    class="block shrink-0"
    :width="width"
    :height="height"
    :viewBox="`0 0 ${width} ${height}`"
    aria-hidden="true"
  >
    <polyline :points="polyline" fill="none" :stroke="strokeColor" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" />
    <circle :cx="endpoint.x" :cy="endpoint.y" r="2" :fill="strokeColor" />
  </svg>
</template>
