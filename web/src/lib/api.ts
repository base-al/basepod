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
import type { Role } from './roles'

// Exported (not just module-local) so sse.ts / LogViewer.vue can build the
// live-stream URL from the same root rather than duplicating it.
export const BASE_URL = '/api/v1'

/** The authenticated user, as returned by GET /auth/me and POST
 * /auth/login (openapi.yaml's User schema). `role` landed alongside the
 * users/roles backend (feat/users-roles) — every caller that gates a
 * control on the current user's permissions (Users.vue, Audit.vue) reads
 * it from here, via a fresh GET /auth/me rather than trusting a
 * possibly-stale copy in the auth store, since a role change takes
 * effect on the target's *next* request, not retroactively. */
export interface User {
  email: string
  name: string
  role: Role
}

export interface LoginResponse {
  token: string
  user: User
}

/** One user as listed by GET /users and returned by every user-
 * management mutation (openapi.yaml's UserSummary schema) — a superset
 * of User with `disabled` and `created_at`, since those only make sense
 * describing *another* user from an admin's point of view, never "the
 * user this request is authenticated as". */
export interface UserSummary {
  email: string
  name: string
  role: Role
  disabled: boolean
  created_at: string
}

/** Body for POST /users/invite (openapi.yaml's InviteUserRequest). */
export interface InviteUserRequest {
  email: string
  role: Role
}

/** Response of POST /users/invite (openapi.yaml's InviteUserResponse).
 * `token` is shown in plaintext exactly once — this response is the only
 * place it ever appears; nothing re-fetches it later (mirrors the
 * webhook secret's write-once treatment in GitPanel.vue). There is no
 * endpoint to list or revoke a pending invitation — it lives until
 * `expires_at` or until it's redeemed, whichever comes first. */
export interface InviteUserResponse {
  email: string
  role: Role
  token: string
  expires_at: string
}

/** Body for PATCH /users/{email}/role (openapi.yaml's
 * ChangeUserRoleRequest). */
export interface ChangeUserRoleRequest {
  role: Role
}

/** Body for POST /invitations/accept (openapi.yaml's
 * AcceptInviteRequest). */
export interface AcceptInviteRequest {
  token: string
  name: string
  password: string
}

/** One audit log entry, newest first from GET /audit (openapi.yaml's
 * AuditEntry schema). `actor_email` is "" for an unauthenticated actor —
 * there is none in this API today, per the schema's own doc comment, but
 * AuditPanel/Audit.vue still renders that case rather than assuming it
 * can't happen. */
export interface AuditEntry {
  id: number
  actor_email: string
  action: string
  target: string
  detail: string
  created_at: string
}

/** deploy_strategy values (v0.5 Task 6) — see store.App.DeployStrategy's
 * doc comment. "replace" stops+removes the app's previous container
 * BEFORE creating the new one (required for volume-backed apps so two
 * containers never write the same named volume at once); the trade is
 * that a failed "replace" deploy leaves the app down with status "error"
 * rather than falling back to the old container, since there is none. */
export type DeployStrategy = 'zero-downtime' | 'replace'

/** One of an app's declared named volumes (v0.5 Task 6), in the exact
 * wire shape internal/api/apps.go's volumeResponse returns. `name` is the
 * app-scoped logical name, not the derived libpod volume name
 * ("bp-<slug>-<name>", internal/deploy.VolumeName) — the API never
 * exposes that. Manual volume create/delete is out of scope this
 * milestone; volumes are declared by Task 8's compose apply only. */
export interface Volume {
  name: string
  container_path: string
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
  /** v0.5 Task 6 additions — see DeployStrategy's and Volume's doc
   * comments. Always present (Volumes is [] rather than omitted/null when
   * an app has none). */
  deploy_strategy: DeployStrategy
  volumes: Volume[]
  /** v0.5 Task 8 additions (internal/api/apps.go's appResponse) — "" / ""
   * / false for a hand-created app; set for one created by
   * POST /api/v1/compose/up. compose_project/compose_service key the app
   * to its compose project + service name (Apps.vue groups by the
   * former); internal is true for a service with no `expose:` in the
   * compose file — no Caddy route, no public URL. */
  compose_project: string
  compose_service: string
  internal: boolean
}

