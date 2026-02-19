-- +goose Up

-- Add missing metadata fields to artists table
ALTER TABLE artists ADD COLUMN type TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN gender TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN disambiguation TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN genres TEXT NOT NULL DEFAULT '[]';
ALTER TABLE artists ADD COLUMN styles TEXT NOT NULL DEFAULT '[]';
ALTER TABLE artists ADD COLUMN moods TEXT NOT NULL DEFAULT '[]';
ALTER TABLE artists ADD COLUMN years_active TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN born TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN formed TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN died TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN disbanded TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN biography TEXT NOT NULL DEFAULT '';

-- Band members table
CREATE TABLE IF NOT EXISTS band_members (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    member_name TEXT NOT NULL,
    member_mbid TEXT,
    instruments TEXT NOT NULL DEFAULT '[]',
    vocal_type TEXT NOT NULL DEFAULT '',
    date_joined TEXT NOT NULL DEFAULT '',
    date_left TEXT NOT NULL DEFAULT '',
    is_original_member INTEGER NOT NULL DEFAULT 0,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_band_members_artist_id ON band_members(artist_id);
CREATE INDEX idx_band_members_member_mbid ON band_members(member_mbid);

-- +goose Down

DROP INDEX IF EXISTS idx_band_members_member_mbid;
DROP INDEX IF EXISTS idx_band_members_artist_id;
DROP TABLE IF EXISTS band_members;

ALTER TABLE artists DROP COLUMN biography;
ALTER TABLE artists DROP COLUMN disbanded;
ALTER TABLE artists DROP COLUMN died;
ALTER TABLE artists DROP COLUMN formed;
ALTER TABLE artists DROP COLUMN born;
ALTER TABLE artists DROP COLUMN years_active;
ALTER TABLE artists DROP COLUMN moods;
ALTER TABLE artists DROP COLUMN styles;
ALTER TABLE artists DROP COLUMN genres;
ALTER TABLE artists DROP COLUMN disambiguation;
ALTER TABLE artists DROP COLUMN gender;
ALTER TABLE artists DROP COLUMN type;
