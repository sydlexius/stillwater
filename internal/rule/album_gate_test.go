package rule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// These tests cover the #2858 album-evidence gate on BOTH internal/rule
// MusicBrainz-ID write paths: MetadataFixer.fixMBID (the nfo_has_mbid rule) and
// BulkExecutor.selfHealMBID (reached through fetchImages).
//
// Every fixture is env-independent: the "local albums" come from a t.TempDir
// with real subdirectories, and the candidate's catalogue comes from the
// existing stubReleaseGroupFetcher. No network, no music library, no binary on
// PATH.
//
// The fixture MBID and search-result shapes deliberately reuse the constants
// and stubs from fixers_mbid_confidence_test.go and bulk_executor_mbid_gate_test.go
// so every case here clears artist.EvaluateMBIDCandidate and can only be
// decided by the ALBUM gate. Each test asserts that precondition explicitly --
// without it, a candidate rejected on score or name similarity would produce
// the same "not adopted" outcome and the test would pass vacuously, proving
// nothing about the album gate at all.

// gateArtistName is the fixture artist's name, shared by the search results and
// every artist built below so the name gates always see an exact match.
const gateArtistName = "Radiohead"

// gateAlbums are the local album directory names used across these tests. Ten
// of them, so a single matching remote title moves the overlap by exactly 10
// percentage points and each threshold band can be hit exactly rather than
// approached.
var gateAlbums = []string{
	"Pablo Honey", "The Bends", "OK Computer", "Kid A", "Amnesiac",
	"Hail to the Thief", "In Rainbows", "The King of Limbs", "A Moon Shaped Pool", "Live Sessions",
}

// gateReleaseGroups returns the first n album titles as MusicBrainz release
// groups, so the candidate's catalogue overlaps the local one by exactly n*10
// percent. Padding beyond the local set would change the overlap denominator,
// so it is deliberately a prefix of the same list.
func gateReleaseGroups(n int) []provider.ReleaseGroupInfo {
	groups := make([]provider.ReleaseGroupInfo, 0, n)
	for _, title := range gateAlbums[:n] {
		groups = append(groups, provider.ReleaseGroupInfo{Title: title})
	}
	return groups
}

// gateArtistDir creates an artist directory holding one subdirectory per album
// name and returns its path. An empty list yields an existing but empty
// directory, which is the EvidenceNone shape (a real determination), NOT the
// EvidenceUnknown shape.
func gateArtistDir(t *testing.T, albums []string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range albums {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("seeding album directory %q: %v", name, err)
		}
	}
	return dir
}

// assertLocalEvidence pins the PRECONDITION that the fixture really is in the
// evidence state the test believes it is. Without this, a typo in the temp-dir
// setup silently turns an "overlap is too low" test into an "evidence was
// unknown so it allowed" test, which passes for the wrong reason.
func assertLocalEvidence(t *testing.T, path string, want artist.AlbumEvidence, wantTitles int) {
	t.Helper()
	set, _ := artist.NewFilesystemAlbumSource().LocalAlbums(context.Background(), &artist.Artist{Name: "fixture", Path: path})
	if set.Evidence != want {
		t.Fatalf("fixture precondition: local album evidence = %s, want %s", set.Evidence, want)
	}
	if len(set.Titles) != wantTitles {
		t.Fatalf("fixture precondition: local album count = %d, want %d", len(set.Titles), wantTitles)
	}
}

// assertNameGatesPass pins the other half of the precondition: the candidate
// must clear artist.EvaluateMBIDCandidate, so the ONLY thing that can decline
// it in the tests below is the album gate.
func assertNameGatesPass(t *testing.T, results []provider.ArtistSearchResult) {
	t.Helper()
	best, runnerUp := artist.BestMBIDCandidates(results)
	if best == nil {
		t.Fatalf("fixture precondition: no usable candidate in the search results")
	}
	if rej := artist.EvaluateMBIDCandidate(gateArtistName, best, runnerUp); rej != nil {
		t.Fatalf("fixture precondition: the candidate must clear the name gates so only the album gate can decline it; it was rejected for %q", rej.Reason)
	}
}

