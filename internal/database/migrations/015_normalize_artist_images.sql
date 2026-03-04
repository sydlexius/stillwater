-- +goose Up

-- Normalized artist images table. Each row represents one image slot
-- for an artist (e.g. thumb slot 0, fanart slot 0, fanart slot 1).
-- The unique constraint on (artist_id, image_type, slot_index) enforces
-- one record per slot per image type per artist.
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
    source      TEXT NOT NULL DEFAULT '',
    UNIQUE(artist_id, image_type, slot_index)
);

CREATE INDEX idx_artist_images_artist_id ON artist_images(artist_id);

-- Migrate existing thumb data.
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder)
SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
       id, 'thumb', 0, thumb_exists, thumb_low_res, thumb_placeholder
FROM artists WHERE thumb_exists != 0 OR thumb_low_res != 0 OR thumb_placeholder != '';

-- Migrate existing fanart data.
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder)
SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
       id, 'fanart', 0, fanart_exists, fanart_low_res, fanart_placeholder
FROM artists WHERE fanart_exists != 0 OR fanart_low_res != 0 OR fanart_placeholder != '';

-- Migrate existing logo data.
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder)
SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
       id, 'logo', 0, logo_exists, logo_low_res, logo_placeholder
FROM artists WHERE logo_exists != 0 OR logo_low_res != 0 OR logo_placeholder != '';

-- Migrate existing banner data.
INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder)
SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
       id, 'banner', 0, banner_exists, banner_low_res, banner_placeholder
FROM artists WHERE banner_exists != 0 OR banner_low_res != 0 OR banner_placeholder != '';

-- Drop image columns from the artists table (SQLite 3.35+).
ALTER TABLE artists DROP COLUMN thumb_exists;
ALTER TABLE artists DROP COLUMN fanart_exists;
ALTER TABLE artists DROP COLUMN logo_exists;
ALTER TABLE artists DROP COLUMN banner_exists;
ALTER TABLE artists DROP COLUMN fanart_count;
ALTER TABLE artists DROP COLUMN thumb_low_res;
ALTER TABLE artists DROP COLUMN fanart_low_res;
ALTER TABLE artists DROP COLUMN logo_low_res;
ALTER TABLE artists DROP COLUMN banner_low_res;
ALTER TABLE artists DROP COLUMN thumb_placeholder;
ALTER TABLE artists DROP COLUMN fanart_placeholder;
ALTER TABLE artists DROP COLUMN logo_placeholder;
ALTER TABLE artists DROP COLUMN banner_placeholder;

-- +goose Down
-- Re-add columns, copy data back, drop normalized table.
ALTER TABLE artists ADD COLUMN thumb_exists INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN fanart_exists INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN logo_exists INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN banner_exists INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN fanart_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN thumb_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN fanart_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN logo_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN banner_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN thumb_placeholder TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN fanart_placeholder TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN logo_placeholder TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN banner_placeholder TEXT NOT NULL DEFAULT '';

UPDATE artists SET thumb_exists = COALESCE((SELECT exists_flag FROM artist_images WHERE artist_id = artists.id AND image_type = 'thumb' AND slot_index = 0), 0);
UPDATE artists SET thumb_low_res = COALESCE((SELECT low_res FROM artist_images WHERE artist_id = artists.id AND image_type = 'thumb' AND slot_index = 0), 0);
UPDATE artists SET thumb_placeholder = COALESCE((SELECT placeholder FROM artist_images WHERE artist_id = artists.id AND image_type = 'thumb' AND slot_index = 0), '');

UPDATE artists SET fanart_exists = COALESCE((SELECT exists_flag FROM artist_images WHERE artist_id = artists.id AND image_type = 'fanart' AND slot_index = 0), 0);
UPDATE artists SET fanart_low_res = COALESCE((SELECT low_res FROM artist_images WHERE artist_id = artists.id AND image_type = 'fanart' AND slot_index = 0), 0);
UPDATE artists SET fanart_placeholder = COALESCE((SELECT placeholder FROM artist_images WHERE artist_id = artists.id AND image_type = 'fanart' AND slot_index = 0), '');
UPDATE artists SET fanart_count = (SELECT COUNT(*) FROM artist_images WHERE artist_id = artists.id AND image_type = 'fanart' AND exists_flag = 1);

UPDATE artists SET logo_exists = COALESCE((SELECT exists_flag FROM artist_images WHERE artist_id = artists.id AND image_type = 'logo' AND slot_index = 0), 0);
UPDATE artists SET logo_low_res = COALESCE((SELECT low_res FROM artist_images WHERE artist_id = artists.id AND image_type = 'logo' AND slot_index = 0), 0);
UPDATE artists SET logo_placeholder = COALESCE((SELECT placeholder FROM artist_images WHERE artist_id = artists.id AND image_type = 'logo' AND slot_index = 0), '');

UPDATE artists SET banner_exists = COALESCE((SELECT exists_flag FROM artist_images WHERE artist_id = artists.id AND image_type = 'banner' AND slot_index = 0), 0);
UPDATE artists SET banner_low_res = COALESCE((SELECT low_res FROM artist_images WHERE artist_id = artists.id AND image_type = 'banner' AND slot_index = 0), 0);
UPDATE artists SET banner_placeholder = COALESCE((SELECT placeholder FROM artist_images WHERE artist_id = artists.id AND image_type = 'banner' AND slot_index = 0), '');

DROP INDEX IF EXISTS idx_artist_images_artist_id;
DROP TABLE IF EXISTS artist_images;
