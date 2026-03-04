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

-- Drop old per-column indexes (no longer needed).
DROP INDEX IF EXISTS idx_artists_musicbrainz_id;
DROP INDEX IF EXISTS idx_artists_audiodb_id;
DROP INDEX IF EXISTS idx_artists_discogs_id;
DROP INDEX IF EXISTS idx_artists_wikidata_id;
DROP INDEX IF EXISTS idx_artists_deezer_id;

-- Drop the provider ID columns from the artists table (SQLite 3.35+).
ALTER TABLE artists DROP COLUMN musicbrainz_id;
ALTER TABLE artists DROP COLUMN audiodb_id;
ALTER TABLE artists DROP COLUMN discogs_id;
ALTER TABLE artists DROP COLUMN wikidata_id;
ALTER TABLE artists DROP COLUMN deezer_id;
ALTER TABLE artists DROP COLUMN spotify_id;
ALTER TABLE artists DROP COLUMN audiodb_id_fetched_at;
ALTER TABLE artists DROP COLUMN discogs_id_fetched_at;
ALTER TABLE artists DROP COLUMN wikidata_id_fetched_at;
ALTER TABLE artists DROP COLUMN lastfm_id_fetched_at;

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
