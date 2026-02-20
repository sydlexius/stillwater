-- +goose Up

CREATE TABLE IF NOT EXISTS bulk_jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'prompt_no_match',
    status TEXT NOT NULL DEFAULT 'pending',
    total_items INTEGER NOT NULL DEFAULT 0,
    processed_items INTEGER NOT NULL DEFAULT 0,
    fixed_items INTEGER NOT NULL DEFAULT 0,
    skipped_items INTEGER NOT NULL DEFAULT 0,
    failed_items INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    started_at TEXT,
    completed_at TEXT
);

CREATE TABLE IF NOT EXISTS bulk_job_items (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES bulk_jobs(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL,
    artist_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    message TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_bulk_job_items_job_id ON bulk_job_items(job_id);

-- +goose Down
DROP TABLE IF EXISTS bulk_job_items;
DROP TABLE IF EXISTS bulk_jobs;
