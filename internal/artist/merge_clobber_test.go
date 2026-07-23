package artist

import (
	"slices"
	"testing"
)

// The tests in this file guard the issue #2748 contract: MergeOptions.Clobber
// is the ONLY thing that entitles a merge to clear a stored value when the
// incoming source omits the field. The zero MergeOptions{} -- which the scan
// and bulk-rule call sites literally pass -- therefore destroys nothing.
//
// SnapshotRestore is the one exception and it is deliberate: a restore is
// authoritative about emptiness by definition, so ApplyMetadata forces the
// clobber grant on for that strategy.

// assertStrings checks a set of named string fields. Each entry maps a field
// name to a {got, want} pair so a failure names the field that regressed.
func assertStrings(t *testing.T, fields map[string][2]string) {
	t.Helper()
	for name, pair := range fields {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}

// assertSlice checks one named slice field.
func assertSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// fullArtist returns an Artist with every one of the 21 merge-table fields set
// to a distinct non-empty value, so a test can tell exactly which field was
// clobbered.
func fullArtist() *Artist {
	return &Artist{
		Name:           "Stored Name",
		SortName:       "Stored, Sort",
		Type:           "person",
		Gender:         "female",
		Origin:         "Bristol, UK",
		Disambiguation: "stored disambiguation",
		MusicBrainzID:  "stored-mbid",
		AudioDBID:      "stored-audiodb",
		DiscogsID:      "stored-discogs",
		WikidataID:     "stored-wikidata",
		DeezerID:       "stored-deezer",
		SpotifyID:      "stored-spotify",
		Biography:      "stored biography",
		Genres:         []string{"stored-genre"},
		Styles:         []string{"stored-style"},
		Moods:          []string{"stored-mood"},
		YearsActive:    "1988-present",
		Born:           "1970-01-01",
		Formed:         "1980-01-01",
		Died:           "2020-01-01",
		Disbanded:      "2000-01-01",
	}
}

// assertFullArtistIntact asserts all 21 merge-table fields still hold the
// values fullArtist set.
func assertFullArtistIntact(t *testing.T, a *Artist) {
	t.Helper()
	assertStrings(t, map[string][2]string{
		"name":           {a.Name, "Stored Name"},
		"sort_name":      {a.SortName, "Stored, Sort"},
		"type":           {a.Type, "person"},
		"gender":         {a.Gender, "female"},
		"origin":         {a.Origin, "Bristol, UK"},
		"disambiguation": {a.Disambiguation, "stored disambiguation"},
		"musicbrainz_id": {a.MusicBrainzID, "stored-mbid"},
		"audiodb_id":     {a.AudioDBID, "stored-audiodb"},
		"discogs_id":     {a.DiscogsID, "stored-discogs"},
		"wikidata_id":    {a.WikidataID, "stored-wikidata"},
		"deezer_id":      {a.DeezerID, "stored-deezer"},
		"spotify_id":     {a.SpotifyID, "stored-spotify"},
		"biography":      {a.Biography, "stored biography"},
		"years_active":   {a.YearsActive, "1988-present"},
		"born":           {a.Born, "1970-01-01"},
		"formed":         {a.Formed, "1980-01-01"},
		"died":           {a.Died, "2020-01-01"},
		"disbanded":      {a.Disbanded, "2000-01-01"},
	})
	assertSlice(t, "genres", a.Genres, []string{"stored-genre"})
	assertSlice(t, "styles", a.Styles, []string{"stored-style"})
	assertSlice(t, "moods", a.Moods, []string{"stored-mood"})
}

// TestNFOImport_ZeroOptionsNeverClearsAnyField is the headline guard: an NFO
// that omits every element, imported with the default MergeOptions{}, must
// leave all 21 merge-table fields exactly as stored.
func TestNFOImport_ZeroOptionsNeverClearsAnyField(t *testing.T) {
	t.Parallel()

	a := fullArtist()
	u := &MetadataUpdate{} // an NFO with no elements at all

	if ApplyMetadata(a, u, NFOImport, MergeOptions{}) {
		t.Error("changed = true; an NFO carrying no values must change nothing")
	}
	assertFullArtistIntact(t, a)
}

// TestZeroMergeOptions_NeverClears_AllStrategies pins the struct-level
// property: MergeOptions{} is the most conservative possible call, whatever
// strategy it is paired with.
//
// SnapshotRestore is deliberately excluded. A restore is authoritative about
// emptiness -- restoring a snapshot in which a field was empty must produce an
// empty field -- so ApplyMetadata forces the clobber grant on for it.
// TestSnapshotRestore_StillClearsWithZeroOptions covers that case.
func TestZeroMergeOptions_NeverClears_AllStrategies(t *testing.T) {
	t.Parallel()

	strategies := map[string]MergeStrategy{
		"OverwriteAttempted": OverwriteAttempted,
		"FillEmpty":          FillEmpty,
		"NFOImport":          NFOImport,
	}
	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := fullArtist()
			if ApplyMetadata(a, &MetadataUpdate{}, strategy, MergeOptions{}) {
				t.Errorf("%s: changed = true with a zero update and zero options", name)
			}
			assertFullArtistIntact(t, a)
		})
	}
}

