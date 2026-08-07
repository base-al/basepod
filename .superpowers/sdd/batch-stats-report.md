# Batch stats endpoint + apps-list sparklines report

## Status

Complete. Branch `feat/batch-stats-sparklines`, based on `origin/main`.
Pushed to `origin/feat/batch-stats-sparklines`. GitHub Actions is in a
known outage this milestone — no CI poller was started; all verification
below is local.

## Commit

- `89716c9` — `feat(api,web): batch stats stream and apps-list CPU sparklines`

## Libpod bulk-stats wire evidence

Verified live against a running podman machine (6.0.1 client / 5.7.1
server, matching this repo's existing per-app stats verification) via
read-only `curl --unix-socket` on the host-side forwarded socket
(`podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}'`),
plus a shallow clone of podman's own source at the matching `v5.7.1` tag
already present in the sandbox from an earlier milestone task.

1. **Route registration** (`pkg/api/server/register_containers.go`):
   `GET /libpod/containers/stats` (no `{name}`) routes to
   `libpod.StatsContainer` — the libpod-**native** handler this time
   (unlike the deprecated per-`{name}` route, served by the docker-compat
   handler per the existing `internal/podman/stats.go` doc comment).
2. **Wire shape**, live-captured:
   `curl .../libpod/containers/stats?stream=false` →
   `{"Error":null,"Stats":[{"AvgCPU":0.83...,"ContainerID":"ad41...","Name":"lisa-shell-build","PerCPU":null,"CPU":0.83...,"CPUNano":...,"MemUsage":136683520,"MemLimit":8293711872,"MemPerc":1.64...,"Network":{"eth0":{"RxBytes":548431857,"TxBytes":2250022,...}},"BlockInput":37580800,"BlockOutput":3710695936,"PIDs":1023,"UpTime":...,"Duration":...},{...bp-caddy...}]}`
   — matches `libpod/define.ContainerStats` (`libpod/define/containerstate.go`)
   exactly: no JSON tags, so wire keys are the bare Go field names.
   Confirmed newline-delimited framing (2 ticks captured in one `curl`
   over 7s produced 2 `\n`-separated JSON objects).
3. **Default enumeration scope** (`pkg/domain/infra/abi/containers.go`'s
   `ContainerEngine.ContainerStats`): with no `containers=` query values,
   the container set comes from `GetRunningContainers` — omitting
   `containers=` already scopes to "every running container", and
   per-container errors while collecting that set are silently skipped
   (`queryAll` mode). **Live-verified the trap this avoids**: passing an
   explicit `containers=<name-that-no-longer-exists>` 404s the **whole**
   request (`{"cause":"no such container",...}`) — a different code path
   (`GetContainersByList`, not skip-tolerant) — so
   `podman.BulkContainerStats` deliberately **never** passes `containers=`.
4. **`filters=` is silently ignored by this endpoint**: live-tested
   `?filters={"label":["basepod.managed=true"]}` still returned
   `lisa-shell-build` (a container with no such label) alongside
   `bp-caddy` — confirmed via `podman inspect`. BasePod-vs-foreign
   attribution is therefore done entirely client-side (see
   `deploy.Engine.RunningAppContainers`, matched against
   `BulkStatsSample.ContainerID`, itself confirmed byte-for-byte identical
   to `ListContainers`' `Id` field for the same container).
5. **CPU-percent scale trap** (the one that would have silently
   under-reported by 5x): `libpod/stats_linux.go`'s `calculateCPUPercent`
   — what populates `define.ContainerStats.CPU` — is
   `(cpuDelta/systemDelta)*100` with **no online-CPU-count factor**,
   unlike this repo's existing per-container `calcCPUPercent`, which
   additionally multiplies by `online_cpus` to match `docker stats`
   convention. Live cross-check: bp-caddy's bulk-endpoint `CPU` (0.0125)
   vs. the *same container, same moment*'s per-container-endpoint
   `online_cpus` (5) vs. `GET /libpod/info`'s `host.cpus` (5, also
   confirmed via `podman info`) — confirms host core count ==
   container's online CPUs for BasePod's un-cpuset-restricted containers,
   so `podman.Client.HostCPUs` (new) + `wireCPU * onlineCPUs` in
   `podman.StreamBulkStats` puts the batch endpoint back on the exact
   same 0-100-per-core scale the existing per-app stream promises.

Full doc-comment trail lives above `podman.Client.BulkContainerStats` and
`podman.StreamBulkStats` in `internal/podman/stats.go`, and above
`podman.Client.HostCPUs` in `internal/podman/client.go`.

**No fan-out was needed** — the bulk endpoint worked as intended; this
report documents where it does and doesn't behave the way a naive read of
"libpod bulk stats" would suggest (points 3-5 above).

## Design summary

- `GET /api/v1/stats`: new `AllStatsProvider` interface
  (`AllStats`/`HostCPUs`/`RunningAppContainers`) satisfied by
  `*deploy.Engine`, wired through `api.New`'s new final parameter.
  `deploy.Engine.RunningAppContainers` reuses the existing
  `ListContainers`+`isForeignInstance` pattern to build a
  containerID→slug map, refreshed once per bulk-stats tick (not once per
  sample) inside `handleAllStats`.
- New stream-token scope `all_stats` (slug always `""`, no
  `deployment_number`), extending `handleCreateStreamToken`'s existing
  scope-coherence checks and `requireAuthSSE`'s per-route middleware
  pattern — a token minted for one scope authenticates only its own
  route, in both directions.
- Apps that aren't running never emit (no entry in the attribution map);
  a transient `RunningAppContainers` failure on one tick reuses the last
  known-good mapping rather than killing the stream.
- `api/openapi.yaml` updated: new `/stats` path, `AllStatsSample` schema,
  `all_stats` added to `StreamTokenRequest.scope`'s enum, and the
  auth-scheme prose updated from "three SSE routes" to "four".

### Frontend

- `web/src/lib/sparkline.ts` / `statsBuffer.ts` (new, unit-tested): pure
  point-math and rolling-window logic, kept out of `.vue` files since this
  repo has no component-rendering test harness.
- `web/src/components/CpuSparkline.vue` (new): hand-rolled SVG, no chart
  library — polyline + emphasized endpoint dot, colored by app status via
  the existing `--color-status-running/-deploying/-error` tokens (the
  concept doc's `--steady/--moving/--broken` names, already wired to
  real, theme-aware, WCAG-checked values in `main.css`).
- `Apps.vue`: one `sse.connect('/api/v1/stats', {scope: 'all_stats', ...})`
  call in `onMounted`, closed in `onBeforeUnmount`. Malformed event JSON
  is silently dropped (no console spam). Seeded flat (`sparklinePoints`'
  own <2-samples case) so rows don't jump before data arrives.
- `web/src/lib/api.ts`: additions kept in one clearly-marked block
  (`AllStatsSample` type, `all_stats` added to `StreamTokenRequest.scope`)
  for an easy merge with the concurrent agent's other `api.ts` work.

## Phone behavior (390px card layout)

**Chose: numbers, no sparkline graphic.** At card width the row already
carries a port+limits text line; a ~60px trend line squeezed in next to
the state chip would either force a wrap or shrink to a few illegible
pixels — and a phone-checking operator cares about the current reading
more than the shape of the last ~100s. The card gained one new line
(`cpu 12% · mem 41 / 512 MB`, degrading to `—` per the same
numbers-unavailable rule as desktop) directly below the existing
port/limits line; the sparkline SVG itself only renders in the desktop
table. Decision and reasoning are documented inline in `CpuSparkline.vue`
and `Apps.vue`.

## Named test results (all PASS, local)

`internal/podman`:
- `TestHostCPUs`, `TestHostCPUsErrorStatus`, `TestHostCPUsZeroIsError`
- `TestStreamBulkStatsLiveCapture` (CPU-normalization happy path)
- `TestStreamBulkStatsMultiContainerTick`
- `TestStreamBulkStatsEmitsPerTick`
- `TestStreamBulkStatsCleanEOF`
- `TestStreamBulkStatsTruncatedFinalObject`
- `TestStreamBulkStatsPropagatesGenuineDecodeError`
- `TestStreamBulkStatsStopsOnFrameError`
- `TestFrameHasErrorNullVsPresent` (4 subtests)
- `TestBulkContainerStatsRequestsStreamTrueNoContainersFilter` — proves
  the client never passes `containers=` (see wire evidence point 3)
- `TestBulkContainerStatsErrorStatus`

`internal/deploy`:
- `TestAllStatsReturnsRuntimeReader`, `TestAllStatsPropagatesRuntimeError`
- `TestHostCPUsReturnsRuntimeValue`, `TestHostCPUsPropagatesRuntimeError`
- `TestRunningAppContainers` (stopped / foreign-instance / unrelated
  containers all correctly excluded)
- `TestRunningAppContainersPropagatesRuntimeError`

`internal/api` — **batch stream happy path**:
- `TestHandleAllStatsEventFraming` — proves per-app attribution AND that
  an unattributed container ("c2") is silently skipped

**Cross-scope token rejection** (both directions, new `all_stats` scope):
- `TestHandleAllStatsAppLogsScopedTokenRejected`
- `TestHandleAllStatsBuildLogScopedTokenRejected`
- `TestHandleAllStatsAppStatsScopedTokenRejected`
- `TestHandleAppStatsAllStatsScopedTokenRejected` (reverse)
- `TestHandleAppLogsAllStatsScopedTokenRejected` (reverse)
- `TestHandleAllStatsSessionTokenInQueryRejected`

**Disconnect teardown**:
- `TestHandleAllStatsDisconnectClosesSource` — proves `src.Close()` is
  called after client disconnect (no goroutine/connection leak)
- `TestHandleAllStatsReleasesSlotOnNormalCompletion`

**Other**:
- `TestHandleAllStatsHostCPUsErrorIs502`, `TestHandleAllStatsOpenErrorIs502`
- `TestHandleAllStatsUnauthorized`
- `TestHandleAllStatsQueryTokenAuth`
- `TestHandleAllStatsTooManyStreamsReturns503`
- `TestCreateStreamTokenAllStatsScopeRejectsSlug`
- `TestCreateStreamTokenAllStatsScopeRejectsDeploymentNumber`
- `TestCreateStreamTokenAllStatsScopeSucceeds`

**Conformance**:
- `TestOpenAPIConformance` — PASS; independently confirmed it fails
  without the new `/stats` spec entry (its own doc comment records the
  same drill from a prior milestone; re-derived here by observing the
  `go test ./... -count=1` run before `api/openapi.yaml` was updated:
  `the router registers 1 route(s) api/openapi.yaml does not document:
  GET /stats`)
- `TestOpenAPISpecHasNoEmptyPathItems`, `TestHandleOpenAPISpecServesEmbeddedYAML`

Web (`vitest`): `sparkline.test.ts` (9 tests), `statsBuffer.test.ts`
(12 tests) — all PASS.

## Verification commands run

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — no output (clean)
- `go test ./... -count=1` — all packages `ok`
- `go test ./internal/api/ -race -count=1` — `ok` in ~263s (~4.4 min,
  under the ~15 min the brief warned about for argon2 under `-race`)
- `cd web && npm install && npm run build` — clean
- `cd web && npm run type-check` — clean (`vue-tsc --noEmit`)
- `cd web && npm test` — 11 files, 89 tests, all PASS

## Concerns

- **`web/dist/index.html` is a committed placeholder** (`.gitignore`
  un-ignores just that one file under `web/dist/*`) — running
  `npm run build` for verification overwrote it with a real hashed build;
  I reverted it with `git checkout -- web/dist/index.html` before
  committing so the placeholder stays intact. Worth flagging in case a
  future agent's verification pass does the same and forgets to revert.
- **CPU% cross-endpoint consistency depends on BasePod never cpuset-
  restricting a container.** `HostCPUs` uses the HOST's online-CPU count
  as the normalization factor for every container uniformly; this is
  correct today (confirmed live: a container's own `online_cpus` already
  equals the host's) but would silently drift if a future feature ever
  pins an app to fewer cores via cgroups — the bulk endpoint's wire shape
  has no per-container online-CPU field to catch that at decode time.
- **`RunningAppContainers` is called once per ~5s tick** (one
  `ListContainers` call, label-filtered) for the batch route regardless
  of how many concurrent `/api/v1/stats` clients are connected — fine at
  BasePod's expected scale (a handful of admins) but not designed to
  fan out to many simultaneous dashboard viewers; the existing per-user/
  global `defaultStreamLimiter` caps still apply per-connection, but each
  connection independently re-polls `ListContainers` rather than sharing
  one.
- The concurrent agent's `web/` changes were not visible in this
  worktree; `web/src/lib/api.ts` additions are isolated in one clearly
  labeled block per the brief, but a real merge should still be reviewed
  by a human before landing.
