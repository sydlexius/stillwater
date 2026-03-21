-- +goose Up
-- Add token lifecycle (active/revoked/archived) and audit log.

-- Add status column. Existing tokens are either active or revoked based on
-- the revoked_at column. Default to "active" for new tokens.
ALTER TABLE api_tokens ADD COLUMN status TEXT NOT NULL DEFAULT 'active';

-- Backfill existing tokens: if revoked_at is set, mark as "revoked".
UPDATE api_tokens SET status = 'revoked' WHERE revoked_at IS NOT NULL;

-- Audit log for token lifecycle events. Entries referencing a deleted token
-- are anonymized (token_id set to NULL, token_name replaced with placeholder).
CREATE TABLE IF NOT EXISTS audit_log (
    id         TEXT PRIMARY KEY,
    action     TEXT NOT NULL,
    token_id   TEXT REFERENCES api_tokens(id) ON DELETE SET NULL,
    token_name TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_audit_log_token_id ON audit_log(token_id);
CREATE INDEX idx_audit_log_user_id ON audit_log(user_id);

-- +goose Down
DROP INDEX IF EXISTS idx_audit_log_user_id;
DROP INDEX IF EXISTS idx_audit_log_token_id;
DROP TABLE IF EXISTS audit_log;

-- SQLite does not support DROP COLUMN before 3.35.0; recreating the table is
-- impractical for a down migration. Leave the column in place.
