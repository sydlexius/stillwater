-- +goose Up
-- Issue #1271 Phase B: remove the Classical library type.
--
-- All libraries with type='classical' are converted to type='regular'. This is
-- a one-shot, non-reversible migration: the Classical type is fully removed in
-- v1.3.0. No artist rows or filesystem files are touched; this only updates the
-- metadata column that classifies the library.
--
-- IMPORTANT: goose migrations run BEFORE EnableForeignKeys is called on the
-- live database, so ON DELETE CASCADE does NOT fire here. This migration does
-- not delete any rows, so FK state is irrelevant; the note is kept for
-- consistency with neighboring migrations.
--
-- Idempotency: UPDATE ... WHERE type = 'classical' is a no-op if no classical
-- rows exist (e.g. on a fresh install or after this migration already ran).

-- +goose StatementBegin
UPDATE libraries SET type = 'regular' WHERE type = 'classical';
-- +goose StatementEnd

-- +goose Down
-- No rollback: the Classical type is being permanently retired in v1.3.0.
-- Re-inserting type='classical' would require knowing which libraries were
-- originally classical, which this migration does not record.
