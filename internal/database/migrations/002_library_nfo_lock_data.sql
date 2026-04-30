-- +goose Up
-- Issue #1264: per-library opt-in for stamping <lockdata>true</lockdata> into
-- NFO files Stillwater writes. Default 0 (off) preserves the post-fix
-- behavior where Stillwater does not pin metadata refresh against the
-- platform unless the user explicitly opts in via the Libraries settings UI.
--
-- This is the first post-v1.0 schema migration. The previous policy of
-- editing 001_initial_schema.sql + adding an idempotent ensureXColumns
-- helper at runtime is retired in favor of additive forward-only migration
-- files: goose runs each new migration exactly once per database, and
-- both fresh and upgraded deployments converge on the same final schema.
ALTER TABLE libraries ADD COLUMN nfo_lock_data INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite's ALTER TABLE supports DROP COLUMN as of 3.35 (Apr 2021); modernc
-- ships well past that, so the down migration is the symmetric inverse.
ALTER TABLE libraries DROP COLUMN nfo_lock_data;
