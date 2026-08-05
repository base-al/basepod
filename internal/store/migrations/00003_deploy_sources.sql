-- +goose Up
ALTER TABLE deployments ADD COLUMN source TEXT NOT NULL DEFAULT 'image';
ALTER TABLE deployments ADD COLUMN build_log_path TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN trigger_kind TEXT NOT NULL DEFAULT 'api';
-- +goose Down
ALTER TABLE deployments DROP COLUMN trigger_kind;
ALTER TABLE deployments DROP COLUMN build_log_path;
ALTER TABLE deployments DROP COLUMN source;
