<script setup lang="ts">
// A single deployment row's expanded build-log drawer, used by
// DeploymentList.vue's `#expanded` slot (see internal/api/logs.go's
// handleDeploymentLog).
//
// Two very different network shapes depending on the deployment's
// status, matching the server exactly rather than guessing from a
// preflight request:
//  - "deploying": the server streams the log as SSE (event: log, data:
//    <raw line text> — NOT JSON, unlike the container-log stream in
//    LogViewer.vue) and closes the connection itself once the deploy
//    reaches a terminal status. That makes this an inherently finite
//    stream (like LogViewer's follow=0 case) — retry: false — and the
//    lines already received by the time it closes are the complete log,
//    so there's nothing to re-fetch afterward.
//  - any terminal status: a single plain-text GET returns the whole log
//    at once (api.buildLogText). A 404 "no_build_log" here is a real,
//    if narrow, possibility even though the row that renders this panel
//    only appears when has_build_log was true at last fetch: a build
//    that failed before ever creating its log file reports 404 too (see
//    handleDeploymentLog's doc comment) — rendered as "log unavailable"
//    rather than an error.
import { nextTick, onMounted, onUnmounted, ref, watch } from 'vue'

import { api, ApiError, BASE_URL, type Deployment } from '../lib/api'
import { connect, type SseConnection, type SseState } from '../lib/sse'

const props = defineProps<{ slug: string; deployment: Deployment }>()

// Same cap as LogViewer.vue's container-log viewer: a long build easily
// produces thousands of lines, and a tab left open must not grow the
// buffer without bound.
const MAX_LINES = 5000

type Phase = 'loading' | 'unavailable' | 'error' | 'ready'
const phase = ref<Phase>('loading')
const errorMessage = ref('')
const connectionState = ref<SseState>('connecting')

const lines = ref<string[]>([])
const container = ref<HTMLElement | null>(null)

let connection: SseConnection | null = null

async function scrollToBottom() {
  await nextTick()
  const el = container.value
  if (el) el.scrollTop = el.scrollHeight
}

function pushLine(text: string) {
  lines.value.push(text)
  if (lines.value.length > MAX_LINES) {
    lines.value.splice(0, lines.value.length - MAX_LINES)
  }
  void scrollToBottom()
}

function buildStreamURL() {
  return `${BASE_URL}/apps/${props.slug}/deployments/${props.deployment.number}/log`
}

function startStream() {
  phase.value = 'ready'
  connection = connect(
    buildStreamURL(),
    { scope: 'build_log', slug: props.slug, deployment_number: props.deployment.number },
    {
      events: ['log'],
      retry: false,
      onEvent: (_name, data) => pushLine(data),
      onStateChange: (state) => {
        connectionState.value = state
      },
    },
  )
}

function stopStream() {
  connection?.close()
  connection = null
}

async function loadText() {
  phase.value = 'loading'
  errorMessage.value = ''
  try {
    const text = await api.buildLogText(props.slug, props.deployment.number)
    lines.value = text.length ? text.split('\n') : []
    phase.value = 'ready'
    void scrollToBottom()
  } catch (err) {
    if (err instanceof ApiError && err.code === 'no_build_log') {
      phase.value = 'unavailable'
    } else {
      phase.value = 'error'
      errorMessage.value = err instanceof ApiError ? err.message : 'Could not load the build log — try again.'
    }
  }
}

function initialize() {
  lines.value = []
  errorMessage.value = ''
  if (props.deployment.status === 'deploying') {
    startStream()
  } else {
    void loadText()
  }
}

onMounted(initialize)
onUnmounted(stopStream)

// A different row's panel reuses this component instance rather than
// remounting it (Vue keys `#expanded` by the currently-expanded row via
// v-if in DeploymentList.vue, so in practice this only fires if slug or
// deployment.number genuinely change under an already-mounted instance).
watch(
  () => [props.slug, props.deployment.number],
  () => {
    stopStream()
    initialize()
  },
)

function retry() {
  stopStream()
  initialize()
}

const connectionChip = () => {
  switch (connectionState.value) {
    case 'connecting':
      return { label: 'Connecting', dotClass: 'bg-amber-400', pulse: true }
    case 'open':
      return { label: 'Live', dotClass: 'bg-emerald-400', pulse: false }
    case 'reconnecting':
      return { label: 'Reconnecting', dotClass: 'bg-amber-400', pulse: true }
    case 'closed':
      return { label: 'Closed', dotClass: 'bg-slate-500', pulse: false }
  }
}
</script>

<template>
  <div class="overflow-hidden rounded-lg border border-slate-800 bg-slate-950">
    <div class="flex items-center justify-between border-b border-slate-800 px-3 py-1.5">
      <span class="text-[11px] font-medium tracking-wide text-slate-500 uppercase">
        Build log — deployment #{{ deployment.number }}
      </span>
      <UBadge
        v-if="deployment.status === 'deploying'"
        variant="subtle"
        size="sm"
        :color="connectionState === 'open' ? 'success' : connectionState === 'closed' ? 'neutral' : 'warning'"
        class="items-center gap-1.5"
      >
        <span
          class="h-1.5 w-1.5 shrink-0 rounded-full"
          :class="[connectionChip()?.dotClass, connectionChip()?.pulse && 'status-pulse']"
          aria-hidden="true"
        />
        {{ connectionChip()?.label }}
      </UBadge>
    </div>

    <div v-if="phase === 'loading'" class="flex items-center justify-center py-10 text-xs text-slate-500">
      Loading build log…
    </div>

    <div v-else-if="phase === 'unavailable'" class="flex items-center justify-center py-10 text-xs text-slate-500">
      Log unavailable.
    </div>

    <div v-else-if="phase === 'error'" class="flex flex-col items-center gap-2 py-10 text-xs">
      <span class="text-red-400">{{ errorMessage }}</span>
      <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-refresh-cw" @click="retry">Retry</UButton>
    </div>

    <pre
      v-else
      ref="container"
      class="max-h-96 overflow-y-auto p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all text-slate-300 select-text"
    ><span v-if="!lines.length" class="text-slate-600">No output yet…</span><template v-else>{{ lines.join('\n') }}</template></pre>
  </div>
</template>
