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
//
// Producer (issue #3078) is a SEPARATE fact from Source: Source records WHAT
// TRIGGERED the write, Producer records WHAT SUPPLIED THE VALUE. See
// history_producer.go for the vocabulary and why the two are independent
// columns rather than one richer Source. As of this PR every row's Producer
// is ProducerUnrecorded ("") -- no write path stamps a real value yet.
type MetadataChange struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Field     string    `json:"field"`
	OldValue  string    `json:"old_value"`
	NewValue  string    `json:"new_value"`
	Source    string    `json:"source"`
	Producer  string    `json:"producer"`
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
	// PerFieldLimit, when > 0, switches to a windowed query that returns at most
	// this many rows per distinct field value (ROW_NUMBER() OVER PARTITION BY
	// mc.field). The global Limit/Offset are ignored on this path. Use this
	// instead of Limit when you need the top-N per field, not the global top-N.
	PerFieldLimit int
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

	// ListBlastRadius returns the currently-destroyed fields: for each
	// (artist, field), the most recent change, kept only when it was an
	// automated writer replacing a value the operator had. Read-only.
	ListBlastRadius(ctx context.Context, filter BlastRadiusFilter) ([]BlastRadiusRow, error)

	// CountBlastRadius returns how the matching rows split by attribution.
	// Both bucket counts ignore filter.Attribution so neither can be hidden.
	CountBlastRadius(ctx context.Context, filter BlastRadiusFilter) (BlastRadiusCounts, error)

	// ListNFOMBIDWrites returns every MusicBrainz ID the nfo_has_mbid rule
	// fixer wrote, one row per write. Read-only.
	ListNFOMBIDWrites(ctx context.Context, filter NFOMBIDFilter) ([]NFOMBIDWriteRow, error)

	// CountNFOMBIDWrites counts those writes and the distinct artists they
	// affect. Both figures are a floor; see NFOMBIDCounts.
	CountNFOMBIDWrites(ctx context.Context, filter NFOMBIDFilter) (NFOMBIDCounts, error)

	// LockDamageCandidates returns every (artist, field) pair whose newest
	// change reads as damage and whose own source names the rule that wrote
	// it. Read-only, and a candidate list rather than a decision: see
	// LockDamageCandidate.
	LockDamageCandidates(ctx context.Context) ([]LockDamageCandidate, error)

	// LockDamageUnattributed returns every (artist, field) pair whose newest
	// change reads as damage but whose source names no rule, so the repair
	// cannot attribute it. Read-only; feeds the unrecoverable tally.
	LockDamageUnattributed(ctx context.Context) ([]LockDamageUnattributedRow, error)

	// LockDamageChainDepths returns, per (artist, field), how many CONSECUTIVE
	// damaging writes precede the newest one in an unbroken value-linked
	// chain. Read-only, reporting only: no predicate consults it. See
	// LockDamageChainDepth.
	LockDamageChainDepths(ctx context.Context) (map[LockDamagePairKey]int, error)
}

// LockDamagePairKey identifies one (artist, field) pair. A comparable struct
// so chain depths can be returned as a map the caller indexes directly,
// rather than a slice every candidate has to scan.
type LockDamagePairKey struct {
	ArtistID string
	Field    string
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

// validHistorySource reports whether source is one of the recognized
// metadata-change source values shared by HistoryService.Record and
// recordHistoryTx. Both callers apply the same allow-list; they differ only
// in whether an identical-value write is skipped (see recordHistoryTx's doc
// comment for why that skip must not be backported).
func validHistorySource(source string) bool {
	return source == "manual" || source == "scan" || source == "import" ||
		source == "revert" ||
		strings.HasPrefix(source, "provider:") || strings.HasPrefix(source, "rule:")
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
	// Only skip when BOTH values are non-empty and identical. If oldValue is ""
	// (as in rule_fix records, which always pass oldValue==""), the guard must
	// not fire even when newValue is also "" - an accidental empty Message on a
	// fix result would otherwise be silently dropped and leave no audit trail.
	if oldValue != "" && oldValue == newValue {
		return nil
	}
	if !validHistorySource(source) {
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
		Producer:  producerForField(ctx, field),
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

// Repo returns the underlying HistoryRepository. Exposed for testing so spy
// wrappers can delegate to the real repository without requiring a separate DB.
func (h *HistoryService) Repo() HistoryRepository {
	return h.repo
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

// ListBlastRadius returns the currently-destroyed fields for the blast-radius
// report: for each (artist, field), the most recent change, kept only when
// that change was an automated writer replacing a value the operator had.
// Already-recovered fields are absent. Read-only; writes nothing.
//
// Coverage is bounded by what metadata_changes records at all, which is exactly
// TrackableFields(). Callers rendering this report must state that limit rather
// than let an absent field read as an undamaged one.
func (h *HistoryService) ListBlastRadius(ctx context.Context, filter BlastRadiusFilter) ([]BlastRadiusRow, error) {
	filter.Validate()
	return h.repo.ListBlastRadius(ctx, filter)
}

// CountBlastRadius returns how the matching rows split between attributed
// automated overwrites and unattributable ones. Both counts are always
// populated regardless of filter.Attribution.
func (h *HistoryService) CountBlastRadius(ctx context.Context, filter BlastRadiusFilter) (BlastRadiusCounts, error) {
	filter.Validate()
	return h.repo.CountBlastRadius(ctx, filter)
}

// ListNFOMBIDWrites returns every MusicBrainz ID the nfo_has_mbid rule fixer
// wrote, so an operator can review artists whose ID a rule guessed. One row per
// write, newest first by default. Read-only; writes nothing and validates no ID.
//
// Coverage is bounded to that one code path. Callers rendering this must state
// so, because other writers assign a MusicBrainz ID without recording anything
// and an artist's absence here is not evidence its ID is correct. The caveat
// texts live alongside the types in nfo_mbid_report.go.
func (h *HistoryService) ListNFOMBIDWrites(ctx context.Context, filter NFOMBIDFilter) ([]NFOMBIDWriteRow, error) {
	filter.Validate()
	return h.repo.ListNFOMBIDWrites(ctx, filter)
}

// CountNFOMBIDWrites returns the number of such writes and the number of
// distinct artists they affect. Both are a floor, not a census.
func (h *HistoryService) CountNFOMBIDWrites(ctx context.Context, filter NFOMBIDFilter) (NFOMBIDCounts, error) {
	filter.Validate()
	return h.repo.CountNFOMBIDWrites(ctx, filter)
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

// TrackableFields returns a copy of the field names tracked by the history system.
func TrackableFields() []string {
	cp := make([]string, len(trackableFields))
	copy(cp, trackableFields)
	return cp
}
