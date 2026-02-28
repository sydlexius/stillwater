-- +goose Up
ALTER TABLE platform_profiles ADD COLUMN use_symlinks INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE platform_profiles DROP COLUMN use_symlinks;
