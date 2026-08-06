// Thin typed fetch client for BasePod's REST API v1. No codegen, no
// generic CRUD abstraction — just the handful of typed methods the web
// app actually calls this milestone.
//
// Auth and error handling are centralized here: every request injects the
// Bearer token from the auth store, and this is the single interception
// point for a 401 — it clears the stored session and redirects to /login
// so no page has to remember to do that itself.

import { useAuthStore } from '../stores/auth'
import { router } from '../router'
import type { AppStatus, DeploymentStatus } from '../theme'

// Exported (not just module-local) so sse.ts / LogViewer.vue can build the
// live-stream URL from the same root rather than duplicating it.
export const BASE_URL = '/api/v1'

export interface User {
  email: string
  name: string
}

export interface LoginResponse {
  token: string
  user: User
}

export interface App {
  slug: string
  image: string
  port: number
  status: AppStatus
  /** Container resource limits (audit finding H2), in the exact wire shape
   * internal/api/apps.go's appResponse returns. 0 means unlimited.
   * memory_limit_mb is MiB; cpu_limit is a core count (e.g. 1.5). Applied
   * on the app's *next* deploy — PATCH never touches a running
   * container. */
  memory_limit_mb: number
  cpu_limit: number
  pids_limit: number
}

/** Body for PATCH /apps/{slug} (internal/api/apps.go's patchAppRequest):
 * every field is optional so a caller can update just one limit — an
 * omitted field leaves that limit unchanged server-side. */
export interface PatchAppRequest {
  memory_limit_mb?: number
  cpu_limit?: number
  pids_limit?: number
}

/** One deploy attempt for an app, in the exact wire shape
 * internal/api/apps.go's deploymentResponse returns — snake_case to match
 * the JSON tags directly. started_at is always an RFC3339 string;
 * finished_at is "" until the deployment reaches a terminal status. */
export interface Deployment {
  number: number
  image: string
  status: DeploymentStatus
  error: string
  started_at: string
  /** RFC3339, "" until terminal. Exposed but not yet rendered anywhere —
   * DeploymentList.vue only shows a relative "Started" column for now;
   * this is here for a future deploy-duration display. */
  finished_at: string
  /** "image" (registry pull) or "tarball" (build-from-upload). */
  source: string
  /** What initiated this deploy: "api" or "rollback". */
  trigger: string
  /** Whether a build log is available for GET .../deployments/{number}/log
   * (true for tarball-sourced deployments only). */
  has_build_log: boolean
}

export interface AppDetail extends App {
  deployments: Deployment[]
}

export interface Domain {
  id: number
  hostname: string
}

export interface DomainsList {
  generated: string
  custom: Domain[]
}

/** One environment variable, in the exact wire shape internal/api/env.go
 * returns/accepts — kept snake_case (is_secret) to match the JSON tag
 * directly rather than introduce a camelCase transform layer. Secret
 * entries always report value:"" from GET; see api.putEnv for the
 * keep-on-empty-secret PUT semantics. */
export interface EnvVar {
  key: string
  value: string
  is_secret: boolean
}

export interface PutEnvResult {
  vars: EnvVar[]
  redeployRequired: boolean
}

export interface SystemInfo {
  version: string
  podman: string
  apps: number
}

/** Typed error thrown for any non-2xx response, parsed from BasePod's
 * {"error":{"code","message"}} envelope. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

interface ErrorEnvelope {
  error?: { code?: string; message?: string }
}

/** Shared fetch + auth-header + 401-interception + error-envelope logic,
 * returning the raw Response so callers that need something beyond the
 * parsed JSON body (putEnv needs the X-Basepod-Redeploy-Required response
 * header) aren't forced through request()'s JSON-only return type. */
