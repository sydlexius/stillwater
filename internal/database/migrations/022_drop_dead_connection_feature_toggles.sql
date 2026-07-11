-- +goose Up
-- Issue #1917 (end-of-M55 kludge sweep, item 10): drop the two dead per-
-- connection "Sends" feature columns. feature_library_import and
-- feature_nfo_write were persisted from the settings UI toggles but never
-- gated any publish/write path on any platform (verified: no reader in
-- internal/publish, internal/imagebridge, internal/scanner, internal/nfo).
-- The toggles set a wrong "Stillwater pushes this to the platform"
-- expectation, so they are removed everywhere including the schema.
--
-- feature_image_write is retained: it is a real gate for Emby/Jellyfin image
-- uploads (imagebridge/bridge.go, publish/reconcile.go, publish/publisher.go).
--
-- SQLite 3.35+ supports DROP COLUMN; modernc.org/sqlite ships well past that
-- (precedent: 004_drop_library_id_and_perf.sql).

-- +goose StatementBegin
ALTER TABLE connections DROP COLUMN feature_library_import;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE connections DROP COLUMN feature_nfo_write;
-- +goose StatementEnd

-- +goose Down
-- Restore the columns with the original NOT NULL DEFAULT 1 so a rollback
-- lands a schema identical to pre-022. The values are cosmetic (never read),
-- so re-defaulting to 1 loses nothing meaningful.
-- +goose StatementBegin
ALTER TABLE connections ADD COLUMN feature_library_import INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE connections ADD COLUMN feature_nfo_write INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd
