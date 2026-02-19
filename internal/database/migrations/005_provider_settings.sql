-- +goose Up

-- Seed default provider priority settings.
INSERT OR IGNORE INTO settings (key, value) VALUES
    ('provider.priority.biography', '["musicbrainz","lastfm","audiodb","discogs","wikidata"]'),
    ('provider.priority.genres', '["musicbrainz","lastfm","audiodb","discogs"]'),
    ('provider.priority.styles', '["audiodb","discogs"]'),
    ('provider.priority.moods', '["audiodb"]'),
    ('provider.priority.members', '["musicbrainz","wikidata"]'),
    ('provider.priority.formed', '["musicbrainz","wikidata","audiodb"]'),
    ('provider.priority.thumb', '["fanarttv","audiodb"]'),
    ('provider.priority.fanart', '["fanarttv","audiodb"]'),
    ('provider.priority.logo', '["fanarttv","audiodb"]'),
    ('provider.priority.banner', '["fanarttv","audiodb"]');

-- +goose Down

DELETE FROM settings WHERE key LIKE 'provider.priority.%';
DELETE FROM settings WHERE key LIKE 'provider.%.api_key';
