package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrChangeNotFound is returned when a metadata change record does not exist.
var ErrChangeNotFound = fmt.Errorf("metadata change not found")

// MetadataChange records a single field-level metadata change for an artist.
// Source values follow the pattern: "manual", "provider:<name>", "rule:<rule_id>",
// "scan", "import", or "revert".
type MetadataChange struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Field     string    `json:"field"`
	OldValue  string    `json:"old_value"`
	NewValue  string    `json:"new_value"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// MetadataChangeWithArtist extends MetadataChange with the artist name,
// used by global (cross-artist) queries where the caller needs to display
// which artist was affected.
type MetadataChangeWithArtist struct {
	MetadataChange
	ArtistName string `json:"artist_name"`
}

// GlobalHistoryFilter specifies filter criteria for cross-artist history queries.
type GlobalHistoryFilter struct {
	ArtistID       string    // optional: restrict to a single artist
	Fields         []string  // optional: e.g. ["biography", "genres"]
	Sources        []string  // optional: e.g. ["manual", "revert"]
	SourcePrefixes []string  // optional: prefix matches e.g. ["provider:", "rule:"]
	From           time.Time // optional: include changes on or after this timestamp
	To             time.Time // optional: include changes on or before this timestamp
	Limit          int
	Offset         int
}

// HistoryRepository defines the persistence interface for metadata change records.
type HistoryRepository interface {
	// Record inserts a new metadata change entry.
	Record(ctx context.Context, change *MetadataChange) error

	// GetByID retrieves a single metadata change by its primary key.
	// Returns ErrChangeNotFound when no matching record exists.
	GetByID(ctx context.Context, id string) (*MetadataChange, error)

	// List returns paginated changes for the given artist, ordered by
	// created_at descending (most recent first).
	List(ctx context.Context, artistID string, limit, offset int) ([]MetadataChange, int, error)

	// ListGlobal returns paginated changes across all artists, ordered by
	// created_at descending. The filter controls which records are returned.
	ListGlobal(ctx context.Context, filter GlobalHistoryFilter) ([]MetadataChangeWithArtist, int, error)
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
		source == "revert" ||
		strings.HasPrefix(source, "provider:") || strings.HasPrefix(source, "rule:")
	if !validSource {
		return fmt.Errorf("invalid source: %s", source)
	}
	// If the caller pre-assigned a change ID via ContextWithHistoryID, use it
	// so the caller can later fetch the resulting row by GetByID without a
	// racy "most recent change for X" lookup. Otherwise generate a fresh UUID.
	id := HistoryIDFromContext(ctx)
	if id == "" {
		id = uuid.New().String()
	}
	change := &MetadataChange{
		ID:        id,
		ArtistID:  artistID,
		Field:     field,
		OldValue:  oldValue,
		NewValue:  newValue,
		Source:    source,
		CreatedAt: time.Now().UTC(),
	}
	return h.repo.Record(ctx, change)
}

// GetByID retrieves a single metadata change by ID.
func (h *HistoryService) GetByID(ctx context.Context, id string) (*MetadataChange, error) {
	if id == "" {
		return nil, fmt.Errorf("change id is required")
	}
	return h.repo.GetByID(ctx, id)
}

// List returns paginated metadata changes for the given artist, ordered by
// most recent first. The total count is returned alongside the records.
// Limit must be between 1 and 500; offset must be non-negative.
func (h *HistoryService) List(ctx context.Context, artistID string, limit, offset int) ([]MetadataChange, int, error) {
	if artistID == "" {
		return nil, 0, fmt.Errorf("artist_id is required")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return h.repo.List(ctx, artistID, limit, offset)
}

// ListGlobal returns paginated metadata changes across all artists.
func (h *HistoryService) ListGlobal(ctx context.Context, filter GlobalHistoryFilter) ([]MetadataChangeWithArtist, int, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	return h.repo.ListGlobal(ctx, filter)
}

// IsTrackableField reports whether the given field name is tracked by the
// history system and can be reverted via field-level undo.
func IsTrackableField(field string) bool {
	for _, f := range trackableFields {
		if f == field {
			return true
		}
	}
	return false
}
