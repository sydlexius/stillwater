package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MetadataChange records a single field-level metadata change for an artist.
// Source values follow the pattern: "manual", "provider:<name>", "rule:<rule_id>",
// "scan", or "import".
type MetadataChange struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Field     string    `json:"field"`
	OldValue  string    `json:"old_value"`
	NewValue  string    `json:"new_value"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// HistoryRepository defines the persistence interface for metadata change records.
type HistoryRepository interface {
	// Record inserts a new metadata change entry.
	Record(ctx context.Context, change *MetadataChange) error

	// List returns paginated changes for the given artist, ordered by
	// created_at descending (most recent first).
	List(ctx context.Context, artistID string, limit, offset int) ([]MetadataChange, int, error)
}

// HistoryService provides metadata change tracking for artists.
type HistoryService struct {
	repo HistoryRepository
}

// NewHistoryService creates a HistoryService backed by SQLite.
func NewHistoryService(db *sql.DB) *HistoryService {
	return &HistoryService{repo: newSQLiteHistoryRepo(db)}
}

// NewHistoryServiceWithRepo creates a HistoryService using the provided repository,
// enabling dependency injection for tests and alternative backends.
func NewHistoryServiceWithRepo(repo HistoryRepository) *HistoryService {
	return &HistoryService{repo: repo}
}

// Record stores a single field-level metadata change. The source argument
// should be one of the defined source values: "manual", "provider:<name>",
// "rule:<rule_id>", "scan", or "import".
// This method is defined for use by Phase 2 integration hooks and is not
// wired into any existing code path yet.
func (h *HistoryService) Record(ctx context.Context, artistID, field, oldValue, newValue, source string) error {
	if artistID == "" {
		return fmt.Errorf("artist_id is required")
	}
	if field == "" {
		return fmt.Errorf("field is required")
	}
	if source == "" {
		return fmt.Errorf("source is required")
	}
	validSource := source == "manual" || source == "scan" || source == "import" ||
		strings.HasPrefix(source, "provider:") || strings.HasPrefix(source, "rule:")
	if !validSource {
		return fmt.Errorf("invalid source: %s", source)
	}
	change := &MetadataChange{
		ID:        uuid.New().String(),
		ArtistID:  artistID,
		Field:     field,
		OldValue:  oldValue,
		NewValue:  newValue,
		Source:    source,
		CreatedAt: time.Now().UTC(),
	}
	return h.repo.Record(ctx, change)
}

// List returns paginated metadata changes for the given artist, ordered by
// most recent first. The total count is returned alongside the records.
// Limit must be between 1 and 200; offset must be non-negative.
func (h *HistoryService) List(ctx context.Context, artistID string, limit, offset int) ([]MetadataChange, int, error) {
	if artistID == "" {
		return nil, 0, fmt.Errorf("artist_id is required")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return h.repo.List(ctx, artistID, limit, offset)
}
