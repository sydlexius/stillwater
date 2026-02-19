-- +goose Up

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

-- Seed built-in profiles
INSERT INTO platform_profiles (id, name, is_builtin, is_active, nfo_enabled, nfo_format, image_naming) VALUES
    ('emby', 'Emby', 1, 0, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('jellyfin', 'Jellyfin', 1, 0, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('kodi', 'Kodi', 1, 1, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('plex', 'Plex', 1, 0, 0, 'kodi', '{"thumb":"artist.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}'),
    ('custom', 'Custom', 1, 0, 1, 'kodi', '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}');

-- +goose Down

DROP TABLE IF EXISTS platform_profiles;
