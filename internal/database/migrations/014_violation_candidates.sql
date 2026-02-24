-- +goose Up

ALTER TABLE rule_violations ADD COLUMN candidates TEXT NOT NULL DEFAULT '[]';

-- +goose Down

ALTER TABLE rule_violations DROP COLUMN IF EXISTS candidates;
