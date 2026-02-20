-- +goose Up
ALTER TABLE connections ADD COLUMN status_message TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite ALTER TABLE DROP COLUMN requires 3.35.0+; recreate table for broad compatibility.
CREATE TABLE connections_backup AS SELECT id, name, type, url, encrypted_api_key, enabled, status, last_checked_at, created_at, updated_at FROM connections;
DROP TABLE connections;
CREATE TABLE connections (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    url TEXT NOT NULL,
    encrypted_api_key TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'unknown',
    last_checked_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO connections SELECT * FROM connections_backup;
DROP TABLE connections_backup;
