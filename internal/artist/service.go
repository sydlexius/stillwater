package artist

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// ctxKey is the type for context value keys used by the artist package.
type ctxKey string

// sourceKey carries the history source through context so callers (e.g.
// refresh handlers) can tag changes with "provider:musicbrainz" etc. without
// changing method signatures.
const sourceKey ctxKey = "history_source"

// ContextWithSource returns a child context that carries the given history
// source string. When the Service records metadata changes, it reads this
// value to populate the MetadataChange.Source field.
func ContextWithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey, source)
}

// sourceFromContext extracts the history source from ctx, defaulting to
// "manual" when no source has been set.
func sourceFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sourceKey).(string); ok && v != "" {
		return v
	}
	return "manual"
}

// trackableFields lists the metadata fields that are tracked by the history
// system when Update() is called. These correspond to the editable fields
// exposed by the field-level API.
var trackableFields = []string{
	"biography", "genres", "styles", "moods",
	"formed", "born", "disbanded", "died",
	"years_active", "type", "gender",
}

// Service provides artist and band member data operations.
type Service struct {
	artists      Repository
	providers    ProviderIDRepository
	members      MemberRepository
	aliases      AliasRepository
	images       ImageRepository
	platformIDs  PlatformIDRepository
	completeness CompletenessRepository
	history      *HistoryService
	mbSnapshots  MBSnapshotRepository
}

// SetHistoryService attaches a HistoryService to the artist Service so that
// metadata mutations automatically record change history. This is a setter
// rather than a constructor parameter to avoid breaking existing NewService
// and NewServiceWithRepos call sites.
//
// Must be called before the HTTP server starts accepting requests.
func (s *Service) SetHistoryService(h *HistoryService) {
	s.history = h
}

// SetMBSnapshotRepository attaches a MBSnapshotRepository to the artist Service
// for tracking last-known MusicBrainz field values. This is a setter rather than
// a constructor parameter to avoid breaking existing call sites.
func (s *Service) SetMBSnapshotRepository(repo MBSnapshotRepository) {
	s.mbSnapshots = repo
}

// UpsertMBSnapshots stores or updates MusicBrainz value snapshots for the given artist.
// No-op if no MBSnapshotRepository has been configured.
func (s *Service) UpsertMBSnapshots(ctx context.Context, artistID string, snapshots []MBSnapshot) error {
	if s.mbSnapshots == nil {
		return nil
	}
	return s.mbSnapshots.UpsertAll(ctx, artistID, snapshots)
}

// GetMBSnapshots returns the last-known MusicBrainz values for the given artist,
// keyed by field name. Returns an empty map if no snapshots exist or no repository
// is configured.
func (s *Service) GetMBSnapshots(ctx context.Context, artistID string) (map[string]MBSnapshot, error) {
	if s.mbSnapshots == nil {
		return make(map[string]MBSnapshot), nil
	}
	return s.mbSnapshots.GetForArtist(ctx, artistID)
}

