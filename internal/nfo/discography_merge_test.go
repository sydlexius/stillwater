package nfo

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// --- releaseYear ---

func TestReleaseYear_FullDate(t *testing.T) {
	t.Parallel()
	if got := releaseYear("2001-09-11"); got != "2001" {
		t.Errorf("got %q, want 2001", got)
	}
}

func TestReleaseYear_YearMonth(t *testing.T) {
	t.Parallel()
	if got := releaseYear("1991-09"); got != "1991" {
		t.Errorf("got %q, want 1991", got)
	}
}

func TestReleaseYear_YearOnly(t *testing.T) {
	t.Parallel()
	if got := releaseYear("2005"); got != "2005" {
		t.Errorf("got %q, want 2005", got)
	}
}

func TestReleaseYear_Empty(t *testing.T) {
	t.Parallel()
	if got := releaseYear(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReleaseYear_Short(t *testing.T) {
	t.Parallel()
	if got := releaseYear("199"); got != "" {
		t.Errorf("got %q, want empty for short input", got)
	}
}

func TestReleaseYear_NonDigits(t *testing.T) {
	t.Parallel()
	if got := releaseYear("abcd-ef"); got != "" {
		t.Errorf("got %q, want empty for non-digit year", got)
	}
}

// --- ParseReleaseTypeFilter ---

func TestParseReleaseTypeFilter_Default(t *testing.T) {
	t.Parallel()
	f := ParseReleaseTypeFilter("")
	if !f.contains("Album") || !f.contains("EP") {
		t.Errorf("default filter should include Album and EP: %v", f)
	}
	if f.contains("Single") {
		t.Errorf("default filter should not include Single: %v", f)
	}
}

func TestParseReleaseTypeFilter_Custom(t *testing.T) {
	t.Parallel()
	f := ParseReleaseTypeFilter("Album,Single")
	if !f.contains("Album") {
		t.Error("should include Album")
	}
	if !f.contains("Single") {
		t.Error("should include Single")
	}
	if f.contains("EP") {
		t.Error("should not include EP")
	}
}

func TestParseReleaseTypeFilter_CaseInsensitive(t *testing.T) {
	t.Parallel()
	f := ParseReleaseTypeFilter("album,ep")
	if !f.contains("Album") || !f.contains("EP") {
		t.Errorf("filter should match case-insensitively: %v", f)
	}
}

func TestParseReleaseTypeFilter_AllEmptyTokens(t *testing.T) {
	t.Parallel()
	// A string of only separators and whitespace yields no tokens, so the
	// parser falls back to the default filter rather than an empty one.
	f := ParseReleaseTypeFilter(" , , ")
	if !f.contains("Album") || !f.contains("EP") {
		t.Errorf("all-empty input should fall back to the default filter: %v", f)
	}
}

func TestReleaseTypeFilter_NilAcceptsAll(t *testing.T) {
	t.Parallel()
	var f ReleaseTypeFilter // nil: documented "include all" behavior
	if !f.contains("Single") || !f.contains("Bootleg") {
		t.Error("a nil filter should accept every release type")
	}
}

// --- MergeDiscographyFromMBReleaseGroups ---

func releaseGroups(entries ...provider.ReleaseGroupInfo) []provider.ReleaseGroupInfo {
	return entries
}

func rg(id, title, primaryType, date string) provider.ReleaseGroupInfo {
	return provider.ReleaseGroupInfo{
		ID:               id,
		Title:            title,
		PrimaryType:      primaryType,
		FirstReleaseDate: date,
	}
}

func album(title, year, mbid string) DiscographyAlbum {
	return DiscographyAlbum{
		Title:                     title,
		Year:                      year,
		MusicBrainzReleaseGroupID: mbid,
	}
}

// TestMerge_EmptyNFO verifies adding to an empty NFO.
func TestMerge_EmptyNFO(t *testing.T) {
	t.Parallel()
	groups := releaseGroups(
		rg("mbid-1", "Bleach", "Album", "1989"),
		rg("mbid-2", "Nevermind", "Album", "1991-09-24"),
	)
	merged, res := MergeDiscographyFromMBReleaseGroups(nil, groups, DefaultReleaseTypeFilter())

	if res.Added != 2 {
		t.Errorf("Added = %d, want 2", res.Added)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2", res.Total)
	}
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	if merged[0].Title != "Bleach" || merged[0].Year != "1989" {
		t.Errorf("merged[0] = %+v", merged[0])
	}
	if merged[1].Year != "1991" {
		t.Errorf("merged[1].Year = %q, want 1991", merged[1].Year)
	}
}

// TestMerge_PreservesExistingByMBID checks that existing entries win on MBID conflict.
func TestMerge_PreservesExistingByMBID(t *testing.T) {
	t.Parallel()
	existing := []DiscographyAlbum{
		album("My Custom Title", "1991", "mbid-1"),
	}
	groups := releaseGroups(
		rg("mbid-1", "Nevermind (incoming)", "Album", "1991"),
	)
	merged, res := MergeDiscographyFromMBReleaseGroups(existing, groups, DefaultReleaseTypeFilter())

	if res.Kept != 1 || res.Added != 0 {
		t.Errorf("Kept=%d Added=%d, want Kept=1 Added=0", res.Kept, res.Added)
	}
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
	// The existing title is preserved, not the incoming one.
	if merged[0].Title != "My Custom Title" {
		t.Errorf("existing entry was overwritten; got %q", merged[0].Title)
	}
}

// TestMerge_PreservesUserAddedAlbums checks that entries without an MBID are kept.
func TestMerge_PreservesUserAddedAlbums(t *testing.T) {
	t.Parallel()
	existing := []DiscographyAlbum{
		album("User Album", "2000", ""), // no MBID
	}
	groups := releaseGroups(
		rg("mbid-1", "New Album", "Album", "2005"),
	)
	merged, res := MergeDiscographyFromMBReleaseGroups(existing, groups, DefaultReleaseTypeFilter())

	if res.Added != 1 {
		t.Errorf("Added = %d, want 1", res.Added)
	}
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	// User album must come first.
	if merged[0].Title != "User Album" {
		t.Errorf("user album not first; merged[0] = %+v", merged[0])
	}
	if merged[1].Title != "New Album" {
		t.Errorf("merged[1] = %+v, want New Album", merged[1])
	}
}

// TestMerge_TypeFilterSkipsUnwanted verifies that release types outside the
// filter are counted as Skipped and not added.
func TestMerge_TypeFilterSkipsUnwanted(t *testing.T) {
	t.Parallel()
	groups := releaseGroups(
		rg("mbid-1", "Album One", "Album", "2001"),
		rg("mbid-2", "Single One", "Single", "2001"),
		rg("mbid-3", "EP One", "EP", "2002"),
	)
	filter := ParseReleaseTypeFilter("Album")
	merged, res := MergeDiscographyFromMBReleaseGroups(nil, groups, filter)

	if res.Added != 1 {
		t.Errorf("Added = %d, want 1 (only Album)", res.Added)
	}
	// Single and EP are skipped.
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", res.Skipped)
	}
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
}

// TestMerge_NoMBIDError verifies that the function handles groups with no ID gracefully.
func TestMerge_NoIDGroup(t *testing.T) {
	t.Parallel()
	groups := releaseGroups(
		rg("", "No ID Album", "Album", "2003"),
	)
	merged, res := MergeDiscographyFromMBReleaseGroups(nil, groups, DefaultReleaseTypeFilter())

	if res.Added != 1 {
		t.Errorf("Added = %d, want 1", res.Added)
	}
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
	if merged[0].MusicBrainzReleaseGroupID != "" {
		t.Errorf("expected empty MBID for no-id group, got %q", merged[0].MusicBrainzReleaseGroupID)
	}
}

// TestMerge_PartialNFO checks merging with a mix of tagged and untagged existing entries.
func TestMerge_PartialNFO(t *testing.T) {
	t.Parallel()
	existing := []DiscographyAlbum{
		album("Manual Album", "1995", ""),       // no MBID
		album("Tagged Album", "1998", "mbid-a"), // has MBID
	}
	groups := releaseGroups(
		rg("mbid-a", "Tagged Album (incoming)", "Album", "1998"),
		rg("mbid-b", "New Album", "Album", "2000"),
	)
	merged, res := MergeDiscographyFromMBReleaseGroups(existing, groups, DefaultReleaseTypeFilter())

	if res.Added != 1 {
		t.Errorf("Added = %d, want 1", res.Added)
	}
	if res.Kept != 1 {
		t.Errorf("Kept = %d, want 1", res.Kept)
	}
	// 3 entries: Manual, Tagged (existing wins), New Album
	if len(merged) != 3 {
		t.Fatalf("len(merged) = %d, want 3; merged = %+v", len(merged), merged)
	}
	if merged[0].Title != "Manual Album" {
		t.Errorf("merged[0] should be Manual Album, got %q", merged[0].Title)
	}
	if merged[1].Title != "Tagged Album" {
		t.Errorf("merged[1] should be Tagged Album (existing), got %q", merged[1].Title)
	}
	if merged[2].Title != "New Album" {
		t.Errorf("merged[2] should be New Album, got %q", merged[2].Title)
	}
}

// TestMerge_PartialResponsePreservesExistingMBID verifies that an existing MBID
// album survives a merge when the incoming set does NOT include that MBID (i.e.,
// MusicBrainz returned a partial response). The album must remain in merged even
// though it was not matched by any incoming release group.
func TestMerge_PartialResponsePreservesExistingMBID(t *testing.T) {
	t.Parallel()
	existing := []DiscographyAlbum{
		album("Old Album", "2000", "mbid-existing"), // present in NFO, absent from MB response
	}
	groups := releaseGroups(
		rg("mbid-new", "New Album", "Album", "2010"), // new entry, no overlap with existing
	)
	merged, res := MergeDiscographyFromMBReleaseGroups(existing, groups, DefaultReleaseTypeFilter())

	if res.Added != 1 {
		t.Errorf("Added = %d, want 1", res.Added)
	}
	if res.Kept != 0 {
		t.Errorf("Kept = %d, want 0 (no MBID match in incoming)", res.Kept)
	}
	// Both the existing album (not in incoming set) and the new album must be present.
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2; partial response must not drop existing MBID album; merged = %+v", len(merged), merged)
	}
	if merged[0].MusicBrainzReleaseGroupID != "mbid-existing" {
		t.Errorf("merged[0] should be existing album (mbid-existing), got %+v", merged[0])
	}
	if merged[1].Title != "New Album" {
		t.Errorf("merged[1] should be New Album, got %+v", merged[1])
	}
}

