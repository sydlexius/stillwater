-- +goose Up

ALTER TABLE artists ADD COLUMN is_excluded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN exclusion_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN is_classical INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE artists DROP COLUMN is_classical;
ALTER TABLE artists DROP COLUMN exclusion_reason;
ALTER TABLE artists DROP COLUMN is_excluded;
