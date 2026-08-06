-- +goose Up
-- git_sources holds per-app git push-to-deploy configuration: one row per
-- app (app_id UNIQUE — an app has at most one connected repo), the
-- webhook path segment (hook_id, a random opaque value — NOT the secret
-- itself, so it's safe to embed in the webhook URL an operator pastes
-- into GitHub/GitLab/Gitea), and two AAD-sealed ciphertexts
-- (secret_encrypted, token_encrypted — see internal/crypto's
-- SealAAD/OpenAAD and internal/crypto.AAD, which callers bind to
-- appID|"git_secret" and appID|"git_token" respectively, mirroring how
-- env var values are bound to appID|key). token_encrypted is '' for a
-- public repo needing no credential.
CREATE TABLE git_sources (
  id               INTEGER PRIMARY KEY,
  app_id           INTEGER NOT NULL UNIQUE REFERENCES apps(id) ON DELETE CASCADE,
  url              TEXT NOT NULL,
  branch           TEXT NOT NULL,
  provider         TEXT NOT NULL DEFAULT '',
  hook_id          TEXT NOT NULL UNIQUE,
  secret_encrypted TEXT NOT NULL DEFAULT '',
  token_encrypted  TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- git_deliveries is an append-only-in-spirit log of received webhook
-- events for the dashboard's "recent deliveries" view (pruned to the
-- newest `keep` rows per app by PruneGitDeliveries — see
-- InsertGitDelivery). detail is a short human-readable string only —
-- NEVER a raw payload body or a secret; status is one of
-- 'deployed'|'ignored_branch'|'ignored_event'|'invalid_signature'|
-- 'rate_limited'|'coalesced'|'error' (enforced by the API layer, not a
-- SQL CHECK constraint, matching how deployments.status etc. are handled
-- elsewhere in this schema). deployment_id is set only for the
-- 'deployed' status and left NULL for every other outcome; ON DELETE SET
-- NULL rather than CASCADE because a delivery record documenting "this
-- webhook fired" should outlive the deployment row it triggered.
CREATE TABLE git_deliveries (
  id            INTEGER PRIMARY KEY,
  app_id        INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  received_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  provider      TEXT NOT NULL DEFAULT '',
  event         TEXT NOT NULL DEFAULT '',
  ref           TEXT NOT NULL DEFAULT '',
  commit_sha    TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  detail        TEXT NOT NULL DEFAULT '',
  deployment_id INTEGER REFERENCES deployments(id) ON DELETE SET NULL
);
CREATE INDEX idx_git_deliveries_app_id_received_at ON git_deliveries(app_id, received_at DESC);

-- deployments.git_sha records the commit a git-sourced deployment built
-- (surfaced in deployment JSON); '' for every non-git deployment. Added
-- here (rather than by whichever task first consumes it) because this is
-- migration 00007's owning task — see the v0.5 git+compose plan's
-- self-review note on migration ownership.
ALTER TABLE deployments ADD COLUMN git_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE deployments DROP COLUMN git_sha;
DROP TABLE git_deliveries;
DROP TABLE git_sources;
