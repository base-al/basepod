<script setup lang="ts">
// Live container-log viewer for AppDetail's Logs tab.
//
// Two networking steps, not one, because native EventSource can't see
// HTTP status codes: a plain fetch (api.logsPreflight) checks up front
// whether the app is even running (surfacing 409 "not_running" as a
// friendly panel instead of a silent, endlessly-reconnecting stream),
// and only once that succeeds does sse.connect() open the actual
// long-lived stream. Neither the preflight nor the stream URL is ever
// logged or surfaced in an error message — the stream URL carries a
// short-lived, single-purpose stream token (minted by sse.connect()
// itself, re-minted on every reconnect) as ?access_token=, never the
// page's own session token.
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'

import { api, ApiError, BASE_URL } from '../lib/api'
import { connect, type SseConnection, type SseState } from '../lib/sse'

const props = defineProps<{ slug: string }>()

// Emitted so AppDetail can send the user to the Overview tab, where the
// deploy actions actually live — LogViewer doesn't own deploy logic.
const emit = defineEmits<{ (e: 'deploy-hint'): void }>()

interface LogLine {
  stream: 'stdout' | 'stderr'
  text: string
}

// Cap applies to both the visible buffer and the paused-pending buffer:
// a long-lived tab left paused (or just left open) must not grow without
// bound, so the oldest lines are dropped once either exceeds this.
const MAX_LINES = 5000

// How close to the bottom (in px) still counts as "at the bottom" for
// stick-to-bottom purposes — a few pixels of rounding/subpixel scroll
// shouldn't be treated as the user deliberately scrolling away.
const BOTTOM_THRESHOLD_PX = 32

const TAIL_OPTIONS = [
  { label: '200 lines', value: 200 },
  { label: '1,000 lines', value: 1000 },
  { label: '5,000 lines', value: 5000 },
]

type Phase = 'loading' | 'not-running' | 'error' | 'ready'
const phase = ref<Phase>('loading')
const errorMessage = ref('')

const connectionState = ref<SseState>('connecting')
const follow = ref(true)
const tail = ref(200)
const paused = ref(false)

const lines = ref<LogLine[]>([])
const pending = ref<LogLine[]>([])

const autoScroll = ref(true)
const logContainer = ref<HTMLElement | null>(null)

let connection: SseConnection | null = null

function pushCapped(arr: LogLine[], line: LogLine) {
  arr.push(line)
  if (arr.length > MAX_LINES) {
    arr.splice(0, arr.length - MAX_LINES)
  }
}

function handleEvent(_name: string, dataJSON: string) {
  let payload: { stream?: string; line?: string }
  try {
    payload = JSON.parse(dataJSON) as { stream?: string; line?: string }
  } catch {
    return // malformed frame — drop rather than let it crash the viewer
  }
  const line: LogLine = { stream: payload.stream === 'stderr' ? 'stderr' : 'stdout', text: payload.line ?? '' }
  pushCapped(paused.value ? pending.value : lines.value, line)
}

function buildStreamURL() {
  return `${BASE_URL}/apps/${props.slug}/logs?follow=${follow.value ? '1' : '0'}&tail=${tail.value}`
}

