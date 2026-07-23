package artist

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The tests in this file guard the issue #2749 contract: a per-field lock the
// operator applied to an artist is honored by ApplyMetadata on EVERY path and
// for EVERY strategy, WITHOUT the caller having to populate
// MergeOptions.LockedFields. Every case below therefore passes a zero
// MergeOptions and relies solely on Artist.LockedFields.
//
// The production failure this reproduces: an operator set a field by hand, a
// library scan (NFOImport, zero MergeOptions) erased it, and the per-field
// lock the operator then applied did nothing because the scan path never
// passed it through.

// TestApplyMetadata_ArtistLock_NFOImport_OmittedElement is the exact
// production failure. Under NFOImport, years_active is modeUnconditional, so
// an NFO that omits <yearsactive> merges an empty string over the operator's
// value. The lock must stop that.
func TestApplyMetadata_ArtistLock_NFOImport_OmittedElement(t *testing.T) {
	t.Parallel()
	a := &Artist{
		YearsActive:  "1988-present",
		Genres:       []string{"trip hop"},
		LockedFields: []string{"years_active", "genres"},
	}
	// An NFO with no <yearsactive> and no <genre> elements parses to zero values.
	u := &MetadataUpdate{Name: "From NFO"}

	ApplyMetadata(a, u, NFOImport, MergeOptions{})

	if a.YearsActive != "1988-present" {
		t.Errorf("locked years_active cleared by NFO that omits the element: got %q, want %q",
			a.YearsActive, "1988-present")
	}
	if len(a.Genres) != 1 || a.Genres[0] != "trip hop" {
		t.Errorf("locked genres cleared by NFO that omits the element: got %v", a.Genres)
	}
}

// TestApplyMetadata_ArtistLock_NFOImport_DifferentValue covers the other half
// of the NFO case: the NFO carries a value, just not the operator's.
func TestApplyMetadata_ArtistLock_NFOImport_DifferentValue(t *testing.T) {
	t.Parallel()
	a := &Artist{
		YearsActive:    "1988-present",
		Disambiguation: "Bristol group",
		Genres:         []string{"trip hop"},
		LockedFields:   []string{"years_active", "disambiguation", "genres"},
	}
	u := &MetadataUpdate{
		YearsActive:    "1991-2010",
		Disambiguation: "from the nfo",
		Genres:         []string{"electronic"},
	}

	ApplyMetadata(a, u, NFOImport, MergeOptions{})

	if a.YearsActive != "1988-present" {
		t.Errorf("locked years_active overwritten by NFO value: got %q", a.YearsActive)
	}
	if a.Disambiguation != "Bristol group" {
		t.Errorf("locked disambiguation overwritten by NFO value: got %q", a.Disambiguation)
	}
	if len(a.Genres) != 1 || a.Genres[0] != "trip hop" {
		t.Errorf("locked genres overwritten by NFO value: got %v", a.Genres)
	}
}

// TestApplyMetadata_ArtistLock_FillEmpty_DoesNotFill covers the bulk-rule
// path. A locked field that is currently EMPTY must stay empty: the operator
// deliberately blanked it, and FillEmpty would otherwise repopulate it from a
// provider on the next bulk run.
func TestApplyMetadata_ArtistLock_FillEmpty_DoesNotFill(t *testing.T) {
	t.Parallel()
	a := &Artist{
		Biography:    "",
		Genres:       nil,
		LockedFields: []string{"biography", "genres"},
	}
	u := &MetadataUpdate{Biography: "from provider", Genres: []string{"rock"}}

	ApplyMetadata(a, u, FillEmpty, MergeOptions{})

	if a.Biography != "" {
		t.Errorf("locked empty biography was filled: got %q", a.Biography)
	}
	if len(a.Genres) != 0 {
		t.Errorf("locked empty genres were filled: got %v", a.Genres)
	}
}

// TestApplyMetadata_ArtistLock_CaseInsensitive proves the documented
// case-insensitive comparison applies to the artist-derived set too, including
// surrounding whitespace, since stored lock tokens come from several writers
// (UI, platform pull, API) with differing casing conventions.
func TestApplyMetadata_ArtistLock_CaseInsensitive(t *testing.T) {
	t.Parallel()
	a := &Artist{
		YearsActive:  "1988-present",
		Genres:       []string{"trip hop"},
		LockedFields: []string{"Years_Active", "  GENRES  "},
	}
	u := &MetadataUpdate{YearsActive: "1991-2010", Genres: []string{"electronic"}}

	ApplyMetadata(a, u, NFOImport, MergeOptions{})

	if a.YearsActive != "1988-present" {
		t.Errorf("mixed-case lock token not honored: years_active = %q", a.YearsActive)
	}
	if len(a.Genres) != 1 || a.Genres[0] != "trip hop" {
		t.Errorf("padded upper-case lock token not honored: genres = %v", a.Genres)
	}
}

