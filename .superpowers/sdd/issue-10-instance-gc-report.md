# Issue #10 — instance-scoped orphan GC / Caddy reconciliation

## Status

Implemented, verified locally, and committed on branch
`fix/instance-scoped-gc` (isolated worktree, branched from
`origin/main`), pushed to origin. See commit hash below.

## What changed

- New `internal/instanceid` package: `LoadOrCreate(dataDir) (string, error)`
  mints a random instance id into `<dataDir>/instance.id` on first boot
  (0600, atomic write-then-rename-then-reread, `sync.Mutex`-guarded —
  mirrors `crypto.LoadOrCreateKey` exactly) and returns the same id on
  every later call, for the life of the data dir.
- `basepod.instance=<id>` is now stamped on every resource BasePod
  creates: app containers (`internal/deploy.runRollout`), named volumes
  (informational only), `bp-caddy` (`internal/caddy.Manager.create`), and
  the shared `basepod` network (`podman.Client.EnsureNetwork`, new
  `instanceID` parameter).
- `deploy.Engine.CleanupOrphans` is rewritten around a three-tier
  ownership check: (1) a different instance's label → never touched, slug
  never even looked up; (2) my own label, or unlabeled with a slug already
  in my DB → adopted/mine, gets the normal unknown-app/stale-generation GC
  logic; (3) unlabeled with an unknown slug → left alone, logged loudly
  ("possibly another instance's, not touching"). Adoption never mutates a
  running container's labels — it only affects GC's own removal decision;
  the container picks up the label on its next real redeploy.
- `caddy.Manager.Ensure` now refuses outright (clear error, boot fails) if
  an existing `bp-caddy` carries a *different* instance's label; an
  unlabeled `bp-caddy` is adopted (no drift check gates on the label, so
  it behaves exactly as before instance ids existed) and picks up this
  instance's label only when a real drift condition later forces an
  actual recreate.
- Defense in depth: `removeOldContainers`, `fail`, `RemoveApp`, `AppLogs`,
  `AppStats` all skip any container carrying a foreign instance's label
  (new `Engine.isForeignInstance` helper) before acting on it, in case two
  instances' apps ever coincidentally share a slug.
