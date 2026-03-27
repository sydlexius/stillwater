package artist

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

type sqliteMBSnapshotRepo struct {
	db *sql.DB
}

func newSQLiteMBSnapshotRepo(db *sql.DB) MBSnapshotRepository {
	return &sqliteMBSnapshotRepo{db: db}
}

// UpsertAll replaces all snapshot entries for the given artist. Each entry is
// upserted by the (artist_id, field) unique constraint.
func (r *sqliteMBSnapshotRepo) UpsertAll(ctx context.Context, artistID string, snapshots []MBSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	const q = `
		INSERT INTO mb_snapshots (id, artist_id, field, mb_value, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(artist_id, field) DO UPDATE SET
			mb_value   = excluded.mb_value,
			fetched_at = excluded.fetched_at`

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	for _, s := range snapshots {
		id := s.ID
		if id == "" {
			id = uuid.New().String()
		}
		fetchedAt := now
		if !s.FetchedAt.IsZero() {
			fetchedAt = s.FetchedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if _, err := tx.ExecContext(ctx, q, id, artistID, s.Field, s.MBValue, fetchedAt); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetForArtist returns all snapshots for the given artist keyed by field name.
func (r *sqliteMBSnapshotRepo) GetForArtist(ctx context.Context, artistID string) (map[string]MBSnapshot, error) {
	const q = `SELECT id, artist_id, field, mb_value, fetched_at FROM mb_snapshots WHERE artist_id = ?`

	rows, err := r.db.QueryContext(ctx, q, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]MBSnapshot)
	for rows.Next() {
		var s MBSnapshot
		var fetchedAt string
		if err := rows.Scan(&s.ID, &s.ArtistID, &s.Field, &s.MBValue, &fetchedAt); err != nil {
			return nil, err
		}
		s.FetchedAt, _ = time.Parse("2006-01-02T15:04:05Z", fetchedAt)
		result[s.Field] = s
	}
	return result, rows.Err()
}

// DeleteByArtistID removes all snapshots for the given artist.
func (r *sqliteMBSnapshotRepo) DeleteByArtistID(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM mb_snapshots WHERE artist_id = ?`, artistID)
	return err
}
