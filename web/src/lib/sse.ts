// Thin EventSource wrapper with auto-reconnect and exponential backoff.
//
// Native EventSource already retries on its own, but it does so silently
// (no state callback) and — critically — cannot set request headers, so
// it can't carry BasePod's Bearer session token. This module fixes both:
// it authenticates via ?access_token= (see internal/api/router.go's
// requireAuthLogs, the one route that accepts it) and drives its own
// reconnect loop instead of relying on the browser's built-in one, so
// callers get an observable connection-state machine.
//
// The token travels in the URL because that is the only channel a native
// EventSource has — treat it accordingly: this module never logs the URL
// or the token, and callers must not either (see LogViewer.vue).

import { useAuthStore } from '../stores/auth'

export type SseState = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface SseCallbacks {
  /** Fired for each named SSE event (e.g. "log"); data is the event's raw
   * (still-JSON-encoded) payload string, left for the caller to parse. */
  onEvent: (name: string, dataJSON: string) => void
  onStateChange: (state: SseState) => void
}

export interface SseOptions extends SseCallbacks {
  /** Named SSE events to subscribe to (EventSource requires an explicit
   * listener per event name; unnamed "message" events and comment-only
   * heartbeat lines are not delivered through onEvent — EventSource
   * already ignores comment lines on its own). */
  events: string[]
  /** Whether a dropped/closed connection should be retried at all. Pass
   * false for a deliberately finite stream (e.g. follow=0's "just send
   * the tail and stop") so its normal completion — which EventSource
   * reports identically to a network failure — surfaces as 'closed'
   * rather than looping reconnect attempts against a stream that was
   * never meant to stay open. Defaults to true. */
  retry?: boolean
}

export interface SseConnection {
  /** Closes the connection and cancels any pending reconnect timer. Safe
   * to call multiple times. */
  close: () => void
}

const INITIAL_RECONNECT_DELAY_MS = 1000
const MAX_RECONNECT_DELAY_MS = 30000

/**
 * Opens (and, unless `retry` is false, keeps re-opening through backoff)
 * an EventSource against `url`. The current session token is appended as
 * `?access_token=` — the URL passed in should NOT already carry one.
 */
export function connect(url: string, options: SseOptions): SseConnection {
  const auth = useAuthStore()
  const token = auth.token ?? ''
  const separator = url.includes('?') ? '&' : '?'
  const fullURL = `${url}${separator}access_token=${encodeURIComponent(token)}`

  const shouldRetry = options.retry ?? true

  let source: EventSource | null = null
  let reconnectDelay = INITIAL_RECONNECT_DELAY_MS
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let closed = false

  function clearReconnectTimer() {
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
  }

  function open() {
    if (closed) return

    options.onStateChange('connecting')

    const es = new EventSource(fullURL)
    source = es

    es.onopen = () => {
      // A successful open proves the round trip works end to end — reset
      // backoff so a later drop starts retrying fast again rather than
      // inheriting whatever delay a previous flaky stretch grew to.
      reconnectDelay = INITIAL_RECONNECT_DELAY_MS
      options.onStateChange('open')
    }

    for (const name of options.events) {
      es.addEventListener(name, (evt) => {
        options.onEvent(name, (evt as MessageEvent<string>).data)
      })
    }

    es.onerror = () => {
      // Always close the native source ourselves rather than letting it
      // retry on its own: that's what lets us own backoff timing (and,
      // via `retry`, opt out of reconnecting at all) instead of racing
      // the browser's independent default retry loop.
      es.close()
      if (source === es) source = null

      if (closed) return

      if (!shouldRetry) {
        options.onStateChange('closed')
        return
      }

      options.onStateChange('reconnecting')
      const delay = reconnectDelay
      reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY_MS)
      reconnectTimer = setTimeout(open, delay)
    }
  }

  open()

  return {
    close() {
      if (closed) return
      closed = true
      clearReconnectTimer()
      source?.close()
      source = null
      options.onStateChange('closed')
    },
  }
}
