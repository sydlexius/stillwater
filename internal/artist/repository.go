package artist

import "context"

// Repository defines core artist CRUD operations.
type Repository interface {
	Create(ctx context.Context, a *Artist) error
	GetByID(ctx context.Context, id string) (*Artist, error)
	GetByMBID(ctx context.Context, mbid string) (*Artist, error)
	GetByMBIDAndLibrary(ctx context.Context, mbid, libraryID string) (*Artist, error)
	GetByNameAndLibrary(ctx context.Context, name, libraryID string) (*Artist, error)
	GetByPath(ctx context.Context, path string) (*Artist, error)
	List(ctx context.Context, params ListParams) ([]Artist, int, error)
	Update(ctx context.Context, a *Artist) error
	UpdateField(ctx context.Context, id, field, value string) error
	ClearField(ctx context.Context, id, field string) error
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, query string) ([]Artist, error)
}

// ProviderIDRepository handles provider-specific ID lookups.
type ProviderIDRepository interface {
	GetByProviderID(ctx context.Context, provider, id string) (*Artist, error)
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

// PlatformIDRepository manages platform ID mappings between Stillwater
// artists and their IDs on external connections (Emby, Jellyfin, Lidarr).
type PlatformIDRepository interface {
	Set(ctx context.Context, artistID, connectionID, platformArtistID string) error
	Get(ctx context.Context, artistID, connectionID string) (string, error)
	GetAll(ctx context.Context, artistID string) ([]PlatformID, error)
	Delete(ctx context.Context, artistID, connectionID string) error
	DeleteByArtistID(ctx context.Context, artistID string) error
}

// ImageRepository is a forward-looking interface for image metadata operations.
// Methods will be added in #374 when image metadata is normalized into its own table.
type ImageRepository interface{}