// TestMerge_TotalCountMatchesInput verifies Total equals len(groups).
func TestMerge_TotalCountMatchesInput(t *testing.T) {
	t.Parallel()
	groups := releaseGroups(
		rg("a", "A", "Album", ""),
		rg("b", "B", "Single", ""),
		rg("c", "C", "EP", ""),
	)
	_, res := MergeDiscographyFromMBReleaseGroups(nil, groups, DefaultReleaseTypeFilter())

	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
}

// --- ReleaseTypeFilter.Includes / CountReleaseGroups ---

// TestReleaseTypeFilter_Includes verifies the exported, case-insensitive
// membership check used by the discography_populated rule checker.
func TestReleaseTypeFilter_Includes(t *testing.T) {
	t.Parallel()
	f := ReleaseTypeFilter{"Album", "EP"}
	if !f.Includes("album") {
		t.Error("Includes should be case-insensitive: album should match Album")
	}
	if !f.Includes("EP") {
		t.Error("EP should be included")
	}
	if f.Includes("Single") {
		t.Error("Single should not be included by an Album,EP filter")
	}
}

// TestReleaseTypeFilter_Includes_NilAcceptsAll verifies a nil/empty filter
// accepts every primary type.
func TestReleaseTypeFilter_Includes_NilAcceptsAll(t *testing.T) {
	t.Parallel()
	var f ReleaseTypeFilter
	if !f.Includes("Single") || !f.Includes("Compilation") {
		t.Error("a nil filter should accept every release type")
	}
}

// TestReleaseTypeFilter_CountReleaseGroups counts only the release groups whose
// primary type the filter accepts, so the discography coverage ratio is
// measured against the same set the merge would apply.
func TestReleaseTypeFilter_CountReleaseGroups(t *testing.T) {
	t.Parallel()
	groups := releaseGroups(
		rg("a", "A", "Album", ""),
		rg("b", "B", "Single", ""),
		rg("c", "C", "EP", ""),
		rg("d", "D", "Album", ""),
		rg("e", "E", "Compilation", ""),
	)
	f := ReleaseTypeFilter{"Album", "EP"}
	if got := f.CountReleaseGroups(groups); got != 3 {
		t.Errorf("CountReleaseGroups = %d, want 3 (2 Album + 1 EP)", got)
	}

	// A nil filter counts every group.
	var all ReleaseTypeFilter
	if got := all.CountReleaseGroups(groups); got != 5 {
		t.Errorf("nil-filter CountReleaseGroups = %d, want 5", got)
	}
}