/** Body for PATCH /apps/{slug} (internal/api/apps.go's patchAppRequest):
 * every field is optional so a caller can update just one limit (or the
 * deploy strategy) — an omitted field leaves it unchanged server-side. An
 * unrecognized deploy_strategy value 422s. */
export interface PatchAppRequest {
  memory_limit_mb?: number
  cpu_limit?: number
  pids_limit?: number
  deploy_strategy?: DeployStrategy
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
  /** "image" (registry pull), "tarball" (build-from-upload), or "git"
   * (build from a cloned repo — v0.5 Task 4/5). */
  source: string
  /** What initiated this deploy: "api", "rollback", "webhook", or
   * "compose". */
  trigger: string
  /** Whether a build log is available for GET .../deployments/{number}/log
   * (true for tarball- and git-sourced deployments only). */
  has_build_log: boolean
  /** The resolved commit a "git"-sourced deployment built — "" for every
   * other source (v0.5 Task 4/5; internal/api/apps.go's
   * deploymentResponse.GitSha). */
  git_sha: string
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

/** An app's connected git repo config, in the exact wire shape
 * internal/api/git.go's gitSourceResponse returns from PUT, GET, and
 * POST .../apps/{slug}/git/rotate-secret. `secret` is write-only (issue
 * #13): it's "" except on the ONE response that just minted a fresh
 * value — a first connect (PUT with nothing previously connected) or a
 * rotate — matching how every other secret in this product works (env
 * values, `token` below). Treat a non-empty `secret` as something to
 * show the caller immediately and never again; nothing re-fetches it
 * later. `token` is masked to "set"/"" like a secret env value and never
 * round-trips — see PutGitSourceRequest. */
export interface GitSource {
  url: string
  branch: string
  provider: string
  hook_id: string
  secret?: string
  token: string
  webhook_url: string
  warnings?: string[]
}

/** Body for PUT /apps/{slug}/git (internal/api/git.go's
 * putGitSourceRequest). `token` write-only: omit or send "" to leave an
 * already-stored token untouched (mirrors env var PUT's keep-on-empty-
 * secret semantics) — never used to clear a token; DELETE disconnects
 * entirely instead. There is no rotate flag any more — minting a fresh
 * webhook secret is `api.rotateGitSecret`'s one job (issue #13); a plain
 * PUT always keeps hook_id/secret stable, so editing just the
 * branch/token never invalidates a webhook already configured on the
 * forge side. */
export interface PutGitSourceRequest {
  url: string
  branch: string
  token?: string
}

/** One webhook delivery outcome (see migration 00007_git_sources.sql's
 * git_deliveries.status and internal/api/webhook.go's handleGitWebhook)
 * — every reason a push did or didn't trigger a deploy. */
export type GitDeliveryStatus =
  | 'deployed'
  | 'ignored_branch'
  | 'ignored_event'
  | 'invalid_signature'
  | 'rate_limited'
  | 'coalesced'
  | 'error'

/** One row of GET /apps/{slug}/git/deliveries, in the exact wire shape
 * internal/api/git.go's gitDeliveryResponse returns, newest first.
 * deployment_number is present only for a delivery that actually
 * triggered a deploy whose deployment row still exists. */
export interface GitDelivery {
  id: number
  received_at: string
  provider: string
  event: string
  ref: string
  commit_sha: string
  status: GitDeliveryStatus
  detail: string
  deployment_number?: number
}

/** A service's `build:` block, present only for a build service (an
 * `image:` service has no `build` field at all) — see
 * internal/api/compose.go's composeServiceBuildResponse. dockerfile is
 * omitted unless the compose file named a custom one. */
export interface ComposeServiceBuild {
  context: string
  dockerfile?: string
}

/** One named-volume mount: the volume's name and the container path it's
 * mounted at — see internal/api/compose.go's composeServiceVolumeResponse. */
export interface ComposeServiceVolume {
  name: string
  path: string
}

/** One service's entry in a ComposePlan (dry-run preview or real apply
 * response), in the exact wire shape internal/api/compose.go's
 * composeServiceResponse returns. deployment_number is 0/omitted for a
 * dry-run entry (nothing was created yet). deploy_strategy is set (with a
 * warning explaining why in `warnings`) only when the plan force-applies
 * "replace" for a volume-bearing service — see internal/compose's
 * ServicePlan.RecommendedStrategy. image/build/volumes/env_keys exist so a
 * preview can show what a service will actually run (issue #12);
 * env_keys carries ONLY the compose file's env var keys, never their
 * values — a value may be a secret, and this response must never round
 * one back out, matching how every other secret in this product is
 * write-only. */
export interface ComposeService {
  name: string
  slug: string
  action: 'create' | 'update'
  internal: boolean
  port: number
  alias: string
  image?: string
  build?: ComposeServiceBuild
  volumes?: ComposeServiceVolume[]
  env_keys?: string[]
  deploy_strategy?: DeployStrategy | ''
  deployment_number?: number
  warnings?: string[]
}

/** Response of POST /api/v1/compose/up, for both a dry run (200, nothing
 * changed) and a real apply (202, per-service deployments queued in
 * dependency order) — see internal/api/compose.go's composePlanResponse.
 * orphans lists slugs of apps that belong to this project from a prior
 * apply but have no corresponding service in the file just applied —
 * still running, never auto-deleted (see compose.go's package doc
 * comment). warnings is top-level (parser/plan warnings not tied to one
 * service); each service also carries its own in `warnings`. */
export interface ComposePlan {
  project: string
  dry_run: boolean
  services: ComposeService[]
  orphans?: string[]
  warnings?: string[]
}

/** One live session, in the exact wire shape internal/api/auth.go's
 * sessionResponse returns (GET /auth/sessions). created_at/expires_at are
 * RFC3339 strings. current flags exactly the session this very request was
 * authenticated with — never more than one entry in a response. */
export interface Session {
  id: number
  created_at: string
  expires_at: string
  current: boolean
}

/** Body for POST /auth/password (internal/api/auth.go's
 * passwordChangeRequest). */
export interface PasswordChangeRequest {
  current_password: string
  new_password: string
}

/** Body for POST /stream-token (internal/api/stream_token.go's
 * streamTokenRequest). scope "app_logs" streams a running app's container
 * logs (GET .../logs) and must NOT carry deployment_number; scope
 * "build_log" streams one deployment's build log (GET
 * .../deployments/{number}/log) and MUST carry deployment_number; scope
 * "stats" streams ONE app's own resource-usage stats (GET
 * .../apps/{slug}/stats — AppDetail's Overview live-resources card, see
 * AppStatsSample below) and must NOT carry deployment_number, mirroring
 * "app_logs"; scope "all_stats" streams every running app's stats (GET
 * /stats — the apps-list sparklines' batch route, see Apps.vue) and must
 * carry slug "" (it names no single app) and no deployment_number — see
 * sse.ts's connect(), the only caller. "stats" and "all_stats" are
 * deliberately separate scopes server-side (internal/api/stream_token.go)
 * even though they stream the same sample shape — a token minted for one
 * cannot open the other's route. */
export interface StreamTokenRequest {
  scope: 'app_logs' | 'build_log' | 'stats' | 'all_stats'
  slug: string
  deployment_number?: number
}

/** Response shape of POST /stream-token: a short-lived (5-minute),
 * single-purpose token good for exactly the (scope, slug,
 * deployment_number) triple requested, and expires_at (RFC3339) it dies
 * at. See internal/api/stream_token.go's streamTokenResponse. */
export interface StreamTokenResponse {
  token: string
  expires_at: string
}

// --- Batch stats stream (GET /stats) — apps-list sparklines ---------------
// Added alongside Apps.vue/CpuSparkline.vue; kept in this one block so a
// merge with unrelated api.ts changes stays easy to reason about. See
// internal/api/allstats.go's allStatsEventPayload for the exact Go source
// of this shape, and lib/statsBuffer.ts for how Apps.vue folds a stream of
// these into a per-app rolling window.

/** One `event: stats` message's JSON body on the batch-stats stream (GET
 * /stats) — the same fields the per-app stats stream would carry (see
 * internal/api/stats.go's statsEventPayload; there is no TS type for that
 * one yet, since no page consumes it this milestone) plus `slug`,
 * identifying which app the sample is for, since one connection here
 * carries every running app's samples interleaved. cpu_percent is 0-100
 * per core (a container fully using 2 cores reads 200) — normalized onto
 * that scale server-side; see internal/podman.StreamBulkStats' doc
 * comment for why that normalization is necessary at all. */
export interface AllStatsSample {
  slug: string
  cpu_percent: number
  mem_used_bytes: number
  mem_limit_bytes: number
  pids: number
  net_rx_bytes: number
  net_tx_bytes: number
  block_read_bytes: number
  block_write_bytes: number
}

// --- Per-app stats stream (GET /apps/{slug}/stats) — AppDetail Overview's
// live-resources card ---------------------------------------------------
// The route itself (internal/api/stats.go's handleAppStats) predates this
// UI consumer — v0.5's plan had it wired for a future per-app view that no
// page used yet (see AllStatsSample's own doc comment above). The IA
// reorg's Overview tab is that consumer.

/** One `event: stats` message's JSON body on the per-app stats stream (GET
 * .../apps/{slug}/stats) — internal/api/stats.go's statsEventPayload
 * exactly. Identical fields to AllStatsSample MINUS `slug`: this route is
 * already scoped to one app by its URL, so the server never repeats the
 * slug in the payload. AppDetail.vue folds a sample into lib/statsBuffer's
 * same rolling-window shape by attaching the app's own (already-known)
 * slug before handing it to pushStatsSample — that keeps this one small
 * consumer from needing its own copy of statsBuffer's logic. */
export type AppStatsSample = Omit<AllStatsSample, 'slug'>

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
 * The server spools and validates the upload synchronously (a bad upload
 * still fails fast with 413/422, handled below exactly like any other
 * error), but responds 202 Accepted — not 200 — the moment that succeeds,
 * before the build+rollout even starts (issue #2): the returned Deployment
 * is therefore always still "deploying". The `xhr.status >= 200 && < 300`
 * check below already treats 202 as success like any other 2xx, so no
 * status-code branching is needed here; callers (NewApp.vue) just navigate
 * to the app detail page with the (already-valid) deployment number, whose
 * own polling/build-log-stream UI (AppDetail.vue, BuildLogPanel.vue)
 * handles an in-flight "deploying" row the same way it always has.
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

/** Uploads a gzipped tar (compose.yaml plus any per-service build
 * contexts) via POST /api/v1/compose/up?project=&dry_run= (see
 * internal/api/compose.go's handleComposeUp). Mirrors uploadTarball's
 * XMLHttpRequest-based approach (upload-progress events, auth header,
 * 401 interception, error-envelope parsing) — see that function's doc
 * comment for why fetch isn't used here either.
 *
 * A dry run (dryRun: true) returns 200 with the full plan and changes
 * nothing server-side; a real apply returns 202 with per-service
 * deployment numbers already queued. Either way the response body is a
 * ComposePlan — callers branch on `dry_run` if they need to, though in
 * practice NewApp.vue's compose flow already knows which it asked for.
 */
function uploadCompose(
  project: string,
  file: Blob,
  dryRun: boolean,
  onProgress?: (fraction: number) => void,
): Promise<ComposePlan> {
  const auth = useAuthStore()

  const params = new URLSearchParams({ project, dry_run: dryRun ? '1' : '0' })

  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `${BASE_URL}/compose/up?${params.toString()}`)
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
        resolve(body as ComposePlan)
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

  /** Read-only list of an app's declared named volumes (v0.5 Task 6) —
   * same data as App.volumes, independently addressable. Manual volume
   * create/delete is out of scope this milestone (see Volume's doc
   * comment), so there is no corresponding write method here. */
  getAppVolumes: (slug: string) => request<Volume[]>(`/apps/${slug}/volumes`),

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

  /** Lists every live session for the current user, newest first, with
   * exactly one entry flagged `current` (see internal/api/auth.go's
   * handleListSessions). */
  listSessions: () => request<Session[]>('/auth/sessions'),

  /** Revokes a session by id (internal/api/auth.go's handleDeleteSession).
   * Scoped server-side to the caller's own sessions — a 404 covers both
   * "no such session" and "not yours", indistinguishably. Revoking the
   * session this very request is authenticated with logs the caller out
   * immediately; SessionsPanel.vue handles that by checking `current`
   * before/after the call rather than the server special-casing it. */
  deleteSession: (id: number) => request<void>(`/auth/sessions/${id}`, { method: 'DELETE' }),

  /** Changes the current user's password: verifies current_password,
   * enforces the same 8-character minimum `basepod setup` does on the new
   * one, then revokes every OTHER session while leaving this one (the one
   * this request is authenticated with) alive — see
   * internal/api/auth.go's handleChangePassword. Errors (401
   * invalid_credentials, 422 validation) carry a message meant to be shown
   * verbatim. */
  changePassword: (payload: PasswordChangeRequest) =>
    request<void>('/auth/password', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  logsPreflight,

  /** Mints a stream token for one SSE connection attempt (internal/api/
   * stream_token.go's handleCreateStreamToken) — see sse.ts's connect(),
   * which calls this before every EventSource open, including reconnects
   * (the token expires in 5 minutes, well short of a long-lived log
   * view). Throws ApiError with code "validation" (422) for an
   * incoherent scope/deployment_number combination, or "app_not_found" /
   * "deployment_not_found" (404) — none of which sse.ts's callers are
   * expected to hit in practice, since they always pass a scope/slug/
   * deployment matching a page that already loaded successfully. */
  mintStreamToken: (payload: StreamTokenRequest) =>
    request<StreamTokenResponse>('/stream-token', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

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

  /** Fetches an app's connected git repo config. Throws ApiError with
   * code "git_not_connected" (404) if no repo is connected — GitPanel.vue
   * treats that as the steady "not connected" state, not a load error.
   * See internal/api/git.go's handleGetGitSource. */
  getGitSource: (slug: string) => request<GitSource>(`/apps/${slug}/git`),

  /** Connects (first PUT) or updates (re-PUT) an app's git repo config —
   * see PutGitSourceRequest's doc comment for the token semantics. Only
   * a first connect's response carries `secret` (issue #13) — GitPanel.vue
   * shows it once, right off this call's result, never from a later GET.
   * internal/api/git.go's handlePutGitSource. */
  putGitSource: (slug: string, payload: PutGitSourceRequest) =>
    request<GitSource>(`/apps/${slug}/git`, {
      method: 'PUT',
      body: JSON.stringify(payload),
    }),

  /** Mints a fresh webhook secret, returned in the response's `secret`
   * exactly once (issue #13) — the only way to see it again after first
   * connect. hook_id/url/branch/token are all left untouched.
   * internal/api/git.go's handleRotateGitSecret. */
  rotateGitSecret: (slug: string) => request<GitSource>(`/apps/${slug}/git/rotate-secret`, { method: 'POST' }),

  /** Disconnects an app's git repo config. Not an error if none was
   * connected. internal/api/git.go's handleDeleteGitSource. */
  deleteGitSource: (slug: string) => request<void>(`/apps/${slug}/git`, { method: 'DELETE' }),

  /** Lists an app's most recent webhook deliveries, newest first
   * (default limit 20 server-side). internal/api/git.go's
   * handleListGitDeliveries. */
  listGitDeliveries: (slug: string) => request<GitDelivery[]>(`/apps/${slug}/git/deliveries`),

  /** Manual "Deploy now": clones the connected repo's configured branch
   * and hands it to the same async build pipeline a tarball deploy uses
   * (202 + deployment JSON — the build log then streams via
   * BuildLogPanel exactly like any other in-flight deployment).
   * internal/api/git.go's handleDeployGit. 422 "git_not_connected" if no
   * repo is connected; 502/503 for a clone failure or unavailable git. */
  deployGit: (slug: string) => request<Deployment>(`/apps/${slug}/deploy/git`, { method: 'POST' }),

  uploadCompose,

  // --- Users & roles (feat/users-ui) --------------------------------------
  // Backed by internal/api's users/invitations handlers — see
  // api/openapi.yaml's `users`/`audit` tags for the full capability-floor
  // breakdown each of these requires (mirrored client-side by
  // lib/roles.ts's predicates, purely so Users.vue/Audit.vue can render
  // the right controls without a doomed round trip; the server is what
  // actually enforces every one of these with 403 `forbidden` or 409
  // `last_owner`).

  /** Requires `users:read` (admin+). */
  listUsers: () => request<UserSummary[]>('/users'),

  /** Requires `users:invite` (admin+); the target role must be at or
   * below the caller's own rank (see lib/roles.ts's assignableRoles).
   * Throws ApiError code "user_exists" (409) if the email already
   * belongs to a user. */
  inviteUser: (payload: InviteUserRequest) =>
    request<InviteUserResponse>('/users/invite', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  /** Requires `users:role_change` (owner floor). Throws ApiError code
   * "last_owner" (409) if `email` is the last remaining active owner. */
  changeUserRole: (email: string, role: Role) =>
    request<UserSummary>(`/users/${encodeURIComponent(email)}/role`, {
      method: 'PATCH',
      body: JSON.stringify({ role } satisfies ChangeUserRoleRequest),
    }),

  /** Requires `users:disable` (admin+). Revokes every one of the
   * target's live sessions as part of this same call. Throws ApiError
   * code "last_owner" (409) if `email` is the last remaining active
   * owner. */
  disableUser: (email: string) => request<UserSummary>(`/users/${encodeURIComponent(email)}/disable`, { method: 'POST' }),

  /** Requires `users:disable` (admin+). Does not restore any session
   * revoked at disable time. */
  enableUser: (email: string) => request<UserSummary>(`/users/${encodeURIComponent(email)}/enable`, { method: 'POST' }),

  /** Requires `users:remove` (owner floor). Throws ApiError code
   * "last_owner" (409) if `email` is the last remaining active owner. */
  deleteUser: (email: string) => request<void>(`/users/${encodeURIComponent(email)}`, { method: 'DELETE' }),

  /** Deliberately unauthenticated (security model is the single-use
   * token itself, like the git webhook receiver) — redeems an invite,
   * creating the user and logging them in immediately with the same
   * response shape as POST /auth/login. Throws ApiError code
   * "invite_not_found" (404), or "invite_already_used" /
   * "invite_expired" / "user_exists" (409) — AcceptInvite.vue shows a
   * distinct message for each rather than one generic failure. */
  acceptInvite: (payload: AcceptInviteRequest) =>
    request<LoginResponse>('/invitations/accept', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  /** Requires `audit:read` (admin+). Newest first; capped at 1000
   * server-side regardless of `limit`. */
  listAudit: (limit = 100) => request<AuditEntry[]>(`/audit?limit=${limit}`),
}
