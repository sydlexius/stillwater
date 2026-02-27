-- +goose Up
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'emby';
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'jellyfin';
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'kodi';
UPDATE platform_profiles SET image_naming = '{"thumb":["artist.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'plex';
UPDATE platform_profiles SET image_naming = '{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}' WHERE id = 'custom';

-- +goose Down
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'emby';
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"backdrop.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'jellyfin';
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'kodi';
UPDATE platform_profiles SET image_naming = '{"thumb":"artist.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'plex';
UPDATE platform_profiles SET image_naming = '{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}' WHERE id = 'custom';
