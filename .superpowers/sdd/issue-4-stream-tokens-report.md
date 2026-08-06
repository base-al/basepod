# Issue #4 — scoped short-lived tokens for SSE streams

## Status
Implementation complete on branch `feat/stream-tokens` (from `origin/main`). All Go and
frontend verification is clean locally; pushed and CI to be confirmed (see below).

## What changed
- `internal/store/migrations/00006_stream_tokens.sql` (new; renumbered from 00005 during integration with fix/alias-namespace, which also shipped a 00005): `stream_tokens` table
  (`token_hash` unique, `user_id`, `scope`, `slug`, `deployment_number` nullable,
  `expires_at`), indexed on `expires_at`.
- `internal/store/store.go`: `StreamToken` type, `CreateStreamToken`,
  `StreamTokenByHash` (lazily prunes all expired rows before every lookup — see
  concerns below), `PruneExpiredStreamTokens`, `UserByID`.
- `internal/auth/token.go`: `NewStreamToken()` — same shape as `NewSessionToken` but
  `bp_stream_`-prefixed, SHA-256-hashed at rest identically.
- `internal/api/stream_token.go` (new): `POST /api/v1/stream-token` (behind
  `requireAuth`), scopes `app_logs`/`build_log`, 5-minute expiry
  (`streamTokenDuration`); `requireAuthSSE`/`requireAuthAppLogs`/`requireAuthBuildLog`
  middleware — header path unchanged (full-authority session token, exactly like
  `requireAuth`), query (`?access_token=`) path now only accepts a stream token whose
  scope+slug+deployment_number exactly match the route.
- `internal/api/router.go`: routes rewired onto the new middleware; old
  `requireAuthLogs` removed.
- `web/src/lib/api.ts`: `api.mintStreamToken`.
- `web/src/lib/sse.ts`: `connect()` now takes a `StreamTokenRequest` and mints a fresh
  token before every connection attempt, including every reconnect (a mint 401 closes
  the stream for good immediately, reusing the existing dead-session-probe logic for
  any other mint failure).
- `web/src/components/LogViewer.vue`, `BuildLogPanel.vue`: pass `{scope, slug[,
  deployment_number]}` to `connect()`.

## Tests
- `internal/store`: mint/lookup roundtrip (both scope shapes), unknown hash, expiry,
  lazy-prune-on-lookup, `UserByID`.
- `internal/auth`: `NewStreamToken` shape/hash, prefix disjoint from session tokens.
- `internal/api`: mint endpoint (success both scopes, requires auth, invalid scope
  422, app not found 404, `app_logs`+deployment_number 422, `build_log` missing/unknown
  deployment_number 422/404); SSE routes — stream token works, session token in query
  now 401, wrong slug 401, wrong deployment number 401, expired 401, cross-scope
  rejected **both directions** (`app_logs` token can't read build log and vice versa),
  header auth unchanged on both routes, bogus/garbage token still 401.
- `web`: new `sse.test.ts` — mints before connecting, re-mints on every reconnect
  (mocked `EventSource` + fake timers), mint-401 closes without retry, `retry:false`
  non-401 failure closes without scheduling, `close()` racing an in-flight mint never
  opens a stream.

## Verification run
- `go build ./...`, `go vet ./...`, `gofmt -l .` — clean.
- `go test ./... -count=1` — all packages pass.
- `go test ./internal/api/ -race -count=1` — one **pre-existing, unrelated** failure:
  `TestHandleLoginGlobalCeilingTrips` (real-wall-clock 1-minute rate-limit window +
  argon2 dummy-hash cost × 100 sequential requests, blown past 60s by `-race`'s
  slowdown — reproduces in isolation, 66s under `-race` vs 6.5s without; zero diff
  in `login_ratelimit_test.go`/`auth.go`/`clientip.go` from this branch). CI's
  workflow does not pass `-race` to `go test`, so this does not affect CI. Re-running
  with only the stream-token-related tests under `-race` (`TestHandleAppLogs*`,
  `TestHandleDeploymentLog*`, `TestHandleCreateStreamToken*`, `TestStream*`) is clean.
- `cd web && npm run build && npm run type-check && npm test` — all clean.
- Confirmed CLI is unaffected: `internal/cli/client.go`'s `LogsStream` and the
  build-log fetch both go through `newRequest`, which only ever sets
  `Authorization: Bearer` — no `?access_token=` path exists in the CLI at all, so
  nothing there needed to change.

## Concerns
- **`scripts/e2e-local.sh` will need an update, but I did not touch it** (out of my
  file ownership, and I was told not to run it). Lines ~404 and ~409-410 currently
  do:
  ```
  logs_output=$(curl -sN --max-time 15 \
    "${API_BASE}/api/v1/apps/${SLUG}/logs?follow=0&tail=50&access_token=${TOKEN}")
  ...
  apps_query_token_code=$(http_code "${API_BASE}/api/v1/apps?access_token=${TOKEN}")
  [ "${apps_query_token_code}" = "401" ] || fail "expected ... 401, got ..."
  ```
  `TOKEN` there is the **session** token from `/auth/login`. After this change, that
  now 401s on the logs route too (by design — this is the whole point of the fix).
  The script needs a `POST /api/v1/stream-token {"scope":"app_logs","slug":"..."}`
  call to mint a real stream token before that curl, and a new assertion that the
  bare session token now gets 401 there as well (mirroring the existing
  `apps_query_token_code` check for non-SSE routes).
- Stream-token pruning: per the task's fallback instruction, I did **not** touch
  `internal/server` (outside this task's file-ownership split — it wasn't listed as
  mine, and the task said to prefer avoiding it). Expired stream tokens are instead
  pruned lazily: every `StreamTokenByHash` lookup (i.e., every SSE connection
  attempt) also deletes every expired row, not just the one being checked. Worst case
  (a minted-but-never-used token) is bounded garbage — 5 minutes' worth, not 30 days'
  — until the next lookup or process restart. Wiring `PruneExpiredStreamTokens` into
  `internal/server`'s existing hourly `pruneSessions` ticker is a clean, independent
  follow-up once that file is free to touch.
- Stream tokens are **not single-use** — reusable for any number of requests until
  they expire or no longer match the route. The issue text calls for "single-purpose"
  (scope-limited to one route), not "single-use" (consumed after first use); since
  `sse.ts` already avoids relying on the browser's native EventSource retry (it drives
  its own reconnect loop and mints a fresh token every time), there was no need for
  single-use semantics, and adding them would have added complexity without a stated
  requirement driving it.

## Commits
`1eb6cb7` — `feat(api,store,auth,web): scoped short-lived tokens for SSE streams`
(on top of `origin/main` @ `90bc386`). Pushed to `origin/feat/stream-tokens`.

## CI result
https://github.com/base-al/basepod/actions/runs/31067182221

- `test` job (go build/vet/test, web build/type-check/test): **success**.
- `e2e` job: **failure** — exactly and only at the anticipated point:
  ```
  [e2e] FAIL: logs stream missing an 'event: log' line:
  {"error":{"code":"unauthorized","message":"invalid or expired token"}}
  ```
  This is `scripts/e2e-local.sh` line ~404, still sending the session token as
  `?access_token=` on the logs route — which the fix now correctly rejects (401).
  This is the intended, correct behavior change, not a regression; the script needs
  the coordinated update described under Concerns above. Per this task's explicit
  scope (own only `internal/api/**`, `internal/store/**`, `internal/auth/**`,
  `web/src/lib/sse.ts` + callers) and instruction not to run/touch
  `scripts/e2e-local.sh`, I did not edit it.
