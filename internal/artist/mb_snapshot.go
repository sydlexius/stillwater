package artist

import (
	"context"
	"time"
)

// MBSnapshot stores the last-known MusicBrainz value for a single metadata field.
// Snapshots are upserted on each provider refresh so Stillwater can compute diffs
// between local edits and upstream MusicBrainz data for contribution workflows.
type MBSnapshot struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Field     string    `json:"field"`
	MBValue   string    `json:"mb_value"`
	FetchedAt time.Time `json:"fetched_at"`
}

// MBSnapshotRepository defines the persistence interface for MusicBrainz value snapshots.
type MBSnapshotRepository interface {
	// UpsertAll replaces all snapshot entries for the given artist with the
	// provided list. Each entry is upserted by the (artist_id, field) unique
	// constraint so that only the latest MusicBrainz value is retained.
	UpsertAll(ctx context.Context, artistID string, snapshots []MBSnapshot) error

	// GetForArtist returns a map of field name to MusicBrainz value for the
	// given artist. The map is empty (not nil) when no snapshots exist.
	GetForArtist(ctx context.Context, artistID string) (map[string]MBSnapshot, error)

	// DeleteByArtistID removes all snapshots for the given artist.
	DeleteByArtistID(ctx context.Context, artistID string) error
}