// gateSearchResults is the single confident, uncontested hit every album-gate
// test searches with. Isolated in one place so a change to the name gates
// cannot silently make half these tests vacuous.
func gateSearchResults() []provider.ArtistSearchResult {
	return []provider.ArtistSearchResult{
		{Name: gateArtistName, MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
	}
}

// gateRedFlagResults is the same confident hit carrying a MusicBrainz
// disambiguation string that artist.CandidateRedFlag recognizes.
func gateRedFlagResults() []provider.ArtistSearchResult {
	return []provider.ArtistSearchResult{
		{Name: gateArtistName, MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz", Disambiguation: "tribute band"},
	}
}

// ---------------------------------------------------------------------------
// Path 1: MetadataFixer.fixMBID (the nfo_has_mbid rule fixer)
// ---------------------------------------------------------------------------

// gatedMBIDFixer builds a MetadataFixer whose album gate is wired with the real
// filesystem album source and the given stub release-group fetcher.
func gatedMBIDFixer(t *testing.T, fetcher ReleaseGroupFetcher, results []provider.ArtistSearchResult) *MetadataFixer {
	t.Helper()
	f := &MetadataFixer{orchestrator: &stubMBIDSearchOrchestrator{results: results}, logger: testLogger()}
	f.SetAlbumGate(artist.NewFilesystemAlbumSource(), fetcher)
	return f
}

// TestFixMBID_AlbumGate_AllowsWhenNoLocalAlbumsReadable is the FAIL-OPEN
// regression guard on the local side: an artist with no recorded path is in
// EvidenceUnknown, which is roughly 43% of a production library. Declining
// those would silently stop nfo_has_mbid fixing anything for most of the
// library, so the gate must ALLOW.
func TestFixMBID_AlbumGate_AllowsWhenNoLocalAlbumsReadable(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	// A wired, WORKING fetcher: this test must fail open on the LOCAL side, not
	// because the candidate half was unavailable too.
	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(0)}
	f := gatedMBIDFixer(t, fetcher, results)

	// No Path recorded -> EvidenceUnknown. Asserted rather than assumed.
	a := &artist.Artist{Name: gateArtistName}
	set, _ := artist.NewFilesystemAlbumSource().LocalAlbums(context.Background(), a)
	if set.Evidence != artist.EvidenceUnknown {
		t.Fatalf("fixture precondition: evidence = %s, want unknown", set.Evidence)
	}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected the write to be ALLOWED with no readable local albums, got Fixed=false (message: %q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
	// The gate must not have spent a provider call: with no local evidence there
	// is nothing to compare the candidate's catalogue against.
	if fetcher.calls != 0 {
		t.Errorf("release-group fetches = %d, want 0: with no local evidence there is nothing to compare", fetcher.calls)
	}
}

// TestFixMBID_AlbumGate_AllowsWhenFetcherIsNil is THE regression guard of this
// issue (trap 1). RuleNFOHasMBID is absent from ruleProviderCapabilities, and an
// install may have no MusicBrainz provider registered at all, so a nil fetcher
// is the normal case rather than an exotic one. A gate that declined here would
// make MusicBrainz a precondition for auto-link and silently disable the rule
// it is supposed to guard.
func TestFixMBID_AlbumGate_AllowsWhenFetcherIsNil(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	// PRECONDITION: local evidence is FOUND and rich. This is what makes the
	// test meaningful -- the artist has ten readable albums, so the ONLY reason
	// the gate cannot decide is the missing fetcher.
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	f := gatedMBIDFixer(t, nil, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected the write to be ALLOWED with a nil release-group fetcher, got Fixed=false (message: %q). "+
			"This is the #2858 regression: nfo_has_mbid must keep working on an install with no MusicBrainz provider.", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
}

// TestFixMBID_AlbumGate_AllowsWhenReleaseGroupFetchFails is the fail-open guard
// on the CANDIDATE side. A MusicBrainz outage must not read as "this candidate
// has an empty catalogue" and start refusing every write.
func TestFixMBID_AlbumGate_AllowsWhenReleaseGroupFetchFails(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	fetcher := &stubReleaseGroupFetcher{err: errors.New("musicbrainz unreachable")}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected the write to be ALLOWED when the release-group fetch fails, got Fixed=false (message: %q)", fr.Message)
	}
	// PRECONDITION on the assertion itself: the fetch must genuinely have been
	// attempted, or this would be re-testing the nil-fetcher case.
	if fetcher.calls != 1 {
		t.Errorf("release-group fetches = %d, want 1: the fetch must actually have been attempted", fetcher.calls)
	}
}

// TestFixMBID_AlbumGate_AllowsWhenArtistHasNoLocalAlbums covers EvidenceNone: a
// real determination that the artist has no albums. There is no catalogue to
// contradict the candidate, so under the fail-open policy this allows -- which
// is the deliberate difference from the internal/api identify tiers, where the
// same state declines.
func TestFixMBID_AlbumGate_AllowsWhenArtistHasNoLocalAlbums(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, nil)
	// An EXISTING but EMPTY directory is EvidenceNone, not EvidenceUnknown. The
	// two are the pair this whole subsystem exists to keep apart, so the
	// distinction is asserted rather than assumed.
	assertLocalEvidence(t, dir, artist.EvidenceNone, 0)

	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(10)}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected the write to be ALLOWED for an artist with no local albums, got Fixed=false (message: %q)", fr.Message)
	}
}

