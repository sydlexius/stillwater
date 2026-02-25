-- +goose Up
-- Track when each provider was last queried for this artist (separate from when the ID was found).
-- If id = '' AND fetched_at IS NOT NULL => provider returned "not found".
-- If id = '' AND fetched_at IS NULL     => provider has never been queried.
-- lastfm_fetched_at tracks metadata fetch attempts; LastFM has no persistent ID of its own.

ALTER TABLE artists ADD COLUMN audiodb_id_fetched_at TEXT;
ALTER TABLE artists ADD COLUMN discogs_id_fetched_at TEXT;
ALTER TABLE artists ADD COLUMN wikidata_id_fetched_at TEXT;
ALTER TABLE artists ADD COLUMN lastfm_id_fetched_at TEXT;

-- +goose Down

ALTER TABLE artists DROP COLUMN lastfm_id_fetched_at;
ALTER TABLE artists DROP COLUMN wikidata_id_fetched_at;
ALTER TABLE artists DROP COLUMN discogs_id_fetched_at;
ALTER TABLE artists DROP COLUMN audiodb_id_fetched_at;
