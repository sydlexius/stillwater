-- +goose Up
-- Unified baseline schema for Stillwater v1.0.0.
-- Consolidates migrations 001 through 018.

-- =============================================================================
-- Authentication
-- =============================================================================

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'operator',
    auth_provider TEXT NOT NULL DEFAULT 'local',
    provider_id   TEXT NOT NULL DEFAULT '',
    is_active     INTEGER NOT NULL DEFAULT 1,
    is_protected  INTEGER NOT NULL DEFAULT 0,
    invited_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- +goose StatementBegin
-- Prevent deactivating the bootstrap administrator at the database level.
CREATE TRIGGER IF NOT EXISTS prevent_deactivate_protected_user
BEFORE UPDATE OF is_active ON users
FOR EACH ROW
WHEN OLD.is_protected = 1 AND NEW.is_active = 0
BEGIN
    SELECT RAISE(ABORT, 'cannot deactivate a protected user');
END;
-- +goose StatementEnd

-- +goose StatementBegin
-- Prevent changing the role of the bootstrap administrator at the database level.
CREATE TRIGGER IF NOT EXISTS prevent_role_change_protected_user
BEFORE UPDATE OF role ON users
FOR EACH ROW
WHEN OLD.is_protected = 1
BEGIN
    SELECT RAISE(ABORT, 'cannot change role of a protected user');
END;
-- +goose StatementEnd

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL DEFAULT 'read,write',
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at TEXT,
    revoked_at   TEXT,
    status       TEXT NOT NULL DEFAULT 'active'
);

CREATE INDEX idx_api_tokens_hash ON api_tokens(token_hash);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

