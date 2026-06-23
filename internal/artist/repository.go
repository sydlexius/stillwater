package artist

import (
	"context"
	"time"
)

// Repository defines core artist CRUD operations.
type Repository interface {
	Create(ctx context.Context, a *Artist) error
	GetByID(ctx context.Context, id string) (*Artist, error)
	GetByMBID(ctx context.Context, mbid string) (*Artist, error)

	// GetByName looks up an artist by case-insensitive exact name match
	// without any library scope. connection populates use this
	// to dedupe across all libraries (the legacy GetByNameAndLibrary scoped
	// to one library and missed cross-library duplicates).
	GetByName(ctx context.Context, name string) (*Artist, error)

	// FindByMBIDOrNameUnscoped performs unscoped MBID-then-name lookup,
	// matching the dedupe priority used by connection populates.
	FindByMBIDOrNameUnscoped(ctx context.Context, mbid, name string) (*Artist, error)

	GetByPath(ctx context.Context, path string) (*Artist, error)
	List(ctx context.Context, params ListParams) ([]Artist, int, error)
	// Count returns the total number of artists matching the given filters
	// without fetching any row data. Use this instead of List when only the
	// count is needed (e.g., sidebar badge).
	Count(ctx context.Context, params CountParams) (int, error)
	Update(ctx context.Context, a *Artist) error
	UpdateField(ctx context.Context, id, field, value string) error
	ClearField(ctx context.Context, id, field string) error
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, query string) ([]Artist, error)
	SetLock(ctx context.Context, id string, locked bool, source string) error

	// SetLockedFields replaces the set of locked field names for an artist.
	// Pass an empty slice to clear all field locks.
	SetLockedFields(ctx context.Context, id string, fields []string) error

	// ListPathsByLibrary returns a map of artist ID to filesystem path for
	// all artists in the given library that have a non-empty path.
	ListPathsByLibrary(ctx context.Context, libraryID string) (map[string]string, error)

	// ListRefsByLibrary returns a lightweight (id, name, path) record for
	// every artist whose membership includes the given library AND whose
	// path is non-empty. Used by the scanner's detectRemoved path so a
	// per-library removal sweep issues a single query instead of paginating
	// the full artist list. Order is not guaranteed.
	ListRefsByLibrary(ctx context.Context, libraryID string) ([]ArtistRef, error)

	// ListByIDs returns the artist rows matching the given IDs in a single
	// query. Order is not guaranteed; callers that need the original
	// request order should reconstruct it from the returned slice's IDs.
	// An empty slice argument yields an empty result with no DB round-trip.
	// IDs filtered by this method use a bound IN-clause, so callers MUST
	// cap the input to MaxListIDs before invoking it.
	ListByIDs(ctx context.Context, ids []string) ([]Artist, error)

	// ListByLibrary returns every artist whose membership includes the
	// given library. Used by the scanner's processDirectory pre-load so
	// the per-directory hot path can read from an in-memory map instead
	// of issuing a GetByPath round-trip per directory.
	ListByLibrary(ctx context.Context, libraryID string) ([]Artist, error)

	// UpdateHealthScore sets only the health_score column for the given artist,
	// avoiding a full row overwrite that could clobber concurrent mutations.
	UpdateHealthScore(ctx context.Context, id string, score float64) error

	// UpdatePath sets only the path column for the given artist. Used by the
	// directory-rename flow so a concurrent metadata edit (Name, Locked, etc.)
	// landing between the rename's hydrated load and this write is not
	// silently reverted by a full-row Update.
	UpdatePath(ctx context.Context, id, path string) error

	// HealthStats returns aggregate health metrics for non-excluded artists.
	// When libraryID is non-empty, only artists in that library are included.
	HealthStats(ctx context.Context, libraryID string) (HealthStatsResult, error)

	// ListUnevaluatedIDs returns IDs of non-excluded artists that have never been evaluated
	// (health_evaluated_at IS NULL).
	ListUnevaluatedIDs(ctx context.Context) ([]string, error)

	// MarkDirty stamps dirty_since for one artist. Used by the rule dirty
	// tracker to flag mutated artists for incremental re-evaluation.
	MarkDirty(ctx context.Context, id string, ts time.Time) error

	// MarkAllDirty stamps dirty_since on every non-excluded, non-locked
	// artist. Returns the number of rows affected. Called when a new rule
	// is added so existing artists are re-evaluated against it.
	MarkAllDirty(ctx context.Context, ts time.Time) (int64, error)

	// MarkRulesEvaluated stamps rules_evaluated_at for one artist after
	// the rule pipeline finishes processing it.
	MarkRulesEvaluated(ctx context.Context, id string, ts time.Time) error

	// ListDirtyIDs returns IDs of artists that need rule re-evaluation
	// (rules_evaluated_at IS NULL or dirty_since > rules_evaluated_at),
	// excluding excluded and locked artists.
	ListDirtyIDs(ctx context.Context) ([]string, error)

	// CountEligibleArtists returns the number of non-excluded, non-locked
	// artists in the catalog -- the denominator for incremental Run Rules
	// progress reporting.
	CountEligibleArtists(ctx context.Context) (int, error)

	// LatestRulesEvaluatedAt returns the most recent rules_evaluated_at
	// timestamp across all non-excluded artists, or nil when no artist has
	// ever been evaluated. Used to hydrate the scheduler's in-memory
	// lastRunAt on startup so the dashboard's "Last evaluated" stat survives
	// a server restart (#1796).
	LatestRulesEvaluatedAt(ctx context.Context) (*time.Time, error)

	// ListIDs returns the IDs of all artists matching the given filters,
	// ordered by sort_name then id for a stable, deterministic sequence.
	// Results are capped at MaxListIDs via a LIMIT clause. A separate
	// COUNT(*) query provides the true total match count, so callers can
	// detect overflow even when the ID slice is truncated (total > len(ids)
	// means capped is true). Used by the "select all matching" affordance
	// on /artists so the client can load the full cross-page selection
	// without a server-side "select-all mode" flag.
	ListIDs(ctx context.Context, params CountParams) (ids []string, total int, err error)
}

