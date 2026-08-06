# Issue #4 integration report: feat/stream-tokens -> main

## Status

DONE. Merged `origin/feat/stream-tokens` into a new branch off `origin/main`
(`feat/stream-tokens-integration`), resolved both anticipated integration
problems plus one unanticipated one (a `-race`-only flaky test), verified
locally, pushed, and confirmed CI green on both jobs. `main` was never
touched.

## Commits (on `feat/stream-tokens-integration`, pushed to origin)

- `f49f487` — merge commit: `origin/main` + `origin/feat/stream-tokens` (no
  textual conflicts; the migration-number collision is a same-filename
  problem git's merge machinery can't see, not a content conflict)
- `8a6c51e` — fix(store): renumber stream-tokens migration past
  alias-scheme's 00005 (00005 -> 00006), add `TestAllMigrationsApplyCleanly`
- `478d215` — fix(api): make `TestHandleLoginGlobalCeilingTrips`
  clock-injectable (unanticipated `-race` flake, fixed per the task's
  standing instruction: inject the clock, don't widen/skip)
- `10a5254` — fix(e2e): mint scoped stream tokens for log-route auth in
  `scripts/e2e-local.sh`

## Test summary (one line)

`go test ./... -count=1` all green; `go vet ./...` and `gofmt -l` clean;
`web`: build + type-check + `npm test` (37/37) all green after removing
stray `.js` build artifacts my own premature `npm run build` had generated
before `npm ci` (see Concerns); CI `test` and `e2e` jobs both green on the
first push.

## CI

Run: https://github.com/base-al/basepod/actions/runs/31068212609
Conclusion: **success**
- `test`: success (1m39s)
- `e2e`: success (3m22s)

## Concerns / things to double-check before merging to main

1. **Migration renumber (00005 -> 00006_stream_tokens.sql).** No code
   references migration filenames directly — goose embeds `migrations/*.sql`
   via glob and orders by the numeric prefix, so the rename is safe by
   construction. Confirmed via `TestAllMigrationsApplyCleanly`
   (`internal/store/store_test.go`): opens a fresh DB, runs `goose.Up`,
   asserts the DB lands on version 6, and asserts both `apps.alias_scheme`
   (migration 5, from `fix/alias-namespace`) and the `stream_tokens` table
   (migration 6, renumbered) exist together. Only non-code reference was a
   line in `.superpowers/sdd/issue-4-stream-tokens-report.md`, which I
   updated to match.

2. **e2e stream-token flow.** `scripts/e2e-local.sh` now mints an
   `app_logs`-scoped token via `POST /api/v1/stream-token` (session token in
   the `Authorization` header) before the finite log-stream fetch, and a
   `build_log`-scoped token (with `deployment_number`) before an added
   query-token fetch of the build log — asserting its body matches the
   existing header-authed fetch byte-for-byte. Both routes also get a new
   negative assertion: the plain session token as `?access_token=` must now
   return 401. This ran green in CI's `e2e` job (3m22s, no retries needed).

3. **CLI is unaffected, confirmed by inspection.** `internal/cli/client.go`'s
   `LogsStream` only ever sets `Authorization: Bearer` (see its own comment,
   line 348: "no ?access_token= query-string fallback needed") — no
   `access_token` string appears anywhere else in `internal/cli`. This
   matches `requireAuthAppLogs`/`requireAuthBuildLog` in
   `internal/api/stream_token.go`, which still accept a session token via
   the header on both SSE routes; only the query-string path was narrowed to
   stream tokens.

4. **Unanticipated flaky test, fixed.** `TestHandleLoginGlobalCeilingTrips`
   (`internal/api/login_ratelimit_test.go`) drives ~100 sequential real HTTP
   round trips against a 1-minute rate-limit window; under `-race` a run
   could take 60-70s, long enough for the earliest attempts to age out of
   the window before the last one was sent, intermittently sinking the
   assertion (observed 2/5 failures in a `-count=5 -race` run before the
   fix). Fixed by splitting `api.New`'s construction from its route-mounting
   (`internal/api/router.go`'s new `newRouter(a *api)`) so tests can build an
   `*api` by hand and inject a frozen clock into `globalLimiter.nowFunc`
   (already-existing, previously untest-reachable, injection point) via a
   new `newTrustedTestServerWithClock` helper. Re-run
   `go test ./internal/api/ -run TestHandleLoginGlobalCeilingTrips -race
   -count=8 -v` after the fix to confirm no residual flake before relying on
   this in CI going forward (CI's own `test` job doesn't pass `-race`, so
   this was purely a local-diligence fix per the task brief, not something
   blocking the green CI run above).

5. **Self-inflicted noise cleaned up, worth a second look.** An early
   `npm run build` I ran before `npm ci` had populated `node_modules`
   caused `vue-tsc` to silently fall back to emitting `.js` twins of every
   `.ts`/`.vue` file into `web/src/` (its extended tsconfig, which sets
   `noEmit: true`, couldn't be resolved without `node_modules` present).
   Those untracked stray files broke `npm test` (duplicate/broken test
   discovery) until I removed them with `trash` (recoverable, per this
   repo's global instructions) — verified via `git status --porcelain`
   before and after that none of them were tracked. A subsequent real build
   also modified the tracked placeholder `web/dist/index.html`
   (`.gitignore` un-ignores just that one file for `go:embed` to have
   something to embed pre-build); reverted via `git checkout -- ` before
   every commit. Neither `web/dist/index.html` nor any `.js` artifact is in
   any of the three commits above — `git status --porcelain` is clean on
   the pushed branch.
