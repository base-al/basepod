import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// sse.ts's only dependency is api.ts's mintStreamToken/me/ApiError — mocked
// wholesale here so this test never touches pinia, vue-router, or a real
// network call (api.ts's own request() pulls in useAuthStore and the
// router, neither of which this module needs to know about).
vi.mock('./api', () => {
  class ApiError extends Error {
    status: number
    code: string
    constructor(status: number, code: string, message: string) {
      super(message)
      this.name = 'ApiError'
      this.status = status
      this.code = code
    }
  }
  return {
    api: {
      mintStreamToken: vi.fn(),
      me: vi.fn(),
    },
    ApiError,
  }
})

import { api, ApiError } from './api'
import { connect } from './sse'

// Minimal fake EventSource: records every instance opened (so tests can
// assert on the URL each one was minted for) and lets tests drive
// onopen/onerror by hand — there is no real network in this test.
class FakeEventSource {
  static instances: FakeEventSource[] = []

  url: string
  closed = false
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener() {
    // No test here drives a named "log" event through onEvent; only
    // connection-lifecycle behavior (mint -> open -> error -> re-mint) is
    // under test.
  }

  close() {
    this.closed = true
  }
}

// vi.useFakeTimers() only fakes timer scheduling (setTimeout/setInterval),
// not the microtask queue a resolved/rejected Promise settles through —
// so awaiting a few queued microtasks reliably lets connect()'s internal
// `await api.mintStreamToken(...)` (and everything chained after it)
// actually run, without needing real wall-clock delay.
async function flushMicrotasks(times = 5) {
  for (let i = 0; i < times; i++) {
    await Promise.resolve()
  }
}

const noop = () => {}

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.mocked(api.mintStreamToken).mockReset()
  vi.mocked(api.me).mockReset()
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('sse.connect', () => {
  it('mints a stream token for the given scope/slug before opening the EventSource, and encodes it as ?access_token=', async () => {
    vi.mocked(api.mintStreamToken).mockResolvedValue({ token: 'tok-1', expires_at: '2026-01-01T00:00:00Z' })

    connect('/api/v1/apps/blog/logs', { scope: 'app_logs', slug: 'blog' }, { events: ['log'], onEvent: noop, onStateChange: noop })

    await flushMicrotasks()

    expect(api.mintStreamToken).toHaveBeenCalledExactlyOnceWith({ scope: 'app_logs', slug: 'blog' })
    expect(FakeEventSource.instances).toHaveLength(1)
    expect(FakeEventSource.instances[0].url).toBe('/api/v1/apps/blog/logs?access_token=tok-1')
  })

  it('mints a fresh stream token on every reconnect, not just the first connect', async () => {
    vi.mocked(api.mintStreamToken)
      .mockResolvedValueOnce({ token: 'tok-1', expires_at: 'x' })
      .mockResolvedValueOnce({ token: 'tok-2', expires_at: 'x' })

    connect('/stream', { scope: 'app_logs', slug: 'blog' }, { events: ['log'], onEvent: noop, onStateChange: noop })
    await flushMicrotasks()
    expect(FakeEventSource.instances).toHaveLength(1)

    // Simulate the connection dropping (a network blip, or the minted
    // token expiring mid-stream) — the module owns reconnecting itself
    // rather than relying on EventSource's built-in retry.
    FakeEventSource.instances[0].onerror?.()

    // Advance past the initial reconnect backoff delay.
    await vi.advanceTimersByTimeAsync(1000)
    await flushMicrotasks()

    expect(api.mintStreamToken).toHaveBeenCalledTimes(2)
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(FakeEventSource.instances[1].url).toBe('/stream?access_token=tok-2')
  })

  it('passes build_log scope/slug/deployment_number through to the mint call unchanged', async () => {
    vi.mocked(api.mintStreamToken).mockResolvedValue({ token: 'tok', expires_at: 'x' })

    connect(
      '/api/v1/apps/blog/deployments/3/log',
      { scope: 'build_log', slug: 'blog', deployment_number: 3 },
      { events: ['log'], onEvent: noop, onStateChange: noop, retry: false },
    )
    await flushMicrotasks()

    expect(api.mintStreamToken).toHaveBeenCalledExactlyOnceWith({ scope: 'build_log', slug: 'blog', deployment_number: 3 })
  })

  it('closes for good (without opening an EventSource) when minting 401s, rather than retrying a dead session', async () => {
    vi.mocked(api.mintStreamToken).mockRejectedValue(new ApiError(401, 'unauthorized', 'session expired'))
    const states: string[] = []

    connect('/stream', { scope: 'app_logs', slug: 'blog' }, { events: ['log'], onEvent: noop, onStateChange: (s) => states.push(s) })
    await flushMicrotasks()

    expect(FakeEventSource.instances).toHaveLength(0)
    expect(states.at(-1)).toBe('closed')

    // No further mint attempt even after the would-be backoff window —
    // a 401 is treated as definitive, not just another retryable failure.
    await vi.advanceTimersByTimeAsync(5000)
    expect(api.mintStreamToken).toHaveBeenCalledTimes(1)
  })

  it('with retry:false, a non-401 mint failure closes without scheduling a reconnect', async () => {
    vi.mocked(api.mintStreamToken).mockRejectedValue(new Error('network blip'))
    const states: string[] = []

    connect('/stream', { scope: 'app_logs', slug: 'blog' }, {
      events: ['log'],
      onEvent: noop,
      onStateChange: (s) => states.push(s),
      retry: false,
    })
    await flushMicrotasks()

    expect(states.at(-1)).toBe('closed')
    expect(FakeEventSource.instances).toHaveLength(0)

    await vi.advanceTimersByTimeAsync(5000)
    expect(api.mintStreamToken).toHaveBeenCalledTimes(1)
  })

  it('close() called before the in-flight mint resolves prevents the EventSource from ever opening', async () => {
    let resolveMint: ((v: { token: string; expires_at: string }) => void) | undefined
    vi.mocked(api.mintStreamToken).mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveMint = resolve
        }),
    )

    const conn = connect('/stream', { scope: 'app_logs', slug: 'blog' }, { events: ['log'], onEvent: noop, onStateChange: noop })
    conn.close()
    resolveMint?.({ token: 'too-late', expires_at: 'x' })
    await flushMicrotasks()

    expect(FakeEventSource.instances).toHaveLength(0)
  })
})
