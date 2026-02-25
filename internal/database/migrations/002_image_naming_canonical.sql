-- +goose Up
-- Migrate platform profiles from array-based image naming to single canonical filenames.

UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'emby';
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'jellyfin';
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'kodi';
UPDATE platform_profiles SET image_naming = '{"thumb":"artist.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'plex';
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'custom';

INSERT INTO rules (id, name, description, category, enabled, config, automation_mode, created_at, updated_at)
VALUES ('extraneous_images', 'Extraneous image files', 'Detects non-canonical image files that may cause display issues on media servers', 'image', 1, '{"severity":"warning"}', 'notify', datetime('now'), datetime('now'))
ON CONFLICT(id) DO UPDATE SET name = excluded.name, description = excluded.description, updated_at = excluded.updated_at;

-- +goose Down
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'emby';
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'jellyfin';
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'kodi';
UPDATE platform_profiles SET image_naming = '{"thumb":["artist.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'plex';
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'custom';

DELETE FROM rules WHERE id = 'extraneous_images';
