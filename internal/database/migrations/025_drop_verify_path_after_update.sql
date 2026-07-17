-- +goose Up
-- Issue #2563: drop the retired verify_path_after_update column. The toggle
-- gated a follow-up GET after Lidarr's UpdateArtistPath PUT that re-read the
-- artist and compared the returned path against what Stillwater sent.
--
-- #2380 made the publish-layer path read-back unconditional on EVERY peer
-- (see publish.verifyPeerPath), which left this toggle gating a duplicate of a
-- check that now always runs -- on the one peer where a read-back is weakest,
-- since Lidarr echoes back whatever path it was sent. #2419 removed the
-- in-client verify GET, after which the column was persisted and read but
-- gated nothing: flipping it changed no observable behavior. The UI (#2573),
-- the export envelope and the API surface were removed first; this drops the
-- storage and the domain field, so the setting is gone end to end.
--
-- SQLite 3.35+ supports DROP COLUMN; modernc.org/sqlite ships well past that
-- (precedent: 004_drop_library_id_and_perf.sql, 022_drop_dead_connection_feature_toggles.sql).

-- +goose StatementBegin
ALTER TABLE connections DROP COLUMN verify_path_after_update;
-- +goose StatementEnd

-- +goose Down
-- Restore the column with the original definition from migration 013 so a
-- rollback lands a schema identical to pre-025. The value is not recoverable
-- and re-defaults to 0, which loses nothing: the toggle gated no behavior by
-- the time it was dropped, and 0 was its original opt-in default.
-- +goose StatementBegin
ALTER TABLE connections ADD COLUMN verify_path_after_update INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
