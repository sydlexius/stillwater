-- +goose Up
-- Add incremental dirty-tracking columns to the artists table.
-- dirty_since: set when artist data changes; triggers re-evaluation.
-- rules_evaluated_at: set after rule evaluation completes for the artist.
--
-- The dirty condition: dirty_since > rules_evaluated_at OR rules_evaluated_at IS NULL.
-- New artists start with rules_evaluated_at NULL so they are always evaluated first.

ALTER TABLE artists ADD COLUMN dirty_since TEXT DEFAULT NULL;
ALTER TABLE artists ADD COLUMN rules_evaluated_at TEXT DEFAULT NULL;

-- Partial index to speed up "list dirty artists" queries on non-excluded artists.
CREATE INDEX IF NOT EXISTS idx_artists_dirty_since
    ON artists (dirty_since, rules_evaluated_at)
    WHERE is_excluded = 0;

-- +goose Down
DROP INDEX IF EXISTS idx_artists_dirty_since;
-- SQLite supports DROP COLUMN since 3.35.0 (2021-03-12); modernc.org/sqlite ships >= 3.40.
ALTER TABLE artists DROP COLUMN rules_evaluated_at;
ALTER TABLE artists DROP COLUMN dirty_since;
