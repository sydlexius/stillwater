package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service provides artist and band member data operations.
type Service struct {
	artists     Repository
	providers   ProviderIDRepository
	members     MemberRepository
	aliases     AliasRepository
	platformIDs PlatformIDRepository
}

// NewService creates an artist service.
func NewService(db *sql.DB) *Service {
	return &Service{
		artists:     newSQLiteArtistRepo(db),
		providers:   newSQLiteProviderIDRepo(db),
		members:     newSQLiteMemberRepo(db),
		aliases:     newSQLiteAliasRepo(db),
		platformIDs: newSQLitePlatformIDRepo(db),
	}
}

// Create inserts a new artist.
func (s *Service) Create(ctx context.Context, a *Artist) error {
	return s.artists.Create(ctx, a)
}

// GetByID retrieves an artist by primary key.
func (s *Service) GetByID(ctx context.Context, id string) (*Artist, error) {
	return s.artists.GetByID(ctx, id)
}

// GetByMBID retrieves an artist by MusicBrainz ID.
func (s *Service) GetByMBID(ctx context.Context, mbid string) (*Artist, error) {
	return s.artists.GetByMBID(ctx, mbid)
}

// GetByProviderID retrieves an artist by a provider-specific ID.
// Supported providers: "musicbrainz", "audiodb", "discogs", "wikidata", "deezer", "spotify".
func (s *Service) GetByProviderID(ctx context.Context, provider, id string) (*Artist, error) {
	return s.providers.GetByProviderID(ctx, provider, id)
}

// GetByNameAndLibrary retrieves an artist by name within a specific library.
// Returns nil, nil when no match is found.
func (s *Service) GetByNameAndLibrary(ctx context.Context, name, libraryID string) (*Artist, error) {
	return s.artists.GetByNameAndLibrary(ctx, name, libraryID)
}

// GetByMBIDAndLibrary retrieves an artist by MusicBrainz ID within a specific library.
// Returns nil, nil when no match is found.
func (s *Service) GetByMBIDAndLibrary(ctx context.Context, mbid, libraryID string) (*Artist, error) {
	return s.artists.GetByMBIDAndLibrary(ctx, mbid, libraryID)
}

// GetByPath retrieves an artist by filesystem path.
func (s *Service) GetByPath(ctx context.Context, path string) (*Artist, error) {
	return s.artists.GetByPath(ctx, path)
}

// List returns a paginated list of artists and the total count.
func (s *Service) List(ctx context.Context, params ListParams) ([]Artist, int, error) {
	return s.artists.List(ctx, params)
}

// Update modifies an existing artist.
func (s *Service) Update(ctx context.Context, a *Artist) error {
	return s.artists.Update(ctx, a)
}

// IsEditableField reports whether the given field name can be updated via
// the field-level API.
func IsEditableField(field string) bool {
	_, ok := fieldColumnMap[field]
	return ok
}

// UpdateField updates a single metadata field on an artist record.
// For slice fields (genres, styles, moods), the value is a comma-separated
// string that gets marshaled to a JSON array for storage.
func (s *Service) UpdateField(ctx context.Context, id, field, value string) error {
	return s.artists.UpdateField(ctx, id, field, value)
}

// ClearField sets a single metadata field to its zero value.
func (s *Service) ClearField(ctx context.Context, id, field string) error {
	return s.artists.ClearField(ctx, id, field)
}

// FieldValueFromArtist extracts a single field's value from an Artist struct.
// For string fields returns the value directly; for slice fields returns
// the comma-joined representation.
func FieldValueFromArtist(a *Artist, field string) string {
	switch field {
	case "biography":
		return a.Biography
	case "genres":
		return strings.Join(a.Genres, ", ")
	case "styles":
		return strings.Join(a.Styles, ", ")
	case "moods":
		return strings.Join(a.Moods, ", ")
	case "formed":
		return a.Formed
	case "born":
		return a.Born
	case "disbanded":
		return a.Disbanded
	case "died":
		return a.Died
	case "years_active":
		return a.YearsActive
	case "type":
		return a.Type
	case "gender":
		return a.Gender
	default:
		return ""
	}
}

// SliceFieldFromArtist extracts a slice field's values from an Artist struct.
func SliceFieldFromArtist(a *Artist, field string) []string {
	switch field {
	case "genres":
		return a.Genres
	case "styles":
		return a.Styles
	case "moods":
		return a.Moods
	default:
		return nil
	}
}

// IsSliceField reports whether the given field stores a JSON array.
func IsSliceField(field string) bool {
	return sliceFields[field]
}