// TestApplyMetadata_ArtistLock_UnlockedFieldsStillMerge is the anti-regression
// guard: the fix must not turn into "stop merging". Fields absent from
// LockedFields must merge exactly as before on both affected paths.
func TestApplyMetadata_ArtistLock_UnlockedFieldsStillMerge(t *testing.T) {
	t.Parallel()

	t.Run("NFOImport", func(t *testing.T) {
		t.Parallel()
		a := &Artist{
			YearsActive:  "1988-present",
			Biography:    "old bio",
			LockedFields: []string{"years_active"},
		}
		u := &MetadataUpdate{YearsActive: "1991-2010", Biography: "new bio"}

		if !ApplyMetadata(a, u, NFOImport, MergeOptions{}) {
			t.Fatal("ApplyMetadata reported no change, want true for the unlocked biography")
		}
		if a.Biography != "new bio" {
			t.Errorf("unlocked biography did not merge: got %q", a.Biography)
		}
		if a.YearsActive != "1988-present" {
			t.Errorf("locked years_active overwritten: got %q", a.YearsActive)
		}
	})

	t.Run("FillEmpty", func(t *testing.T) {
		t.Parallel()
		a := &Artist{
			Biography:    "",
			Origin:       "",
			LockedFields: []string{"biography"},
		}
		u := &MetadataUpdate{Biography: "from provider", Origin: "Bristol, UK"}

		if !ApplyMetadata(a, u, FillEmpty, MergeOptions{}) {
			t.Fatal("ApplyMetadata reported no change, want true for the unlocked origin")
		}
		if a.Origin != "Bristol, UK" {
			t.Errorf("unlocked origin did not fill: got %q", a.Origin)
		}
		if a.Biography != "" {
			t.Errorf("locked biography was filled: got %q", a.Biography)
		}
	})
}

// TestApplyMetadata_ArtistLock_UnionsWithOptions proves the artist-derived set
// ADDS to MergeOptions.LockedFields rather than replacing it, so the
// call-scoped escape hatch keeps working for callers that use it.
func TestApplyMetadata_ArtistLock_UnionsWithOptions(t *testing.T) {
	t.Parallel()
	a := &Artist{
		YearsActive:  "1988-present",
		Origin:       "Bristol, UK",
		LockedFields: []string{"years_active"},
	}
	u := &MetadataUpdate{YearsActive: "1991-2010", Origin: "elsewhere"}

	ApplyMetadata(a, u, NFOImport, MergeOptions{LockedFields: []string{"origin"}})

	if a.YearsActive != "1988-present" {
		t.Errorf("artist-derived lock lost when opts.LockedFields was set: got %q", a.YearsActive)
	}
	if a.Origin != "Bristol, UK" {
		t.Errorf("opts.LockedFields lock lost: got %q", a.Origin)
	}
}

// TestApplyMetadata_NilArtist guards the nil-receiver path added alongside the
// artist-derived lock read: ApplyMetadata now dereferences the artist before
// walking the field tables, so a nil must return false rather than panic.
func TestApplyMetadata_NilArtist(t *testing.T) {
	t.Parallel()
	if ApplyMetadata(nil, &MetadataUpdate{Name: "x"}, NFOImport, MergeOptions{}) {
		t.Error("ApplyMetadata(nil artist) = true, want false")
	}
}

// TestApplyMetadata_UnenforceableLockLogsError covers the no-silent-failure
// requirement: a stored lock token that matches no known lockable field
// protects nothing anywhere, so it must be reported loudly instead of being
// dropped. Not parallel: it swaps the process-wide default slog handler.
func TestApplyMetadata_UnenforceableLockLogsError(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	a := &Artist{
		ID:           "artist-1",
		YearsActive:  "1988-present",
		LockedFields: []string{"years_active", "not_a_real_field"},
	}
	ApplyMetadata(a, &MetadataUpdate{YearsActive: "1991-2010"}, NFOImport, MergeOptions{})

	out := buf.String()
	if !strings.Contains(out, "not_a_real_field") {
		t.Errorf("unknown lock token not reported; log output = %q", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("unknown lock token not reported at ERROR level; log output = %q", out)
	}
	// The enforceable lock in the same set must still be honored.
	if a.YearsActive != "1988-present" {
		t.Errorf("valid lock dropped because a sibling token was unknown: got %q", a.YearsActive)
	}
}

// TestApplyMetadata_KnownLocksLogNothing is the false-positive guard for the
// test above. "members" is lockable but is a separate relation rather than a
// merge-table column, so it must not be reported as unknown. Not parallel: it
// swaps the process-wide default slog handler.
func TestApplyMetadata_KnownLocksLogNothing(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	a := &Artist{LockedFields: []string{"members", "musicbrainz_id", "years_active"}}
	ApplyMetadata(a, &MetadataUpdate{Name: "x"}, NFOImport, MergeOptions{})

	if out := buf.String(); out != "" {
		t.Errorf("known lock tokens were reported as unenforceable: %q", out)
	}
}

// TestAllLockableFields_CoversFieldNameConstants keeps the vocabulary list
// honest: every FieldName constant must appear in AllLockableFields, or a
// newly added lockable field would be reported as unknown at runtime.
func TestAllLockableFields_CoversFieldNameConstants(t *testing.T) {
	t.Parallel()
	// Enumerated by hand rather than by reflection: the point is to fail when
	// someone adds a constant above and forgets the slice.
	want := []FieldName{
		FieldArtistName, FieldSortName, FieldBiography, FieldGenres, FieldStyles,
		FieldMoods, FieldMembers, FieldType, FieldGender, FieldOrigin,
		FieldDisambiguation, FieldFormed, FieldBorn, FieldDied, FieldDisbanded,
		FieldYearsActive, FieldDiscogsID, FieldAudioDBID,
	}
	if len(AllLockableFields) != len(want) {
		t.Fatalf("AllLockableFields has %d entries, want %d -- add the new constant to the slice",
			len(AllLockableFields), len(want))
	}
	for _, f := range want {
		if _, ok := lockableFieldNames[strings.ToLower(string(f))]; !ok {
			t.Errorf("lockable field %q missing from the lock vocabulary", f)
		}
	}
}
