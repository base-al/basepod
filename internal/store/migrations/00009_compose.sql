-- +goose Up
-- v0.5 Task 8: compose apply — per-service app metadata.
--
-- compose_project/compose_service key an app back to the compose.yaml
-- service it was created from ("" for a hand-created app, which never has
-- either set) — see internal/compose.BuildPlan's (project, service) pair
-- and internal/api/compose.go, the only writer. Re-applying the same
-- compose file looks an app up by this pair (not by recomputing its slug)
-- so a compose apply is idempotent: update in place, never duplicate.
--
-- internal marks a service that had no `expose:` entries in compose.yaml:
-- no Caddy route (see internal/deploy's Routes(), which skips internal
-- apps entirely) and no HTTP health probe target (an internal service may
-- not even speak HTTP — e.g. a Postgres/Redis-style service — see
-- runRollout, which skips probing when this is set). A hand-created app is
-- never internal (the app-create API still requires port >= 1 for it);
-- only compose can set this.
ALTER TABLE apps ADD COLUMN compose_project TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN compose_service TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN internal INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE apps DROP COLUMN internal;
ALTER TABLE apps DROP COLUMN compose_service;
ALTER TABLE apps DROP COLUMN compose_project;