// UpdateProviderFetchedAt records when a provider ID fetch was last attempted.
// The provider argument must be one of "audiodb", "discogs", "wikidata", "lastfm".
func (s *Service) UpdateProviderFetchedAt(ctx context.Context, artistID, prov string) error {
	return s.providers.UpdateProviderFetchedAt(ctx, artistID, prov)
}

// Delete removes an artist by ID. Cascade deletes related rows.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.artists.Delete(ctx, id)
}

// Search finds artists by name substring match.
func (s *Service) Search(ctx context.Context, query string) ([]Artist, error) {
	return s.artists.Search(ctx, query)
}

// ListMembersByArtistID returns all band members for an artist, ordered by sort_order.
func (s *Service) ListMembersByArtistID(ctx context.Context, artistID string) ([]BandMember, error) {
	return s.members.ListByArtistID(ctx, artistID)
}

// CreateMember inserts a new band member.
func (s *Service) CreateMember(ctx context.Context, m *BandMember) error {
	return s.members.Create(ctx, m)
}

// DeleteMember removes a band member by ID.
func (s *Service) DeleteMember(ctx context.Context, id string) error {
	return s.members.Delete(ctx, id)
}

// DeleteMembersByArtistID removes all band members for an artist.
func (s *Service) DeleteMembersByArtistID(ctx context.Context, artistID string) error {
	return s.members.DeleteByArtistID(ctx, artistID)
}

// UpsertMembers replaces all band members for an artist with the given list.
func (s *Service) UpsertMembers(ctx context.Context, artistID string, members []BandMember) error {
	return s.members.Upsert(ctx, artistID, members)
}

// AddAlias adds an alias for an artist.
func (s *Service) AddAlias(ctx context.Context, artistID, alias, source string) (*Alias, error) {
	if alias == "" {
		return nil, fmt.Errorf("alias is required")
	}

	// Check artist exists
	_, err := s.artists.GetByID(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("artist not found: %w", err)
	}

	a := &Alias{
		ID:        uuid.New().String(),
		ArtistID:  artistID,
		Alias:     alias,
		Source:    source,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.aliases.Create(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// RemoveAlias removes an alias by ID.
func (s *Service) RemoveAlias(ctx context.Context, aliasID string) error {
	return s.aliases.Delete(ctx, aliasID)
}

// ListAliases returns all aliases for an artist.
func (s *Service) ListAliases(ctx context.Context, artistID string) ([]Alias, error) {
	return s.aliases.ListByArtistID(ctx, artistID)
}

// SearchWithAliases searches artists by name or alias text.
func (s *Service) SearchWithAliases(ctx context.Context, query string) ([]Artist, error) {
	return s.aliases.SearchWithAliases(ctx, query)
}

// FindDuplicates returns groups of artists that appear to be duplicates.
// Detection is based on shared aliases, matching MBIDs, or similar names.
func (s *Service) FindDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	var groups []DuplicateGroup

	mbidGroups, err := s.aliases.FindMBIDDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding MBID duplicates: %w", err)
	}
	groups = append(groups, mbidGroups...)

	aliasGroups, err := s.aliases.FindAliasDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding alias duplicates: %w", err)
	}
	groups = append(groups, aliasGroups...)

	return groups, nil
}

// SetPlatformID stores or updates the platform artist ID for an artist on a connection.
func (s *Service) SetPlatformID(ctx context.Context, artistID, connectionID, platformArtistID string) error {
	if artistID == "" || connectionID == "" || platformArtistID == "" {
		return fmt.Errorf("artist_id, connection_id, and platform_artist_id are required")
	}
	return s.platformIDs.Set(ctx, artistID, connectionID, platformArtistID)
}

// GetPlatformID retrieves the platform artist ID for an artist on a specific connection.
func (s *Service) GetPlatformID(ctx context.Context, artistID, connectionID string) (string, error) {
	return s.platformIDs.Get(ctx, artistID, connectionID)
}

// GetPlatformIDs returns all platform artist IDs for an artist across all connections.
func (s *Service) GetPlatformIDs(ctx context.Context, artistID string) ([]PlatformID, error) {
	return s.platformIDs.GetAll(ctx, artistID)
}

// DeletePlatformID removes the platform artist ID mapping for an artist on a connection.
func (s *Service) DeletePlatformID(ctx context.Context, artistID, connectionID string) error {
	return s.platformIDs.Delete(ctx, artistID, connectionID)
}

// DeletePlatformIDsByArtist removes all platform artist ID mappings for an artist.
func (s *Service) DeletePlatformIDsByArtist(ctx context.Context, artistID string) error {
	return s.platformIDs.DeleteByArtistID(ctx, artistID)
}
