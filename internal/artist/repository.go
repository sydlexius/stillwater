package artist

import "context"

// Repository defines core artist CRUD operations.
type Repository interface {
	Create(ctx context.Context, a *Artist) error
	GetByID(ctx context.Context, id string) (*Artist, error)
	GetByMBID(ctx context.Context, mbid string) (*Artist, error)
	GetByMBIDAndLibrary(ctx context.Context, mbid, libraryID string) (*Artist, error)
	GetByNameAndLibrary(ctx context.Context, name, libraryID string) (*Artist, error)
	FindByMBIDOrName(ctx context.Context, mbid, name, libraryID string) (*Artist, error)
	GetByPath(ctx context.Context, path string) (*Artist, error)
	List(ctx context.Context, params ListParams) ([]Artist, int, error)
	Update(ctx context.Context, a *Artist) error
	UpdateField(ctx context.Context, id, field, value string) error
	ClearField(ctx context.Context, id, field string) error
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, query string) ([]Artist, error)
	SetLock(ctx context.Context, id string, locked bool, source string) error

	// ListPathsByLibrary returns a map of artist ID to filesystem path for
	// all artists in the given library that have a non-empty path.
	ListPathsByLibrary(ctx context.Context, libraryID string) (map[string]string, error)

	// UpdateHealthScore sets only the health_score column for the given artist,
	// avoiding a full row overwrite that could clobber concurrent mutations.
	UpdateHealthScore(ctx context.Context, id string, score float64) error

	// HealthStats returns aggregate health metrics for non-excluded artists.
	// When libraryID is non-empty, only artists in that library are included.
	HealthStats(ctx context.Context, libraryID string) (HealthStatsResult, error)

	// ListUnevaluatedIDs returns IDs of non-excluded artists that have never been evaluated
	// (health_evaluated_at IS NULL).
	ListUnevaluatedIDs(ctx context.Context) ([]string, error)
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
}