- `deploy.New` and `caddy.NewManager` both gained a new trailing
  `instanceID string` parameter; `internal/server.Run` loads/creates the
  id (via `instanceid.LoadOrCreate(cfg.DataDir)`) before constructing the
  Caddy manager (which needs it for `Ensure`'s ownership check) and wires
  it through to the deploy engine too.
- Docs: doc 03's "Naming & labels" section (renamed "Naming & labels, and
  instance identity") now documents `instance.id`, the three-tier
  ownership rule, and the Caddy refusal/adoption behavior. Doc 02's data
  dir table lists `instance.id` alongside `secret.key`, and a new
  "v0.4.2 → v0.5" paragraph under Upgrades explains the adoption rule for
  the real production upgrade case (base.al, v0.4.2, no instance labels).

## Named test results

```
$ go test ./internal/deploy/ ./internal/caddy/ -race -count=1
ok  	github.com/base-al/basepod/internal/deploy	9.985s
ok  	github.com/base-al/basepod/internal/caddy	1.248s
```

```
$ go build ./...        # clean
$ go vet ./...           # clean
$ gofmt -l .             # no output — clean
$ go test ./... -count=1
ok  	github.com/base-al/basepod/cmd/basepod
ok  	github.com/base-al/basepod/internal/api
ok  	github.com/base-al/basepod/internal/auth
ok  	github.com/base-al/basepod/internal/build
ok  	github.com/base-al/basepod/internal/caddy
ok  	github.com/base-al/basepod/internal/cli
ok  	github.com/base-al/basepod/internal/compose
ok  	github.com/base-al/basepod/internal/config
ok  	github.com/base-al/basepod/internal/crypto
ok  	github.com/base-al/basepod/internal/deploy
ok  	github.com/base-al/basepod/internal/gitsource
ok  	github.com/base-al/basepod/internal/instanceid
ok  	github.com/base-al/basepod/internal/manifest
ok  	github.com/base-al/basepod/internal/podman
ok  	github.com/base-al/basepod/internal/server
ok  	github.com/base-al/basepod/internal/store
ok  	github.com/base-al/basepod/internal/tarpack
ok  	github.com/base-al/basepod/web
```

Key new/updated tests (all passing, `internal/deploy/deploy_test.go`
unless noted):

- `TestCleanupOrphansRemovesUnknownSlug` — updated to carry my own
  instance label (stale semantics unchanged for a genuinely-mine orphan).
- `TestCleanupOrphansLeavesUnlabeledUnknownSlugAlone` — new; unlabeled +
  unknown slug → not removed.
- `TestCleanupOrphansSkipsForeignInstanceContainerEvenWithUnknownSlug` —
  new; foreign-instance label wins over "unknown slug", never touched.
- `TestCleanupOrphansAdoptsUnlabeledContainerWithKnownSlug` — new;
  unlabeled + known slug is adopted and gets full GC treatment (current
  generation kept, stale generation removed).
- `TestCleanupOrphansRegressionTwoInstancesSharePodmanHost` — new;
  reproduces the reported incident directly (two `Engine`s, two `Store`s,
  one shared fake runtime; instance B's GC against an empty DB removes
  nothing of instance A's).
- `internal/caddy/manager_test.go`: `TestEnsureAdoptsUnlabeledCaddy` (new),
  `TestEnsureRefusesForeignInstanceCaddy` (new; asserts the error message
  and that no Pull/Stop/Remove/Create call is ever made).
- `internal/podman/client_test.go`:
  `TestEnsureNetworkCreatesWithInstanceLabel` (new).
- `internal/instanceid/instanceid_test.go`: creation, 0600 perms,
  stability across repeated calls ("restarts"), distinct ids per data
  dir, `TestLoadOrCreateConcurrentFirstBoot` (32 goroutines racing a
  never-before-seen data dir, asserts all agree and the on-disk file is
  well-formed), rejection of an empty/whitespace-only existing file,
  missing-parent-dir creation.

## Handling the unlabeled-upgrade case

An unlabeled `basepod.managed` container (every container any BasePod
build before this fix ever created — the real state of base.al's running
v0.4.2 production containers) is adopted, not orphaned, under exactly one
condition: its `basepod.app` slug already exists in *this* instance's
database. If so, `CleanupOrphans` treats it precisely as if it already
carried this instance's own label — it is never deleted just because it
lacks the new label, and it is never mutated in place to add one (libpod
doesn't support relabeling a running container without recreating it, and
recreating a healthy container purely to relabel it would itself be an
outage risk). It picks up `basepod.instance=<id>` organically the next
time it's actually redeployed, via `runRollout`'s `spec.Labels`.

If the slug is *not* known to this instance's database, the container is
left completely alone and logged loudly ("possibly another instance's,
not touching") — this is the fail-safe branch that directly prevents the
reported incident (a second instance's empty-DB boot must never delete a
first instance's live container just because it doesn't recognize the
slug).

`bp-caddy` gets the parallel treatment: unlabeled → adopted (no drift
check even looks at the label, so behavior is unchanged from before this
fix); labeled with a *different* instance's id → `Ensure` returns an
error and refuses to touch it at all, rather than recreating it under
this instance's config (the second half of the original incident).

## Concerns

- **`caddy.Manager.Ensure` now fails boot outright** on a genuine
  two-instance conflict over `bp-caddy`, rather than degrading gracefully
  (e.g. running without a managed proxy). This was a deliberate choice —
  "refuse to fight over the proxy" reads more like a hard stop than a
  silent no-op to me, and a silently-proxy-less instance seemed like a
  worse failure mode — but it does mean a genuine two-instance conflict is
  a **boot failure**, not a warning; worth confirming that's the intended
  UX before release.
- **Volume labels** (`basepod.instance` in `volumeLabels`) are
  informational only — nothing GCs, adopts, or refuses to touch volumes by
  instance id today (volumes were already effectively out of scope for
  issue #10's container/Caddy focus). Flagging in case a future pass wants
  volume-level isolation too.
- **Same-slug coincidence across instances**: the "defense in depth" skip
  added to `removeOldContainers`/`fail`/`RemoveApp`/`AppLogs`/`AppStats`
  guards against two *different* instances' databases happening to name an
  app the same slug. This is a narrow edge case (each instance's own slug
  uniqueness is enforced only within its own DB) but a real one on a
  shared host, so I erred on the side of filtering everywhere a
  `basepod.app`-scoped `ListContainers` call feeds a mutation or a log/stat
  stream.
- The `EnsureNetwork` signature changed (`instanceID string` added) purely
  for the network's forensic label — no behavioral enforcement is built on
  it (the `basepod` network is a single shared resource by design, name
  fixed as a constant, so there's nothing to "refuse" or "adopt" there
  today).

## Commit

See the top-level report handed back to the caller for the exact commit
hash(es) on `fix/instance-scoped-gc`.
