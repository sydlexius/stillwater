-- +goose Up
-- Issue #1726: extend artists.lock_source CHECK constraint to allow the
-- new attribution values introduced by the lock-loop fix:
--   * 'initial_import' -- replaces the scanner re-import 'imported' on the
--                         new-artist code path; never written on re-scan.
--   * 'platform'       -- written by connection.LockSync when a platform-side
--                         <lockdata> change is pulled into Stillwater.
--
-- SQLite cannot ALTER a CHECK constraint in place: the canonical pattern is
-- create-new-table + INSERT SELECT + drop-old + rename. The rebuild reflects
-- the post-004 schema (no library_id column) and re-creates every index that
-- was attached to the old table (idx_artists_name, _path, _locked,
-- _dirty_eval from 001; _name_lower from 004; _name_id from 011).

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;

CREATE TABLE artists_new (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    sort_name TEXT,
    type TEXT NOT NULL DEFAULT '',
    gender TEXT NOT NULL DEFAULT '',
    origin TEXT NOT NULL DEFAULT '',
    disambiguation TEXT NOT NULL DEFAULT '',
    genres TEXT NOT NULL DEFAULT '[]',
    styles TEXT NOT NULL DEFAULT '[]',
    moods TEXT NOT NULL DEFAULT '[]',
    years_active TEXT NOT NULL DEFAULT '',
    born TEXT NOT NULL DEFAULT '',
    formed TEXT NOT NULL DEFAULT '',
    died TEXT NOT NULL DEFAULT '',
    disbanded TEXT NOT NULL DEFAULT '',
    biography TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    nfo_exists INTEGER NOT NULL DEFAULT 0,
    health_score REAL NOT NULL DEFAULT 0.0,
    health_evaluated_at TEXT DEFAULT NULL,
    dirty_since TEXT DEFAULT NULL,
    rules_evaluated_at TEXT DEFAULT NULL,
    is_excluded INTEGER NOT NULL DEFAULT 0,
    exclusion_reason TEXT NOT NULL DEFAULT '',
    is_classical INTEGER NOT NULL DEFAULT 0,
    locked INTEGER NOT NULL DEFAULT 0,
    lock_source TEXT NOT NULL DEFAULT '' CHECK (lock_source IN ('', 'user', 'imported', 'initial_import', 'platform')),
    locked_at TEXT CHECK (locked = 0 OR (locked = 1 AND locked_at IS NOT NULL)),
    locked_fields TEXT NOT NULL DEFAULT '[]',
    metadata_sources TEXT NOT NULL DEFAULT '{}',
    last_scanned_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO artists_new SELECT
    id, name, sort_name, type, gender, origin, disambiguation,
    genres, styles, moods, years_active, born, formed, died, disbanded,
    biography, path, nfo_exists, health_score, health_evaluated_at,
    dirty_since, rules_evaluated_at, is_excluded, exclusion_reason,
    is_classical, locked, lock_source, locked_at, locked_fields,
    metadata_sources, last_scanned_at, created_at, updated_at
FROM artists;

DROP TABLE artists;
ALTER TABLE artists_new RENAME TO artists;

CREATE INDEX idx_artists_name ON artists(name);
CREATE INDEX idx_artists_path ON artists(path);
CREATE INDEX idx_artists_locked ON artists(locked);
CREATE INDEX idx_artists_dirty_eval ON artists(dirty_since, rules_evaluated_at);
CREATE INDEX idx_artists_name_lower ON artists(LOWER(name));
CREATE INDEX idx_artists_name_id ON artists(name, id);

PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;

-- Coerce rows written with the new values back into the legacy
-- vocabulary so the old CHECK constraint accepts them. Silently
-- relabeling as 'user' would misattribute the lock, so the safer
-- choice is to clear those rows (mirrors migration 014's intent).
UPDATE artists
SET locked = 0, locked_at = NULL, lock_source = ''
WHERE lock_source IN ('platform', 'initial_import');

CREATE TABLE artists_old (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    sort_name TEXT,
    type TEXT NOT NULL DEFAULT '',
    gender TEXT NOT NULL DEFAULT '',
    origin TEXT NOT NULL DEFAULT '',
    disambiguation TEXT NOT NULL DEFAULT '',
    genres TEXT NOT NULL DEFAULT '[]',
    styles TEXT NOT NULL DEFAULT '[]',
    moods TEXT NOT NULL DEFAULT '[]',
    years_active TEXT NOT NULL DEFAULT '',
    born TEXT NOT NULL DEFAULT '',
    formed TEXT NOT NULL DEFAULT '',
    died TEXT NOT NULL DEFAULT '',
    disbanded TEXT NOT NULL DEFAULT '',
    biography TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    nfo_exists INTEGER NOT NULL DEFAULT 0,
    health_score REAL NOT NULL DEFAULT 0.0,
    health_evaluated_at TEXT DEFAULT NULL,
    dirty_since TEXT DEFAULT NULL,
    rules_evaluated_at TEXT DEFAULT NULL,
    is_excluded INTEGER NOT NULL DEFAULT 0,
    exclusion_reason TEXT NOT NULL DEFAULT '',
    is_classical INTEGER NOT NULL DEFAULT 0,
    locked INTEGER NOT NULL DEFAULT 0,
    lock_source TEXT NOT NULL DEFAULT '' CHECK (lock_source IN ('', 'user', 'imported')),
    locked_at TEXT CHECK (locked = 0 OR (locked = 1 AND locked_at IS NOT NULL)),
    locked_fields TEXT NOT NULL DEFAULT '[]',
    metadata_sources TEXT NOT NULL DEFAULT '{}',
    last_scanned_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO artists_old SELECT
    id, name, sort_name, type, gender, origin, disambiguation,
    genres, styles, moods, years_active, born, formed, died, disbanded,
    biography, path, nfo_exists, health_score, health_evaluated_at,
    dirty_since, rules_evaluated_at, is_excluded, exclusion_reason,
    is_classical, locked, lock_source, locked_at, locked_fields,
    metadata_sources, last_scanned_at, created_at, updated_at
FROM artists;

DROP TABLE artists;
ALTER TABLE artists_old RENAME TO artists;

CREATE INDEX idx_artists_name ON artists(name);
CREATE INDEX idx_artists_path ON artists(path);
CREATE INDEX idx_artists_locked ON artists(locked);
CREATE INDEX idx_artists_dirty_eval ON artists(dirty_since, rules_evaluated_at);
CREATE INDEX idx_artists_name_lower ON artists(LOWER(name));
CREATE INDEX idx_artists_name_id ON artists(name, id);

PRAGMA foreign_keys = ON;
-- +goose StatementEnd
