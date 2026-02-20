package nfo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Snapshot represents a saved copy of an artist's NFO file content.
type Snapshot struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// SnapshotService provides NFO snapshot data operations.
type SnapshotService struct {
	db *sql.DB
}

// NewSnapshotService creates an NFO snapshot service.
func NewSnapshotService(db *sql.DB) *SnapshotService {
	return &SnapshotService{db: db}
}

// Save stores the current NFO content as a snapshot for an artist.
func (s *SnapshotService) Save(ctx context.Context, artistID, content string) (*Snapshot, error) {
	snap := &Snapshot{
		ID:        uuid.New().String(),
		ArtistID:  artistID,
		Content:   content,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nfo_snapshots (id, artist_id, content, created_at)
		VALUES (?, ?, ?, ?)
	`, snap.ID, snap.ArtistID, snap.Content, snap.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("saving nfo snapshot: %w", err)
	}
	return snap, nil
}

// List returns all snapshots for an artist, newest first.
func (s *SnapshotService) List(ctx context.Context, artistID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, artist_id, content, created_at
		FROM nfo_snapshots
		WHERE artist_id = ?
		ORDER BY created_at DESC
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing nfo snapshots: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var snapshots []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning nfo snapshot: %w", err)
		}
		snapshots = append(snapshots, *snap)
	}
	return snapshots, rows.Err()
}

// GetByID retrieves a single snapshot.
func (s *SnapshotService) GetByID(ctx context.Context, id string) (*Snapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, artist_id, content, created_at
		FROM nfo_snapshots WHERE id = ?
	`, id)
	snap, err := scanSnapshot(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("snapshot not found: %s", id)
		}
		return nil, fmt.Errorf("getting nfo snapshot: %w", err)
	}
	return snap, nil
}

func scanSnapshot(row interface{ Scan(...any) error }) (*Snapshot, error) {
	var snap Snapshot
	var createdAt string
	err := row.Scan(&snap.ID, &snap.ArtistID, &snap.Content, &createdAt)
	if err != nil {
		return nil, err
	}
	snap.CreatedAt = parseTime(createdAt)
	return &snap, nil
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
