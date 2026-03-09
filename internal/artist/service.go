package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Service provides artist and band member data operations.
type Service struct {
	artists     Repository
	providers   ProviderIDRepository
	members     MemberRepository
	aliases     AliasRepository
	images      ImageRepository
	platformIDs PlatformIDRepository
}

// NewService creates an artist service backed by SQLite.
func NewService(db *sql.DB) *Service {
	return &Service{
		artists:     newSQLiteArtistRepo(db),
		providers:   newSQLiteProviderIDRepo(db),
		members:     newSQLiteMemberRepo(db),
		aliases:     newSQLiteAliasRepo(db),
		images:      newSQLiteImageRepo(db),
		platformIDs: newSQLitePlatformIDRepo(db),
	}
}

// NewServiceWithRepos creates an artist service using the provided repository
// implementations, enabling dependency injection for tests and alternative backends.
func NewServiceWithRepos(
	artists Repository,
	providers ProviderIDRepository,
	members MemberRepository,
	aliases AliasRepository,
	images ImageRepository,
	platformIDs PlatformIDRepository,
) *Service {
	return &Service{
		artists:     artists,
		providers:   providers,
		members:     members,
		aliases:     aliases,
		images:      images,
		platformIDs: platformIDs,
	}
}

// Create inserts a new artist and persists its provider IDs and image metadata.
// If normalized data persistence fails, the artist row is deleted as a
// best-effort rollback (CASCADE handles child tables).
func (s *Service) Create(ctx context.Context, a *Artist) error {
	if err := s.artists.Create(ctx, a); err != nil {
		return err
	}
	if err := s.persistNormalized(ctx, a); err != nil {
		_ = s.artists.Delete(ctx, a.ID) // best-effort rollback
		return err
	}
	return nil
}