// TestNFOImport_DifferentValueStillOverwrites proves the fix did not simply
// disable NFO import: each of the eleven fields that used to clear on absence
// still takes a different non-empty incoming value.
func TestNFOImport_DifferentValueStillOverwrites(t *testing.T) {
	t.Parallel()

	a := fullArtist()
	u := &MetadataUpdate{
		// An individual type: a group type would legitimately clear gender via
		// the post-merge type-consistency pass, which would mask whether the
		// incoming gender overwrote the stored one.
		Type:           "solo",
		Gender:         "male",
		Disambiguation: "incoming disambiguation",
		YearsActive:    "1991-2010",
		Born:           "1971-02-02",
		Formed:         "1981-02-02",
		Died:           "2021-02-02",
		Disbanded:      "2001-02-02",
		Genres:         []string{"incoming-genre", "second-genre"},
		Styles:         []string{"incoming-style"},
		Moods:          []string{"incoming-mood"},
	}

	if !ApplyMetadata(a, u, NFOImport, MergeOptions{}) {
		t.Error("changed = false; non-empty incoming values must still overwrite")
	}
	assertStrings(t, map[string][2]string{
		"type":           {a.Type, "solo"},
		"gender":         {a.Gender, "male"},
		"disambiguation": {a.Disambiguation, "incoming disambiguation"},
		"years_active":   {a.YearsActive, "1991-2010"},
		"born":           {a.Born, "1971-02-02"},
		"formed":         {a.Formed, "1981-02-02"},
		"died":           {a.Died, "2021-02-02"},
		"disbanded":      {a.Disbanded, "2001-02-02"},
	})
	assertSlice(t, "genres", a.Genres, []string{"incoming-genre", "second-genre"})
	assertSlice(t, "styles", a.Styles, []string{"incoming-style"})
	assertSlice(t, "moods", a.Moods, []string{"incoming-mood"})
}

// TestSnapshotRestore_StillClearsWithZeroOptions is what the
// strategy == SnapshotRestore floor in ApplyMetadata buys: a restore stays
// faithful even though the caller passed a bare MergeOptions{}.
func TestSnapshotRestore_StillClearsWithZeroOptions(t *testing.T) {
	t.Parallel()

	a := fullArtist()
	if !ApplyMetadata(a, &MetadataUpdate{}, SnapshotRestore, MergeOptions{}) {
		t.Fatal("changed = false; a snapshot restore of an empty snapshot must clear")
	}
	assertStrings(t, map[string][2]string{
		"name":           {a.Name, ""},
		"type":           {a.Type, ""},
		"gender":         {a.Gender, ""},
		"origin":         {a.Origin, ""},
		"disambiguation": {a.Disambiguation, ""},
		"biography":      {a.Biography, ""},
		"years_active":   {a.YearsActive, ""},
		"born":           {a.Born, ""},
	})
	assertSlice(t, "genres", a.Genres, nil)
}

