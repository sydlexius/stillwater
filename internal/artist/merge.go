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
	Origin         string
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

// fieldMode describes the merge operation applied to a single field.
type fieldMode int

const (
	// modeSkip means the field is never touched by this strategy.
	modeSkip fieldMode = iota
	// modeUnconditional overwrites dst with val regardless of current or incoming value.
	modeUnconditional
	// modeNonEmpty overwrites dst only when val is non-empty.
	modeNonEmpty
	// modeFillEmpty sets dst only when dst is currently empty and val is non-empty.
	modeFillEmpty
	// modeAttemptedPopulated sets dst only when the field appears in both
	// AttemptedFields and PopulatedFields. Used by OverwriteAttempted for
	// clearing-semantics fields (biography, tags, dates).
	modeAttemptedPopulated
)

// strField describes merge behavior for a single string-typed metadata field.
type strField struct {
	name string
	get  func(*MetadataUpdate) string
	dst  func(*Artist) *string

	// one entry per MergeStrategy constant, in declaration order
	modes [4]fieldMode
}

// sliceField describes merge behavior for a single []string-typed metadata field.
type sliceField struct {
	name string
	get  func(*MetadataUpdate) []string
	dst  func(*Artist) *[]string

	// one entry per MergeStrategy constant, in declaration order
	modes [4]fieldMode
}

