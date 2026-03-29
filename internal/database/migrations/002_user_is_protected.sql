-- +goose Up
-- Add is_protected column and enforcement triggers for existing databases.
-- Fresh installs already have these via 001_initial_schema.sql.

ALTER TABLE users ADD COLUMN is_protected INTEGER NOT NULL DEFAULT 0;

-- +goose StatementBegin
-- Backfill: mark the bootstrap admin (the oldest local administrator) as protected.
UPDATE users
SET is_protected = 1
WHERE id = (
    SELECT id FROM users
    WHERE role = 'administrator'
      AND auth_provider = 'local'
      AND is_active = 1
    ORDER BY created_at ASC
    LIMIT 1
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS prevent_deactivate_protected_user
BEFORE UPDATE OF is_active ON users
FOR EACH ROW
WHEN OLD.is_protected = 1 AND NEW.is_active = 0
BEGIN
    SELECT RAISE(ABORT, 'cannot deactivate a protected user');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS prevent_role_change_protected_user
BEFORE UPDATE OF role ON users
FOR EACH ROW
WHEN OLD.is_protected = 1
BEGIN
    SELECT RAISE(ABORT, 'cannot change role of a protected user');
END;
-- +goose StatementEnd

-- +goose Down
-- SQLite does not support DROP COLUMN; drop and recreate the triggers only.
DROP TRIGGER IF EXISTS prevent_deactivate_protected_user;
DROP TRIGGER IF EXISTS prevent_role_change_protected_user;