// ProviderIDRepository handles provider-specific ID lookups and persistence.
type ProviderIDRepository interface {
	GetByProviderID(ctx context.Context, provider, id string) (*Artist, error)
	GetForArtist(ctx context.Context, artistID string) ([]ProviderID, error)
	GetForArtists(ctx context.Context, artistIDs []string) (map[string][]ProviderID, error)
	UpsertAll(ctx context.Context, artistID string, ids []ProviderID) error
	DeleteAll(ctx context.Context, artistID string) error
	UpdateProviderFetchedAt(ctx context.Context, artistID, provider string) error
}

// MemberRepository manages band member records.
type MemberRepository interface {
	ListByArtistID(ctx context.Context, artistID string) ([]BandMember, error)
	Create(ctx context.Context, m *BandMember) error
	Delete(ctx context.Context, id string) error
	DeleteByArtistID(ctx context.Context, artistID string) error
	Upsert(ctx context.Context, artistID string, members []BandMember) error
}

// AliasRepository manages artist alias records and duplicate detection.
type AliasRepository interface {
	Create(ctx context.Context, a *Alias) error
	Delete(ctx context.Context, aliasID string) error
	ListByArtistID(ctx context.Context, artistID string) ([]Alias, error)
	SearchWithAliases(ctx context.Context, query string) ([]Artist, error)
	FindMBIDDuplicates(ctx context.Context) ([]DuplicateGroup, error)
	FindAliasDuplicates(ctx context.Context) ([]DuplicateGroup, error)
}