// NewService creates an artist service backed by SQLite.
func NewService(db *sql.DB) *Service {
	return &Service{
		artists:      newSQLiteArtistRepo(db),
		providers:    newSQLiteProviderIDRepo(db),
		members:      newSQLiteMemberRepo(db),
		aliases:      newSQLiteAliasRepo(db),
		images:       newSQLiteImageRepo(db),
		platformIDs:  newSQLitePlatformIDRepo(db),
		completeness: newSQLiteCompletenessRepo(db),
		mbSnapshots:  newSQLiteMBSnapshotRepo(db),
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
	completeness CompletenessRepository,
) *Service {
	return &Service{
		artists:      artists,
		providers:    providers,
		members:      members,
		aliases:      aliases,
		images:       images,
		platformIDs:  platformIDs,
		completeness: completeness,
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

// GetHealthStats returns aggregate health metrics for non-excluded artists.
// When libraryID is non-empty, only artists in that library are included.
func (s *Service) GetHealthStats(ctx context.Context, libraryID string) (HealthStatsResult, error) {
	return s.artists.HealthStats(ctx, libraryID)
}

// UpdateHealthScore sets only the health_score column for the given artist,
// avoiding a full row overwrite that could clobber concurrent mutations.
func (s *Service) UpdateHealthScore(ctx context.Context, id string, score float64) error {
	return s.artists.UpdateHealthScore(ctx, id, score)
}

// ListUnevaluatedIDs returns IDs of non-excluded artists that have never been evaluated
// (health_evaluated_at IS NULL), used by the bootstrap process to identify artists
// needing initial health score calculation.
func (s *Service) ListUnevaluatedIDs(ctx context.Context) ([]string, error) {
	return s.artists.ListUnevaluatedIDs(ctx)
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

// FindByMBIDOrName finds an artist by MBID first, then falls back to
// case-insensitive name match, both scoped to the given library.
// Returns nil, nil when no match is found.
func (s *Service) FindByMBIDOrName(ctx context.Context, mbid, name, libraryID string) (*Artist, error) {
	a, err := s.artists.FindByMBIDOrName(ctx, mbid, name, libraryID)
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
//
// When a HistoryService is attached, Update diffs every trackable field
// between the old and new artist values and records a MetadataChange for
// each field that changed. History recording is best-effort: failures are
// logged but do not cause the update to fail.
func (s *Service) Update(ctx context.Context, a *Artist) error {
	// Snapshot the old state before writing, so we can diff after the update.
	var old *Artist
	if s.history != nil {
		var err error
		old, err = s.artists.GetByID(ctx, a.ID)
		if err != nil {
			// Artist may not exist yet (first insert via Update), or DB error.
			// Either way, skip history recording rather than blocking the update.
			slog.Warn("history: could not fetch old artist for diff",
				"artist_id", a.ID, "error", err)
			old = nil
		}
	}

	if err := s.artists.Update(ctx, a); err != nil {
		return err
	}

	// Record field-level changes after the successful update.
	if s.history != nil && old != nil {
		source := sourceFromContext(ctx)
		for _, field := range trackableFields {
			oldVal := FieldValueFromArtist(old, field)
			newVal := FieldValueFromArtist(a, field)
			if oldVal != newVal {
				if err := s.history.Record(ctx, a.ID, field, oldVal, newVal, source); err != nil {
					slog.Warn("history: failed to record change",
						"artist_id", a.ID, "field", field, "error", err)
				}
			}
		}
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

// UpdateImageProvenance updates the provenance-related fields (phash, source,
// file_format, last_written_at) on an existing image row without touching display
// fields like exists_flag, low_res, or placeholder. This is called after an image
// save to record evidence of what was written and when.
func (s *Service) UpdateImageProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, source, fileFormat, lastWrittenAt string) error {
	return s.images.UpdateProvenance(ctx, artistID, imageType, slotIndex, phash, source, fileFormat, lastWrittenAt)
}

// ClearImageFlag sets the exists flag to false for a single image slot.
// This is called when a request to serve an image discovers the file is
// missing on disk, clearing the stale flag so subsequent UI renders show a
// placeholder instead of a broken image tag.
func (s *Service) ClearImageFlag(ctx context.Context, artistID, imageType string, slotIndex int) error {
	return s.images.ClearExistsFlag(ctx, artistID, imageType, slotIndex)
}

// IsEditableField reports whether the given field name can be updated via
// the field-level API. This includes both direct-column fields (in
// fieldColumnMap) and provider-ID fields (in providerFieldMap).
func IsEditableField(field string) bool {
	if _, ok := fieldColumnMap[field]; ok {
		return true
	}
	_, ok := providerFieldMap[field]
	return ok
}

// IsProviderIDField reports whether the given field name maps to the
// artist_provider_ids normalized table rather than a direct artists column.
func IsProviderIDField(field string) bool {
	_, ok := providerFieldMap[field]
	return ok
}

// UpdateField updates a single metadata field on an artist record.
// For slice fields (genres, styles, moods), the value is a comma-separated
// string that gets marshaled to a JSON array for storage.
//
// When a HistoryService is attached, the old value is read before the update
// and a MetadataChange is recorded if the value changed. History recording is
// best-effort and will not cause the update to fail.
func (s *Service) UpdateField(ctx context.Context, id, field, value string) error {
	// Capture old value before the mutation so we can record the diff.
	// Track whether the read succeeded so we don't fabricate history from
	// an unknown old state if GetByID fails transiently.
	var oldValue string
	oldKnown := false
	if s.history != nil {
		a, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist for UpdateField diff",
				"artist_id", id, "field", field, "error", err)
		} else {
			oldValue = FieldValueFromArtist(a, field)
			oldKnown = true
		}
	}

	if err := s.artists.UpdateField(ctx, id, field, value); err != nil {
		return err
	}

	// Record the change by comparing normalized representations. Re-fetch
	// after the mutation so both old and new use FieldValueFromArtist, avoiding
	// format mismatches for slice fields (e.g. "Rock, Alternative" vs "Rock,Alternative").
	if s.history != nil && oldKnown {
		newA, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist after UpdateField",
				"artist_id", id, "field", field, "error", err)
		} else {
			newValue := FieldValueFromArtist(newA, field)
			if oldValue != newValue {
				source := sourceFromContext(ctx)
				if err := s.history.Record(ctx, id, field, oldValue, newValue, source); err != nil {
					slog.Warn("history: failed to record UpdateField change",
						"artist_id", id, "field", field, "error", err)
				}
			}
		}
	}

	return nil
}

// ClearField sets a single metadata field to its zero value.
//
// When a HistoryService is attached, the old value is read before clearing
// and a MetadataChange is recorded if the field was non-empty. History
// recording is best-effort and will not cause the clear to fail.
func (s *Service) ClearField(ctx context.Context, id, field string) error {
	// Capture old value before clearing so we can record the diff.
	var oldValue string
	if s.history != nil {
		a, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist for ClearField diff",
				"artist_id", id, "field", field, "error", err)
		} else {
			oldValue = FieldValueFromArtist(a, field)
		}
	}

	if err := s.artists.ClearField(ctx, id, field); err != nil {
		return err
	}

	// Record the change only if the field was non-empty before clearing.
	// Use FieldValueFromArtist on the post-clear state for consistent representation.
	if s.history != nil && oldValue != "" {
		newA, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist after ClearField",
				"artist_id", id, "field", field, "error", err)
		} else {
			newValue := FieldValueFromArtist(newA, field)
			if oldValue != newValue {
				source := sourceFromContext(ctx)
				if err := s.history.Record(ctx, id, field, oldValue, newValue, source); err != nil {
					slog.Warn("history: failed to record ClearField change",
						"artist_id", id, "field", field, "error", err)
				}
			}
		}
	}

	return nil
}

