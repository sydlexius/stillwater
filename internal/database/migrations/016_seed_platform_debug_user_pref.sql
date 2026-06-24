-- +goose Up
-- Issue #2060: seed the per-user show_platform_debug preference from the
-- legacy global app setting for every existing user.
--
-- The startup Go migration (migratePlatformDebugPref in router.go) that used
-- to do this on each server start is replaced by this one-shot SQL migration.
-- INSERT OR IGNORE ensures existing per-user rows written by the Go migration
-- or by the user themselves are never overwritten.  A missing global setting
-- (the common case -- default is false) results in "false" being seeded,
-- matching the compiled default.
INSERT OR IGNORE INTO user_preferences (user_id, key, value, updated_at)
SELECT id,
       'show_platform_debug',
       COALESCE((SELECT value FROM settings WHERE key = 'show_platform_debug'), 'false'),
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM users;

-- +goose Down
-- One-way migration. We cannot know which rows were seeded by this migration
-- versus which were set by the user, so Down is a no-op.
SELECT 1;
