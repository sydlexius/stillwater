-- +goose Up
-- Issue #1170: admin deletion of inactive user accounts.
--
-- Adds last_login on users to power the inactive-only filter, plus
-- actor_user_id + target_user_id on audit_log so a user.delete entry
-- survives the FK SET NULL when the target user is later wiped.
-- The pre-existing audit_log.user_id stays ON DELETE CASCADE (SQLite
-- cannot alter FK semantics in place without a table rebuild); for
-- user.delete actions the new actor/target columns are authoritative.

-- +goose StatementBegin
ALTER TABLE users ADD COLUMN last_login TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE audit_log ADD COLUMN actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE audit_log ADD COLUMN target_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- Defense-in-depth: the bootstrap admin must remain un-deletable even if
-- some future code path (or a manual sqlite session) tries DELETE FROM
-- users WHERE id = '<protected>'. Mirrors the existing
-- prevent_deactivate_protected_user / prevent_role_change_protected_user
-- triggers from 001 so the protected guarantee holds at every layer.
CREATE TRIGGER IF NOT EXISTS prevent_delete_protected_user
BEFORE DELETE ON users
FOR EACH ROW
WHEN OLD.is_protected = 1
BEGIN
    SELECT RAISE(ABORT, 'cannot delete a protected user');
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS prevent_delete_protected_user;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE audit_log DROP COLUMN target_user_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE audit_log DROP COLUMN actor_user_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN last_login;
-- +goose StatementEnd