// UpdateProviderField sets a single provider ID field (musicbrainz_id,
// audiodb_id, discogs_id, wikidata_id, or deezer_id) on the artist.
// It re-fetches the artist, applies the field update, and calls Update so
// that all provider IDs in the normalized table are written consistently.
func (s *Service) UpdateProviderField(ctx context.Context, id, field, value string) error {
	providerName, ok := providerFieldMap[field]
	if !ok {
		return fmt.Errorf("unknown provider field: %s", field)
	}

	a, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}

	// Apply the field update to the in-memory struct.
	applyProviderFieldToArtist(a, providerName, value)

	return s.Update(ctx, a)
}

// ClearProviderField removes a single provider ID field by setting it to
// empty string. It re-fetches the artist and calls Update so that the
// normalized artist_provider_ids table is kept consistent.
func (s *Service) ClearProviderField(ctx context.Context, id, field string) error {
	return s.UpdateProviderField(ctx, id, field, "")
}

// applyProviderFieldToArtist applies a single provider ID value to the
// appropriate field of the Artist struct. The providerName argument must be
// the canonical provider key (e.g. "musicbrainz", "audiodb"), not the API
// field name.
func applyProviderFieldToArtist(a *Artist, providerName, value string) {
	switch providerName {
	case "musicbrainz":
		a.MusicBrainzID = value
	case "audiodb":
		a.AudioDBID = value
	case "discogs":
		a.DiscogsID = value
	case "wikidata":
		a.WikidataID = value
	case "deezer":
		a.DeezerID = value
	}
}

// ValidateFieldUpdate returns a non-nil error when the field value is
// invalid. Validation rules:
//   - "name" must not be empty or whitespace-only.
//   - "musicbrainz_id" must be a valid UUID (or empty, which clears the ID).
//
// All other fields are accepted as-is (free-form text).
func ValidateFieldUpdate(field, value string) error {
	switch field {
	case "name":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("name cannot be empty")
		}
	case "musicbrainz_id":
		if value != "" && !isValidMBID(value) {
			return fmt.Errorf("invalid MusicBrainz ID format (expected UUID)")
		}
	}
	return nil
}

