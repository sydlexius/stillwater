-- +goose Up

CREATE TABLE scraper_config (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL DEFAULT '{}',
    overrides_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_scraper_config_scope ON scraper_config(scope);

ALTER TABLE artists ADD COLUMN metadata_sources TEXT NOT NULL DEFAULT '{}';

-- +goose Down

DROP INDEX IF EXISTS idx_scraper_config_scope;
DROP TABLE IF EXISTS scraper_config;
ALTER TABLE artists DROP COLUMN metadata_sources;
