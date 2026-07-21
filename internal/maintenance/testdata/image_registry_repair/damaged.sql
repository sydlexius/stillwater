-- #2640 fixture: DAMAGED state.
-- Extracted verbatim from the production snapshot taken 2026-07-19 02:54,
-- after the incident. Names and paths redacted; geometry is real.
-- Regenerate with ./generate.sh -- do not hand-edit.

BEGIN;
INSERT INTO artists (id, name, path) VALUES ('0a34be96-46f4-465b-b802-2e07fd6963c7', 'artist-0a34be96', '/library/artist-0a34be96');
INSERT INTO artists (id, name, path) VALUES ('0fc67357-38cb-4a4e-a7da-9b5426268d2c', 'artist-0fc67357', '/library/artist-0fc67357');
INSERT INTO artists (id, name, path) VALUES ('8d14014a-7f20-4aa8-a42a-5c1b8151d690', 'artist-8d14014a', '/library/artist-8d14014a');
INSERT INTO artists (id, name, path) VALUES ('f02eea6a-7c11-42e1-a183-caa32ce11f23', 'artist-f02eea6a', '/library/artist-f02eea6a');

INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0a34be96-fanart-0', '0a34be96-46f4-465b-b802-2e07fd6963c7', 'fanart', 0, 0, 0, 'REDACTED', 2500, 1406, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0a34be96-logo-0', '0a34be96-46f4-465b-b802-2e07fd6963c7', 'logo', 0, 0, 0, 'REDACTED', 716, 662, '', '', 'png', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0a34be96-thumb-0', '0a34be96-46f4-465b-b802-2e07fd6963c7', 'thumb', 0, 0, 0, 'REDACTED', 1280, 1280, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0fc67357-banner-0', '0fc67357-38cb-4a4e-a7da-9b5426268d2c', 'banner', 0, 0, 0, 'REDACTED', 1000, 185, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0fc67357-fanart-0', '0fc67357-38cb-4a4e-a7da-9b5426268d2c', 'fanart', 0, 0, 0, 'REDACTED', 1920, 1080, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0fc67357-logo-0', '0fc67357-38cb-4a4e-a7da-9b5426268d2c', 'logo', 0, 0, 1, 'REDACTED', 375, 290, '', '', 'png', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('0fc67357-thumb-0', '0fc67357-38cb-4a4e-a7da-9b5426268d2c', 'thumb', 0, 0, 0, 'REDACTED', 1000, 1000, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('8d14014a-banner-0', '8d14014a-7f20-4aa8-a42a-5c1b8151d690', 'banner', 0, 0, 0, 'REDACTED', 1000, 185, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('8d14014a-fanart-0', '8d14014a-7f20-4aa8-a42a-5c1b8151d690', 'fanart', 0, 0, 0, 'REDACTED', 1920, 1080, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('8d14014a-logo-0', '8d14014a-7f20-4aa8-a42a-5c1b8151d690', 'logo', 0, 0, 0, 'REDACTED', 787, 232, '', '', 'png', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('8d14014a-thumb-0', '8d14014a-7f20-4aa8-a42a-5c1b8151d690', 'thumb', 0, 0, 0, 'REDACTED', 1000, 1000, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('f02eea6a-fanart-0', 'f02eea6a-7c11-42e1-a183-caa32ce11f23', 'fanart', 0, 0, 0, 'REDACTED', 1920, 1080, '', '', 'jpeg', 'scan', 0);
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, content_hash, file_format, source, locked)
  VALUES ('f02eea6a-thumb-0', 'f02eea6a-7c11-42e1-a183-caa32ce11f23', 'thumb', 0, 0, 0, 'REDACTED', 1000, 1000, '', '', 'jpeg', 'scan', 0);
COMMIT;