// isValidMBID reports whether s is a syntactically valid UUID, as used by
// MusicBrainz for all entity identifiers.
func isValidMBID(s string) bool {
	// A MusicBrainz UUID has the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.
	// We validate the format by checking length and character set rather than
	// importing an external UUID package, keeping this pure-stdlib.
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
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
	case "name":
		return a.Name
	case "sort_name":
		return a.SortName
	case "disambiguation":
		return a.Disambiguation
	case "musicbrainz_id":
		return a.MusicBrainzID
	case "audiodb_id":
		return a.AudioDBID
	case "discogs_id":
		return a.DiscogsID
	case "wikidata_id":
		return a.WikidataID
	case "deezer_id":
		return a.DeezerID
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
// The provider argument must be a known provider name (e.g. "fanarttv", "audiodb").
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

// validLockSources enumerates the allowed values for lock_source.
var validLockSources = map[string]bool{
	"user":     true,
	"imported": true,
}

// Lock sets the metadata lock on an artist with the given source ("user" or "imported").
// When locked, automated operations (rule fixers, metadata fetchers, image operations)
// skip the artist. Manual edits remain allowed.
func (s *Service) Lock(ctx context.Context, id, source string) error {
	if !validLockSources[source] {
		return fmt.Errorf("invalid lock source %q: must be \"user\" or \"imported\"", source)
	}
	return s.artists.SetLock(ctx, id, true, source)
}

// Unlock removes the metadata lock from an artist.
func (s *Service) Unlock(ctx context.Context, id string) error {
	return s.artists.SetLock(ctx, id, false, "")
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

// GetPlatformPresenceForArtists returns platform presence (Emby, Jellyfin,
// Lidarr) for a batch of artists. Artists with no platform ID mappings are
// omitted from the result map.
func (s *Service) GetPlatformPresenceForArtists(ctx context.Context, artistIDs []string) (map[string]PlatformPresence, error) {
	return s.platformIDs.GetPresenceForArtists(ctx, artistIDs)
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

// GetImagesForArtist returns the raw ArtistImage rows for a given artist.
// This is useful when callers need direct access to image metadata fields
// (phash, source, last_written_at) rather than the summarized flags on Artist.
func (s *Service) GetImagesForArtist(ctx context.Context, artistID string) ([]ArtistImage, error) {
	if artistID == "" {
		return nil, fmt.Errorf("artist ID is required")
	}
	return s.images.GetForArtist(ctx, artistID)
}

// NewestWriteTimesByArtistForLibrary returns a map of artist_id to their most
// recent last_written_at timestamp string for all artists in the given library.
// Only artists with at least one recorded write are included.
func (s *Service) NewestWriteTimesByArtistForLibrary(ctx context.Context, libraryID string) (map[string]string, error) {
	return s.images.NewestWriteTimesByArtist(ctx, libraryID)
}

// ListPathsByLibrary returns a map of artist ID to filesystem path for all
// artists in the given library that have a non-empty path.
func (s *Service) ListPathsByLibrary(ctx context.Context, libraryID string) (map[string]string, error) {
	return s.artists.ListPathsByLibrary(ctx, libraryID)
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
			a.ThumbWidth = img.Width
			a.ThumbHeight = img.Height
		case "fanart":
			if img.SlotIndex == 0 {
				a.FanartExists = img.Exists
				a.FanartLowRes = img.LowRes
				a.FanartPlaceholder = img.Placeholder
				a.FanartWidth = img.Width
				a.FanartHeight = img.Height
			}
			if img.Exists {
				fanartCount++
			}
		case "logo":
			a.LogoExists = img.Exists
			a.LogoLowRes = img.LowRes
			a.LogoPlaceholder = img.Placeholder
			a.LogoWidth = img.Width
			a.LogoHeight = img.Height
		case "banner":
			a.BannerExists = img.Exists
			a.BannerLowRes = img.LowRes
			a.BannerPlaceholder = img.Placeholder
			a.BannerWidth = img.Width
			a.BannerHeight = img.Height
		}
	}
	a.FanartCount = fanartCount
}

// extractImageMetadata builds an ArtistImage slice from the Artist struct's
// image fields, ready for persistence. Provenance fields (phash, source,
// file_format, last_written_at) are populated separately via
// UpdateImageProvenance after the image file is saved to disk.
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
			Width:       a.ThumbWidth,
			Height:      a.ThumbHeight,
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
			Width:       a.FanartWidth,
			Height:      a.FanartHeight,
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
			Width:       a.LogoWidth,
			Height:      a.LogoHeight,
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
			Width:       a.BannerWidth,
			Height:      a.BannerHeight,
		})
	}

	return imgs
}
