-- +goose Up
-- Consolidated baseline schema.

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'admin',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS connections (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    url TEXT NOT NULL,
    encrypted_api_key TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'unknown',
    status_message TEXT NOT NULL DEFAULT '',
    last_checked_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS libraries (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'regular' CHECK(type IN ('regular', 'classical')),
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS artists (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    sort_name TEXT,
    type TEXT NOT NULL DEFAULT '',
    gender TEXT NOT NULL DEFAULT '',
    disambiguation TEXT NOT NULL DEFAULT '',
    musicbrainz_id TEXT,
    audiodb_id TEXT,
    discogs_id TEXT,
    wikidata_id TEXT,
    deezer_id TEXT NOT NULL DEFAULT '',
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
    thumb_low_res INTEGER NOT NULL DEFAULT 0,
    fanart_low_res INTEGER NOT NULL DEFAULT 0,
    logo_low_res INTEGER NOT NULL DEFAULT 0,
    banner_low_res INTEGER NOT NULL DEFAULT 0,
    health_score REAL NOT NULL DEFAULT 0.0,
    is_excluded INTEGER NOT NULL DEFAULT 0,
    exclusion_reason TEXT NOT NULL DEFAULT '',
    is_classical INTEGER NOT NULL DEFAULT 0,
    metadata_sources TEXT NOT NULL DEFAULT '{}',
    audiodb_id_fetched_at TEXT,
    discogs_id_fetched_at TEXT,
    wikidata_id_fetched_at TEXT,
    lastfm_id_fetched_at TEXT,
    last_scanned_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_artists_name ON artists(name);
CREATE INDEX idx_artists_musicbrainz_id ON artists(musicbrainz_id);
CREATE INDEX idx_artists_audiodb_id ON artists(audiodb_id);
CREATE INDEX idx_artists_discogs_id ON artists(discogs_id);
CREATE INDEX idx_artists_wikidata_id ON artists(wikidata_id);
CREATE INDEX idx_artists_deezer_id ON artists(deezer_id);
CREATE INDEX idx_artists_path ON artists(path);
CREATE INDEX idx_artists_library_id ON artists(library_id);

CREATE TABLE IF NOT EXISTS artist_aliases (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    source TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_artist_aliases_artist_id ON artist_aliases(artist_id);

CREATE TABLE IF NOT EXISTS nfo_snapshots (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_nfo_snapshots_artist_id ON nfo_snapshots(artist_id);

CREATE TABLE IF NOT EXISTS band_members (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    member_name TEXT NOT NULL,
    member_mbid TEXT,
    instruments TEXT NOT NULL DEFAULT '[]',
    vocal_type TEXT NOT NULL DEFAULT '',
    date_joined TEXT NOT NULL DEFAULT '',
    date_left TEXT NOT NULL DEFAULT '',
    is_original_member INTEGER NOT NULL DEFAULT 0,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_band_members_artist_id ON band_members(artist_id);
CREATE INDEX idx_band_members_member_mbid ON band_members(member_mbid);

CREATE TABLE IF NOT EXISTS rules (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    category TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    config TEXT NOT NULL DEFAULT '{}',
    automation_mode TEXT NOT NULL DEFAULT 'auto',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS rule_violations (
    id TEXT PRIMARY KEY,
    rule_id TEXT NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    artist_name TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'warning',
    message TEXT NOT NULL,
    fixable INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'open',
    dismissed_at TEXT,
    resolved_at TEXT,
    candidates TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(rule_id, artist_id)
);

CREATE INDEX idx_rule_violations_rule_id ON rule_violations(rule_id);
CREATE INDEX idx_rule_violations_artist_id ON rule_violations(artist_id);
CREATE INDEX idx_rule_violations_status ON rule_violations(status);

CREATE TABLE IF NOT EXISTS health_history (
    id TEXT PRIMARY KEY,
    total_artists INTEGER NOT NULL,
    compliant_artists INTEGER NOT NULL,
    score REAL NOT NULL,
    recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_health_history_recorded_at ON health_history(recorded_at);

CREATE TABLE IF NOT EXISTS webhooks (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'generic',
    events TEXT NOT NULL DEFAULT '[]',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS platform_profiles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    is_builtin INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 0,
    nfo_enabled INTEGER NOT NULL DEFAULT 1,
    nfo_format TEXT NOT NULL DEFAULT 'kodi',
    image_naming TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Seed built-in platform profiles (canonical single-filename format).
INSERT OR IGNORE INTO platform_profiles (id, name, is_builtin, is_active, nfo_enabled, nfo_format, image_naming) VALUES
    ('emby',     'Emby',     1, 0, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('jellyfin', 'Jellyfin', 1, 0, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('kodi',     'Kodi',     1, 1, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('plex',     'Plex',     1, 0, 0, 'kodi', '{"thumb":"artist.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('custom',   'Custom',   1, 0, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}');

CREATE TABLE IF NOT EXISTS bulk_jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'prompt_no_match',
    status TEXT NOT NULL DEFAULT 'pending',
    total_items INTEGER NOT NULL DEFAULT 0,
    processed_items INTEGER NOT NULL DEFAULT 0,
    fixed_items INTEGER NOT NULL DEFAULT 0,
    skipped_items INTEGER NOT NULL DEFAULT 0,
    failed_items INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    started_at TEXT,
    completed_at TEXT
);

CREATE TABLE IF NOT EXISTS bulk_job_items (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES bulk_jobs(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL,
    artist_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    message TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_bulk_job_items_job_id ON bulk_job_items(job_id);

CREATE TABLE IF NOT EXISTS scraper_config (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL DEFAULT '{}',
    overrides_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_scraper_config_scope ON scraper_config(scope);

-- Seed default provider priority settings.
INSERT OR IGNORE INTO settings (key, value) VALUES
    ('provider.priority.biography', '["musicbrainz","lastfm","audiodb","discogs","wikidata"]'),
    ('provider.priority.genres',    '["musicbrainz","lastfm","audiodb","discogs"]'),
    ('provider.priority.styles',    '["audiodb","discogs"]'),
    ('provider.priority.moods',     '["audiodb"]'),
    ('provider.priority.members',   '["musicbrainz","wikidata"]'),
    ('provider.priority.formed',    '["musicbrainz","wikidata","audiodb"]'),
    ('provider.priority.thumb',     '["fanarttv","audiodb","deezer"]'),
    ('provider.priority.fanart',    '["fanarttv","audiodb"]'),
    ('provider.priority.logo',      '["fanarttv","audiodb"]'),
    ('provider.priority.banner',    '["fanarttv","audiodb"]');

-- Seed extraneous images rule.
INSERT OR IGNORE INTO rules (id, name, description, category, enabled, config, automation_mode, created_at, updated_at)
VALUES ('extraneous_images', 'Extraneous image files', 'Detects non-canonical image files that may cause display issues on media servers', 'image', 1, '{"severity":"warning"}', 'notify', datetime('now'), datetime('now'));

-- +goose Down
-- Greenfield schema: no rollback needed.