async function requestRaw(path: string, init: RequestInit = {}): Promise<Response> {
  const auth = useAuthStore()

  const headers = new Headers(init.headers)
  headers.set('Content-Type', 'application/json')
  if (auth.token) {
    headers.set('Authorization', `Bearer ${auth.token}`)
  }

  const res = await fetch(`${BASE_URL}${path}`, { ...init, headers })

  if (!res.ok) {
    let code = 'unknown'
    let message = res.statusText || 'request failed'
    try {
      const body = (await res.json()) as ErrorEnvelope
      code = body.error?.code ?? code
      message = body.error?.message ?? message
    } catch {
      // Non-JSON error body (e.g. proxy/network failure) — keep the
      // statusText fallback above.
    }

    if (res.status === 401) {
      auth.clearSession()
      if (router.currentRoute.value.name !== 'login') {
        void router.push({ name: 'login' })
      }
    }

    throw new ApiError(res.status, code, message)
  }

  return res
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await requestRaw(path, init)
  if (res.status === 204) {
    return undefined as T
  }
  return (await res.json()) as T
}

/**
 * Checks whether GET .../logs would succeed, without actually opening a
 * stream. This exists only because EventSource (used for the live viewer,
 * see sse.ts) gives JS no access to the response's HTTP status or body on
 * failure — so there is no way to distinguish "app not running" (409) from
 * a network blip from inside an EventSource error handler. A plain fetch
 * can see that, so LogViewer.vue calls this once before handing off to
 * sse.connect() for the real stream, using follow=0/tail=1 to keep it
 * cheap. Reuses the same auth-header + 401-handling path as request(),
 * but skips JSON decoding since a successful response here is
 * text/event-stream, not JSON — the body is discarded either way.
 */
async function logsPreflight(slug: string): Promise<void> {
  const auth = useAuthStore()
  const headers = new Headers()
  if (auth.token) {
    headers.set('Authorization', `Bearer ${auth.token}`)
  }

  const res = await fetch(`${BASE_URL}/apps/${slug}/logs?follow=0&tail=1`, { headers })

  if (!res.ok) {
    let code = 'unknown'
    let message = res.statusText || 'request failed'
    try {
      const body = (await res.json()) as ErrorEnvelope
      code = body.error?.code ?? code
      message = body.error?.message ?? message
    } catch {
      // Non-JSON error body — keep the statusText fallback above.
    }

    if (res.status === 401) {
      auth.clearSession()
      if (router.currentRoute.value.name !== 'login') {
        void router.push({ name: 'login' })
      }
    }

    throw new ApiError(res.status, code, message)
  }

  await res.body?.cancel()
}

/** Uploads a gzipped tar build context via POST /apps/{slug}/deploy/tarball
 * (see internal/api/apps.go's handleDeployTarball / maxTarballBody).
 * Implemented with XMLHttpRequest rather than fetch, because fetch
 * exposes no upload-progress events — onProgress (if given) is called
 * with the fraction (0..1) of the file sent so far as the browser
 * streams the request body.
 *
 * Mirrors requestRaw's auth-header injection, 401 interception, and
 * {"error":{code,message}} envelope parsing, but can't reuse it directly
 * since requestRaw is fetch-based. Also note the request body here is
 * the raw gzip stream itself, not JSON — no Content-Type: application/json
 * override like requestRaw applies.
 */
function uploadTarball(slug: string, file: Blob, onProgress?: (fraction: number) => void): Promise<Deployment> {
  const auth = useAuthStore()

  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `${BASE_URL}/apps/${slug}/deploy/tarball`)
    if (auth.token) {
      xhr.setRequestHeader('Authorization', `Bearer ${auth.token}`)
    }
    xhr.setRequestHeader('Content-Type', 'application/gzip')

    xhr.upload.onprogress = (evt) => {
      if (onProgress && evt.lengthComputable) {
        onProgress(evt.loaded / evt.total)
      }
    }

    xhr.onload = () => {
      let body: unknown
      try {
        body = JSON.parse(xhr.responseText) as unknown
      } catch {
        body = undefined
      }

      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(body as Deployment)
        return
      }

      const envelope = body as ErrorEnvelope | undefined
      const code = envelope?.error?.code ?? 'unknown'
      const message = envelope?.error?.message ?? xhr.statusText ?? 'request failed'

      if (xhr.status === 401) {
        auth.clearSession()
        if (router.currentRoute.value.name !== 'login') {
          void router.push({ name: 'login' })
        }
      }

      reject(new ApiError(xhr.status, code, message))
    }

    xhr.onerror = () => {
      reject(new ApiError(0, 'network_error', 'Network error during upload'))
    }

    xhr.send(file)
  })
}