function startStream() {
  connection = connect(
    buildStreamURL(),
    { scope: 'app_logs', slug: props.slug },
    {
      events: ['log'],
      // follow=0 asks the server for a finite tail that ends on its own;
      // EventSource reports that completion identically to a dropped
      // connection, so retrying it would just re-fetch the same lines in
      // a loop. Only a follow=1 stream — meant to stay open — should
      // reconnect.
      retry: follow.value,
      onEvent: handleEvent,
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

function restartStream() {
  if (phase.value !== 'ready') return
  stopStream()
  startStream()
}

// tail and follow both feed the stream URL's query string — changing
// either only makes sense as a fresh connection, not something the
// existing one can pick up mid-stream.
watch(tail, restartStream)
watch(follow, restartStream)

// Bumped on every initialize() call so a preflight response that lands
// after a newer one was already kicked off (e.g. the slug changed again,
// or retry() was clicked twice) is recognized as stale and ignored rather
// than clobbering phase/connection state that a later call already set up.
let initializeRequestId = 0

async function initialize() {
  const requestId = ++initializeRequestId
  phase.value = 'loading'
  errorMessage.value = ''
  try {
    await api.logsPreflight(props.slug)
    if (requestId !== initializeRequestId) return
    phase.value = 'ready'
    startStream()
  } catch (err) {
    if (requestId !== initializeRequestId) return
    if (err instanceof ApiError && err.code === 'not_running') {
      phase.value = 'not-running'
    } else {
      phase.value = 'error'
      errorMessage.value = err instanceof ApiError ? err.message : 'Could not connect to the log stream — try again.'
    }
  }
}

onMounted(initialize)
onUnmounted(stopStream)

function retry() {
  stopStream()
  void initialize()
}

// AppDetail keeps one LogViewer instance mounted across route-param
// changes for the same route (e.g. navigating from one app's logs
// straight to another's without the tab itself remounting), so a slug
// change has to be handled explicitly rather than relying on
// onMounted/onUnmounted to fire again — otherwise the viewer would keep
// streaming the *previous* app's logs under the new slug's tab.
watch(
  () => props.slug,
  () => {
    stopStream()
    lines.value = []
    pending.value = []
    paused.value = false
    autoScroll.value = true
    void initialize()
  },
)

async function scrollToBottom() {
  await nextTick()
  const el = logContainer.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(
  () => lines.value.length,
  () => {
    if (autoScroll.value) void scrollToBottom()
  },
)

function onLogScroll() {
  const el = logContainer.value
  if (!el) return
  const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
  if (distanceFromBottom > BOTTOM_THRESHOLD_PX) {
    autoScroll.value = false
  }
}

function jumpToLatest() {
  autoScroll.value = true
  void scrollToBottom()
}

function togglePause() {
  if (paused.value) {
    for (const line of pending.value) pushCapped(lines.value, line)
    pending.value = []
  }
  paused.value = !paused.value
}

function clearLogs() {
  lines.value = []
  pending.value = []
}

function downloadLog() {
  const text = lines.value.map((line) => line.text).join('\n')
  const blob = new Blob([text ? `${text}\n` : ''], { type: 'text/plain' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${props.slug}.log`
  a.click()
  URL.revokeObjectURL(url)
}

const connectionChip = computed(() => {
  switch (connectionState.value) {
    case 'connecting':
      return { label: 'Connecting', dotClass: 'bg-ember-400', pulse: true }
    case 'open':
      return { label: 'Live', dotClass: 'bg-moss-400', pulse: false }
    case 'reconnecting':
      return { label: 'Reconnecting', dotClass: 'bg-ember-400', pulse: true }
    case 'closed':
      return { label: 'Closed', dotClass: 'bg-ink-500', pulse: false }
  }
})
</script>

<template>
  <div class="flex flex-col gap-3">
    <template v-if="phase === 'loading'">
      <div class="flex items-center justify-center rounded-lg border border-line py-24 text-sm text-content-muted">
        Checking app status…
      </div>
    </template>

    <template v-else-if="phase === 'not-running'">
      <div class="flex flex-col items-center gap-3 rounded-lg border border-line py-24 text-center">
        <UIcon name="i-lucide-power-off" class="h-6 w-6 text-content-muted" />
        <p class="text-sm font-medium text-content-secondary">App isn't running</p>
        <p class="max-w-sm text-xs text-content-muted">Deploy it to start a container — logs will stream here once it's up.</p>
        <div class="mt-1 flex items-center gap-2">
          <UButton size="sm" color="primary" variant="soft" icon="i-lucide-rocket" @click="emit('deploy-hint')">
            Go to Overview to deploy
          </UButton>
          <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-refresh-cw" @click="retry">Retry</UButton>
        </div>
      </div>
    </template>

    <template v-else-if="phase === 'error'">
      <UAlert color="error" variant="subtle" title="Couldn't load logs" :description="errorMessage" icon="i-lucide-alert-circle">
        <template #actions>
          <UButton size="sm" color="error" variant="soft" icon="i-lucide-refresh-cw" @click="retry">Retry</UButton>
        </template>
      </UAlert>
    </template>

    <template v-else>
      <div class="flex flex-wrap items-center gap-3 rounded-lg border border-line px-3 py-2">
        <UBadge :color="connectionState === 'open' ? 'success' : connectionState === 'closed' ? 'neutral' : 'warning'" variant="subtle" class="items-center gap-1.5">
          <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="[connectionChip?.dotClass, connectionChip?.pulse && 'status-pulse']" aria-hidden="true" />
          {{ connectionChip?.label }}
        </UBadge>

        <USwitch v-model="follow" label="Follow" />

        <USelect v-model="tail" :items="TAIL_OPTIONS" class="w-36" />

        <div class="ml-auto flex items-center gap-1.5">
          <UButton
            size="sm"
            color="neutral"
            variant="ghost"
            :icon="paused ? 'i-lucide-play' : 'i-lucide-pause'"
            @click="togglePause"
          >
            {{ paused ? `Resume${pending.length ? ` (${pending.length})` : ''}` : 'Pause' }}
          </UButton>
          <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-eraser" @click="clearLogs">Clear</UButton>
          <UButton size="sm" color="neutral" variant="ghost" icon="i-lucide-download" :disabled="!lines.length" @click="downloadLog">
            Download
          </UButton>
        </div>
      </div>

      <div class="relative">
        <div
          ref="logContainer"
          class="h-[32rem] overflow-y-auto rounded-lg border border-line bg-surface p-3 font-mono text-xs leading-relaxed"
          @scroll="onLogScroll"
        >
          <p v-if="!lines.length" class="text-content-muted">No log lines yet — waiting for output…</p>
          <div v-for="(line, i) in lines" :key="i" class="whitespace-pre-wrap break-all select-text" :class="line.stream === 'stderr' ? 'text-status-error' : 'text-content-secondary'">
            {{ line.text }}
          </div>
        </div>

        <div v-if="!autoScroll" class="pointer-events-none absolute inset-x-0 bottom-3 flex justify-center">
          <UButton class="pointer-events-auto" size="sm" color="primary" variant="solid" icon="i-lucide-arrow-down" @click="jumpToLatest">
            Jump to latest
          </UButton>
        </div>
      </div>
    </template>
  </div>
</template>
