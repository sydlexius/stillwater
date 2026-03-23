-- +goose Up
-- Tracks individual field-level metadata changes for each artist.
-- Recorded by callers that mutate artist metadata (Phase 2 integration hooks).
-- Phase 1 adds the table and service infrastructure; writes are wired in later.

CREATE TABLE IF NOT EXISTS metadata_changes (
    id         TEXT PRIMARY KEY,
    artist_id  TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    field      TEXT NOT NULL,
    old_value  TEXT NOT NULL DEFAULT '',
    new_value  TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'manual',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX idx_metadata_changes_artist ON metadata_changes(artist_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_metadata_changes_artist;
DROP TABLE IF EXISTS metadata_changes;