export const api = {
  login: (email: string, password: string) =>
    request<LoginResponse>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    }),

  me: () => request<User>('/auth/me'),

  /** Revokes the current session server-side. Called best-effort by the
   * auth store's logout() — local state is cleared regardless of whether
   * this succeeds. */
  logout: () => request<void>('/auth/logout', { method: 'POST' }),

  listApps: () => request<App[]>('/apps'),

  createApp: (name: string, image: string, port: number) =>
    request<App>('/apps', {
      method: 'POST',
      body: JSON.stringify({ name, image, port }),
    }),

  getApp: (slug: string) => request<AppDetail>(`/apps/${slug}`),

  /** Updates one or more of an app's container resource limits (audit
   * finding H2). See internal/api/apps.go's handlePatchApp: 0 means
   * unlimited, an omitted field is left unchanged, and out-of-range
   * values 422 naming the field. Takes effect on the app's next deploy,
   * not immediately. */
  patchApp: (slug: string, patch: PatchAppRequest) =>
    request<App>(`/apps/${slug}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),

  /** Triggers a deploy. Omitting `image` redeploys the app's current
   * image. This call is synchronous server-side and can take minutes
   * (image pull + health probe) — request() sets no client-side timeout,
   * so the mutation just waits for it. */
  deploy: (slug: string, image?: string) =>
    request<Deployment>(`/apps/${slug}/deploy`, {
      method: 'POST',
      body: JSON.stringify(image ? { image } : {}),
    }),

  /** Rolls the app back to an earlier deployment's exact image. This call
   * runs the same synchronous, potentially-minutes-long rollout as
   * deploy() (see internal/api/apps.go's handleRollback) — no client-side
   * timeout here either. */
  rollbackApp: (slug: string, number: number) =>
    request<Deployment>(`/apps/${slug}/rollback`, {
      method: 'POST',
      body: JSON.stringify({ number }),
    }),

  deleteApp: (slug: string) => request<void>(`/apps/${slug}`, { method: 'DELETE' }),

  getEnv: (slug: string) => request<EnvVar[]>(`/apps/${slug}/env`),

  /** Full replace of an app's env var set. See internal/api/env.go's
   * handlePutEnv: an entry with is_secret=true and value:"" keeps
   * whatever secret is already stored for that key rather than
   * overwriting it with an empty one. The response carries the new GET
   * shape plus the X-Basepod-Redeploy-Required header, which is "true"
   * only when the effective (decrypted) env set actually changed — env
   * changes don't auto-redeploy a running container. */
  putEnv: async (slug: string, vars: EnvVar[]): Promise<PutEnvResult> => {
    const res = await requestRaw(`/apps/${slug}/env`, {
      method: 'PUT',
      body: JSON.stringify(vars),
    })
    const data = (await res.json()) as EnvVar[]
    return { vars: data, redeployRequired: res.headers.get('X-Basepod-Redeploy-Required') === 'true' }
  },

  getDomains: (slug: string) => request<DomainsList>(`/apps/${slug}/domains`),

  addDomain: (slug: string, hostname: string) =>
    request<Domain>(`/apps/${slug}/domains`, {
      method: 'POST',
      body: JSON.stringify({ hostname }),
    }),

  deleteDomain: (slug: string, id: number) => request<void>(`/apps/${slug}/domains/${id}`, { method: 'DELETE' }),

  system: () => request<SystemInfo>('/system'),

  logsPreflight,

  /** Fetches a finished deployment's full build log as plain text (see
   * GET .../deployments/{number}/log in internal/api/logs.go — the
   * plain-text branch of handleDeploymentLog, which serves once the
   * deployment is no longer "deploying"). Throws ApiError with code
   * "no_build_log" (404) if there's nothing to show — either an
   * image-sourced deployment or a build that failed before ever writing
   * its log file. For a deployment still "deploying", callers should use
   * sse.connect() against the same URL instead (see DeploymentList.vue's
   * BuildLogPanel) — the server streams it live rather than returning
   * this all at once. */
  buildLogText: async (slug: string, number: number): Promise<string> => {
    const res = await requestRaw(`/apps/${slug}/deployments/${number}/log`)
    return res.text()
  },

  uploadTarball,
}
