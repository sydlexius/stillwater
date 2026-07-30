-- +goose Up
-- Issue #2809: index metadata_changes on source so the source-filtered report
-- queries stop scanning the whole change history.
--
-- The table had two indexes, (artist_id, created_at DESC) and (created_at DESC).
-- Neither leads with source, so a query filtering on source alone -- which is
-- what the rule-written MusicBrainz ID report does -- scanned every row and
-- discarded the ones that did not match. That cost grows with the change
-- history, not with the size of the answer, which is the wrong shape for a
-- report whose result set is a small slice of a large table.
--
-- created_at is the SECOND column rather than artist_id (the leading candidate
-- when this was first raised) because the report's default ordering is
-- created_at DESC. A (source, artist_id) index serves the filter but leaves
-- SQLite sorting the matched rows in a temporary B-tree; making created_at the
-- second column lets one index satisfy the filter and the ORDER BY together.
-- Verified with EXPLAIN QUERY PLAN: the temporary B-tree disappears, and the
-- companion COUNT still reads it as a covering index.
--
-- The single-artist narrowing is already served by the existing
-- (artist_id, created_at DESC) index, so no second index is added here.
--
-- Index-only: no table is rewritten and no data changes, so this is safe to
-- apply to a live database and needs no backfill.

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_metadata_changes_source
    ON metadata_changes(source, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_metadata_changes_source;
-- +goose StatementEnd