// TestFixMBID_AlbumGate_AllowsOnHighOverlap is the corroborated happy path: the
// candidate's catalogue agrees with the local albums at or above the auto-link
// floor, so the write proceeds.
func TestFixMBID_AlbumGate_AllowsOnHighOverlap(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	// 8 of 10 local albums present remotely = 80%, at or above
	// AlbumOverlapAutoLinkFloor (70).
	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(8)}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected the write to be ALLOWED at 80%% overlap, got Fixed=false (message: %q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
	if fetcher.calls != 1 {
		t.Errorf("release-group fetches = %d, want 1", fetcher.calls)
	}
}

// TestFixMBID_AlbumGate_BlocksOnMidOverlap covers the review band: real
// evidence, not enough of it. The candidate is withheld from the unattended
// write; the violation stays open so an operator can still link by hand.
func TestFixMBID_AlbumGate_BlocksOnMidOverlap(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	// 5 of 10 = 50%: at or above AlbumOverlapReviewFloor (30), below
	// AlbumOverlapAutoLinkFloor (70).
	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(5)}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("expected the write to be BLOCKED at 50%% overlap, got Fixed=true")
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: a blocked candidate must not be written", a.MusicBrainzID)
	}
	// A blocked candidate must NOT be stamped as machine-picked: that marker is
	// the recovery query for IDs this rule actually adopted.
	if _, ok := a.MetadataSources[artist.SourceKeyMusicBrainzID]; ok {
		t.Errorf("a blocked candidate must not stamp MBID provenance, got %v", a.MetadataSources)
	}
	if !strings.Contains(fr.Message, "overlap") {
		t.Errorf("message %q does not name the catalogue-overlap reason", fr.Message)
	}
}

// TestFixMBID_AlbumGate_BlocksOnLowOverlap is the measured #2828 shape one step
// short of an empty stub: the candidate has a catalogue, and it disagrees.
func TestFixMBID_AlbumGate_BlocksOnLowOverlap(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	// 1 of 10 = 10%, below AlbumOverlapReviewFloor (30).
	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("expected the write to be BLOCKED at 10%% overlap, got Fixed=true")
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
}

// TestFixMBID_AlbumGate_BlocksOnEmptyCandidateCatalogue is the EXACT measured
// failure behind #2828: an empty MusicBrainz stub matches the name at 100% and
// owns no catalogue that could contradict it, while the local artist has a full
// album directory. All 18 of 18 wrong adoptions had this shape.
func TestFixMBID_AlbumGate_BlocksOnEmptyCandidateCatalogue(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	// A SUCCESSFUL fetch returning zero release groups -- distinct from the
	// failed fetch above, which allows.
	fetcher := &stubReleaseGroupFetcher{groups: nil}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("expected the write to be BLOCKED for an empty-catalogue candidate, got Fixed=true")
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
	if !strings.Contains(fr.Message, "no release groups") {
		t.Errorf("message %q does not name the empty-catalogue reason", fr.Message)
	}
}

