package artist

import (
	"log/slog"
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
	// Under OverwriteAttempted specifically, Type/Gender/YearsActive use
	// non-empty overwrite, so they are never cleared by this strategy.
	// Provider IDs use fill-empty semantics (never overwrite existing IDs).
	//
	// Whether ANY strategy may clear a stored value when the incoming source
	// omits the field is governed by MergeOptions.Clobber, not by the strategy
	// alone. See the note above mergeStrFields.
	OverwriteAttempted MergeStrategy = iota

	// FillEmpty only sets fields that are currently empty/zero on the artist.
	// Never overwrites existing data.
	FillEmpty

	// NFOImport applies NFO-takes-precedence semantics for values the NFO
	// actually carries. With the default MergeOptions{} (Clobber false), an
	// element that is ABSENT from the NFO preserves the stored value for EVERY
	// field -- an omitted element means "this file says nothing about it", not
	// "it is empty". A present, non-empty value still overwrites the stored one
	// for every field except the provider IDs' own rules below.
	//
	//   - Identity fields (Name, SortName, MBID, AudioDBID, Biography): non-empty overwrite
	//   - Provider IDs (Discogs, Wikidata, Deezer, Spotify): non-empty overwrite
	//   - Classification fields (Type, Gender, Disambiguation), lists and dates:
	//     non-empty overwrite by default; they clear on absence ONLY when the
	//     caller sets MergeOptions.Clobber. No NFO-import call site does.
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

	// LockedFields lists ADDITIONAL field names that must not be overwritten,
	// regardless of strategy. Compared case-insensitively.
	//
	// Callers do NOT need to populate this to honor an operator's per-field
	// locks: ApplyMetadata always reads Artist.LockedFields off the artist it
	// is merging into and unions it with this slice (issue #2749). Leaving
	// this nil is the safe default -- the artist's own persisted locks are
	// still enforced. Use it only for locks that exist for the duration of one
	// call and are not persisted on the artist.
	LockedFields []string

	// Clobber grants this merge the right to CLEAR a stored value when the
	// incoming source omits the field. With the zero value (false), an absent
	// field means "this source says nothing about it", not "it is empty", so
	// the stored value survives. Set true only when the source is genuinely
	// authoritative about emptiness -- an operator-initiated re-identify, for
	// example.
	//
	// One strategy is authoritative about emptiness by definition and does not
	// need this flag: SnapshotRestore always clobbers, whatever this field is
	// set to (see ApplyMetadata). The zero value therefore means "do not clear"
	// for every strategy EXCEPT SnapshotRestore.
	//
	// Clobber is a ceiling on destructiveness, not a license to ignore other
	// guarantees: locked fields and the post-merge type-consistency passes
	// apply regardless of its value.
	Clobber bool
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

// The merge engine is two-layer, and the two layers answer different questions.
//
//	Layer 1 -- the tables below say what a field MEANS under each strategy.
//	Layer 2 -- MergeOptions.Clobber says whether the CALLER is entitled to
//	           assert emptiness at all.
//
// modeUnconditional in a table cell is therefore a CEILING, not a promise: it
// is reachable only when the call carries Clobber: true (or is a
// SnapshotRestore, which is authoritative about emptiness by definition).
// Without that entitlement, effectiveMode demotes modeUnconditional to
// modeNonEmpty for the duration of the call, so an absent incoming value
// preserves the stored one while a present one still overwrites. Every other
// mode is unaffected.
//
// The practical consequence: a bare MergeOptions{} is the most conservative
// possible call, not the most destructive one. To find every place entitled to
// destroy operator data, grep for "Clobber: true".
//
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
	// Origin: non-empty overwrite in OverwriteAttempted, fill-empty in FillEmpty,
	// non-empty overwrite in NFOImport (sparse NFOs must not clear existing origin),
	// unconditional in SnapshotRestore. The field-level guard here predates and is
	// now also backed by the call-level Clobber default, which gives every other
	// NFOImport field the same protection.
	{
		name:  "origin",
		get:   func(u *MetadataUpdate) string { return u.Origin },
		dst:   func(a *Artist) *string { return &a.Origin },
		modes: [4]fieldMode{modeNonEmpty, modeFillEmpty, modeNonEmpty, modeUnconditional},
	},
	// Disambiguation: non-empty overwrite in OverwriteAttempted; skipped in
	// FillEmpty (avoid overwriting user-set disambiguation with provider noise);
	// unconditional in NFOImport and SnapshotRestore.
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
	// YearsActive: non-empty overwrite in OverwriteAttempted; fill-empty in
	// FillEmpty; unconditional for NFOImport and SnapshotRestore.
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

// buildLockedSet normalizes one or more locked-fields slices into a single
// lowercase lookup set. Blank and whitespace-only tokens are dropped so a
// slice like []{"", " "} produces a nil set rather than one that would match a
// lookup for "". Returns nil when no valid tokens remain so isLocked can
// short-circuit on len==0 without allocating.
//
// It takes multiple slices because ApplyMetadata unions the artist's own
// persisted Artist.LockedFields with any per-call MergeOptions.LockedFields.
func buildLockedSet(fieldSets ...[]string) map[string]struct{} {
	total := 0
	for _, fields := range fieldSets {
		total += len(fields)
	}
	if total == 0 {
		return nil
	}
	out := make(map[string]struct{}, total)
	for _, fields := range fieldSets {
		for _, f := range fields {
			key := strings.ToLower(strings.TrimSpace(f))
			if key == "" {
				continue
			}
			out[key] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeableFieldNames is the set of field names the merge tables above can
// actually gate on. Built once at init from the same tables that drive
// applyFields, so it can never drift from them.
var mergeableFieldNames = func() map[string]struct{} {
	out := make(map[string]struct{}, len(mergeStrFields)+len(mergeSliceFields))
	for _, f := range mergeStrFields {
		out[f.name] = struct{}{}
	}
	for _, f := range mergeSliceFields {
		out[f.name] = struct{}{}
	}
	return out
}()

// lockableFieldNames is the full vocabulary of meaningful lock tokens: every
// field the merge engine can gate, plus every field the rest of the app treats
// as lockable (e.g. "members", which is a separate relation rather than a
// merge-table column, so a lock on it is honored elsewhere and must not be
// reported as unknown here).
var lockableFieldNames = func() map[string]struct{} {
	out := make(map[string]struct{}, len(mergeableFieldNames)+len(AllLockableFields))
	for name := range mergeableFieldNames {
		out[name] = struct{}{}
	}
	for _, f := range AllLockableFields {
		out[strings.ToLower(string(f))] = struct{}{}
	}
	return out
}()

// reportUnenforceableLocks logs an error for any lock token that matches no
// known lockable field. Such a token protects nothing anywhere in the
// codebase -- the operator asked for a guarantee the system cannot deliver --
// so per the no-silent-failure rule it fails loudly instead of being dropped
// on the floor. Callers still proceed: the enforceable locks in the same set
// are honored normally.
func reportUnenforceableLocks(a *Artist, locked map[string]struct{}) {
	var unknown []string
	for name := range locked {
		if _, ok := lockableFieldNames[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return
	}
	slices.Sort(unknown) // stable output; map iteration order is random
	artistID := ""
	if a != nil {
		artistID = a.ID
	}
	slog.Error("locked field name is not a recognized field; this lock cannot be enforced and the value is unprotected",
		"artist_id", artistID,
		"unknown_locked_fields", unknown)
}

// normalizeFieldSet lowercases and trims a field-name slice into a lookup set,
// dropping blank and whitespace-only tokens so an empty entry cannot match a
// lookup for "". Used for MergeOptions.AttemptedFields and PopulatedFields,
// which modeAttemptedPopulated consults by field name.
func normalizeFieldSet(fields []string) map[string]bool {
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" {
			continue
		}
		out[key] = true
	}
	return out
}

// ApplyMetadata merges incoming metadata into an Artist using the specified
// strategy. Returns true when the merge brought in something worth persisting
// and publishing -- a field the incoming update actually moved, or a new
// metadata-source attribution.
//
// It is NOT a "the struct differs from before" flag. The post-merge
// type-consistency repair mutates the artist without contributing to it, so a
// no-op update over an already-inconsistent stored row reports false while
// still handing back a corrected artist. See applyTypeConsistency for why.
//
// Per-field locks are enforced on EVERY path and for EVERY strategy. The
// enforced set is the union of the artist's own persisted Artist.LockedFields
// and any per-call MergeOptions.LockedFields, so a caller cannot accidentally
// opt an operator's pinned values out of protection by passing a zero
// MergeOptions (issue #2749: the scan and bulk-rule paths did exactly that,
// and a lock the operator had applied was silently overwritten on the next
// scan). Safe behavior is what you get by doing nothing.
func ApplyMetadata(a *Artist, u *MetadataUpdate, strategy MergeStrategy, opts MergeOptions) bool {
	if a == nil || u == nil {
		return false
	}
	if strategy < OverwriteAttempted || strategy > SnapshotRestore {
		return false
	}

	// Derived from the artist itself, not from the caller: this is what makes
	// the lock contract hold on every call site rather than only the ones that
	// remembered to populate opts.
	locked := buildLockedSet(a.LockedFields, opts.LockedFields)
	reportUnenforceableLocks(a, locked)

	attempted := normalizeFieldSet(opts.AttemptedFields)
	populated := normalizeFieldSet(opts.PopulatedFields)

	// A snapshot restore is authoritative about emptiness by definition:
	// restoring a snapshot in which a field was empty must produce an empty
	// field. It is not downgraded by a caller that passes a bare MergeOptions{}.
	clobber := opts.Clobber || strategy == SnapshotRestore

	changed := applyFields(a, u, strategy, locked, attempted, populated, clobber)

	// Deliberately NOT folded into `changed`: see applyTypeConsistency.
	applyTypeConsistency(a, locked, opts.FilterDatesByType)

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

// effectiveMode demotes modeUnconditional to modeNonEmpty when the caller has
// not been granted the right to assert emptiness. Every other mode is
// unaffected: this is a ceiling on destructiveness, not a new merge axis.
func effectiveMode(m fieldMode, clobber bool) fieldMode {
	if m == modeUnconditional && !clobber {
		return modeNonEmpty
	}
	return m
}

// applyFields walks the field policy tables and applies each field according
// to the mode defined for the given strategy, capped by the caller's clobber
// entitlement (see effectiveMode).
func applyFields(a *Artist, u *MetadataUpdate, strategy MergeStrategy, locked map[string]struct{}, attempted, populated map[string]bool, clobber bool) bool {
	idx := int(strategy)
	changed := false

	for _, f := range mergeStrFields {
		if isLocked(locked, f.name) {
			continue
		}
		dst := f.dst(a)
		val := f.get(u)
		switch effectiveMode(f.modes[idx], clobber) {
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
		switch effectiveMode(f.modes[idx], clobber) {
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
//
// The match is EXACT -- the raw a.Type is switched on, with no trim or
// lowercase. That is a real divergence from its gender sibling
// FilterGenderByArtistType, which matches via the normalizing IsGenderlessType,
// and it is long-standing behavior this pass has always had. Measured:
//
//	type="group"    -> gender CLEARED, born/died CLEARED
//	type="Group"    -> gender CLEARED, born/died KEPT
//	type=" group "  -> gender CLEARED, born/died KEPT
//
// So a stored "Group" loses its gender but keeps born/died. MusicBrainz
// lowercases its type values, but a hand-written NFO carrying <type>Group</type>
// reaches here unnormalized through nfo.ToMetadataUpdate. Normalizing this pass
// too would make the siblings consistent, at the cost of newly clearing
// born/died on rows that currently keep them; that is a behavior change and is
// deliberately not made here. Do not read the "sibling" language elsewhere in
// this file as a claim that the two passes normalize alike -- they do not.
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

// IsGenderlessType reports whether an artist type is positively known to
// describe a collective, which by definition has members rather than a gender:
// group, orchestra, choir. It is a closed ALLOW-LIST, and it must stay one.
//
// WHY IT IS AN ALLOW-LIST, AND WHAT A "SIMPLIFICATION" BACK TO
// !IsIndividualType(t) ACTUALLY COSTS:
//
// The gender pass was introduced as the sibling of FilterDatesByArtistType,
// with the same stated intent, but it was written with the OPPOSITE predicate
// structure, and that inversion is the whole defect. Compare the two:
//
//	FilterDatesByArtistType  ENUMERATES the types it will clear for.
//	                         Its default branch does nothing.
//	FilterGenderByArtistType NEGATED the types it would keep for.
//	                         Its default branch DESTROYS.
//
// The enumerating form is inert on every value nobody thought to list. The
// negating form destroys data on every value nobody thought to list -- and the
// set of values nobody thought to list is exactly the set that grows over time,
// as providers, imports and hand-written NFOs invent type strings.
//
// Concretely: IsIndividualType returns false by DEFAULT, so every value outside
// {solo, person, character} falls into its default branch -- MusicBrainz's
// catch-all "Other", "unknown", and any future or hand-written string. Real
// people routinely sit on "Other" in production data; the maintainer reports it
// as the MAIN case, not an edge case, and internal/artist/scan.go's type_other
// facet is defined as the complement of the named types precisely because it
// sweeps up Character, MusicBrainz "Other" and untyped rows alike. Under the
// negated form, a merge silently deleted the stored gender of all of them.
//
// The asymmetry is about which way the default errs. IsIndividualType is
// correct for its own purpose: the producers (nfo.ToMetadataUpdate,
// FetchResultToUpdate) use it to decide whether to WRITE a gender, where
// falling through to "no" merely declines to ADD data. Inverting the same
// predicate into a decision to DELETE flips that default from conservative to
// destructive. "Not a known individual type" is not proof of "cannot have a
// gender". "Other" means UNKNOWN, which is semantically identical to the empty
// type this pass has always, correctly, spared -- it is not a statement that
// the artist is a collective.
//
// So: do not "simplify" this to a negated individual check. It is not a
// simplification, it is a data-loss bug, and it will not fail loudly.
//
// The comparison is trimmed and lowercased, matching the convention of
// provider.isIndividualTypeValue: stored type strings are known to arrive in
// mixed case, so a stored "Group" is still a group. Widening the match is safe
// in this direction because it only ever adds values to a closed collective
// list, never to the destructive default.
//
// Note this normalization is NOT shared with FilterDatesByArtistType, which
// switches on the raw type: a stored "Group" clears gender here but keeps
// born/died there. See that function for the measured divergence.
func IsGenderlessType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "group", "orchestra", "choir":
		return true
	default:
		return false
	}
}

// FilterGenderByArtistType clears gender when the artist's type is positively
// known to be genderless. It is the gender sibling of FilterDatesByArtistType
// in intent, and like that pass it filters only on types it recognizes: an
// unknown, unrecognized, "Other" or empty type is left completely alone. The
// two are NOT alike in matching, though: this pass normalizes the type through
// IsGenderlessType while the date pass matches it raw, so a stored "Group" is
// recognized here and not there.
//
// This pass exists because the producers that build a MetadataUpdate signal
// "gender is inapplicable for this type" by emitting an empty Gender, which is
// indistinguishable from "this source said nothing about gender". Merges that
// may not clobber preserve the stored gender in both cases, so the
// inapplicable case has to be re-applied here, after the type is known.
//
// See IsGenderlessType for why this is an allow-list and not !IsIndividualType.
func FilterGenderByArtistType(a *Artist) {
	if IsGenderlessType(a.Type) {
		a.Gender = ""
	}
}

// applyTypeConsistency runs the post-merge passes that clear fields which are
// semantically wrong for the artist's resulting type. It MUTATES the artist and
// deliberately reports NOTHING: it must never be the sole reason ApplyMetadata
// returns true; see the sections below for why, and for how a genuine type
// change is still distinguished from a repair of a pre-existing bad row.
//
// Each pass snapshots the fields it may blank and restores the locked ones
// afterwards, so a pinned value survives both the per-field merge skip and the
// filter (e.g. a user who pinned Born, or Gender, on a group type). A lock is
// the stronger guarantee: it wins over type consistency.
//
// The gender pass always runs; the date passes run only when the caller asks
// for them via MergeOptions.FilterDatesByType. The asymmetry is deliberate --
// dates are filtered only where the caller knows the incoming type is
// trustworthy, whereas an artist carrying both a group type and a gender is
// always an inconsistent state (see FilterGenderByArtistType).
//
// # WHY THIS PASS DOES NOT REPORT CHANGED
//
// ApplyMetadata's return value is not "the struct in memory differs from what
// it was". It is a persist-and-publish signal, and its consumers treat it that
// way: internal/rule/bulk_executor.go skips an artist outright when it is
// false, and on true performs an artistService.Update (a DB write plus a
// metadata_changes audit row), an NFO rewrite on disk, and a PublishMetadata
// push to Emby/Jellyfin.
//
// These passes fire on the artist's RESULTING state, so they fire just as
// readily on an inconsistency that was already sitting in the database as on
// one the merge just created. If the repair fed the return value, a bulk sweep
// carrying no new metadata at all would still persist and PUBLISH every
// pre-existing bad row it walked past -- for artists the operator never
// touched. That is the defect this reports-nothing contract exists to prevent.
//
// SEPARATING "THE MERGE CHANGED THE TYPE" FROM "WE REPAIRED A BAD ROW"
//
// The two cases go through this identical code, so the separation is made
// upstream rather than here, and it needs no extra bookkeeping:
//
//   - A genuine type FLIP (incoming type: group onto a stored solo/female) is
//     a change to the Type field, so applyFields has ALREADY set changed=true
//     before this runs. Clearing the now-inapplicable gender is part of that
//     change and rides along on a signal that is true for an independent
//     reason. The original #2748 behavior is intact.
//   - A repair-only pass (no field moved; the stored row was simply already
//     inconsistent) leaves changed=false, which is correct: nothing was
//     learned, so there is nothing to publish. The corrected value still
//     lives on the in-memory artist, so the next merge that persists for a
//     real reason carries it along.
//
// In other words the repair is never LOST, only never SELF-TRIGGERING.
func applyTypeConsistency(a *Artist, locked map[string]struct{}, filterDates bool) {
	beforeGender := a.Gender
	FilterGenderByArtistType(a)
	if isLocked(locked, "gender") {
		a.Gender = beforeGender
	}

	if !filterDates {
		return
	}

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
	if slices.Equal(*dst, val) && ((*dst == nil) == (val == nil)) {
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
