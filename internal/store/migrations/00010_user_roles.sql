-- +goose Up
-- Multi-user support: roles, disable/enable, invitations, and an audit
-- log (users + roles milestone; see docs/plan/08.teams-and-rbac.md for the
-- permission-matrix thinking — this migration implements the no-teams
-- subset of it: every app stays shared across the instance, and what
-- changes is *who may do what*, not *which apps a user can see*).
--
-- role is one of "viewer" | "member" | "admin" | "owner" (see
-- internal/store.RoleViewer etc. and internal/authz's capability table,
-- the single source of truth for what each role may do). The column
-- default is "owner" rather than the lowest-privilege role deliberately:
-- this is an ADDITIVE migration against a database that may already have
-- users (today, in production, exactly one — the admin created by
-- `basepod setup`), and SQLite's ALTER TABLE ADD COLUMN applies a
-- column's default to every existing row at the moment the column is
-- added. Defaulting to "owner" means that pre-existing admin comes out of
-- this migration as an owner — nobody is locked out of administering
-- their own instance — rather than "viewer", which would have silently
-- demoted the only account able to grant anyone a role at all. Every NEW
-- user going forward is created with an explicit role (see
-- store.CreateUserWithRole), never relying on this column default.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'owner';

-- disabled_at is NULL for an active user, or the timestamp a
-- disable took effect. A disabled user's sessions are revoked
-- immediately at disable time (see store.DisableUser) and
-- UserBySessionTokenHash additionally excludes any session belonging to
-- a disabled user as defense in depth — so a disabled user with a
-- lingering session token can never authenticate, even if a future
-- caller of the disable path forgets the explicit revoke.
ALTER TABLE users ADD COLUMN disabled_at TEXT;

-- invitations: a single-use, expiring, tokenized invite an admin creates
-- for a new user (see internal/api's invite/accept-invite handlers).
-- token_hash is the sha256 hex digest of the raw token — exactly like
-- sessions.token_hash and stream_tokens.token_hash, the raw token itself
-- is never persisted, only shown once in the invite-creation response.
-- accepted_at is NULL until the invite is redeemed; a non-NULL
-- accepted_at makes the invite permanently unusable (single-use) even if
-- it hasn't technically expired yet.
CREATE TABLE invitations (
  id            INTEGER PRIMARY KEY,
  token_hash    TEXT NOT NULL UNIQUE,
  email         TEXT NOT NULL,
  role          TEXT NOT NULL,
  created_by    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  expires_at    TEXT NOT NULL,
  accepted_at   TEXT
);

-- audit_log: an append-only record of every mutating operation (deploys,
-- app create/delete, env changes — never values, see internal/api's audit
-- helper — user/role changes, logins). actor_id is nullable (ON DELETE
-- SET NULL, not CASCADE, unlike every other user-owned table in this
-- schema) so a row survives the actor's own account being deleted later —
-- an audit trail that vanished the moment the account it was watching got
-- removed would defeat its own purpose. actor_email is captured
-- redundantly at write time for the same reason: it stays readable in the
-- UI/CLI even after actor_id goes NULL.
CREATE TABLE audit_log (
  id          INTEGER PRIMARY KEY,
  actor_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
  actor_email TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,
  target      TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX idx_audit_log_created_at ON audit_log(created_at);

-- +goose Down
DROP INDEX idx_audit_log_created_at;
DROP TABLE audit_log;
DROP TABLE invitations;
ALTER TABLE users DROP COLUMN disabled_at;
ALTER TABLE users DROP COLUMN role;
