-- +goose Up
-- Issue #1726: clear locks parasitically applied by the pre-fix scanner
-- re-import loop.
--
-- Before #1726 the scanner stamped lock_source='imported', locked=1 on every
-- re-scan whenever the on-disk NFO carried <lockdata>true</lockdata>, and the
-- NFOExistsFixer hardcoded <lockdata>true</lockdata> on every NFO it wrote.
-- The combination silently re-locked most artists in users' libraries and
-- undid any unlock via the UI.
--
-- After the fix:
--  - The scanner re-scan path never writes artists.locked.
--  - The new-artist discovery path uses lock_source='initial_import'.
--  - The platform-pull path uses lock_source='platform'.
--  - User UI toggles continue to use lock_source='user'.
--
-- That leaves any row with lock_source='imported' as a legacy artifact of
-- the loop. The safe-by-construction recovery is to clear them in one shot;
-- users who intentionally want those rows locked can re-lock from the UI
-- (which then carries the correct 'user' provenance).
UPDATE artists
SET locked = 0,
    locked_at = NULL,
    lock_source = ''
WHERE lock_source = 'imported';

-- +goose Down
-- One-way migration. The legacy 'imported' provenance is meaningless after
-- the loop is gone and there is no information in the DB to reconstruct
-- which rows were originally written by the scanner loop versus which were
-- legitimately locked. Down is therefore a no-op rather than an attempt at
-- a fake restoration.
SELECT 1;
