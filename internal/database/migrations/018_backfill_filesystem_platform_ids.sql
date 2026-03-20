-- +goose Up

-- Part 1: Delete orphaned platform ID rows whose connection no longer exists.
-- These bypass ON DELETE CASCADE when connections are removed while foreign
-- keys are disabled or when data is inconsistent.
DELETE FROM artist_platform_ids
WHERE connection_id NOT IN (SELECT id FROM connections);

-- Part 2: Backfill platform IDs to filesystem (manual-source) artists.
-- For each platform ID on a connection-library artist, find the corresponding
-- manual-library artist by shared MusicBrainz ID and copy the mapping.
-- Name-based backfill is intentionally skipped here; the next scan will
-- handle name matches via the new FindByMBIDOrName fallback.
INSERT OR IGNORE INTO artist_platform_ids
    (artist_id, connection_id, platform_artist_id, created_at, updated_at)
SELECT
    fs_artist.id,
    plat.connection_id,
    plat.platform_artist_id,
    plat.created_at,
    plat.updated_at
FROM artist_platform_ids plat
JOIN artists conn_artist ON conn_artist.id = plat.artist_id
JOIN libraries conn_lib ON conn_lib.id = conn_artist.library_id
    AND conn_lib.source != 'manual'
-- Find the filesystem artist with the same MBID.
JOIN artist_provider_ids conn_mbid
    ON conn_mbid.artist_id = conn_artist.id
    AND conn_mbid.provider = 'musicbrainz'
JOIN artist_provider_ids fs_mbid
    ON fs_mbid.provider = 'musicbrainz'
    AND fs_mbid.provider_id = conn_mbid.provider_id
    AND fs_mbid.artist_id != conn_artist.id
JOIN artists fs_artist ON fs_artist.id = fs_mbid.artist_id
JOIN libraries fs_lib ON fs_lib.id = fs_artist.library_id
    AND fs_lib.source = 'manual';

-- +goose Down
-- No rollback: orphaned rows are already invalid, and backfilled rows are
-- indistinguishable from manually created mappings. A re-scan will recreate
-- any mappings if this migration is re-run.
