-- +goose Up
ALTER TABLE libraries ADD COLUMN source TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE libraries ADD COLUMN connection_id TEXT REFERENCES connections(id) DEFAULT NULL;
ALTER TABLE libraries ADD COLUMN external_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_libraries_connection_external
    ON libraries(connection_id, external_id)
    WHERE connection_id IS NOT NULL AND external_id != '';

-- +goose Down
DROP INDEX IF EXISTS idx_libraries_connection_external;
-- SQLite does not support DROP COLUMN; down migration is best-effort.