// GetByID retrieves an artist by primary key, including provider IDs and image metadata.
func (s *Service) GetByID(ctx context.Context, id string) (*Artist, error) {
	a, err := s.artists.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateProviderIDs(ctx, a); err != nil {
		return nil, err
	}
	if err := s.hydrateImages(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByMBID retrieves an artist by MusicBrainz ID, including provider IDs and image metadata.
func (s *Service) GetByMBID(ctx context.Context, mbid string) (*Artist, error) {
	a, err := s.artists.GetByMBID(ctx, mbid)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.hydrateProviderIDs(ctx, a); err != nil {
		return nil, err
	}
	if err := s.hydrateImages(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByProviderID retrieves an artist by a provider-specific ID, including all provider IDs
// and image metadata.
// Supported providers: "musicbrainz", "audiodb", "discogs", "wikidata", "deezer", "spotify".
func (s *Service) GetByProviderID(ctx context.Context, provider, id string) (*Artist, error) {
	a, err := s.providers.GetByProviderID(ctx, provider, id)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.hydrateProviderIDs(ctx, a); err != nil {
		return nil, err
	}
	if err := s.hydrateImages(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByNameAndLibrary retrieves an artist by name within a specific library.
// Returns nil, nil when no match is found.
func (s *Service) GetByNameAndLibrary(ctx context.Context, name, libraryID string) (*Artist, error) {
	a, err := s.artists.GetByNameAndLibrary(ctx, name, libraryID)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.hydrateProviderIDs(ctx, a); err != nil {
		return nil, err
	}
	if err := s.hydrateImages(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByMBIDAndLibrary retrieves an artist by MusicBrainz ID within a specific library.
// Returns nil, nil when no match is found.
func (s *Service) GetByMBIDAndLibrary(ctx context.Context, mbid, libraryID string) (*Artist, error) {
	a, err := s.artists.GetByMBIDAndLibrary(ctx, mbid, libraryID)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.hydrateProviderIDs(ctx, a); err != nil {
		return nil, err
	}
	if err := s.hydrateImages(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByPath retrieves an artist by filesystem path, including provider IDs and image metadata.
func (s *Service) GetByPath(ctx context.Context, path string) (*Artist, error) {
	a, err := s.artists.GetByPath(ctx, path)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.hydrateProviderIDs(ctx, a); err != nil {
		return nil, err
	}
	if err := s.hydrateImages(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// List returns a paginated list of artists and the total count, with provider IDs
// and image metadata hydrated.
func (s *Service) List(ctx context.Context, params ListParams) ([]Artist, int, error) {
	artists, total, err := s.artists.List(ctx, params)
	if err != nil {
		return nil, 0, err
	}
	if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
		return nil, 0, err
	}
	if err := s.hydrateImagesBatch(ctx, artists); err != nil {
		return nil, 0, err
	}
	return artists, total, nil
}

// Update modifies an existing artist and persists its provider IDs and image metadata.
// Note: if provider or image persistence fails after the artist row is updated,
// we cannot rollback the artist update (the old data is already overwritten).
// In practice this is acceptable because the normalized tables use
// delete-then-insert which is idempotent, and the next Update call will retry.
func (s *Service) Update(ctx context.Context, a *Artist) error {
	if err := s.artists.Update(ctx, a); err != nil {
		return err
	}
	return s.persistNormalized(ctx, a)
}

// persistNormalized writes provider IDs and image metadata to the normalized
// tables for the given artist.
func (s *Service) persistNormalized(ctx context.Context, a *Artist) error {
	if err := s.providers.UpsertAll(ctx, a.ID, extractProviderIDs(a)); err != nil {
		return err
	}
	return s.images.UpsertAll(ctx, a.ID, extractImageMetadata(a))
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

// Search finds artists by name substring match, with provider IDs and image metadata hydrated.
func (s *Service) Search(ctx context.Context, query string) ([]Artist, error) {
	artists, err := s.artists.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
		return nil, err
	}
	if err := s.hydrateImagesBatch(ctx, artists); err != nil {
		return nil, err
	}
	return artists, nil
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
		return nil, err
	}

	a := &Alias{
		ArtistID: artistID,
		Alias:    alias,
		Source:   source,
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

// SearchWithAliases searches artists by name or alias text, with provider IDs
// and image metadata hydrated.
func (s *Service) SearchWithAliases(ctx context.Context, query string) ([]Artist, error) {
	artists, err := s.aliases.SearchWithAliases(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
		return nil, err
	}
	if err := s.hydrateImagesBatch(ctx, artists); err != nil {
		return nil, err
	}
	return artists, nil
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

	if err := s.hydrateDuplicateGroups(ctx, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// hydrateDuplicateGroups loads provider IDs and image metadata for all artists
// across all duplicate groups in two bulk queries (one for providers, one for images),
// then distributes the results back to each group's artists.
func (s *Service) hydrateDuplicateGroups(ctx context.Context, groups []DuplicateGroup) error {
	// Collect all unique artist IDs across every group.
	seen := make(map[string]struct{})
	for _, g := range groups {
		for _, a := range g.Artists {
			seen[a.ID] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	allIDs := make([]string, 0, len(seen))
	for id := range seen {
		allIDs = append(allIDs, id)
	}

	provMap, err := s.providers.GetForArtists(ctx, allIDs)
	if err != nil {
		return fmt.Errorf("bulk hydrating provider IDs for duplicates: %w", err)
	}
	imgMap, err := s.images.GetForArtists(ctx, allIDs)
	if err != nil {
		return fmt.Errorf("bulk hydrating images for duplicates: %w", err)
	}

	for gi := range groups {
		for ai := range groups[gi].Artists {
			a := &groups[gi].Artists[ai]
			applyProviderIDs(a, provMap[a.ID])
			applyImageMetadata(a, imgMap[a.ID])
		}
	}
	return nil
}

// SetPlatformID stores or updates the platform artist ID for an artist on a connection.
func (s *Service) SetPlatformID(ctx context.Context, artistID, connectionID, platformArtistID string) error {
	if artistID == "" || connectionID == "" || platformArtistID == "" {
		return fmt.Errorf("artist_id, connection_id, and platform_artist_id are required")
	}
	return s.platformIDs.Set(ctx, artistID, connectionID, platformArtistID)
}

// GetPlatformID retrieves the platform artist ID for an artist on a specific connection.
// If no mapping exists, it returns an empty string and a nil error.
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

// hydrateProviderIDs loads provider IDs from the normalized table and applies
// them to the Artist struct fields.
func (s *Service) hydrateProviderIDs(ctx context.Context, a *Artist) error {
	ids, err := s.providers.GetForArtist(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("hydrating provider IDs: %w", err)
	}
	applyProviderIDs(a, ids)
	return nil
}

// hydrateProviderIDsBatch loads provider IDs for multiple artists in a single query.
func (s *Service) hydrateProviderIDsBatch(ctx context.Context, artists []Artist) error {
	if len(artists) == 0 {
		return nil
	}
	ids := make([]string, len(artists))
	for i := range artists {
		ids[i] = artists[i].ID
	}
	idMap, err := s.providers.GetForArtists(ctx, ids)
	if err != nil {
		return fmt.Errorf("batch hydrating provider IDs: %w", err)
	}
	for i := range artists {
		applyProviderIDs(&artists[i], idMap[artists[i].ID])
	}
	return nil
}

// applyProviderIDs sets the Artist struct's provider ID fields from the
// normalized ProviderID slice.
func applyProviderIDs(a *Artist, ids []ProviderID) {
	for _, p := range ids {
		switch p.Provider {
		case "musicbrainz":
			a.MusicBrainzID = p.ProviderID
		case "audiodb":
			a.AudioDBID = p.ProviderID
			a.AudioDBIDFetchedAt = p.FetchedAt
		case "discogs":
			a.DiscogsID = p.ProviderID
			a.DiscogsIDFetchedAt = p.FetchedAt
		case "wikidata":
			a.WikidataID = p.ProviderID
			a.WikidataIDFetchedAt = p.FetchedAt
		case "deezer":
			a.DeezerID = p.ProviderID
		case "spotify":
			a.SpotifyID = p.ProviderID
		case "lastfm":
			a.LastFMFetchedAt = p.FetchedAt
		}
	}
}

// extractProviderIDs builds a ProviderID slice from the Artist struct's
// provider ID fields, ready for persistence.
func extractProviderIDs(a *Artist) []ProviderID {
	var ids []ProviderID

	if a.MusicBrainzID != "" {
		ids = append(ids, ProviderID{Provider: "musicbrainz", ProviderID: a.MusicBrainzID})
	}
	if a.AudioDBID != "" || a.AudioDBIDFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "audiodb", ProviderID: a.AudioDBID, FetchedAt: a.AudioDBIDFetchedAt})
	}
	if a.DiscogsID != "" || a.DiscogsIDFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "discogs", ProviderID: a.DiscogsID, FetchedAt: a.DiscogsIDFetchedAt})
	}
	if a.WikidataID != "" || a.WikidataIDFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "wikidata", ProviderID: a.WikidataID, FetchedAt: a.WikidataIDFetchedAt})
	}
	if a.DeezerID != "" {
		ids = append(ids, ProviderID{Provider: "deezer", ProviderID: a.DeezerID})
	}
	if a.SpotifyID != "" {
		ids = append(ids, ProviderID{Provider: "spotify", ProviderID: a.SpotifyID})
	}
	if a.LastFMFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "lastfm", ProviderID: "", FetchedAt: a.LastFMFetchedAt})
	}

	return ids
}

// hydrateImages loads image metadata from the normalized table and applies
// it to the Artist struct fields.
func (s *Service) hydrateImages(ctx context.Context, a *Artist) error {
	imgs, err := s.images.GetForArtist(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("hydrating images: %w", err)
	}
	applyImageMetadata(a, imgs)
	return nil
}

// hydrateImagesBatch loads image metadata for multiple artists in a single query.
func (s *Service) hydrateImagesBatch(ctx context.Context, artists []Artist) error {
	if len(artists) == 0 {
		return nil
	}
	ids := make([]string, len(artists))
	for i := range artists {
		ids[i] = artists[i].ID
	}
	imgMap, err := s.images.GetForArtists(ctx, ids)
	if err != nil {
		return fmt.Errorf("batch hydrating images: %w", err)
	}
	for i := range artists {
		applyImageMetadata(&artists[i], imgMap[artists[i].ID])
	}
	return nil
}

// applyImageMetadata sets the Artist struct's image fields from the
// normalized ArtistImage slice.
func applyImageMetadata(a *Artist, imgs []ArtistImage) {
	fanartCount := 0
	for _, img := range imgs {
		switch img.ImageType {
		case "thumb":
			a.ThumbExists = img.Exists
			a.ThumbLowRes = img.LowRes
			a.ThumbPlaceholder = img.Placeholder
		case "fanart":
			if img.SlotIndex == 0 {
				a.FanartExists = img.Exists
				a.FanartLowRes = img.LowRes
				a.FanartPlaceholder = img.Placeholder
			}
			if img.Exists {
				fanartCount++
			}
		case "logo":
			a.LogoExists = img.Exists
			a.LogoLowRes = img.LowRes
			a.LogoPlaceholder = img.Placeholder
		case "banner":
			a.BannerExists = img.Exists
			a.BannerLowRes = img.LowRes
			a.BannerPlaceholder = img.Placeholder
		}
	}
	a.FanartCount = fanartCount
}

// extractImageMetadata builds an ArtistImage slice from the Artist struct's
// image fields, ready for persistence.
func extractImageMetadata(a *Artist) []ArtistImage {
	var imgs []ArtistImage

	if a.ThumbExists || a.ThumbLowRes || a.ThumbPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "thumb",
			SlotIndex:   0,
			Exists:      a.ThumbExists,
			LowRes:      a.ThumbLowRes,
			Placeholder: a.ThumbPlaceholder,
		})
	}
	if a.FanartExists || a.FanartLowRes || a.FanartPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "fanart",
			SlotIndex:   0,
			Exists:      a.FanartExists,
			LowRes:      a.FanartLowRes,
			Placeholder: a.FanartPlaceholder,
		})
		// Persist additional fanart slots so FanartCount round-trips through the DB.
		// Slots 1..FanartCount-1 only track existence; per-slot metadata (dimensions,
		// placeholder) is not maintained for non-primary fanart files.
		if a.FanartExists {
			for i := 1; i < a.FanartCount; i++ {
				imgs = append(imgs, ArtistImage{
					ArtistID:  a.ID,
					ImageType: "fanart",
					SlotIndex: i,
					Exists:    true,
				})
			}
		}
	}
	if a.LogoExists || a.LogoLowRes || a.LogoPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "logo",
			SlotIndex:   0,
			Exists:      a.LogoExists,
			LowRes:      a.LogoLowRes,
			Placeholder: a.LogoPlaceholder,
		})
	}
	if a.BannerExists || a.BannerLowRes || a.BannerPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "banner",
			SlotIndex:   0,
			Exists:      a.BannerExists,
			LowRes:      a.BannerLowRes,
			Placeholder: a.BannerPlaceholder,
		})
	}

	return imgs
}