// TestLockedFieldSurvivesClobberTrue proves the precedence: the per-field lock
// outranks the clobber grant. The lock check runs before mode resolution, so a
// locked field is skipped regardless of Clobber and regardless of strategy.
//
// Each sub-test also asserts that an UNLOCKED sibling IS cleared. Without that
// negative half the test would pass vacuously if the merge did nothing at all.
func TestLockedFieldSurvivesClobberTrue(t *testing.T) {
	t.Parallel()

	strategies := map[string]MergeStrategy{
		"NFOImport":       NFOImport,
		"SnapshotRestore": SnapshotRestore,
	}
	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &Artist{
				Gender:       "female",
				Genres:       []string{"trip hop"},
				Type:         "person",        // unlocked sibling, string
				Styles:       []string{"dub"}, // unlocked sibling, slice
				LockedFields: []string{"gender", "genres"},
			}

			if !ApplyMetadata(a, &MetadataUpdate{}, strategy, MergeOptions{Clobber: true}) {
				t.Fatal("changed = false; the unlocked siblings should have been cleared, " +
					"so this test would otherwise pass vacuously")
			}

			if a.Gender != "female" {
				t.Errorf("locked gender cleared by Clobber: got %q, want %q", a.Gender, "female")
			}
			assertSlice(t, "locked genres", a.Genres, []string{"trip hop"})

			// Negative half: the lock is what protected the two above, not an
			// inert merge.
			if a.Type != "" {
				t.Errorf("unlocked type = %q, want cleared", a.Type)
			}
			assertSlice(t, "unlocked styles", a.Styles, nil)
		})
	}
}

// TestClobberTrue_RestoresTableSemantics pins Clobber as a ceiling rather than
// a redefinition of the per-field table: with the grant, NFOImport clears
// exactly the eleven fields whose NFOImport cell is modeUnconditional, and the
// ten modeNonEmpty fields still survive.
func TestClobberTrue_RestoresTableSemantics(t *testing.T) {
	t.Parallel()

	a := fullArtist()
	if !ApplyMetadata(a, &MetadataUpdate{}, NFOImport, MergeOptions{Clobber: true}) {
		t.Fatal("changed = false; Clobber: true must restore the historical clearing")
	}

	// The eleven modeUnconditional cells clear.
	assertStrings(t, map[string][2]string{
		"type":           {a.Type, ""},
		"gender":         {a.Gender, ""},
		"disambiguation": {a.Disambiguation, ""},
		"years_active":   {a.YearsActive, ""},
		"born":           {a.Born, ""},
		"formed":         {a.Formed, ""},
		"died":           {a.Died, ""},
		"disbanded":      {a.Disbanded, ""},
	})
	assertSlice(t, "genres", a.Genres, nil)
	assertSlice(t, "styles", a.Styles, nil)
	assertSlice(t, "moods", a.Moods, nil)

	// The ten modeNonEmpty cells are untouched: Clobber lifts the ceiling, it
	// does not rewrite the table.
	assertStrings(t, map[string][2]string{
		"name":           {a.Name, "Stored Name"},
		"sort_name":      {a.SortName, "Stored, Sort"},
		"origin":         {a.Origin, "Bristol, UK"},
		"biography":      {a.Biography, "stored biography"},
		"musicbrainz_id": {a.MusicBrainzID, "stored-mbid"},
		"audiodb_id":     {a.AudioDBID, "stored-audiodb"},
		"discogs_id":     {a.DiscogsID, "stored-discogs"},
		"wikidata_id":    {a.WikidataID, "stored-wikidata"},
		"deezer_id":      {a.DeezerID, "stored-deezer"},
		"spotify_id":     {a.SpotifyID, "stored-spotify"},
	})
}

// --- post-merge gender/type consistency (the #2748 follow-up) ---

// TestTypeConsistency_GenderClearedForGroup covers the first of the two
// meanings an empty Gender carries on the wire: "gender is inapplicable for
// this type". Both nfo.ToMetadataUpdate and FetchResultToUpdate blank Gender
// when the incoming Type is non-individual, so that deliberate blank is
// indistinguishable from silence once it reaches ApplyMetadata. Making
// silence non-destructive (issue #2748) stopped the deliberate blank too; the
// post-merge pass re-applies it.
//
// The counterpart is TestTypeConsistency_GenderPreservedWhenSilent below. A
// fix that only satisfied one of the two would be the original bug or the
// defect it introduced, so both must be present.
func TestTypeConsistency_GenderClearedForGroup(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"group", "orchestra", "choir"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			a := &Artist{Type: "solo", Gender: "female"}
			// Precondition: the type really is one the model says cannot
			// carry a gender. Without this the loop could silently be
			// iterating over individual types and assert nothing.
			if IsIndividualType(typ) {
				t.Fatalf("precondition failed: %q is an individual type", typ)
			}

			u := &MetadataUpdate{Type: typ, Gender: ""}
			if !ApplyMetadata(a, u, NFOImport, MergeOptions{}) {
				t.Error("changed = false; the type change and the gender clear are both changes")
			}
			if a.Type != typ {
				t.Errorf("type = %q, want %q", a.Type, typ)
			}
			if a.Gender != "" {
				t.Errorf("gender = %q, want empty; a %s cannot carry a gender", a.Gender, typ)
			}
		})
	}
}

