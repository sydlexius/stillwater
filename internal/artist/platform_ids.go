package artist

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PlatformID maps a Stillwater artist to their ID on a specific platform connection.
type PlatformID struct {
	ArtistID         string    `json:"artist_id"`
	ConnectionID     string    `json:"connection_id"`
	PlatformArtistID string    `json:"platform_artist_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SetPlatformID stores or updates the platform artist ID for an artist on a connection.
func (s *Service) SetPlatformID(ctx context.Context, artistID, connectionID, platformArtistID string) error {
	if artistID == "" || connectionID == "" || platformArtistID == "" {
		return fmt.Errorf("artist_id, connection_id, and platform_artist_id are required")
	}

	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.ExecContext(ctx, `
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

// GetPlatformID retrieves the platform artist ID for an artist on a specific connection.
// Returns empty string and nil error if not found.
func (s *Service) GetPlatformID(ctx context.Context, artistID, connectionID string) (string, error) {
	var platformArtistID string
	err := s.db.QueryRowContext(ctx, `
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

// GetPlatformIDs returns all platform artist IDs for an artist across all connections.
func (s *Service) GetPlatformIDs(ctx context.Context, artistID string) ([]PlatformID, error) {
	rows, err := s.db.QueryContext(ctx, `
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
		p.CreatedAt = parseTime(createdAt)
		p.UpdatedAt = parseTime(updatedAt)
		ids = append(ids, p)
	}
	return ids, rows.Err()
}

// DeletePlatformID removes the platform artist ID mapping for an artist on a connection.
func (s *Service) DeletePlatformID(ctx context.Context, artistID, connectionID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM artist_platform_ids WHERE artist_id = ? AND connection_id = ?
	`, artistID, connectionID)
	if err != nil {
		return fmt.Errorf("deleting platform id: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("platform id not found")
	}
	return nil
}

// DeletePlatformIDsByArtist removes all platform artist ID mappings for an artist.
func (s *Service) DeletePlatformIDsByArtist(ctx context.Context, artistID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM artist_platform_ids WHERE artist_id = ?
	`, artistID)
	if err != nil {
		return fmt.Errorf("deleting platform ids for artist: %w", err)
	}
	return nil
}
