-- +goose Up
ALTER TABLE artists ADD COLUMN fanart_count INTEGER NOT NULL DEFAULT 0;
UPDATE artists SET fanart_count = 1 WHERE fanart_exists = 1;

-- +goose Down
ALTER TABLE artists DROP COLUMN fanart_count;
