-- +goose Up
-- v0.5 Task 6: named volumes + the `replace` deploy strategy — substrate
-- for compose's stateful services (Task 8).
--
-- volumes records, per app, which named volumes a rollout mounts into
-- every new container it creates (see internal/deploy's runRollout and
-- internal/podman.CreateSpec.NamedVolumes). `name` is the short logical
-- name an operator (or compose) assigns; the actual libpod volume name a
-- rollout creates/mounts is derived as "bp-<slug>-<name>" (see
-- internal/deploy.volumeName) — not stored here, since it's a pure
-- function of the app's slug and this row's name and would just be a
-- second source of truth to keep in sync. `container_path` is the
-- absolute path inside the container the volume is mounted at.
-- UNIQUE(app_id, name) keeps one app from declaring the same logical
-- volume twice (which would otherwise silently collide on the same
-- derived libpod volume name).
--
-- Volume ROWS are owned by the app they belong to (ON DELETE CASCADE), but
-- the underlying libpod VOLUME is deliberately NOT deleted when an app (or
-- this row) is removed — see internal/api's handleDeleteApp: data outlives
-- the app row, consistent with BasePod's standing "never delete data
-- without confirmation" stance. Manual volume create/delete stays out of
-- scope this milestone (v0.6+); Task 8's compose apply is the only writer.
CREATE TABLE volumes (
  id             INTEGER PRIMARY KEY,
  app_id         INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  name           TEXT NOT NULL,
  container_path TEXT NOT NULL,
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE(app_id, name)
);

-- deploy_strategy selects how a rollout cuts over an app's container
-- generation (see internal/deploy's runRollout):
--
--   'zero-downtime' (the default, and the only behavior before this
--   migration): create+start the new container, probe it, cut traffic
--   over, THEN stop+remove the old one — the old generation stays up as a
--   fallback for the whole rollout, so a failed deploy leaves the
--   previously-running app untouched.
--
--   'replace': stop+remove the old container BEFORE creating the new one.
--   Required for volume-backed stateful services (SQLite/Postgres-style
--   apps) — the zero-downtime overlap would otherwise run two containers
--   against the same named volume at once, which corrupts on-disk state
--   for that whole class of database engine. The accepted trade: a failed
--   'replace' deploy leaves the app DOWN with status 'error' (there is no
--   old container left to fall back to) rather than rolling back — see
--   internal/deploy/deploy.go's runRollout and internal/api's app JSON,
--   which both document and surface this as a property of the app, not a
--   surprise failure mode. The operator opts in explicitly (or Task 8's
--   compose apply opts an app in automatically, with a warning, when it
--   declares a volume).
ALTER TABLE apps ADD COLUMN deploy_strategy TEXT NOT NULL DEFAULT 'zero-downtime';

-- +goose Down
ALTER TABLE apps DROP COLUMN deploy_strategy;
DROP TABLE volumes;