// TestFixMBID_AlbumGate_BlocksOnRedFlag covers the secondary disqualifier: a
// tribute act's catalogue legitimately overlaps the original's, so the overlap
// check alone cannot catch it. The disambiguation string can, and it routes the
// candidate to a human rather than throwing it away.
func TestFixMBID_AlbumGate_BlocksOnRedFlag(t *testing.T) {
	results := gateRedFlagResults()
	assertNameGatesPass(t, results)
	// PRECONDITION: the fixture's disambiguation really is one the red-flag
	// matcher recognizes. Without this the test would pass on a plain
	// high-overlap allow if the word list ever changed.
	if flag := artist.CandidateRedFlag(&results[0]); flag == "" {
		t.Fatalf("fixture precondition: the candidate must carry a recognized red flag")
	}

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	// 10 of 10 = 100% overlap, so ONLY the red flag can block this.
	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(10)}
	f := gatedMBIDFixer(t, fetcher, results)
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("expected the write to be BLOCKED for a red-flagged candidate at 100%% overlap, got Fixed=true")
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
	if !strings.Contains(fr.Message, "tribute") {
		t.Errorf("message %q does not name the red flag", fr.Message)
	}
}

// TestFixMBID_AlbumGate_UnwiredGateIsInert pins the zero value. Every existing
// construction site (and the many tests that build a bare MetadataFixer)
// carries no gate at all, and must keep behaving exactly as it did before
// #2858 rather than silently refusing every write.
func TestFixMBID_AlbumGate_UnwiredGateIsInert(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	// No SetAlbumGate call at all.
	f := &MetadataFixer{orchestrator: &stubMBIDSearchOrchestrator{results: results}, logger: testLogger()}
	a := &artist.Artist{Name: gateArtistName, Path: dir}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("an unwired gate must permit every candidate, got Fixed=false (message: %q)", fr.Message)
	}
}

// ---------------------------------------------------------------------------
// Path 2: BulkExecutor.fetchImages (the bulk image job's MBID self-heal)
// ---------------------------------------------------------------------------

// bulkGateArtist points the executor's fixture artist at an album directory and
// returns the executor plus the artist. It reuses newBulkGateExecutor so the
// self-heal branch is reached on exactly the same terms the #2825 tests use.
func bulkGateArtist(t *testing.T, albums []string, fetcher ReleaseGroupFetcher, results []provider.ArtistSearchResult) (*BulkExecutor, *artist.Service, *artist.Artist) {
	t.Helper()
	e, artistSvc, _, a := newBulkGateExecutor(t, results)
	if albums == nil {
		// Explicitly no path: the EvidenceUnknown shape.
		a.Path = ""
	} else {
		a.Path = gateArtistDir(t, albums)
	}
	e.SetAlbumGate(artist.NewFilesystemAlbumSource(), fetcher)
	return e, artistSvc, a
}

