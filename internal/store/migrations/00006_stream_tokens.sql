-- +goose Up
CREATE TABLE stream_tokens (
  id                INTEGER PRIMARY KEY,
  token_hash        TEXT NOT NULL UNIQUE,
  user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  scope             TEXT NOT NULL,
  slug              TEXT NOT NULL,
  deployment_number INTEGER,
  created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  expires_at        TEXT NOT NULL
);
CREATE INDEX idx_stream_tokens_expires_at ON stream_tokens(expires_at);
-- +goose Down
DROP TABLE stream_tokens;
