package artist

import (
	"context"
	"database/sql"
	"errors"
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

	// a UNIQUE index on (connection_id, platform_artist_id)
	// prevents two artist rows from claiming the same platform item.
	// SQLite supports only one ON CONFLICT clause per INSERT, and the
	// existing one targets (artist_id, connection_id) for upsert. Detect
	// the cross-artist collision explicitly and return a typed sentinel so
	// the manual-library backfill (and any other best-effort caller) can
	// distinguish "already claimed by someone else, skip" from a real
	// database error.
	var existingArtistID string
	err := r.db.QueryRowContext(ctx, `
		SELECT artist_id FROM artist_platform_ids
		WHERE connection_id = ? AND platform_artist_id = ?
	`, connectionID, platformArtistID).Scan(&existingArtistID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No collision; fall through to upsert.
	case err != nil:
		return fmt.Errorf("checking existing platform id holder: %w", err)
	case existingArtistID != artistID:
		return ErrPlatformIDClaimedByAnotherArtist
	}

	_, err = r.db.ExecContext(ctx, `
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

// GetPresenceForArtists returns a map of artist ID to PlatformPresence.
// presence derives from artist_libraries memberships (the
// authoritative "currently observed by" record), not from artist_platform_ids
// (which can lag behind library state -- a library unlink leaves the
// connection and its mappings intact). One query covers all four presence
// flags; library.connection_id IS NULL maps to filesystem presence,
// otherwise the connection's type maps to HasEmby/HasJellyfin/HasLidarr.
//
// Artists with no membership rows are omitted from the result map; the
// caller treats a missing entry as "no presence."
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

	// Alias is "presence_kind" rather than "source" to avoid colliding
	// with libraries.source in the GROUP BY (SQLite resolves the bare
	// name to the table column, raising "ambiguous column" otherwise).
	//
	// Membership-derived only. The legacy artists.library_id column was
	// dropped in migration 004; artist_libraries is the authoritative
	// presence record.
	in := strings.Join(placeholders, ",")
	query := `SELECT al.artist_id, ` + //nolint:gosec // G202: placeholders are "?" literals
		`CASE WHEN l.connection_id IS NULL THEN 'filesystem' ELSE COALESCE(c.type, '') END AS presence_kind ` +
		`FROM artist_libraries al ` +
		`JOIN libraries l ON l.id = al.library_id ` +
		`LEFT JOIN connections c ON c.id = l.connection_id ` +
		`WHERE al.artist_id IN (` + in + `)`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch getting platform presence: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]PlatformPresence, len(artistIDs))
	for rows.Next() {
		var artistID, kind string
		if err := rows.Scan(&artistID, &kind); err != nil {
			return nil, fmt.Errorf("scanning platform presence row: %w", err)
		}
		p := result[artistID]
		switch kind {
		case "filesystem":
			p.HasFilesystem = true
		case "emby":
			p.HasEmby = true
		case "jellyfin":
			p.HasJellyfin = true
		case "lidarr":
			p.HasLidarr = true
		}
		result[artistID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating platform presence rows: %w", err)
	}
	return result, nil
}
