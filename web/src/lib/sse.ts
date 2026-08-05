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
//
// One consequence of EventSource hiding HTTP status from JS: if the
// session token dies (expired/revoked) while a stream is open, every
// reconnect attempt fails exactly the same way a network blip would —
// there's no 401 to see, just an endless "reconnecting" chip, while every
// *other* 401 in the app immediately clears the session and bounces to
// /login. See maybeProbeSession below for how this module closes that gap.

import { api, ApiError } from './api'
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

// After this many *consecutive* failures (errors with no successful open
// in between), pause the backoff loop and check whether the session
// itself is still valid before trying again — see maybeProbeSession.
const FAILURE_PROBE_THRESHOLD = 3

/**
 * Calls the existing authenticated GET /auth/me (Bearer header via
 * fetch, not the query string — this never puts the token anywhere an
 * EventSource-style URL would) to find out whether repeated stream
 * failures are a dead session or just a network blip:
 *
 *  - 401 means the session is actually gone. api.me() already ran
 *    through request()'s single 401-interception point by the time it
 *    rejects, so the auth store is already cleared and the app already
 *    redirected to /login — this function just reports that so the
 *    caller can stop retrying a stream nothing will ever reauthenticate.
 *  - Any other outcome (success, or a non-401 failure — e.g. the probe
 *    itself hit a network blip) means the session might still be good,
 *    so the reconnect loop should keep going.
 */
async function probeSessionIsAlive(): Promise<boolean> {
  try {
    await api.me()
    return true
  } catch (err) {
    return !(err instanceof ApiError && err.status === 401)
  }
}

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
  let consecutiveFailures = 0
  let probing = false

  function clearReconnectTimer() {
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
  }

  function permanentlyClose() {
    closed = true
    clearReconnectTimer()
    source?.close()
    source = null
    options.onStateChange('closed')
  }

  function scheduleReconnect() {
    options.onStateChange('reconnecting')
    const delay = reconnectDelay
    reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY_MS)
    reconnectTimer = setTimeout(open, delay)
  }

  function open() {
    if (closed) return

    options.onStateChange('connecting')

    const es = new EventSource(fullURL)
    source = es

    es.onopen = () => {
      // A successful open proves the round trip works end to end — reset
      // backoff and the failure streak so a later drop starts retrying
      // fast again rather than inheriting whatever delay/streak a
      // previous flaky stretch grew to.
      reconnectDelay = INITIAL_RECONNECT_DELAY_MS
      consecutiveFailures = 0
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

      if (closed || probing) return

      if (!shouldRetry) {
        options.onStateChange('closed')
        return
      }

      consecutiveFailures += 1

      if (consecutiveFailures < FAILURE_PROBE_THRESHOLD) {
        scheduleReconnect()
        return
      }

      // Three-plus failures in a row with no successful open between them
      // — indistinguishable from a dead session by EventSource alone.
      // Pause the backoff loop and ask an endpoint that *can* see a 401.
      probing = true
      options.onStateChange('reconnecting')
      void probeSessionIsAlive().then((alive) => {
        probing = false
        if (closed) return

        if (!alive) {
          // The session is confirmed gone (and api.me() already cleared
          // it / redirected to /login) — retrying this stream can never
          // succeed, so stop for good instead of spinning forever.
          permanentlyClose()
          return
        }

        // Session's still good — this was a real network blip, not a
        // dead token. Reset the streak so the next probe only fires
        // after another run of failures, and keep going.
        consecutiveFailures = 0
        scheduleReconnect()
      })
    }
  }

  open()

  return {
    close() {
      if (closed) return
      permanentlyClose()
    },
  }
}
