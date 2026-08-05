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

const BASE_URL = '/api/v1'

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
}

/** One deploy attempt for an app. Note: the API does not return any
 * started/finished timestamps (internal/store.Deployment has no time
 * fields at all) — there is no data to render a "relative time" column
 * from, so the UI omits one rather than fabricate it. */
export interface Deployment {
  number: number
  image: string
  status: DeploymentStatus
  error: string
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

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
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

  if (res.status === 204) {
    return undefined as T
  }
  return (await res.json()) as T
}

export const api = {
  login: (email: string, password: string) =>
    request<LoginResponse>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    }),

  me: () => request<User>('/auth/me'),

  listApps: () => request<App[]>('/apps'),

  createApp: (name: string, image: string, port: number) =>
    request<App>('/apps', {
      method: 'POST',
      body: JSON.stringify({ name, image, port }),
    }),

  getApp: (slug: string) => request<AppDetail>(`/apps/${slug}`),

  /** Triggers a deploy. Omitting `image` redeploys the app's current
   * image. This call is synchronous server-side and can take minutes
   * (image pull + health probe) — request() sets no client-side timeout,
   * so the mutation just waits for it. */
  deploy: (slug: string, image?: string) =>
    request<Deployment>(`/apps/${slug}/deploy`, {
      method: 'POST',
      body: JSON.stringify(image ? { image } : {}),
    }),

  deleteApp: (slug: string) => request<void>(`/apps/${slug}`, { method: 'DELETE' }),

  domains: (slug: string) => request<DomainsList>(`/apps/${slug}/domains`),

  system: () => request<SystemInfo>('/system'),
}
