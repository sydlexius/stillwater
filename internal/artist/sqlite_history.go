package artist

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
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

// GetByID retrieves a single metadata change by its primary key.
func (r *sqliteHistoryRepo) GetByID(ctx context.Context, id string) (*MetadataChange, error) {
	const q = `
		SELECT id, artist_id, field, old_value, new_value, source, created_at
		FROM metadata_changes
		WHERE id = ?`

	var c MetadataChange
	var createdAtStr string
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&c.ID, &c.ArtistID, &c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: %s", ErrChangeNotFound, id)
		}
		return nil, fmt.Errorf("querying metadata change: %w", err)
	}

	c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
	return &c, nil
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
		c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating metadata change rows: %w", err)
	}

	return changes, total, nil
}

// ListGlobal returns paginated metadata changes across all artists, joining
// the artists table to include the artist name in each result.
func (r *sqliteHistoryRepo) ListGlobal(ctx context.Context, filter GlobalHistoryFilter) ([]MetadataChangeWithArtist, int, error) {
	// Build dynamic WHERE clause.
	var where []string
	var args []any

	if filter.ArtistID != "" {
		where = append(where, "mc.artist_id = ?")
		args = append(args, filter.ArtistID)
	}
	if len(filter.Fields) > 0 {
		placeholders := make([]string, len(filter.Fields))
		for i, f := range filter.Fields {
			placeholders[i] = "?"
			args = append(args, f)
		}
		where = append(where, "mc.field IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(filter.Sources) > 0 {
		placeholders := make([]string, len(filter.Sources))
		for i, s := range filter.Sources {
			placeholders[i] = "?"
			args = append(args, s)
		}
		where = append(where, "mc.source IN ("+strings.Join(placeholders, ", ")+")")
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Count total matching rows. The JOIN ensures orphaned metadata_changes
	// rows (where the artist was deleted) are excluded from the count,
	// matching the behavior of the select query below.
	countQ := "SELECT COUNT(*) FROM metadata_changes mc JOIN artists a ON a.id = mc.artist_id " + whereClause
	var total int
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting global metadata changes: %w", err)
	}
	if total == 0 {
		return []MetadataChangeWithArtist{}, 0, nil
	}

	// Fetch rows with artist name.
	selectQ := `
		SELECT mc.id, mc.artist_id, a.name, mc.field, mc.old_value, mc.new_value, mc.source, mc.created_at
		FROM metadata_changes mc
		JOIN artists a ON a.id = mc.artist_id
		` + whereClause + `
		ORDER BY mc.created_at DESC, mc.id DESC
		LIMIT ? OFFSET ?`

	queryArgs := make([]any, len(args))
	copy(queryArgs, args)
	queryArgs = append(queryArgs, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, selectQ, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying global metadata changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var changes []MetadataChangeWithArtist
	for rows.Next() {
		var c MetadataChangeWithArtist
		var createdAtStr string
		if err := rows.Scan(
			&c.ID, &c.ArtistID, &c.ArtistName,
			&c.Field, &c.OldValue, &c.NewValue, &c.Source, &createdAtStr,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning global metadata change row: %w", err)
		}
		c.CreatedAt = parseHistoryTimestamp(c.ID, createdAtStr)
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating global metadata change rows: %w", err)
	}

	return changes, total, nil
}

// parseHistoryTimestamp parses a created_at string from the metadata_changes
// table, trying RFC3339 first, then SQLite datetime format. Falls back to
// current time with a warning if both fail.
func parseHistoryTimestamp(changeID, raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t, err = time.Parse("2006-01-02 15:04:05", raw)
		if err != nil {
			slog.Warn("unparsable created_at in metadata_changes",
				"change_id", changeID,
				"raw_value", raw,
				"error", err,
			)
			return time.Now().UTC()
		}
	}
	return t.UTC()
}
