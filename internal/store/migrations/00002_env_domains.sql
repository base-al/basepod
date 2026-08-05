-- +goose Up
CREATE TABLE env_vars (
  id INTEGER PRIMARY KEY,
  app_id INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  key TEXT NOT NULL, value_encrypted TEXT NOT NULL,
  is_secret INTEGER NOT NULL DEFAULT 0,
  UNIQUE(app_id, key)
);
CREATE TABLE domains (
  id INTEGER PRIMARY KEY,
  app_id INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
-- +goose Down
DROP TABLE domains; DROP TABLE env_vars;
