package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqlitePlatformIDRepo struct {
	db *sql.DB
}

func newSQLitePlatformIDRepo(db *sql.DB) *sqlitePlatformIDRepo {
	return &sqlitePlatformIDRepo{db: db}
}

func (r *sqlitePlatformIDRepo) Set(ctx context.Context, artistID, connectionID, platformArtistID string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (artist_id, connection_id)
		DO UPDATE SET platform_artist_id = excluded.platform_artist_id, updated_at = excluded.updated_at
	`, artistID, connectionID, platformArtistID, now, now)
	if err != nil {
		return fmt.Errorf("setting platform id: %w", err)
	}
	return nil
}

func (r *sqlitePlatformIDRepo) Get(ctx context.Context, artistID, connectionID string) (string, error) {
	var platformArtistID string
	err := r.db.QueryRowContext(ctx, `
		SELECT platform_artist_id FROM artist_platform_ids
		WHERE artist_id = ? AND connection_id = ?
	`, artistID, connectionID).Scan(&platformArtistID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("getting platform id: %w", err)
	}
	return platformArtistID, nil
}

func (r *sqlitePlatformIDRepo) GetAll(ctx context.Context, artistID string) ([]PlatformID, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT artist_id, connection_id, platform_artist_id, created_at, updated_at
		FROM artist_platform_ids WHERE artist_id = ? ORDER BY created_at
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing platform ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []PlatformID
	for rows.Next() {
		var p PlatformID
		var createdAt, updatedAt string
		if err := rows.Scan(&p.ArtistID, &p.ConnectionID, &p.PlatformArtistID, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning platform id: %w", err)
		}
		p.CreatedAt = dbutil.ParseTime(createdAt)
		p.UpdatedAt = dbutil.ParseTime(updatedAt)
		ids = append(ids, p)
	}
	return ids, rows.Err()
}

func (r *sqlitePlatformIDRepo) Delete(ctx context.Context, artistID, connectionID string) error {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM artist_platform_ids WHERE artist_id = ? AND connection_id = ?
	`, artistID, connectionID)
	if err != nil {
		return fmt.Errorf("deleting platform id: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrPlatformIDNotFound
	}
	return nil
}

func (r *sqlitePlatformIDRepo) DeleteByArtistID(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM artist_platform_ids WHERE artist_id = ?
	`, artistID)
	if err != nil {
		return fmt.Errorf("deleting platform ids for artist: %w", err)
	}
	return nil
}

// GetPresenceForArtists returns a map of artist ID to PlatformPresence by
// joining artist_platform_ids with connections to determine which platform
// types each artist has a mapping for. Artists with no mappings are omitted.
func (r *sqlitePlatformIDRepo) GetPresenceForArtists(ctx context.Context, artistIDs []string) (map[string]PlatformPresence, error) {
	if len(artistIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(artistIDs))
	args := make([]any, len(artistIDs))
	for i, id := range artistIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	// Return one row per (artist, connection_type) pair. GROUP BY collapses
	// multiple connections of the same type into a single row per artist.
	query := `SELECT ap.artist_id, c.type ` + //nolint:gosec // G202: placeholders are "?" literals
		`FROM artist_platform_ids ap ` +
		`JOIN connections c ON c.id = ap.connection_id ` +
		`WHERE ap.artist_id IN (` + strings.Join(placeholders, ",") + `) ` +
		`GROUP BY ap.artist_id, c.type`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch getting platform presence: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]PlatformPresence, len(artistIDs))
	for rows.Next() {
		var artistID, connType string
		if err := rows.Scan(&artistID, &connType); err != nil {
			return nil, fmt.Errorf("scanning platform presence row: %w", err)
		}
		p := result[artistID]
		switch connType {
		case "emby":
			p.HasEmby = true
		case "jellyfin":
			p.HasJellyfin = true
		case "lidarr":
			p.HasLidarr = true
		}
		result[artistID] = p
	}
	return result, rows.Err()
}