// TestTypeConsistency_GenderPreservedWhenSilent covers the second meaning:
// "this source said nothing about gender". This is the #2748 fix itself, and
// it is the half a careless gender clear would destroy -- an implementation
// that simply restored the unconditional clear would pass the test above and
// fail this one.
func TestTypeConsistency_GenderPreservedWhenSilent(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		storedType   string
		incomingType string
	}{
		"type unchanged":    {"solo", "solo"},
		"type absent":       {"solo", ""},
		"stays individual":  {"solo", "person"},
		"stored type empty": {"", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &Artist{Type: tc.storedType, Gender: "female"}
			u := &MetadataUpdate{Type: tc.incomingType, Gender: ""}

			ApplyMetadata(a, u, NFOImport, MergeOptions{})

			if a.Gender != "female" {
				t.Errorf("gender = %q, want %q; the source was silent about gender, "+
					"which is not a statement that the artist has none", a.Gender, "female")
			}
		})
	}
}

// TestTypeConsistency_LockedGenderSurvivesGroupType proves the lock is the
// stronger guarantee, exactly as a locked Born survives FilterDatesByArtistType.
// An operator who pinned Gender keeps it even when the incoming type says the
// artist cannot have one.
//
// The unlocked half is asserted too: without it the test would pass on an
// implementation where the gender pass never ran at all.
func TestTypeConsistency_LockedGenderSurvivesGroupType(t *testing.T) {
	t.Parallel()

	locked := &Artist{Type: "solo", Gender: "female", LockedFields: []string{"gender"}}
	ApplyMetadata(locked, &MetadataUpdate{Type: "group"}, NFOImport, MergeOptions{})
	if locked.Type != "group" {
		t.Errorf("type = %q, want %q; the lock covers gender only", locked.Type, "group")
	}
	if locked.Gender != "female" {
		t.Errorf("locked gender = %q, want %q; a lock outranks type consistency",
			locked.Gender, "female")
	}

	// Negative half: an identical artist without the lock IS cleared, so the
	// assertion above is about the lock and not about an inert pass.
	unlocked := &Artist{Type: "solo", Gender: "female"}
	ApplyMetadata(unlocked, &MetadataUpdate{Type: "group"}, NFOImport, MergeOptions{})
	if unlocked.Gender != "" {
		t.Errorf("unlocked gender = %q, want cleared; the lock in the first half "+
			"protected nothing that was not already safe", unlocked.Gender)
	}
}

// TestTypeConsistency_GenderClearedUnderEveryStrategy pins the pass as a
// property of ApplyMetadata rather than of one strategy: no strategy may leave
// a group carrying a gender.
func TestTypeConsistency_GenderClearedUnderEveryStrategy(t *testing.T) {
	t.Parallel()

	strategies := map[string]MergeStrategy{
		"OverwriteAttempted": OverwriteAttempted,
		"FillEmpty":          FillEmpty,
		"NFOImport":          NFOImport,
		"SnapshotRestore":    SnapshotRestore,
	}
	for name, strategy := range strategies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &Artist{Type: "group", Gender: "female"}
			// The update says nothing at all: the inconsistency is already on
			// the stored artist, so any strategy that reaches the pass must
			// resolve it.
			ApplyMetadata(a, &MetadataUpdate{}, strategy, MergeOptions{})
			if a.Gender != "" {
				t.Errorf("%s: gender = %q, want cleared for a group type", name, a.Gender)
			}
		})
	}
}

// --- the genderless-type allow-list (the #2748 second-round fix) ---