// mergeStrFields is the single source of truth for string field merge policies.
// Index order matches MergeStrategy constants: OverwriteAttempted=0, FillEmpty=1, NFOImport=2, SnapshotRestore=3.
var mergeStrFields = []strField{
	// Name and SortName: NFOImport/SnapshotRestore use non-empty/unconditional;
	// OverwriteAttempted and FillEmpty never touch them (handler manages display name separately).
	{
		name:  "name",
		get:   func(u *MetadataUpdate) string { return u.Name },
		dst:   func(a *Artist) *string { return &a.Name },
		modes: [4]fieldMode{modeSkip, modeSkip, modeNonEmpty, modeUnconditional},
	},
	{
		name:  "sort_name",
		get:   func(u *MetadataUpdate) string { return u.SortName },
		dst:   func(a *Artist) *string { return &a.SortName },
		modes: [4]fieldMode{modeSkip, modeSkip, modeNonEmpty, modeUnconditional},
	},
	// Classification: non-empty overwrite in OverwriteAttempted; fill-empty in
	// FillEmpty; unconditional in NFOImport and SnapshotRestore.
	{
		name:  "type",
		get:   func(u *MetadataUpdate) string { return u.Type },
		dst:   func(a *Artist) *string { return &a.Type },
		modes: [4]fieldMode{modeNonEmpty, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "gender",
		get:   func(u *MetadataUpdate) string { return u.Gender },
		dst:   func(a *Artist) *string { return &a.Gender },
		modes: [4]fieldMode{modeNonEmpty, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "origin",
		get:   func(u *MetadataUpdate) string { return u.Origin },
		dst:   func(a *Artist) *string { return &a.Origin },
		modes: [4]fieldMode{modeNonEmpty, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "disambiguation",
		get:   func(u *MetadataUpdate) string { return u.Disambiguation },
		dst:   func(a *Artist) *string { return &a.Disambiguation },
		modes: [4]fieldMode{modeNonEmpty, modeSkip, modeUnconditional, modeUnconditional},
	},
	// Biography: clearing semantics in OverwriteAttempted (requires both
	// attempted AND populated); non-empty overwrite in NFO; fill-empty in
	// FillEmpty; unconditional in SnapshotRestore.
	{
		name:  "biography",
		get:   func(u *MetadataUpdate) string { return u.Biography },
		dst:   func(a *Artist) *string { return &a.Biography },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	// Provider IDs: fill-empty in OverwriteAttempted and FillEmpty;
	// non-empty overwrite in NFOImport; unconditional in SnapshotRestore.
	{
		name:  "musicbrainz_id",
		get:   func(u *MetadataUpdate) string { return u.MusicBrainzID },
		dst:   func(a *Artist) *string { return &a.MusicBrainzID },
		modes: [4]fieldMode{modeFillEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	{
		name:  "audiodb_id",
		get:   func(u *MetadataUpdate) string { return u.AudioDBID },
		dst:   func(a *Artist) *string { return &a.AudioDBID },
		modes: [4]fieldMode{modeFillEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	{
		name:  "discogs_id",
		get:   func(u *MetadataUpdate) string { return u.DiscogsID },
		dst:   func(a *Artist) *string { return &a.DiscogsID },
		modes: [4]fieldMode{modeFillEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	{
		name:  "wikidata_id",
		get:   func(u *MetadataUpdate) string { return u.WikidataID },
		dst:   func(a *Artist) *string { return &a.WikidataID },
		modes: [4]fieldMode{modeFillEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	{
		name:  "deezer_id",
		get:   func(u *MetadataUpdate) string { return u.DeezerID },
		dst:   func(a *Artist) *string { return &a.DeezerID },
		modes: [4]fieldMode{modeFillEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	{
		name:  "spotify_id",
		get:   func(u *MetadataUpdate) string { return u.SpotifyID },
		dst:   func(a *Artist) *string { return &a.SpotifyID },
		modes: [4]fieldMode{modeFillEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	// YearsActive: non-empty overwrite across OverwriteAttempted and FillEmpty;
	// unconditional for NFOImport and SnapshotRestore.
	{
		name:  "years_active",
		get:   func(u *MetadataUpdate) string { return u.YearsActive },
		dst:   func(a *Artist) *string { return &a.YearsActive },
		modes: [4]fieldMode{modeNonEmpty, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	// Date fields: clearing semantics in OverwriteAttempted (requires both
	// attempted AND populated); fill-empty in FillEmpty; unconditional in
	// NFOImport and SnapshotRestore.
	{
		name:  "born",
		get:   func(u *MetadataUpdate) string { return u.Born },
		dst:   func(a *Artist) *string { return &a.Born },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "formed",
		get:   func(u *MetadataUpdate) string { return u.Formed },
		dst:   func(a *Artist) *string { return &a.Formed },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "died",
		get:   func(u *MetadataUpdate) string { return u.Died },
		dst:   func(a *Artist) *string { return &a.Died },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "disbanded",
		get:   func(u *MetadataUpdate) string { return u.Disbanded },
		dst:   func(a *Artist) *string { return &a.Disbanded },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
}

// mergeSliceFields is the single source of truth for []string field merge policies.
var mergeSliceFields = []sliceField{
	{
		name:  "genres",
		get:   func(u *MetadataUpdate) []string { return u.Genres },
		dst:   func(a *Artist) *[]string { return &a.Genres },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "styles",
		get:   func(u *MetadataUpdate) []string { return u.Styles },
		dst:   func(a *Artist) *[]string { return &a.Styles },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
	{
		name:  "moods",
		get:   func(u *MetadataUpdate) []string { return u.Moods },
		dst:   func(a *Artist) *[]string { return &a.Moods },
		modes: [4]fieldMode{modeAttemptedPopulated, modeFillEmpty, modeUnconditional, modeUnconditional},
	},
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

	locked := buildLockedSet(opts.LockedFields)

	attempted := make(map[string]bool, len(opts.AttemptedFields))
	for _, f := range opts.AttemptedFields {
		attempted[f] = true
	}
	populated := make(map[string]bool, len(opts.PopulatedFields))
	for _, f := range opts.PopulatedFields {
		populated[f] = true
	}

	changed := applyFields(a, u, strategy, locked, attempted, populated)

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

// applyFields walks the field policy tables and applies each field according
// to the mode defined for the given strategy.
func applyFields(a *Artist, u *MetadataUpdate, strategy MergeStrategy, locked map[string]struct{}, attempted, populated map[string]bool) bool {
	idx := int(strategy)
	changed := false

	for _, f := range mergeStrFields {
		if isLocked(locked, f.name) {
			continue
		}
		dst := f.dst(a)
		val := f.get(u)
		switch f.modes[idx] {
		case modeSkip:
			// never touch this field for this strategy
		case modeUnconditional:
			changed = setString(dst, val) || changed
		case modeNonEmpty:
			changed = setNonEmpty(dst, val) || changed
		case modeFillEmpty:
			changed = fillEmpty(dst, val) || changed
		case modeAttemptedPopulated:
			if attempted[f.name] && populated[f.name] {
				changed = setString(dst, val) || changed
			}
		}
	}

	for _, f := range mergeSliceFields {
		if isLocked(locked, f.name) {
			continue
		}
		dst := f.dst(a)
		val := f.get(u)
		switch f.modes[idx] {
		case modeSkip:
			// never touch this field for this strategy
		case modeUnconditional:
			changed = setSlice(dst, val) || changed
		case modeNonEmpty:
			// slice "non-empty" means overwrite only when val is non-empty
			if len(val) > 0 {
				changed = setSlice(dst, val) || changed
			}
		case modeFillEmpty:
			changed = fillEmptySlice(dst, val) || changed
		case modeAttemptedPopulated:
			if attempted[f.name] && populated[f.name] {
				changed = setSlice(dst, val) || changed
			}
		}
	}

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
		Origin:         m.Origin,
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