// assertBulkAdopted checks the full adoption outcome: in-memory, persisted, and
// stamped. Reading the row back matters because fetchImages mutates the
// in-memory artist BEFORE the write, so an in-memory-only assertion cannot tell
// an adoption from a rolled-back one.
func assertBulkAdopted(t *testing.T, svc *artist.Service, a *artist.Artist, status, msg string) {
	t.Helper()
	if a.MusicBrainzID != mbidRadiohead {
		t.Fatalf("a.MusicBrainzID = %q, want %q (status %q, message %q)", a.MusicBrainzID, mbidRadiohead, status, msg)
	}
	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if reloaded.MusicBrainzID != mbidRadiohead {
		t.Errorf("persisted MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, mbidRadiohead)
	}
}

// assertBulkNotAdopted checks the full refusal outcome, in memory and on disk.
func assertBulkNotAdopted(t *testing.T, svc *artist.Service, a *artist.Artist, status, msg string) {
	t.Helper()
	if status != BulkItemSkipped {
		t.Fatalf("status = %q, want %q (message: %q)", status, BulkItemSkipped, msg)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: a blocked candidate must not be adopted", a.MusicBrainzID)
	}
	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if reloaded.MusicBrainzID != "" {
		t.Errorf("persisted MusicBrainzID = %q, want empty", reloaded.MusicBrainzID)
	}
}

// TestFetchImages_AlbumGate_AllowsWhenNoLocalAlbumsReadable is the bulk-side
// fail-open guard on the local half: no recorded path is EvidenceUnknown, and
// declining it would stop the bulk job self-healing most of a library.
func TestFetchImages_AlbumGate_AllowsWhenNoLocalAlbumsReadable(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(0)}
	e, svc, a := bulkGateArtist(t, nil, fetcher, results)
	if a.Path != "" {
		t.Fatalf("fixture precondition: artist must have no path, got %q", a.Path)
	}

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	if status != "" {
		t.Fatalf("expected the self-heal to be ALLOWED with no readable local albums, got status %q (message: %q)", status, msg)
	}
	assertBulkAdopted(t, svc, a, status, msg)
	if fetcher.calls != 0 {
		t.Errorf("release-group fetches = %d, want 0", fetcher.calls)
	}
}

// TestFetchImages_AlbumGate_AllowsWhenFetcherIsNil is the bulk half of trap 1:
// the BulkExecutor has no capability path at all, so an install with no
// MusicBrainz provider leaves this fetcher nil. That must ALLOW.
func TestFetchImages_AlbumGate_AllowsWhenFetcherIsNil(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	e, svc, a := bulkGateArtist(t, gateAlbums, nil, results)
	// PRECONDITION: the artist really does have ten readable albums, so the only
	// missing half is the candidate's catalogue.
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	if status != "" {
		t.Fatalf("expected the self-heal to be ALLOWED with a nil release-group fetcher, got status %q (message: %q). "+
			"This is the #2858 regression guard: the bulk job must keep working with no MusicBrainz provider.", status, msg)
	}
	assertBulkAdopted(t, svc, a, status, msg)
}

// TestFetchImages_AlbumGate_AllowsWhenReleaseGroupFetchFails is the bulk-side
// fail-open guard on the candidate half.
func TestFetchImages_AlbumGate_AllowsWhenReleaseGroupFetchFails(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	fetcher := &stubReleaseGroupFetcher{err: errors.New("musicbrainz unreachable")}
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	if status != "" {
		t.Fatalf("expected the self-heal to be ALLOWED when the release-group fetch fails, got status %q (message: %q)", status, msg)
	}
	assertBulkAdopted(t, svc, a, status, msg)
	if fetcher.calls != 1 {
		t.Errorf("release-group fetches = %d, want 1: the fetch must actually have been attempted", fetcher.calls)
	}
}

// TestFetchImages_AlbumGate_AllowsOnHighOverlap is the bulk corroborated happy
// path.
func TestFetchImages_AlbumGate_AllowsOnHighOverlap(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(8)} // 80%
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	if status != "" {
		t.Fatalf("expected the self-heal to be ALLOWED at 80%% overlap, got status %q (message: %q)", status, msg)
	}
	assertBulkAdopted(t, svc, a, status, msg)
}

// TestFetchImages_AlbumGate_BlocksOnMidOverlap covers the bulk review band.
func TestFetchImages_AlbumGate_BlocksOnMidOverlap(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(5)} // 50%
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	assertBulkNotAdopted(t, svc, a, status, msg)
	if !strings.Contains(msg, "overlap") {
		t.Errorf("message %q does not name the catalogue-overlap reason", msg)
	}
	// A blocked candidate must leave no provenance stamp behind.
	if _, ok := a.MetadataSources[artist.SourceKeyMusicBrainzID]; ok {
		t.Errorf("a blocked candidate must not stamp MBID provenance, got %v", a.MetadataSources)
	}
}