// TestIsGenderlessType_AllowList is the direct guard on the predicate that
// decides whether a stored gender may be DELETED.
//
// THE MAIN CASE IS "Other", NOT AN EDGE CASE. The maintainer reports that
// individuals -- a well-known solo female recording artist was the example --
// routinely sit on the catch-all "Other" type in production data. "Other" means
// UNKNOWN. This table therefore groups it with the empty type, which the pass
// has always spared, and NEVER with the collectives: unknown is not a statement
// that an artist is a group.
//
// It exists because the first attempt at the gender pass expressed the delete
// decision as !IsIndividualType, which returns false by default and so cleared
// the gender of every artist whose type was not one of {solo, person,
// character}. The UNKNOWN block below is the regression block: under
// !IsIndividualType every one of those rows reports true, which is the
// destructive bug. If any of them ever flips to true again, someone re-broadened
// the predicate back to a negation.
func TestIsGenderlessType_AllowList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  string
		want bool
	}{
		// --- COLLECTIVE: positively known to have members, not a gender.
		// The ONLY values that may clear a stored gender.
		{"group", "group", true},
		{"orchestra", "orchestra", true},
		{"choir", "choir", true},
		// Mixed case of the collectives: stored data is known to carry these.
		{"group mixed case", "Group", true},
		{"orchestra mixed case", "Orchestra", true},
		{"choir upper case", "CHOIR", true},
		{"group padded", "  group  ", true},

		// --- INDIVIDUAL: never genderless.
		{"solo", "solo", false},
		{"person", "person", false},
		{"character", "character", false},

		// --- UNKNOWN: the regression block. Every row here is a type that
		// says NOTHING about whether the artist is a person or a collective,
		// so every row must be treated exactly like the empty type and leave
		// the gender alone. "Other" heads the list because it is where real
		// individuals actually live in production data.
		{"MusicBrainz Other -- REAL INDIVIDUALS LIVE HERE", "Other", false},
		{"lowercase other -- REAL INDIVIDUALS LIVE HERE", "other", false},
		{"Unknown", "Unknown", false},
		{"lowercase unknown", "unknown", false},
		{"arbitrary unrecognized string", "tribute band", false},
		{"band is not a stored type value", "band", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsGenderlessType(tt.typ); got != tt.want {
				t.Errorf("IsGenderlessType(%q) = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}

// TestFilterGenderByArtistType_OnlyCollectivesLoseGender runs the same table
// through the pass that actually mutates the artist, so the guard covers the
// observable behavior and not just the predicate. The table is expressed as
// "does the stored gender SURVIVE", which is the property the maintainer cares
// about.
//
// Read the wantSurvives column as three blocks: COLLECTIVE loses gender,
// INDIVIDUAL keeps it, and UNKNOWN keeps it. "Other" is in the UNKNOWN block,
// alongside the empty type and next to nothing else -- that placement IS the
// fix. It is not a hypothetical edge case: the maintainer reports individuals
// routinely sitting on "Other" in production, so this block is the main case.
func TestFilterGenderByArtistType_OnlyCollectivesLoseGender(t *testing.T) {
	t.Parallel()

	const storedGender = "female"

	tests := []struct {
		name         string
		typ          string
		wantSurvives bool
	}{
		// --- COLLECTIVE: gender is genuinely inapplicable, so it is cleared.
		{"group", "group", false},
		{"orchestra", "orchestra", false},
		{"choir", "choir", false},
		{"group mixed case", "Group", false},
		{"orchestra mixed case", "Orchestra", false},
		{"choir upper case", "CHOIR", false},

		// --- INDIVIDUAL: gender applies, so it survives.
		{"solo", "solo", true},
		{"person", "person", true},
		{"character", "character", true},

		// --- UNKNOWN: the type tells us nothing, so we destroy nothing. Every
		// row behaves identically to the empty type at the bottom of the
		// block. "Other" leads it because that is where real individuals -- a
		// solo female recording artist was the maintainer's example -- actually
		// sit in production data. Under the old !IsIndividualType predicate
		// every row in this block came back with gender == "".
		{"MusicBrainz Other -- REAL INDIVIDUALS LIVE HERE", "Other", true},
		{"lowercase other -- REAL INDIVIDUALS LIVE HERE", "other", true},
		{"Unknown", "Unknown", true},
		{"lowercase unknown", "unknown", true},
		{"arbitrary unrecognized string", "tribute band", true},
		{"band is not a stored type value", "band", true},
		{"empty -- the type Other is semantically identical to", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &Artist{Type: tt.typ, Gender: storedGender}
			// Precondition: the gender really is set going in. Without this
			// every "cleared" assertion would pass vacuously on an artist
			// that never had a gender to lose.
			if a.Gender != storedGender {
				t.Fatalf("precondition failed: gender = %q, want %q", a.Gender, storedGender)
			}

			FilterGenderByArtistType(a)

			if tt.wantSurvives && a.Gender != storedGender {
				t.Errorf("gender = %q, want %q preserved; type %q is not positively "+
					"known to be a collective, and unknown is not genderless",
					a.Gender, storedGender, tt.typ)
			}
			if !tt.wantSurvives && a.Gender != "" {
				t.Errorf("gender = %q, want cleared; %q is a collective type",
					a.Gender, tt.typ)
			}
			// The pass touches gender only.
			if a.Type != tt.typ {
				t.Errorf("type = %q, want %q unchanged", a.Type, tt.typ)
			}
		})
	}
}

// TestTypeConsistency_GenderSurvivesOtherTypeThroughApplyMetadata is the
// end-to-end form of the production defect: the maintainer's artist arrives
// through a merge, not through a direct call to the filter. A silent NFO on an
// artist typed "Other" must leave the stored gender intact.
func TestTypeConsistency_GenderSurvivesOtherTypeThroughApplyMetadata(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"Other", "other", "Unknown", "tribute band"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()

			// Precondition: this type is NOT one the model claims is a
			// collective, so any clearing would be the deny-list bug.
			if IsGenderlessType(typ) {
				t.Fatalf("precondition failed: %q is a genderless type", typ)
			}

			a := &Artist{Type: "solo", Gender: "female"}
			ApplyMetadata(a, &MetadataUpdate{Type: typ, Gender: ""}, NFOImport, MergeOptions{})

			if a.Type != typ {
				t.Errorf("type = %q, want %q", a.Type, typ)
			}
			if a.Gender != "female" {
				t.Errorf("gender = %q, want %q; %q is an UNKNOWN type, not a "+
					"collective -- real individuals sit on it in production data",
					a.Gender, "female", typ)
			}
		})
	}
}

