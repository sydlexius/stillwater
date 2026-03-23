package artist

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

type sqliteHistoryRepo struct {
	db *sql.DB
}

func newSQLiteHistoryRepo(db *sql.DB) HistoryRepository {
	return &sqliteHistoryRepo{db: db}
}

// Record inserts a new metadata change row.
func (r *sqliteHistoryRepo) Record(ctx context.Context, change *MetadataChange) error {
	const q = `
		INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := r.db.ExecContext(ctx, q,
		change.ID,
		change.ArtistID,
		change.Field,
		change.OldValue,
		change.NewValue,
		change.Source,
		change.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("inserting metadata change: %w", err)
	}
	return nil
}

// List returns paginated metadata changes for an artist, ordered by created_at DESC.
// Returns the changes for the requested page and the total count across all pages.
func (r *sqliteHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) ([]MetadataChange, int, error) {
	// Fetch total count first.
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = ?`, artistID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting metadata changes: %w", err)
	}

	if total == 0 {
		return []MetadataChange{}, 0, nil
	}

	const q = `
		SELECT id, artist_id, field, old_value, new_value, source, created_at
		FROM metadata_changes
		WHERE artist_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, q, artistID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("querying metadata changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var changes []MetadataChange
	for rows.Next() {
		var c MetadataChange
		var createdAtStr string
		if err := rows.Scan(&c.ID, &c.ArtistID, &c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr); err != nil {
			return nil, 0, fmt.Errorf("scanning metadata change row: %w", err)
		}
		t, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			// Fall back to SQLite's datetime() format (space separator, no timezone suffix).
			t, err = time.Parse("2006-01-02 15:04:05", createdAtStr)
			if err != nil {
				slog.Warn("unparsable created_at in metadata_changes",
					"change_id", c.ID,
					"raw_value", createdAtStr,
					"error", err,
				)
				// Use current time as fallback so clients never see a zero-value timestamp.
				t = time.Now()
			}
		}
		c.CreatedAt = t.UTC()
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating metadata change rows: %w", err)
	}

	return changes, total, nil
}
