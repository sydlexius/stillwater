package artist

import (
	"slices"

	"github.com/sydlexius/stillwater/internal/provider"
)

// MergeStrategy controls how ApplyMetadata merges incoming fields into an Artist.
type MergeStrategy int

const (
	// OverwriteAttempted overwrites fields that were attempted by providers.
	// Un-attempted fields are untouched. Attempted fields can be cleared.
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
// For OverwriteAttempted, zero values in attempted fields clear the field.
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

	// FilterDatesByType clears semantically inappropriate date fields after
	// merging (e.g., formed/disbanded for solo artists). Typically true for
	// provider-sourced data, false for NFO imports.
	FilterDatesByType bool

	// Sources records which provider supplied each field. When non-nil,
	// populates a.MetadataSources.
	Sources []provider.FieldSource
}

// ApplyMetadata merges incoming metadata into an Artist using the specified
// strategy. Returns true if any field was modified.
func ApplyMetadata(a *Artist, u *MetadataUpdate, strategy MergeStrategy, opts MergeOptions) bool {
	if u == nil {
		return false
	}

	var changed bool

	switch strategy {
	case OverwriteAttempted:
		changed = applyOverwriteAttempted(a, u, opts.AttemptedFields)
	case FillEmpty:
		changed = applyFillEmpty(a, u)
	case NFOImport:
		changed = applyNFOImport(a, u)
	case SnapshotRestore:
		changed = applySnapshotRestore(a, u)
	default:
		// Unknown strategy: no-op. This should not happen in normal use.
		return false
	}

	if opts.FilterDatesByType {
		before := [4]string{a.Born, a.Died, a.Formed, a.Disbanded}
		FilterDatesByArtistType(a)
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
// yearsActive, and fill-empty for provider IDs. Name, SortName, and
// Disambiguation are never touched (handled separately in handler code).
func applyOverwriteAttempted(a *Artist, u *MetadataUpdate, attemptedFields []string) bool {
	attempted := make(map[string]bool, len(attemptedFields))
	for _, f := range attemptedFields {
		attempted[f] = true
	}

	changed := false

	// Attempted metadata fields: overwrite (including clearing).
	if attempted["biography"] {
		changed = setString(&a.Biography, u.Biography) || changed
	}
	if attempted["genres"] {
		changed = setSlice(&a.Genres, u.Genres) || changed
	}
	if attempted["styles"] {
		changed = setSlice(&a.Styles, u.Styles) || changed
	}
	if attempted["moods"] {
		changed = setSlice(&a.Moods, u.Moods) || changed
	}
	if attempted["formed"] {
		changed = setString(&a.Formed, u.Formed) || changed
	}
	if attempted["born"] {
		changed = setString(&a.Born, u.Born) || changed
	}
	if attempted["died"] {
		changed = setString(&a.Died, u.Died) || changed
	}
	if attempted["disbanded"] {
		changed = setString(&a.Disbanded, u.Disbanded) || changed
	}

	// Type, Gender, YearsActive: non-empty overwrite only (never clear).
	changed = setNonEmpty(&a.Type, u.Type) || changed
	changed = setNonEmpty(&a.Gender, u.Gender) || changed
	changed = setNonEmpty(&a.YearsActive, u.YearsActive) || changed

	// Provider IDs: fill-empty only.
	changed = fillEmpty(&a.MusicBrainzID, u.MusicBrainzID) || changed
	changed = fillEmpty(&a.AudioDBID, u.AudioDBID) || changed
	changed = fillEmpty(&a.DiscogsID, u.DiscogsID) || changed
	changed = fillEmpty(&a.WikidataID, u.WikidataID) || changed
	changed = fillEmpty(&a.DeezerID, u.DeezerID) || changed
	changed = fillEmpty(&a.SpotifyID, u.SpotifyID) || changed

	return changed
}

// applyFillEmpty only sets fields that are currently empty/zero on the artist.
// Name, SortName, and Disambiguation are skipped (same as OverwriteAttempted).
func applyFillEmpty(a *Artist, u *MetadataUpdate) bool {
	changed := false

	changed = fillEmpty(&a.Type, u.Type) || changed
	changed = fillEmpty(&a.Gender, u.Gender) || changed
	changed = fillEmpty(&a.MusicBrainzID, u.MusicBrainzID) || changed
	changed = fillEmpty(&a.AudioDBID, u.AudioDBID) || changed
	changed = fillEmpty(&a.DiscogsID, u.DiscogsID) || changed
	changed = fillEmpty(&a.WikidataID, u.WikidataID) || changed
	changed = fillEmpty(&a.DeezerID, u.DeezerID) || changed
	changed = fillEmpty(&a.SpotifyID, u.SpotifyID) || changed
	changed = fillEmpty(&a.Biography, u.Biography) || changed
	changed = fillEmptySlice(&a.Genres, u.Genres) || changed
	changed = fillEmptySlice(&a.Styles, u.Styles) || changed
	changed = fillEmptySlice(&a.Moods, u.Moods) || changed
	changed = fillEmpty(&a.YearsActive, u.YearsActive) || changed
	changed = fillEmpty(&a.Born, u.Born) || changed
	changed = fillEmpty(&a.Formed, u.Formed) || changed
	changed = fillEmpty(&a.Died, u.Died) || changed
	changed = fillEmpty(&a.Disbanded, u.Disbanded) || changed

	return changed
}

// applyNFOImport applies NFO-takes-precedence semantics:
//   - Identity fields (Name, SortName, MBID, AudioDBID, Biography): non-empty overwrite
//   - All provider IDs: non-empty overwrite
//   - Classification (Type, Gender, Disambiguation): unconditional
//   - Lists (Genres, Styles, Moods) and dates: unconditional
func applyNFOImport(a *Artist, u *MetadataUpdate) bool {
	changed := false

	// Identity fields: non-empty overwrite (NFO takes precedence when it has a value).
	changed = setNonEmpty(&a.Name, u.Name) || changed
	changed = setNonEmpty(&a.SortName, u.SortName) || changed
	changed = setNonEmpty(&a.MusicBrainzID, u.MusicBrainzID) || changed
	changed = setNonEmpty(&a.AudioDBID, u.AudioDBID) || changed
	changed = setNonEmpty(&a.DiscogsID, u.DiscogsID) || changed
	changed = setNonEmpty(&a.WikidataID, u.WikidataID) || changed
	changed = setNonEmpty(&a.DeezerID, u.DeezerID) || changed
	changed = setNonEmpty(&a.SpotifyID, u.SpotifyID) || changed
	changed = setNonEmpty(&a.Biography, u.Biography) || changed

	// Classification fields: unconditional overwrite.
	changed = setString(&a.Type, u.Type) || changed
	changed = setString(&a.Gender, u.Gender) || changed
	changed = setString(&a.Disambiguation, u.Disambiguation) || changed

	// Lists: unconditional overwrite.
	changed = setSlice(&a.Genres, u.Genres) || changed
	changed = setSlice(&a.Styles, u.Styles) || changed
	changed = setSlice(&a.Moods, u.Moods) || changed

	// Dates and years: unconditional overwrite.
	changed = setString(&a.YearsActive, u.YearsActive) || changed
	changed = setString(&a.Born, u.Born) || changed
	changed = setString(&a.Formed, u.Formed) || changed
	changed = setString(&a.Died, u.Died) || changed
	changed = setString(&a.Disbanded, u.Disbanded) || changed

	return changed
}

// applySnapshotRestore unconditionally sets all metadata fields.
func applySnapshotRestore(a *Artist, u *MetadataUpdate) bool {
	changed := false

	changed = setString(&a.Name, u.Name) || changed
	changed = setString(&a.SortName, u.SortName) || changed
	changed = setString(&a.Type, u.Type) || changed
	changed = setString(&a.Gender, u.Gender) || changed
	changed = setString(&a.Disambiguation, u.Disambiguation) || changed
	changed = setString(&a.MusicBrainzID, u.MusicBrainzID) || changed
	changed = setString(&a.AudioDBID, u.AudioDBID) || changed
	changed = setString(&a.DiscogsID, u.DiscogsID) || changed
	changed = setString(&a.WikidataID, u.WikidataID) || changed
	changed = setString(&a.DeezerID, u.DeezerID) || changed
	changed = setString(&a.SpotifyID, u.SpotifyID) || changed
	changed = setString(&a.Biography, u.Biography) || changed
	changed = setSlice(&a.Genres, u.Genres) || changed
	changed = setSlice(&a.Styles, u.Styles) || changed
	changed = setSlice(&a.Moods, u.Moods) || changed
	changed = setString(&a.YearsActive, u.YearsActive) || changed
	changed = setString(&a.Born, u.Born) || changed
	changed = setString(&a.Formed, u.Formed) || changed
	changed = setString(&a.Died, u.Died) || changed
	changed = setString(&a.Disbanded, u.Disbanded) || changed

	return changed
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
	return &MetadataUpdate{
		Name:           m.Name,
		SortName:       m.SortName,
		Type:           m.Type,
		Gender:         m.Gender,
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
