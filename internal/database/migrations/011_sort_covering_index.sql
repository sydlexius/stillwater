-- +goose Up
-- Issue #1080: sortable columns -- restore index coverage for name+id ORDER BY.
--
-- When the sortable-columns feature added a mandatory ", id ASC" tiebreaker to
-- every ORDER BY path, the existing idx_artists_name(name) index lost coverage
-- for the name sort: SQLite previously used that index cleanly for
-- "ORDER BY name ASC LIMIT N", but with the two-column clause
-- "ORDER BY name ASC, id ASC LIMIT N" it falls back to a temp B-TREE for the
-- second term. EXPLAIN QUERY PLAN evidence (run against the dev DB):
--
--   BEFORE tiebreaker: SCAN artists USING INDEX idx_artists_name
--   AFTER  tiebreaker: SCAN artists USING INDEX idx_artists_name
--                      USE TEMP B-TREE FOR LAST TERM OF ORDER BY
--
-- A composite index (name, id) eliminates the temp sort entirely:
--   AFTER composite: SCAN artists USING COVERING INDEX idx_artists_name_id
--
-- The other sort columns (type, origin, sort_name, health_score, updated_at,
-- created_at) already required a temp B-TREE both before and after the
-- tiebreaker change, so they are pre-existing and this migration does not add
-- indexes for them -- that would be a separate, broader perf initiative.
--
-- The LOWER(name) index added in migration 004 is for case-insensitive lookup
-- queries, not for ORDER BY; both can coexist.

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_artists_name_id ON artists(name, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_artists_name_id;
-- +goose StatementEnd
