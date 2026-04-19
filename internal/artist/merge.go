package artist

import (
	"slices"
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// MergeStrategy controls how ApplyMetadata merges incoming fields into an Artist.
type MergeStrategy int

const (
	// OverwriteAttempted overwrites fields that were attempted AND populated
	// by providers. Un-attempted fields are untouched. Attempted-but-empty
	// fields are also untouched so a localized lookup with no match preserves
	// pre-existing data (#952 graceful fallback).
	// Type/Gender/YearsActive use non-empty overwrite (never cleared).
	// Provider IDs use fill-empty semantics (never overwrite existing IDs).
	OverwriteAttempted MergeStrategy = iota

	// FillEmpty only sets fields that are currently empty/zero on the artist.
	// Never overwrites existing data.
	FillEmpty

	// NFOImport applies NFO-takes-precedence semantics:
	//   - Identity fields (Name, SortName, MBID, AudioDBID, Biography): non-empty overwrite
	//   - Provider IDs (Discogs, Wikidata, Deezer, Spotify): non-empty overwrite
	//   - Classification fields (Type, Gender, Disambiguation): unconditional
	//   - Lists and dates: unconditional
	NFOImport

	// SnapshotRestore unconditionally sets all fields from the source.
	// Used for NFO snapshot restores where exact state reproduction is required.
	SnapshotRestore
)

// MetadataUpdate holds incoming metadata fields to merge into an Artist.
// Zero values mean "not provided" for FillEmpty and NFOImport strategies.
// For OverwriteAttempted, zero values are not authorized to clear: only
// fields listed in MergeOptions.PopulatedFields are eligible for overwrite,
// and PopulatedFields only contains fields where a provider returned data.
type MetadataUpdate struct {
	Name           string
	SortName       string
	Type           string
	Gender         string
	Disambiguation string
	MusicBrainzID  string
	AudioDBID      string
	DiscogsID      string
	WikidataID     string
	DeezerID       string
	SpotifyID      string
	Biography      string
	Genres         []string
	Styles         []string
	Moods          []string
	YearsActive    string
	Born           string
	Formed         string
	Died           string
	Disbanded      string
}

// MergeOptions provides per-call configuration for ApplyMetadata.
type MergeOptions struct {
	// AttemptedFields lists which fields the provider tried to fetch.
	// Only used by OverwriteAttempted. Un-attempted fields are untouched.
	AttemptedFields []string

	// PopulatedFields lists which fields actually had data returned by at
	// least one provider. Subset of AttemptedFields. Only used by
	// OverwriteAttempted: clear-on-empty semantics for biography, tag lists,
	// and date fields require both attempted AND populated. This is the
	// graceful-fallback contract from #952 -- a localized lookup that returns
	// nothing must not clobber pre-existing values on the artist.
	PopulatedFields []string

	// FilterDatesByType clears semantically inappropriate date fields after
	// merging (e.g., formed/disbanded for solo artists). Typically true for
	// provider-sourced data, false for NFO imports.
	FilterDatesByType bool

	// Sources records which provider supplied each field. When non-nil,
	// populates a.MetadataSources.
	Sources []provider.FieldSource

	// LockedFields lists field names that must not be overwritten regardless
	// of strategy. Compared case-insensitively. Used by the refresh path so a
	// user's pinned values survive provider refetches.
	LockedFields []string
}

// isLocked reports whether the given field name is in locked (case-insensitive).
func isLocked(locked map[string]struct{}, field string) bool {
	if len(locked) == 0 {
		return false
	}
	_, ok := locked[strings.ToLower(field)]
	return ok
}

// buildLockedSet normalizes a locked-fields slice to a lowercase lookup set.
// Blank and whitespace-only tokens are dropped so a slice like []{"", " "}
// produces a nil set rather than one that would match a lookup for "".
// Returns nil when no valid tokens remain so isLocked can short-circuit on
// len==0 without allocating.
func buildLockedSet(fields []string) map[string]struct{} {
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" {
			continue
		}
		out[key] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ApplyMetadata merges incoming metadata into an Artist using the specified
// strategy. Returns true if any field was modified.
func ApplyMetadata(a *Artist, u *MetadataUpdate, strategy MergeStrategy, opts MergeOptions) bool {
	if u == nil {
		return false
	}

	var changed bool
	locked := buildLockedSet(opts.LockedFields)

	switch strategy {
	case OverwriteAttempted:
		changed = applyOverwriteAttempted(a, u, opts.AttemptedFields, opts.PopulatedFields, locked)
	case FillEmpty:
		changed = applyFillEmpty(a, u, locked)
	case NFOImport:
		changed = applyNFOImport(a, u, locked)
	case SnapshotRestore:
		changed = applySnapshotRestore(a, u, locked)
	default:
		// Unknown strategy: no-op. This should not happen in normal use.
		return false
	}

	if opts.FilterDatesByType {
		// Snapshot before the filter so we can detect both genuine changes
		// and any date that was blanked despite a user lock. Locked date
		// fields are restored from the snapshot after filtering so a pinned
		// value survives both the per-field merge skip and the post-merge
		// type filter (e.g. a user who pinned Born on a group type).
		before := [4]string{a.Born, a.Died, a.Formed, a.Disbanded}
		FilterDatesByArtistType(a)
		if isLocked(locked, "born") {
			a.Born = before[0]
		}
		if isLocked(locked, "died") {
			a.Died = before[1]
		}
		if isLocked(locked, "formed") {
			a.Formed = before[2]
		}
		if isLocked(locked, "disbanded") {
			a.Disbanded = before[3]
		}
		if a.Born != before[0] || a.Died != before[1] || a.Formed != before[2] || a.Disbanded != before[3] {
			changed = true
		}
	}

	if len(opts.Sources) > 0 {
		if a.MetadataSources == nil {
			a.MetadataSources = make(map[string]string)
		}
		for _, src := range opts.Sources {
			val := string(src.Provider)
			if a.MetadataSources[src.Field] != val {
				a.MetadataSources[src.Field] = val
				changed = true
			}
		}
	}

	return changed
}

// applyOverwriteAttempted implements the refresh handler merge: overwrite fields
// that were attempted by providers, use non-empty overwrite for type/gender/
// disambiguation/yearsActive, and fill-empty for provider IDs. Name and
// SortName are never touched (handled separately in handler code so the user's
// chosen display name is not overwritten mid-refresh).
//
// Fields with clearing semantics (biography, genres, styles, moods, dates)
// require BOTH attempted AND populated to be authorized for overwrite. An
// attempted-but-not-populated field is left untouched so that an empty
// localized lookup does not clobber pre-existing data (#952 graceful fallback).
func applyOverwriteAttempted(a *Artist, u *MetadataUpdate, attemptedFields, populatedFields []string, locked map[string]struct{}) bool {
	attempted := make(map[string]bool, len(attemptedFields))
	for _, f := range attemptedFields {
		attempted[f] = true
	}
	populated := make(map[string]bool, len(populatedFields))
	for _, f := range populatedFields {
		populated[f] = true
	}

	changed := false

	// Authorize a clearing-semantics overwrite only when both attempted and
	// populated. Attempted-only means "we asked but got nothing" -- preserving
	// the existing value is the graceful-fallback behavior #952 requires.
	canOverwrite := func(field string) bool {
		return attempted[field] && populated[field] && !isLocked(locked, field)
	}

	if canOverwrite("biography") {
		changed = setString(&a.Biography, u.Biography) || changed
	}
	if canOverwrite("genres") {
		changed = setSlice(&a.Genres, u.Genres) || changed
	}
	if canOverwrite("styles") {
		changed = setSlice(&a.Styles, u.Styles) || changed
	}
	if canOverwrite("moods") {
		changed = setSlice(&a.Moods, u.Moods) || changed
	}
	if canOverwrite("formed") {
		changed = setString(&a.Formed, u.Formed) || changed
	}
	if canOverwrite("born") {
		changed = setString(&a.Born, u.Born) || changed
	}
	if canOverwrite("died") {
		changed = setString(&a.Died, u.Died) || changed
	}
	if canOverwrite("disbanded") {
		changed = setString(&a.Disbanded, u.Disbanded) || changed
	}

	// Type, Gender, Disambiguation, YearsActive: non-empty overwrite only
	// (never clear). Disambiguation is populated here so a provider refresh
	// records the value returned by MusicBrainz without waiting for a
	// separate NFO import pass.
	if !isLocked(locked, "type") {
		changed = setNonEmpty(&a.Type, u.Type) || changed
	}
	if !isLocked(locked, "gender") {
		changed = setNonEmpty(&a.Gender, u.Gender) || changed
	}
	if !isLocked(locked, "disambiguation") {
		changed = setNonEmpty(&a.Disambiguation, u.Disambiguation) || changed
	}
	if !isLocked(locked, "years_active") {
		changed = setNonEmpty(&a.YearsActive, u.YearsActive) || changed
	}

	// Provider IDs: fill-empty only.
	if !isLocked(locked, "musicbrainz_id") {
		changed = fillEmpty(&a.MusicBrainzID, u.MusicBrainzID) || changed
	}
	if !isLocked(locked, "audiodb_id") {
		changed = fillEmpty(&a.AudioDBID, u.AudioDBID) || changed
	}
	if !isLocked(locked, "discogs_id") {
		changed = fillEmpty(&a.DiscogsID, u.DiscogsID) || changed
	}
	if !isLocked(locked, "wikidata_id") {
		changed = fillEmpty(&a.WikidataID, u.WikidataID) || changed
	}
	if !isLocked(locked, "deezer_id") {
		changed = fillEmpty(&a.DeezerID, u.DeezerID) || changed
	}
	if !isLocked(locked, "spotify_id") {
		changed = fillEmpty(&a.SpotifyID, u.SpotifyID) || changed
	}

	return changed
}

// applyFillEmpty only sets fields that are currently empty/zero on the artist.
// Name, SortName, and Disambiguation are skipped (same as OverwriteAttempted).
func applyFillEmpty(a *Artist, u *MetadataUpdate, locked map[string]struct{}) bool {
	changed := false

	setStr := func(field string, dst *string, val string) {
		if !isLocked(locked, field) {
			changed = fillEmpty(dst, val) || changed
		}
	}
	setSl := func(field string, dst *[]string, val []string) {
		if !isLocked(locked, field) {
			changed = fillEmptySlice(dst, val) || changed
		}
	}

	setStr("type", &a.Type, u.Type)
	setStr("gender", &a.Gender, u.Gender)
	setStr("musicbrainz_id", &a.MusicBrainzID, u.MusicBrainzID)
	setStr("audiodb_id", &a.AudioDBID, u.AudioDBID)
	setStr("discogs_id", &a.DiscogsID, u.DiscogsID)
	setStr("wikidata_id", &a.WikidataID, u.WikidataID)
	setStr("deezer_id", &a.DeezerID, u.DeezerID)
	setStr("spotify_id", &a.SpotifyID, u.SpotifyID)
	setStr("biography", &a.Biography, u.Biography)
	setSl("genres", &a.Genres, u.Genres)
	setSl("styles", &a.Styles, u.Styles)
	setSl("moods", &a.Moods, u.Moods)
	setStr("years_active", &a.YearsActive, u.YearsActive)
	setStr("born", &a.Born, u.Born)
	setStr("formed", &a.Formed, u.Formed)
	setStr("died", &a.Died, u.Died)
	setStr("disbanded", &a.Disbanded, u.Disbanded)

	return changed
}

// applyNFOImport applies NFO-takes-precedence semantics:
//   - Identity fields (Name, SortName, MBID, AudioDBID, Biography): non-empty overwrite
//   - All provider IDs: non-empty overwrite
//   - Classification (Type, Gender, Disambiguation): unconditional
//   - Lists (Genres, Styles, Moods) and dates: unconditional
func applyNFOImport(a *Artist, u *MetadataUpdate, locked map[string]struct{}) bool {
	changed := false

	nonEmpty := func(field string, dst *string, val string) {
		if !isLocked(locked, field) {
			changed = setNonEmpty(dst, val) || changed
		}
	}
	setStr := func(field string, dst *string, val string) {
		if !isLocked(locked, field) {
			changed = setString(dst, val) || changed
		}
	}
	setSl := func(field string, dst *[]string, val []string) {
		if !isLocked(locked, field) {
			changed = setSlice(dst, val) || changed
		}
	}

	// Identity fields: non-empty overwrite.
	nonEmpty("name", &a.Name, u.Name)
	nonEmpty("sort_name", &a.SortName, u.SortName)
	nonEmpty("musicbrainz_id", &a.MusicBrainzID, u.MusicBrainzID)
	nonEmpty("audiodb_id", &a.AudioDBID, u.AudioDBID)
	nonEmpty("discogs_id", &a.DiscogsID, u.DiscogsID)
	nonEmpty("wikidata_id", &a.WikidataID, u.WikidataID)
	nonEmpty("deezer_id", &a.DeezerID, u.DeezerID)
	nonEmpty("spotify_id", &a.SpotifyID, u.SpotifyID)
	nonEmpty("biography", &a.Biography, u.Biography)

	// Classification: unconditional overwrite.
	setStr("type", &a.Type, u.Type)
	setStr("gender", &a.Gender, u.Gender)
	setStr("disambiguation", &a.Disambiguation, u.Disambiguation)

	// Lists: unconditional overwrite.
	setSl("genres", &a.Genres, u.Genres)
	setSl("styles", &a.Styles, u.Styles)
	setSl("moods", &a.Moods, u.Moods)

	// Dates and years: unconditional overwrite.
	setStr("years_active", &a.YearsActive, u.YearsActive)
	setStr("born", &a.Born, u.Born)
	setStr("formed", &a.Formed, u.Formed)
	setStr("died", &a.Died, u.Died)
	setStr("disbanded", &a.Disbanded, u.Disbanded)

	return changed
}

// applySnapshotRestore unconditionally sets all metadata fields. Locked fields
// are preserved so snapshot restores never clobber user pins.
func applySnapshotRestore(a *Artist, u *MetadataUpdate, locked map[string]struct{}) bool {
	changed := false

	setStr := func(field string, dst *string, val string) {
		if !isLocked(locked, field) {
			changed = setString(dst, val) || changed
		}
	}
	setSl := func(field string, dst *[]string, val []string) {
		if !isLocked(locked, field) {
			changed = setSlice(dst, val) || changed
		}
	}

	setStr("name", &a.Name, u.Name)
	setStr("sort_name", &a.SortName, u.SortName)
	setStr("type", &a.Type, u.Type)
	setStr("gender", &a.Gender, u.Gender)
	setStr("disambiguation", &a.Disambiguation, u.Disambiguation)
	setStr("musicbrainz_id", &a.MusicBrainzID, u.MusicBrainzID)
	setStr("audiodb_id", &a.AudioDBID, u.AudioDBID)
	setStr("discogs_id", &a.DiscogsID, u.DiscogsID)
	setStr("wikidata_id", &a.WikidataID, u.WikidataID)
	setStr("deezer_id", &a.DeezerID, u.DeezerID)
	setStr("spotify_id", &a.SpotifyID, u.SpotifyID)
	setStr("biography", &a.Biography, u.Biography)
	setSl("genres", &a.Genres, u.Genres)
	setSl("styles", &a.Styles, u.Styles)
	setSl("moods", &a.Moods, u.Moods)
	setStr("years_active", &a.YearsActive, u.YearsActive)
	setStr("born", &a.Born, u.Born)
	setStr("formed", &a.Formed, u.Formed)
	setStr("died", &a.Died, u.Died)
	setStr("disbanded", &a.Disbanded, u.Disbanded)

	return changed
}

// IsIndividualType returns true for artist types that represent a single person
// who can have a gender field (solo, person, character). Group-like types
// (group, orchestra, choir) do not carry gender. Callers should check for
// empty type separately before using this to clear gender.
func IsIndividualType(t string) bool {
	switch t {
	case "solo", "person", "character":
		return true
	default:
		return false
	}
}

// FilterDatesByArtistType clears date fields that are semantically wrong for
// the artist's type. Solo/person/character artists should not have
// formed/disbanded; group/orchestra/choir artists should not have born/died.
// Unknown or empty type: no filtering.
func FilterDatesByArtistType(a *Artist) {
	switch a.Type {
	case "solo", "person", "character":
		a.Formed = ""
		a.Disbanded = ""
	case "group", "orchestra", "choir":
		a.Born = ""
		a.Died = ""
	}
}

// FetchResultToUpdate converts a provider FetchResult's metadata into a
// MetadataUpdate suitable for passing to ApplyMetadata. Returns nil if the
// FetchResult has no metadata.
func FetchResultToUpdate(result *provider.FetchResult) *MetadataUpdate {
	if result == nil || result.Metadata == nil {
		return nil
	}
	m := result.Metadata
	gender := m.Gender
	if m.Type != "" && !IsIndividualType(m.Type) {
		gender = ""
	}
	return &MetadataUpdate{
		Name:           m.Name,
		SortName:       m.SortName,
		Type:           m.Type,
		Gender:         gender,
		Disambiguation: m.Disambiguation,
		MusicBrainzID:  m.MusicBrainzID,
		AudioDBID:      m.AudioDBID,
		DiscogsID:      m.DiscogsID,
		WikidataID:     m.WikidataID,
		DeezerID:       m.DeezerID,
		SpotifyID:      m.SpotifyID,
		Biography:      m.Biography,
		Genres:         m.Genres,
		Styles:         m.Styles,
		Moods:          m.Moods,
		YearsActive:    m.YearsActive,
		Born:           m.Born,
		Formed:         m.Formed,
		Died:           m.Died,
		Disbanded:      m.Disbanded,
	}
}

// setString unconditionally sets *dst to val. Returns true if the value changed.
func setString(dst *string, val string) bool {
	if *dst == val {
		return false
	}
	*dst = val
	return true
}

// setNonEmpty sets *dst to val only when val is non-empty. Returns true if changed.
func setNonEmpty(dst *string, val string) bool {
	if val == "" || *dst == val {
		return false
	}
	*dst = val
	return true
}

// fillEmpty sets *dst to val only when *dst is empty and val is non-empty.
func fillEmpty(dst *string, val string) bool {
	if *dst != "" || val == "" {
		return false
	}
	*dst = val
	return true
}

// setSlice unconditionally replaces *dst with val. Returns true if changed.
func setSlice(dst *[]string, val []string) bool {
	if slices.Equal(*dst, val) {
		return false
	}
	*dst = val
	return true
}

// fillEmptySlice sets *dst to val only when *dst is nil/empty and val is non-empty.
func fillEmptySlice(dst *[]string, val []string) bool {
	if len(*dst) > 0 || len(val) == 0 {
		return false
	}
	*dst = val
	return true
}
