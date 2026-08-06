# Issue #2: Async deploys with live build logs in the CLI

**Status:** Done — CI fully green (`test` and `e2e` both passed on first push).

**Branch:** `feat/async-deploys` (pushed to `origin/feat/async-deploys`, based on `origin/main`; `main` never touched).

**Commits:**
- `5ae2785` feat(build,store): split tarball build into prepare + build phases
- `63baf2c` feat(deploy): async tarball deploy engine + boot-time stuck sweep
- `40392f3` feat(api,server): 202 async tarball deploy + GET deployments/{n}
- `e2f8beb` feat(cli): follow async tarball deploys with a live build log
- `b9112d1` test(e2e): cover the async tarball deploy flow

**Test summary:** `go test ./... -count=1` all green (14 packages); `go test ./internal/deploy/ ./internal/api/ -race -count=1` green; `go vet ./...` clean; `gofmt -l .` clean; `CGO_ENABLED=0 go build ./...` clean; `govulncheck ./...` 0 vulnerabilities; web `npm run build && npm run type-check && npm test` all green (37 tests); `npm audit --audit-level=high` clean (only a pre-existing low-severity esbuild dev-server advisory).

**CI run:** https://github.com/base-al/basepod/actions/runs/31110106249 — `test`: success (1m27s), `e2e`: success (2m13s).

**Concerns / follow-ups:**
- The dashboard (Task 6) needed no functional code change: `web/src/lib/api.ts`'s `uploadTarball` (XHR) and `requestRaw` (fetch) both already treat any 2xx status as success, so a 202 was already handled correctly before this change — only a doc-comment was added to make that explicit.
- `SweepStuckDeployments` deliberately leaves alone the rare edge case where a "deploying" row's generation *does* have a running container (process died in the narrow window after the health probe passed but before the store write landed) — it doesn't try to retroactively mark it healthy, just leaves it for a human/redeploy to notice. Documented in the method's doc comment and covered by `TestSweepStuckDeploymentsLeavesRunningGenerationAlone`.
- `backgroundDeployGrace` (30s) and `asyncBuildTimeout` (60m) are new tunables in `internal/server` and `internal/deploy` respectively — not exposed via config, matching how `deployTimeout`/`buildTimeout` were already hardcoded before this change.
- CLI's `basepod deploy` mints a `build_log` stream token for the live log even though the route also accepts the plain session-token header — done to match the task's explicit instruction and the dashboard's own stream-token model, not because the header path doesn't work.
