-- +goose Up
CREATE TABLE artist_platform_ids (
    artist_id     TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    platform_artist_id TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (artist_id, connection_id)
);

CREATE INDEX idx_artist_platform_ids_connection
    ON artist_platform_ids(connection_id);

-- +goose Down
DROP TABLE IF EXISTS artist_platform_ids;