// TestFetchImages_AlbumGate_BlocksOnLowOverlap covers the bulk decline band.
func TestFetchImages_AlbumGate_BlocksOnLowOverlap(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)} // 10%
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	assertBulkNotAdopted(t, svc, a, status, msg)
}

// TestFetchImages_AlbumGate_BlocksOnEmptyCandidateCatalogue is the measured
// 18/18 shape on the bulk path: a full local album directory against a
// candidate whose catalogue is genuinely empty.
func TestFetchImages_AlbumGate_BlocksOnEmptyCandidateCatalogue(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	fetcher := &stubReleaseGroupFetcher{groups: nil}
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	assertBulkNotAdopted(t, svc, a, status, msg)
	if !strings.Contains(msg, "no release groups") {
		t.Errorf("message %q does not name the empty-catalogue reason", msg)
	}
}

// TestFetchImages_AlbumGate_BlocksOnRedFlag covers the bulk red-flag route.
func TestFetchImages_AlbumGate_BlocksOnRedFlag(t *testing.T) {
	results := gateRedFlagResults()
	assertNameGatesPass(t, results)
	if flag := artist.CandidateRedFlag(&results[0]); flag == "" {
		t.Fatalf("fixture precondition: the candidate must carry a recognized red flag")
	}

	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(10)} // 100%
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	assertBulkNotAdopted(t, svc, a, status, msg)
	if !strings.Contains(msg, "tribute") {
		t.Errorf("message %q does not name the red flag", msg)
	}
}

// TestFetchImages_AlbumGate_BlockedCandidateWritesNoHistory pins the audit-trail
// half: a candidate the gate withheld must leave no metadata_changes row, since
// that record is what an operator queries to find IDs this job actually adopted.
func TestFetchImages_AlbumGate_BlockedCandidateWritesNoHistory(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	e, artistSvc, historySvc, a := newBulkGateExecutor(t, results)
	a.Path = gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))
	e.SetAlbumGate(artist.NewFilesystemAlbumSource(), &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)})

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	assertBulkNotAdopted(t, artistSvc, a, status, msg)

	changes, _, err := historySvc.List(context.Background(), a.ID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	for _, c := range changes {
		if c.Field == "musicbrainz_id" {
			t.Errorf("unexpected musicbrainz_id history row on a gate-blocked candidate: %+v", c)
		}
	}
}

// TestFetchImages_AlbumGate_ReachableThroughFetchImages closes the seam the
// selfHealMBID extraction opened.
//
// Every other bulk case above calls selfHealMBID DIRECTLY, which proves the gate
// decides correctly but NOT that fetchImages still routes through it. Severing
// the delegation in fetchImages leaves all of those passing -- the extraction
// would have silently disabled the gate on the real entry point and no
// album-gate test would have noticed. (The eight pre-existing
// TestFetchImages_MBIDGate_* cases do catch that severance, but they exercise
// artist.EvaluateMBIDCandidate, not this gate, so they would not catch the gate
// specifically being bypassed.)
//
// This one drives the PUBLIC path end to end with a candidate only the ALBUM
// gate can refuse, so it fails if fetchImages ever stops consulting it.
func TestFetchImages_AlbumGate_ReachableThroughFetchImages(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	// 10% overlap: clears every name gate, refused only by the album gate.
	fetcher := &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)}
	e, svc, a := bulkGateArtist(t, gateAlbums, fetcher, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	// PRECONDITION: the artist must genuinely still need images, or fetchImages
	// could short-circuit on "all images present" and never reach the self-heal.
	if a.ThumbExists || a.FanartExists || a.LogoExists {
		t.Fatalf("fixture precondition: artist must need images, got thumb=%v fanart=%v logo=%v",
			a.ThumbExists, a.FanartExists, a.LogoExists)
	}

	status, msg := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	assertBulkNotAdopted(t, svc, a, status, msg)
	if !strings.Contains(msg, "overlap") {
		t.Errorf("message %q does not name the catalogue-overlap reason, so fetchImages may not be consulting the album gate", msg)
	}
}
