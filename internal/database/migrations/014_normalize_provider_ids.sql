-- +goose Up

-- Normalized provider IDs table. Each row maps an artist to a single
-- provider identity (musicbrainz, audiodb, discogs, wikidata, deezer,
-- spotify, lastfm). The compound PK (artist_id, provider) enforces
-- one ID per provider per artist.
CREATE TABLE IF NOT EXISTS artist_provider_ids (
    artist_id  TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    fetched_at TEXT,
    PRIMARY KEY (artist_id, provider)
);

CREATE INDEX idx_provider_ids_lookup ON artist_provider_ids(provider, provider_id);

-- Migrate existing provider IDs from columns to rows.
INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
SELECT id, 'musicbrainz', musicbrainz_id FROM artists WHERE musicbrainz_id IS NOT NULL AND musicbrainz_id != '';

INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'audiodb', audiodb_id, audiodb_id_fetched_at FROM artists WHERE audiodb_id IS NOT NULL AND audiodb_id != '';

INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'discogs', discogs_id, discogs_id_fetched_at FROM artists WHERE discogs_id IS NOT NULL AND discogs_id != '';

INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'wikidata', wikidata_id, wikidata_id_fetched_at FROM artists WHERE wikidata_id IS NOT NULL AND wikidata_id != '';

INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
SELECT id, 'deezer', deezer_id FROM artists WHERE deezer_id != '';

INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
SELECT id, 'spotify', spotify_id FROM artists WHERE spotify_id != '';

-- lastfm has no ID column, only fetched_at. Migrate rows where fetched_at is set.
INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'lastfm', '', lastfm_id_fetched_at FROM artists WHERE lastfm_id_fetched_at IS NOT NULL;

-- Also migrate audiodb/discogs/wikidata rows that have ONLY a fetched_at (no ID).
-- These represent "we checked this provider but found nothing" records.
INSERT OR IGNORE INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'audiodb', '', audiodb_id_fetched_at FROM artists
WHERE audiodb_id_fetched_at IS NOT NULL AND (audiodb_id IS NULL OR audiodb_id = '');

INSERT OR IGNORE INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'discogs', '', discogs_id_fetched_at FROM artists
WHERE discogs_id_fetched_at IS NOT NULL AND (discogs_id IS NULL OR discogs_id = '');

INSERT OR IGNORE INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
SELECT id, 'wikidata', '', wikidata_id_fetched_at FROM artists
WHERE wikidata_id_fetched_at IS NOT NULL AND (wikidata_id IS NULL OR wikidata_id = '');

-- Remove provider ID columns from artists using the table-recreation pattern.
-- This replaces 10 individual ALTER TABLE DROP COLUMN statements that each
-- trigger _sqlite3RenameTokenMap in the pure-Go SQLite implementation,
-- causing test hangs under the Go race detector.
CREATE TABLE artists_new (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    sort_name TEXT,
    type TEXT NOT NULL DEFAULT '',
    gender TEXT NOT NULL DEFAULT '',
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
    library_id TEXT REFERENCES libraries(id) DEFAULT NULL,
    nfo_exists INTEGER NOT NULL DEFAULT 0,
    thumb_exists INTEGER NOT NULL DEFAULT 0,
    fanart_exists INTEGER NOT NULL DEFAULT 0,
    logo_exists INTEGER NOT NULL DEFAULT 0,
    banner_exists INTEGER NOT NULL DEFAULT 0,
    fanart_count INTEGER NOT NULL DEFAULT 0,
    thumb_low_res INTEGER NOT NULL DEFAULT 0,
    fanart_low_res INTEGER NOT NULL DEFAULT 0,
    logo_low_res INTEGER NOT NULL DEFAULT 0,
    banner_low_res INTEGER NOT NULL DEFAULT 0,
    thumb_placeholder TEXT NOT NULL DEFAULT '',
    fanart_placeholder TEXT NOT NULL DEFAULT '',
    logo_placeholder TEXT NOT NULL DEFAULT '',
    banner_placeholder TEXT NOT NULL DEFAULT '',
    health_score REAL NOT NULL DEFAULT 0.0,
    is_excluded INTEGER NOT NULL DEFAULT 0,
    exclusion_reason TEXT NOT NULL DEFAULT '',
    is_classical INTEGER NOT NULL DEFAULT 0,
    metadata_sources TEXT NOT NULL DEFAULT '{}',
    last_scanned_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO artists_new SELECT
    id, name, sort_name, type, gender, disambiguation,
    genres, styles, moods, years_active, born, formed, died, disbanded, biography,
    path, library_id, nfo_exists,
    thumb_exists, fanart_exists, logo_exists, banner_exists, fanart_count,
    thumb_low_res, fanart_low_res, logo_low_res, banner_low_res,
    thumb_placeholder, fanart_placeholder, logo_placeholder, banner_placeholder,
    health_score, is_excluded, exclusion_reason, is_classical, metadata_sources,
    last_scanned_at, created_at, updated_at
FROM artists;

DROP TABLE artists;
ALTER TABLE artists_new RENAME TO artists;

-- Recreate surviving indexes (provider-specific indexes are no longer needed).
CREATE INDEX idx_artists_name ON artists(name);
CREATE INDEX idx_artists_path ON artists(path);
CREATE INDEX idx_artists_library_id ON artists(library_id);

-- +goose Down
-- Re-add columns, copy data back, drop normalized table.
ALTER TABLE artists ADD COLUMN musicbrainz_id TEXT;
ALTER TABLE artists ADD COLUMN audiodb_id TEXT;
ALTER TABLE artists ADD COLUMN discogs_id TEXT;
ALTER TABLE artists ADD COLUMN wikidata_id TEXT;
ALTER TABLE artists ADD COLUMN deezer_id TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN spotify_id TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN audiodb_id_fetched_at TEXT;
ALTER TABLE artists ADD COLUMN discogs_id_fetched_at TEXT;
ALTER TABLE artists ADD COLUMN wikidata_id_fetched_at TEXT;
ALTER TABLE artists ADD COLUMN lastfm_id_fetched_at TEXT;

UPDATE artists SET musicbrainz_id = (SELECT provider_id FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'musicbrainz');
UPDATE artists SET audiodb_id = (SELECT provider_id FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'audiodb');
UPDATE artists SET discogs_id = (SELECT provider_id FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'discogs');
UPDATE artists SET wikidata_id = (SELECT provider_id FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'wikidata');
UPDATE artists SET deezer_id = COALESCE((SELECT provider_id FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'deezer'), '');
UPDATE artists SET spotify_id = COALESCE((SELECT provider_id FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'spotify'), '');
UPDATE artists SET audiodb_id_fetched_at = (SELECT fetched_at FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'audiodb');
UPDATE artists SET discogs_id_fetched_at = (SELECT fetched_at FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'discogs');
UPDATE artists SET wikidata_id_fetched_at = (SELECT fetched_at FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'wikidata');
UPDATE artists SET lastfm_id_fetched_at = (SELECT fetched_at FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'lastfm');

CREATE INDEX idx_artists_musicbrainz_id ON artists(musicbrainz_id);
CREATE INDEX idx_artists_audiodb_id ON artists(audiodb_id);
CREATE INDEX idx_artists_discogs_id ON artists(discogs_id);
CREATE INDEX idx_artists_wikidata_id ON artists(wikidata_id);
CREATE INDEX idx_artists_deezer_id ON artists(deezer_id);

DROP INDEX IF EXISTS idx_provider_ids_lookup;
DROP TABLE IF EXISTS artist_provider_ids;