// ImageRepository manages artist image metadata records.
type ImageRepository interface {
	GetForArtist(ctx context.Context, artistID string) ([]ArtistImage, error)
	GetForArtists(ctx context.Context, artistIDs []string) (map[string][]ArtistImage, error)
	Upsert(ctx context.Context, img *ArtistImage) error
	UpsertAll(ctx context.Context, artistID string, images []ArtistImage) error
	UpdateProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, source, fileFormat, lastWrittenAt string) error
	// ClearExistsFlag sets exists_flag=0 for the given artist/image_type/slot.
	// Used to mark stale image entries when the file is confirmed missing on disk.
	ClearExistsFlag(ctx context.Context, artistID, imageType string, slotIndex int) error

	// SetLock toggles the lock flag for a single image row identified by its
	// primary key id.
	SetLock(ctx context.Context, imageID string, locked bool) error

	DeleteByArtistID(ctx context.Context, artistID string) error

	// NewestWriteTimesByArtist returns a map of artist_id to their most recent
	// last_written_at timestamp string for all artists in the given library.
	// Only artists with at least one non-empty last_written_at are included.
	NewestWriteTimesByArtist(ctx context.Context, libraryID string) (map[string]string, error)
}

// CompletenessRepository computes aggregate metadata completeness metrics
// across the artist catalog.
type CompletenessRepository interface {
	// GetCompletenessRows returns one row per non-excluded artist with the
	// raw boolean flags needed to compute field coverage. When libraryID is
	// non-empty only artists in that library are included.
	GetCompletenessRows(ctx context.Context, libraryID string) ([]CompletenessRow, error)

	// GetLowestCompleteness returns the bottom-N non-excluded artists sorted
	// by health_score ascending. When libraryID is non-empty only artists in
	// that library are included.
	GetLowestCompleteness(ctx context.Context, libraryID string, limit int) ([]LowestCompletenessArtist, error)
}

// CompletenessRow holds the raw per-artist boolean flags for completeness
// calculations. It is intentionally lightweight to avoid the overhead of
// hydrating provider IDs or image metadata for every artist.
type CompletenessRow struct {
	ID          string
	Name        string
	Type        string // "group", "person", "orchestra", ""
	LibraryID   string
	Biography   string
	Genres      string // raw JSON array stored in DB
	Styles      string
	Moods       string
	YearsActive string
	Born        string
	Formed      string
	Died        string
	Disbanded   string
	NFOExists   bool
	HasMBID     bool
	HasThumb    bool
	HasFanart   bool
	HasLogo     bool
	HasBanner   bool
}

// LowestCompletenessArtist is a compact artist record for the lowest-completeness table.
type LowestCompletenessArtist struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	LibraryID   string  `json:"library_id"`
	HealthScore float64 `json:"health_score"`
}

// PlatformIDRepository manages platform ID mappings between Stillwater
// artists and their IDs on external connections (Emby, Jellyfin, Lidarr).
type PlatformIDRepository interface {
	// Set stores or updates the mapping from a Stillwater artist to a
	// platform-specific artist ID for the given connection.
	Set(ctx context.Context, artistID, connectionID, platformArtistID string) error

	// Get looks up the platform-specific artist ID for the given Stillwater
	// artist and connection. If no mapping exists, it returns an empty string
	// and a nil error. A non-nil error indicates an actual lookup failure.
	Get(ctx context.Context, artistID, connectionID string) (string, error)

	// GetAll returns all platform ID mappings for the given Stillwater artist.
	GetAll(ctx context.Context, artistID string) ([]PlatformID, error)

	// Delete removes the mapping for the given Stillwater artist and connection.
	Delete(ctx context.Context, artistID, connectionID string) error

	// DeleteByArtistID removes all platform ID mappings for the given artist.
	DeleteByArtistID(ctx context.Context, artistID string) error

	// GetPresenceForArtists returns per-artist platform presence (filesystem,
	// Emby, Jellyfin, Lidarr) derived from artist_libraries memberships joined
	// with libraries: a NULL library.connection_id maps to HasFilesystem; a
	// non-NULL connection_id maps to HasEmby/HasJellyfin/HasLidarr based on the
	// connection type. Artists with no membership rows are omitted from the
	// result map; the caller treats a missing entry as no presence.
	GetPresenceForArtists(ctx context.Context, artistIDs []string) (map[string]PlatformPresence, error)

	// ListArtistsWithPlatformMappings returns distinct artist IDs that have at
	// least one row in artist_platform_ids. Used by the background artwork
	// reconciler to determine which artists have connected mirrors to check.
	ListArtistsWithPlatformMappings(ctx context.Context) ([]string, error)
}
