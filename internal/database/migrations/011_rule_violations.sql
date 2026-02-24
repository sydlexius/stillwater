-- +goose Up

ALTER TABLE rules ADD COLUMN automation_mode TEXT NOT NULL DEFAULT 'auto';

CREATE TABLE IF NOT EXISTS rule_violations (
    id TEXT PRIMARY KEY,
    rule_id TEXT NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    artist_name TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'warning',
    message TEXT NOT NULL,
    fixable INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'open',
    dismissed_at TEXT,
    resolved_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(rule_id, artist_id)
);

CREATE INDEX idx_rule_violations_rule_id ON rule_violations(rule_id);
CREATE INDEX idx_rule_violations_artist_id ON rule_violations(artist_id);
CREATE INDEX idx_rule_violations_status ON rule_violations(status);

-- +goose Down

DROP TABLE IF EXISTS rule_violations;

ALTER TABLE rules DROP COLUMN IF EXISTS automation_mode;
