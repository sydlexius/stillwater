-- +goose Up

ALTER TABLE artists ADD COLUMN thumb_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN fanart_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN logo_low_res INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN banner_low_res INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE artists DROP COLUMN thumb_low_res;
ALTER TABLE artists DROP COLUMN fanart_low_res;
ALTER TABLE artists DROP COLUMN logo_low_res;
ALTER TABLE artists DROP COLUMN banner_low_res;
