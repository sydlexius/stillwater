-- +goose Up
-- Issue #1246 (feat(scanner): foreign-file detection via EXIF provenance)
-- added foreign_files and foreign_file_allowlist to 001_initial_schema.sql
-- after existing databases had already been initialized. This migration
-- backfills those tables for any installation that predates #1246.

CREATE TABLE IF NOT EXISTS foreign_files (
    id          TEXT NOT NULL PRIMARY KEY,
    artist_id   TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    file_path   TEXT NOT NULL,
    file_name   TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    detected_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(artist_id, file_path)
);

CREATE INDEX IF NOT EXISTS idx_foreign_files_artist ON foreign_files(artist_id);
CREATE INDEX IF NOT EXISTS idx_foreign_files_detected_at ON foreign_files(detected_at);

-- Permanent allowlist suppressing re-detection. Two scopes:
--   scope='global'  -- match every artist; artist_id is NULL.
--   scope='artist'  -- match a specific artist_id. file_name is the basename
--                      (e.g. "backdrop.jpg") so the allowlist survives a
--                      directory rename for the same artist.
-- A NULL artist_id with scope='artist' is rejected by the writer (see
-- internal/foreign/allowlist.go) so the partial unique indexes stay sound.
CREATE TABLE IF NOT EXISTS foreign_file_allowlist (
    id         TEXT NOT NULL PRIMARY KEY,
    scope      TEXT NOT NULL CHECK (scope IN ('global','artist')),
    artist_id  TEXT REFERENCES artists(id) ON DELETE CASCADE,
    file_name  TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Partial unique indexes: one global entry per file_name, one artist-scoped
-- entry per (artist_id, file_name). Modeled as two indexes because SQLite
-- treats NULLs in a UNIQUE constraint as distinct, which would otherwise
-- allow duplicate global rows.
CREATE UNIQUE INDEX IF NOT EXISTS idx_foreign_allowlist_global
    ON foreign_file_allowlist(file_name) WHERE scope = 'global';
CREATE UNIQUE INDEX IF NOT EXISTS idx_foreign_allowlist_artist
    ON foreign_file_allowlist(artist_id, file_name) WHERE scope = 'artist';

-- +goose Down
DROP TABLE IF EXISTS foreign_file_allowlist;
DROP TABLE IF EXISTS foreign_files;
