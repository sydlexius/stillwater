-- +goose Up
-- Remove duplicate connection rows, keeping only the most recent per type+url.
DELETE FROM connections WHERE id NOT IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (PARTITION BY type, url ORDER BY created_at DESC) AS rn
        FROM connections
    ) WHERE rn = 1
);

-- +goose Down
-- No-op: cannot restore deleted duplicate rows.