// TestTypeConsistency_MixedCaseCollectiveStillClearsGender proves the
// case-insensitive half is load-bearing through the merge and not just in the
// predicate: a stored "Group" is still a group, so it must still lose gender.
func TestTypeConsistency_MixedCaseCollectiveStillClearsGender(t *testing.T) {
	t.Parallel()

	a := &Artist{Type: "solo", Gender: "female"}
	ApplyMetadata(a, &MetadataUpdate{Type: "Group", Gender: ""}, NFOImport, MergeOptions{})

	if a.Type != "Group" {
		t.Errorf("type = %q, want %q", a.Type, "Group")
	}
	if a.Gender != "" {
		t.Errorf("gender = %q, want cleared; %q is a collective regardless of casing", a.Gender, "Group")
	}
}

// TestFilterGenderByArtistType_OtherBehavesExactlyLikeEmpty states the
// semantic claim directly, rather than leaving a reader to infer it from two
// rows of a table: "Other" means UNKNOWN, and UNKNOWN is what an empty type
// already meant. The pass has always spared the empty type; it must spare
// "Other" for the identical reason.
//
// This is the assertion that makes the fix's INTENT reviewable. A future
// change that special-cases "Other" back into the destructive branch would
// leave the tables above partially passing, but cannot survive this.
func TestFilterGenderByArtistType_OtherBehavesExactlyLikeEmpty(t *testing.T) {
	t.Parallel()

	for _, unknownType := range []string{"Other", "other", "Unknown", "tribute band"} {
		t.Run(unknownType, func(t *testing.T) {
			t.Parallel()

			empty := &Artist{Type: "", Gender: "female", Born: "1970"}
			unknown := &Artist{Type: unknownType, Gender: "female", Born: "1970"}

			// Precondition: the two artists differ ONLY in type, so any
			// divergence after the pass is attributable to the type alone.
			if empty.Gender != unknown.Gender || empty.Born != unknown.Born {
				t.Fatalf("precondition failed: fixtures differ in more than type")
			}

			FilterGenderByArtistType(empty)
			FilterGenderByArtistType(unknown)

			if unknown.Gender != empty.Gender {
				t.Errorf("type %q gender = %q, but empty type gender = %q; an UNKNOWN "+
					"type must be treated exactly like an absent one -- neither is a "+
					"statement that the artist is a collective",
					unknownType, unknown.Gender, empty.Gender)
			}
			// Guard against the comparison passing because BOTH were cleared:
			// the empty type is the known-good reference and must be intact.
			if empty.Gender != "female" {
				t.Fatalf("reference artist lost its gender on an empty type (%q); the "+
					"comparison above proves nothing", empty.Gender)
			}
		})
	}
}