CREATE TABLE IF NOT EXISTS audit_log (
    id         TEXT PRIMARY KEY,
    action     TEXT NOT NULL,
    token_id   TEXT REFERENCES api_tokens(id) ON DELETE SET NULL,
    token_name TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_audit_log_token_id ON audit_log(token_id);
CREATE INDEX idx_audit_log_user_id ON audit_log(user_id);

CREATE TABLE IF NOT EXISTS invites (
    id          TEXT PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    role        TEXT NOT NULL DEFAULT 'operator',
    created_by  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TEXT NOT NULL,
    redeemed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    redeemed_at TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_invites_code ON invites(code);

-- =============================================================================
-- Connections and Libraries
-- =============================================================================

CREATE TABLE IF NOT EXISTS connections (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    url TEXT NOT NULL,
    encrypted_api_key TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'unknown',
    status_message TEXT NOT NULL DEFAULT '',
    feature_library_import INTEGER NOT NULL DEFAULT 1,
    feature_nfo_write INTEGER NOT NULL DEFAULT 1,
    feature_image_write INTEGER NOT NULL DEFAULT 1,
    platform_user_id TEXT,
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
    source TEXT NOT NULL DEFAULT 'manual',
    connection_id TEXT REFERENCES connections(id) DEFAULT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    fs_watch INTEGER NOT NULL DEFAULT 0,
    fs_poll_interval INTEGER NOT NULL DEFAULT 60,
    shared_fs_status TEXT NOT NULL DEFAULT '',
    shared_fs_evidence TEXT NOT NULL DEFAULT '',
    shared_fs_peer_library_ids TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX idx_libraries_connection_external
    ON libraries(connection_id, external_id)
    WHERE connection_id IS NOT NULL AND external_id <> '';

-- =============================================================================
-- Artists (core)
-- =============================================================================

CREATE TABLE IF NOT EXISTS artists (
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
    health_score REAL NOT NULL DEFAULT 0.0,
    health_evaluated_at TEXT DEFAULT NULL,
    is_excluded INTEGER NOT NULL DEFAULT 0,
    exclusion_reason TEXT NOT NULL DEFAULT '',
    is_classical INTEGER NOT NULL DEFAULT 0,
    locked INTEGER NOT NULL DEFAULT 0,
    lock_source TEXT NOT NULL DEFAULT '' CHECK (lock_source IN ('', 'user', 'imported')),
    locked_at TEXT CHECK (locked = 0 OR (locked = 1 AND locked_at IS NOT NULL)),
    metadata_sources TEXT NOT NULL DEFAULT '{}',
    last_scanned_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_artists_name ON artists(name);
CREATE INDEX idx_artists_path ON artists(path);
CREATE INDEX idx_artists_library_id ON artists(library_id);
CREATE INDEX idx_artists_locked ON artists(locked);

-- =============================================================================
-- Artist relationships (normalized)
-- =============================================================================

CREATE TABLE IF NOT EXISTS artist_provider_ids (
    artist_id  TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    fetched_at TEXT,
    PRIMARY KEY (artist_id, provider)
);

CREATE INDEX idx_provider_ids_lookup ON artist_provider_ids(provider, provider_id);

CREATE TABLE IF NOT EXISTS artist_images (
    id          TEXT NOT NULL PRIMARY KEY,
    artist_id   TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    image_type  TEXT NOT NULL,
    slot_index  INTEGER NOT NULL DEFAULT 0,
    exists_flag INTEGER NOT NULL DEFAULT 0,
    low_res     INTEGER NOT NULL DEFAULT 0,
    placeholder TEXT NOT NULL DEFAULT '',
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    phash       TEXT NOT NULL DEFAULT '',
    file_format TEXT NOT NULL DEFAULT '',
    source         TEXT NOT NULL DEFAULT '',
    last_written_at TEXT NOT NULL DEFAULT '',
    UNIQUE(artist_id, image_type, slot_index)
);

CREATE INDEX idx_artist_images_artist_id ON artist_images(artist_id);

CREATE TABLE IF NOT EXISTS artist_platform_ids (
    artist_id     TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    platform_artist_id TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (artist_id, connection_id)
);

CREATE INDEX idx_artist_platform_ids_connection
    ON artist_platform_ids(connection_id);

CREATE TABLE IF NOT EXISTS artist_aliases (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    source TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_artist_aliases_artist_id ON artist_aliases(artist_id);

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

CREATE TABLE IF NOT EXISTS nfo_snapshots (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_nfo_snapshots_artist_id ON nfo_snapshots(artist_id);

-- Tracks individual field-level metadata changes for each artist.
CREATE TABLE IF NOT EXISTS metadata_changes (
    id         TEXT PRIMARY KEY,
    artist_id  TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    field      TEXT NOT NULL,
    old_value  TEXT NOT NULL DEFAULT '',
    new_value  TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'manual',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX idx_metadata_changes_artist ON metadata_changes(artist_id, created_at DESC);

-- Stores the last-known MusicBrainz value for each metadata field per artist.
-- Upserted on each provider refresh so that Stillwater can compute diffs between
-- local edits and upstream MusicBrainz data for contribution workflows.
CREATE TABLE IF NOT EXISTS mb_snapshots (
    id          TEXT PRIMARY KEY,
    artist_id   TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    field       TEXT NOT NULL,
    mb_value    TEXT NOT NULL DEFAULT '',
    fetched_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(artist_id, field)
);

CREATE INDEX idx_mb_snapshots_artist ON mb_snapshots(artist_id);

-- =============================================================================
-- Rule engine
-- =============================================================================

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
CREATE INDEX idx_rule_violations_created_at ON rule_violations(created_at);
CREATE INDEX idx_rule_violations_resolved_at ON rule_violations(resolved_at);

-- =============================================================================
-- Health tracking
-- =============================================================================

CREATE TABLE IF NOT EXISTS health_history (
    id TEXT PRIMARY KEY,
    total_artists INTEGER NOT NULL,
    compliant_artists INTEGER NOT NULL,
    score REAL NOT NULL,
    recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_health_history_recorded_at ON health_history(recorded_at);

-- =============================================================================
-- Webhooks
-- =============================================================================

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

-- =============================================================================
-- Platform profiles
-- =============================================================================

CREATE TABLE IF NOT EXISTS platform_profiles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    is_builtin INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 0,
    nfo_enabled INTEGER NOT NULL DEFAULT 1,
    nfo_format TEXT NOT NULL DEFAULT 'kodi',
    image_naming TEXT NOT NULL DEFAULT '{}',
    use_symlinks INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- =============================================================================
-- Bulk jobs
-- =============================================================================

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

-- =============================================================================
-- Scraper configuration
-- =============================================================================

CREATE TABLE IF NOT EXISTS scraper_config (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL DEFAULT '{}',
    overrides_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_scraper_config_scope ON scraper_config(scope);

-- =============================================================================
-- Seed data
-- =============================================================================

-- Built-in platform profiles.
INSERT OR IGNORE INTO platform_profiles (id, name, is_builtin, is_active, nfo_enabled, nfo_format, image_naming) VALUES
    ('emby',     'Emby',     1, 0, 1, 'kodi', '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}'),
    ('jellyfin', 'Jellyfin', 1, 0, 1, 'kodi', '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}'),
    ('kodi',     'Kodi',     1, 1, 1, 'kodi', '{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}'),
    ('plex',     'Plex',     1, 0, 0, 'kodi', '{"thumb":["artist.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}'),
    ('custom',   'Custom',   1, 0, 1, 'kodi', '{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}');

-- Default provider priority settings.
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

-- MusicBrainz contribution mode (disabled, web_form, api).
INSERT OR IGNORE INTO settings (key, value) VALUES ('musicbrainz.contributions', 'disabled');

-- Authentication method (local, emby, jellyfin). Instance-level setting chosen during setup.
INSERT OR IGNORE INTO settings (key, value) VALUES ('auth.method', 'local');

-- Multi-user mode. When false, the instance operates in single-admin mode.
INSERT OR IGNORE INTO settings (key, value) VALUES ('multi_user.enabled', 'false');

-- Default rule: extraneous images.
INSERT OR IGNORE INTO rules (id, name, description, category, enabled, config, automation_mode, created_at, updated_at)
VALUES ('extraneous_images', 'Extraneous image files', 'Detects non-canonical image files that may cause display issues on media servers', 'image', 1, '{"severity":"warning"}', 'manual', datetime('now'), datetime('now'));

-- +goose Down
-- Greenfield schema: full teardown.
DROP TABLE IF EXISTS bulk_job_items;
DROP TABLE IF EXISTS bulk_jobs;
DROP TABLE IF EXISTS rule_violations;
DROP TABLE IF EXISTS rules;
DROP TABLE IF EXISTS health_history;
DROP TABLE IF EXISTS metadata_changes;
DROP TABLE IF EXISTS nfo_snapshots;
DROP TABLE IF EXISTS band_members;
DROP TABLE IF EXISTS artist_aliases;
DROP TABLE IF EXISTS artist_images;
DROP TABLE IF EXISTS artist_provider_ids;
DROP TABLE IF EXISTS artist_platform_ids;
DROP TABLE IF EXISTS artists;
DROP TABLE IF EXISTS libraries;
DROP TABLE IF EXISTS webhooks;
DROP TABLE IF EXISTS platform_profiles;
DROP TABLE IF EXISTS scraper_config;
DROP TABLE IF EXISTS invites;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS connections;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS users;
