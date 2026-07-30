package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Tests for the never-replace invariant (#2826), the shared-gate Tier 3 swap
// (#2827), and the identity audit row (#2845).
//
// Every test here reloads the artist row from the store rather than trusting a
// returned counter, and asserts the PRECONDITION (what the artist's MBID was
// BEFORE the call) explicitly. Without that precondition, "no write happened"
// passes just as well against an artist that never had an MBID to protect,
// which would make the whole file vacuous.

// The two identities every never-replace test discriminates between: u1 is what
// the artist already has stored, u2 is what some automated source proposes.
// They are distinct valid UUIDs so a test can never pass because the two
// coincided.
const (
	mbidStored   = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	mbidProposed = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
)

// errHistoryRepo is a HistoryRepository whose Record always fails, used to
// prove a failed audit write never fails the operation it records. Every other
// method delegates to the shared always-error stub, which is not exercised
// here.
type errRecordHistoryRepo struct{ alwaysErrHistoryRepo }

func (errRecordHistoryRepo) Record(_ context.Context, _ *artist.MetadataChange) error {
	return errors.New("simulated history write failure")
}

// identifyHistoryRouter builds a Router with a REAL SQLite-backed history
// service so tests can read metadata_changes back, plus the stub orchestrator
// wiring newIdentifyTestServer installs. testRouterWithLibrary leaves
// historyService nil (RouterDeps omits HistoryService), so it has to be
// attached explicitly -- which is itself worth knowing: the nil shape is the
// default in this package, hence TestApplyIdentity_NilHistoryServiceIsSafe.
func identifyHistoryRouter(t *testing.T, search func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error)) (*Router, *artist.Service, *artist.HistoryService) {
	t.Helper()
	r, artistSvc := newIdentifyTestServer(t, search, nil)
	histSvc := artist.NewHistoryService(r.db)
	r.historyService = histSvc
	return r, artistSvc, histSvc
}

// seedIdentifiedArtist creates an artist carrying mbidStored and asserts the
// precondition that the store really holds it. The MBID is fixed rather than a
// parameter because every caller protects the same stored identity and proposes
// mbidProposed against it; a parameter nothing varies would just invite a caller
// to pass the proposed value by mistake. MetadataSources is left unset:
// that is the realistic case, since nothing wrote SourceOperatorConfirmed
// before this change, so every pre-existing operator-set ID in the field is
// unmarked.
func seedIdentifiedArtist(t *testing.T, svc *artist.Service, name string) *artist.Artist {
	t.Helper()
	ctx := context.Background()
	const mbid = mbidStored
	a := &artist.Artist{Name: name, SortName: name, Type: "group", MusicBrainzID: mbid}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seeded artist: %v", err)
	}
	if stored.MusicBrainzID != mbid {
		t.Fatalf("precondition failed: seeded MBID = %q, want %q; a test asserting "+
			"an MBID SURVIVED proves nothing if there was none to begin with",
			stored.MusicBrainzID, mbid)
	}
	if stored.MetadataSources[artist.SourceKeyMusicBrainzID] != "" {
		t.Fatalf("precondition failed: seeded provenance = %q, want unset (the unmarked case)",
			stored.MetadataSources[artist.SourceKeyMusicBrainzID])
	}
	return stored
}

// reloadMBID reads the artist's persisted MusicBrainz ID.
func reloadMBID(t *testing.T, svc *artist.Service, id string) string {
	t.Helper()
	got, err := svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	return got.MusicBrainzID
}

// mbidHistoryRows returns every musicbrainz_id change recorded for the artist.
func mbidHistoryRows(t *testing.T, h *artist.HistoryService, artistID string) []artist.MetadataChange {
	t.Helper()
	rows, _, err := h.List(context.Background(), artistID, 100, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	var out []artist.MetadataChange
	for _, row := range rows {
		if row.Field == artist.SourceKeyMusicBrainzID {
			out = append(out, row)
		}
	}
	return out
}

// --- #2826's first explicitly required case ---

// TestIdentifyArtist_Tier1_StoredMBIDSurvivesSingleDifferingEntry encodes
// #2826 itself. The old guard looped over entries[1:], so with exactly ONE
// connection entry it never executed and the entry's MBID overwrote the stored
// one purely because the name string matched.
func TestIdentifyArtist_Tier1_StoredMBIDSurvivesSingleDifferingEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, _ := identifyHistoryRouter(t, nil)

	a := seedIdentifiedArtist(t, artistSvc, "Pink Floyd")

	// Exactly ONE entry: the count that made the old guard vacuous. Same name
	// (similarity 100, so the similarity leg cannot explain the decline) and a
	// valid but DIFFERENT MBID.
	idx := &connectionIndex{byName: map[string][]connEntry{
		"pink floyd": {{Name: "Pink Floyd", MusicBrainzID: mbidProposed}},
	}}

	got := r.identifyArtist(ctx, a, idx)

	if got.Outcome != outcomeQueued {
		t.Fatalf("Outcome = %v, want queued", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidStored {
		t.Errorf("stored MBID = %q, want %q; an automated pass replaced a stored identity", mbid, mbidStored)
	}
	if got.Candidate == nil {
		t.Fatal("Candidate = nil, want the differing identity offered for review")
	}
	if got.Candidate.Tier != "connection" {
		t.Errorf("Tier = %q, want %q", got.Candidate.Tier, "connection")
	}
	if len(got.Candidate.Candidates) != 1 {
		t.Fatalf("Candidates len = %d, want 1", len(got.Candidate.Candidates))
	}
	if got.Candidate.Candidates[0].MusicBrainzID != mbidProposed {
		t.Errorf("offered MBID = %q, want %q", got.Candidate.Candidates[0].MusicBrainzID, mbidProposed)
	}
}

// --- #2826's second explicitly required case ---

// TestBulkAction_ReIdentifyAuto_DoesNotReplaceStoredMBID drives the ENTRY POINT
// where the overwrite is actually reachable, end to end, rather than asserting
// reachability by reading the code.
//
// It matters that this is the bulk-ACTION path and not handleBulkIdentify:
// handleBulkIdentify lists with Filter "missing_mbid", so an artist that has an
// MBID never enters its corpus at all. The bulk-action path loads exactly the
// IDs the operator selected and calls identifyArtist on each with no such
// filter, which is why "the operator asked to re-identify" could destroy a
// stored identity.
func TestBulkAction_ReIdentifyAuto_DoesNotReplaceStoredMBID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, libSvc, artistSvc := testRouterWithLibrary(t)

	// A CONNECTION library (non-manual) holding the differing identity, so the
	// handler's own buildConnectionIndex call finds it. Building the index from
	// real rows rather than injecting a fake connectionIndex is what makes this
	// an end-to-end reachability proof.
	connLib := &library.Library{Name: "Emby Music", Type: library.TypeRegular, Source: library.SourceEmby}
	if err := libSvc.Create(ctx, connLib); err != nil {
		t.Fatalf("creating connection library: %v", err)
	}
	connArtist := &artist.Artist{
		Name: "Contested", SortName: "Contested", Type: "group",
		Path: "/emby/Contested", LibraryID: connLib.ID, MusicBrainzID: mbidProposed,
	}
	if err := artistSvc.Create(ctx, connArtist); err != nil {
		t.Fatalf("creating connection artist: %v", err)
	}

	manualLib := &library.Library{
		Name: "Local Music", Type: library.TypeRegular,
		Source: library.SourceManual, Path: t.TempDir(),
	}
	if err := libSvc.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}
	target := &artist.Artist{
		Name: "Contested", SortName: "Contested", Type: "group",
		LibraryID: manualLib.ID, MusicBrainzID: mbidStored,
	}
	if err := artistSvc.Create(ctx, target); err != nil {
		t.Fatalf("creating target artist: %v", err)
	}
	if mbid := reloadMBID(t, artistSvc, target.ID); mbid != mbidStored {
		t.Fatalf("precondition failed: target MBID = %q, want %q", mbid, mbidStored)
	}

	payload := `{"action":"re_identify_auto","ids":["` + target.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	waitBulkActionCompleted(t, r)

	if mbid := reloadMBID(t, artistSvc, target.ID); mbid != mbidStored {
		t.Errorf("stored MBID = %q, want %q; bulk re-identify replaced a stored identity", mbid, mbidStored)
	}

	// The refused candidate must reach the review queue, not vanish: losing it
	// would leave the operator with no way to act on the disagreement.
	r.identifyMu.RLock()
	progress := r.identifyProgress
	r.identifyMu.RUnlock()
	if progress == nil {
		t.Fatal("identifyProgress = nil, want the review queue populated")
	}
	progress.mu.RLock()
	defer progress.mu.RUnlock()
	var found bool
	for _, c := range progress.ReviewQueue {
		if c.ArtistID == target.ID {
			found = true
			if c.Tier != "connection" {
				t.Errorf("Tier = %q, want %q", c.Tier, "connection")
			}
		}
	}
	if !found {
		t.Errorf("artist %s absent from the review queue (%d entries)", target.ID, len(progress.ReviewQueue))
	}
}

// --- The chokepoint invariant ---

// TestApplyIdentity_RefusesReplaceWithoutConsent exercises the real function
// that would do the overwriting, not a flag: applyIdentity is handed a genuine
// replacement with the zero-value AllowReplace and must refuse it.
func TestApplyIdentity_RefusesReplaceWithoutConsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

	a := seedIdentifiedArtist(t, artistSvc, "Refuser")

	_, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:       mbidProposed,
		Source:     artist.IdentifySourceConnection,
		Provenance: artist.SourceMachinePicked,
		// AllowReplace deliberately omitted: the zero value must refuse.
	})
	if !errors.Is(err, errIdentityReplaceRefused) {
		t.Fatalf("err = %v, want errIdentityReplaceRefused", err)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidStored {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidStored)
	}
	if rows := mbidHistoryRows(t, histSvc, a.ID); len(rows) != 0 {
		t.Errorf("history rows = %d, want 0; a refused write must not be recorded as one", len(rows))
	}
}

// TestApplyIdentity_OperatorLinkMayReplace is the other half of the test above:
// without it, the refusal test would pass equally well against a function that
// refuses EVERYTHING, including the operator's legitimate correction.
func TestApplyIdentity_OperatorLinkMayReplace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

	a := seedIdentifiedArtist(t, artistSvc, "Consenter")

	if _, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:         mbidProposed,
		Source:       artist.IdentifySourceOperator,
		Provenance:   artist.SourceOperatorConfirmed,
		AllowReplace: true,
	}); err != nil {
		t.Fatalf("applyIdentity: %v", err)
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if reloaded.MusicBrainzID != mbidProposed {
		t.Errorf("stored MBID = %q, want %q", reloaded.MusicBrainzID, mbidProposed)
	}
	if got := reloaded.MetadataSources[artist.SourceKeyMusicBrainzID]; got != artist.SourceOperatorConfirmed {
		t.Errorf("provenance = %q, want %q", got, artist.SourceOperatorConfirmed)
	}
	rows := mbidHistoryRows(t, histSvc, a.ID)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].Source != artist.IdentifySourceOperator {
		t.Errorf("source = %q, want %q; a human decision must not be filed as machine damage",
			rows[0].Source, artist.IdentifySourceOperator)
	}
	if rows[0].OldValue != mbidStored {
		t.Errorf("old_value = %q, want %q", rows[0].OldValue, mbidStored)
	}
}

// --- #2845's recording half ---

// TestApplyIdentity_RecordsReplacedMBIDInOldValue is what distinguishes this
// change from the pre-existing rule-path recorders, which all hardcode
// oldValue = "" because their callers are blank-fill-only. Carrying the
// replaced ID is the entire point of #2845: without it nothing in the tree can
// tell an operator what an automated pass destroyed.
func TestApplyIdentity_RecordsReplacedMBIDInOldValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

	a := seedIdentifiedArtist(t, artistSvc, "Recorded")

	if _, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:         mbidProposed,
		Source:       artist.IdentifySourceOperator,
		Provenance:   artist.SourceOperatorConfirmed,
		AllowReplace: true,
	}); err != nil {
		t.Fatalf("applyIdentity: %v", err)
	}

	rows := mbidHistoryRows(t, histSvc, a.ID)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].OldValue != mbidStored {
		t.Errorf("old_value = %q, want %q; the replaced identity is unrecoverable without it",
			rows[0].OldValue, mbidStored)
	}
	if rows[0].NewValue != mbidProposed {
		t.Errorf("new_value = %q, want %q", rows[0].NewValue, mbidProposed)
	}
}

// TestApplyIdentity_BlankFillRecordsEmptyOldValue differs from the test above
// along exactly the axis under test (stored vs blank), so neither test can pass
// for the other's reason.
func TestApplyIdentity_BlankFillRecordsEmptyOldValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

	a := &artist.Artist{Name: "Blank", SortName: "Blank", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Fatalf("precondition failed: seeded MBID = %q, want blank", mbid)
	}

	if _, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:       mbidProposed,
		Source:     artist.IdentifySourceConnection,
		Provenance: artist.SourceMachinePicked,
	}); err != nil {
		t.Fatalf("applyIdentity: %v", err)
	}

	rows := mbidHistoryRows(t, histSvc, a.ID)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].OldValue != "" {
		t.Errorf("old_value = %q, want empty for a blank fill", rows[0].OldValue)
	}
	if rows[0].NewValue != mbidProposed {
		t.Errorf("new_value = %q, want %q", rows[0].NewValue, mbidProposed)
	}
}

// TestApplyIdentity_HistoryFailureDoesNotFailTheWrite encodes #2845's
// "a failed history write never fails the underlying operation" requirement.
// The audit row is supplementary diagnostics; losing it must not roll back or
// report failure on a write that already succeeded.
func TestApplyIdentity_HistoryFailureDoesNotFailTheWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc := newIdentifyTestServer(t, nil, nil)
	r.historyService = artist.NewHistoryServiceWithRepo(errRecordHistoryRepo{})

	a := &artist.Artist{Name: "HistErr", SortName: "HistErr", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:       mbidProposed,
		Source:     artist.IdentifySourceConnection,
		Provenance: artist.SourceMachinePicked,
	}); err != nil {
		t.Fatalf("applyIdentity returned %v; a failed audit write must not fail the operation", err)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidProposed {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidProposed)
	}
}

// TestApplyIdentity_NilHistoryServiceIsSafe guards the most likely panic this
// change could introduce: a nil historyService is the shape every
// testRouterWithLibrary produces, and production wiring that omits
// RouterDeps.HistoryService would hit the same path.
func TestApplyIdentity_NilHistoryServiceIsSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc := newIdentifyTestServer(t, nil, nil)
	if r.historyService != nil {
		t.Fatal("precondition failed: expected a nil historyService from the default test wiring")
	}

	a := &artist.Artist{Name: "NilHist", SortName: "NilHist", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:       mbidProposed,
		Source:     artist.IdentifySourceConnection,
		Provenance: artist.SourceMachinePicked,
	}); err != nil {
		t.Fatalf("applyIdentity: %v", err)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidProposed {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidProposed)
	}
}

// TestApplyIdentity_CorroborationWritesNoHistoryRow proves the
// oldValue == newValue suppression inside HistoryService.Record is actually
// REACHED on the corroboration path rather than assumed, which is why
// applyIdentity does not need a second guard of its own.
func TestApplyIdentity_CorroborationWritesNoHistoryRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

	a := seedIdentifiedArtist(t, artistSvc, "Corroborated")

	if _, err := r.applyIdentity(ctx, a, identityWrite{
		MBID:   mbidStored, // same identity: agreement, not a change
		Source: artist.IdentifySourceConnection,
	}); err != nil {
		t.Fatalf("applyIdentity: %v", err)
	}
	if rows := mbidHistoryRows(t, histSvc, a.ID); len(rows) != 0 {
		t.Errorf("history rows = %d, want 0; corroboration changed nothing to record", len(rows))
	}
}

// --- Tier 1 gate legs ---

// TestIdentifyArtist_Tier1_BelowNameSimilarityFloorFallsThrough exercises the
// one gate leg that transfers to Tier 1 unchanged from the shared gate.
func TestIdentifyArtist_Tier1_BelowNameSimilarityFloorFallsThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _, artistSvc := testRouterWithLibrary(t)

	a := &artist.Artist{Name: "Beatles", SortName: "Beatles", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// The lookup key is the artist's normalized name, so the entry is FOUND;
	// its display name is what scores badly. Assert the fixture really is below
	// the floor, or the test could pass because the entry was never returned.
	const entryName = "Completely Different Ensemble"
	if sim := provider.NameSimilarity(a.Name, entryName); sim >= artist.MBIDMinNameSimilarity {
		t.Fatalf("precondition failed: similarity %d is not below the %d floor",
			sim, artist.MBIDMinNameSimilarity)
	}
	idx := &connectionIndex{byName: map[string][]connEntry{
		"beatles": {{Name: entryName, MusicBrainzID: mbidProposed}},
	}}

	// No orchestrator wired, so falling through to Tier 2/3 lands on unmatched.
	got := r.identifyArtist(ctx, a, idx)
	if got.Outcome != outcomeUnmatched {
		t.Fatalf("Outcome = %v, want unmatched (fell through with no orchestrator)", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank; a poor name match was adopted", mbid)
	}
}

// TestIdentifyArtist_Tier1_InvalidMBIDIsNotAdopted is the regression test for
// the unvalidated-platform-ID hole: buildConnectionIndex filters only on
// MusicBrainzID != "", so nothing ever checked that a platform-sourced ID was
// even a UUID.
func TestIdentifyArtist_Tier1_InvalidMBIDIsNotAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _, artistSvc := testRouterWithLibrary(t)

	a := &artist.Artist{Name: "Shapeless", SortName: "Shapeless", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Identical name, so similarity is 100 and only the validity leg can
	// explain the decline.
	const bogus = "not-a-uuid"
	if artist.IsValidMBID(bogus) {
		t.Fatalf("precondition failed: %q is unexpectedly a valid MBID", bogus)
	}
	idx := &connectionIndex{byName: map[string][]connEntry{
		"shapeless": {{Name: "Shapeless", MusicBrainzID: bogus}},
	}}

	got := r.identifyArtist(ctx, a, idx)
	if got.Outcome != outcomeUnmatched {
		t.Fatalf("Outcome = %v, want unmatched", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank; a non-UUID was adopted as an identity", mbid)
	}
}

// TestIdentifyArtist_Tier1_DistinctMBIDsWithBlankFallThroughToLaterTiers is the
// ambiguity leg re-expressed: two entries the index cannot discriminate
// between. Both clear the similarity floor and both carry valid UUIDs, so the
// distinct-MBID axis is the ONLY thing that can explain the decline.
//
// Fall through rather than queue is deliberate: album comparison is better
// evidence than the platform index and may discriminate where the index cannot.
func TestIdentifyArtist_Tier1_DistinctMBIDsWithBlankFallThroughToLaterTiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _, artistSvc := testRouterWithLibrary(t)

	a := &artist.Artist{Name: "Ambiguous", SortName: "Ambiguous", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	idx := &connectionIndex{byName: map[string][]connEntry{
		"ambiguous": {
			{Name: "Ambiguous", MusicBrainzID: mbidStored},
			{Name: "Ambiguous", MusicBrainzID: mbidProposed},
		},
	}}

	got := r.identifyArtist(ctx, a, idx)
	if got.Outcome != outcomeUnmatched {
		t.Fatalf("Outcome = %v, want unmatched (fell through, not queued)", got.Outcome)
	}
	if got.Candidate != nil {
		t.Errorf("Candidate = %+v, want nil; Tier 1 must defer to later tiers, not queue", got.Candidate)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank; one of two indistinguishable identities was adopted", mbid)
	}
}

// TestIdentifyArtist_Tier1_SingleAgreeingEntryFillsBlankAndStampsProvenance is
// the happy path. It must keep working or the fix is a regression that stops
// Tier 1 linking anything -- which matters because buildConnectionIndex indexes
// per library, so a single-connection install genuinely has exactly one entry
// per artist.
func TestIdentifyArtist_Tier1_SingleAgreeingEntryFillsBlankAndStampsProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

	a := &artist.Artist{Name: "Fillable", SortName: "Fillable", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	idx := &connectionIndex{byName: map[string][]connEntry{
		"fillable": {{Name: "Fillable", MusicBrainzID: mbidProposed, DiscogsID: "d-fill"}},
	}}

	got := r.identifyArtist(ctx, a, idx)
	if got.Outcome != outcomeAutoLinked {
		t.Fatalf("Outcome = %v, want autoLinked", got.Outcome)
	}
	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if reloaded.MusicBrainzID != mbidProposed {
		t.Errorf("stored MBID = %q, want %q", reloaded.MusicBrainzID, mbidProposed)
	}
	if reloaded.DiscogsID != "d-fill" {
		t.Errorf("DiscogsID = %q, want d-fill", reloaded.DiscogsID)
	}
	if got := reloaded.MetadataSources[artist.SourceKeyMusicBrainzID]; got != artist.SourceMachinePicked {
		t.Errorf("provenance = %q, want %q", got, artist.SourceMachinePicked)
	}
	rows := mbidHistoryRows(t, histSvc, a.ID)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].Source != artist.IdentifySourceConnection {
		t.Errorf("source = %q, want %q", rows[0].Source, artist.IdentifySourceConnection)
	}
}

// --- Tier 3: the shared gate (#2827) ---

// TestIdentifyArtist_Tier3_SingleHighScorePoorNameMatchDoesNotAutoLink is
// #2827's acceptance criterion exactly. The old clause was
// len(results) == 1 && Score >= 90, so a lone hit at score 95 auto-linked no
// matter how little its name resembled the artist -- and MusicBrainz's score is
// a relevance rank that folds in popularity, so a well-known artist genuinely
// comes back at 95 for a barely-related query.
func TestIdentifyArtist_Tier3_SingleHighScorePoorNameMatchDoesNotAutoLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const hitName = "Some Totally Unrelated Orchestra"
	r, artistSvc, _ := identifyHistoryRouter(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:          hitName,
				MusicBrainzID: mbidProposed,
				Score:         95, // clears the old >= 90 scarcity clause
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		})

	a := &artist.Artist{Name: "Tiny Local Band", SortName: "Tiny Local Band", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if sim := provider.NameSimilarity(a.Name, hitName); sim >= artist.MBIDMinNameSimilarity {
		t.Fatalf("precondition failed: similarity %d is not below the %d floor",
			sim, artist.MBIDMinNameSimilarity)
	}

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Fatal("Outcome = autoLinked; a high relevance score with a poor name match must not auto-link")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}
}

// TestIdentifyArtist_Tier3_AmbiguousRivalDeclines proves the ambiguity margin
// participates at Tier 3, which len(results) == 1 structurally prevented: with
// two results the old clause simply never evaluated the scores at all.
func TestIdentifyArtist_Tier3_AmbiguousRivalDeclines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, _ := identifyHistoryRouter(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			// Two DIFFERENT artists sharing a name, both clearing the score and
			// similarity floors, within the ambiguity margin of each other.
			return []provider.ArtistSearchResult{
				{Name: "Twins", MusicBrainzID: mbidStored, Score: 100, Source: string(provider.NameMusicBrainz)},
				{Name: "Twins", MusicBrainzID: mbidProposed, Score: 95, Source: string(provider.NameMusicBrainz)},
			}, nil
		})

	a := &artist.Artist{Name: "Twins", SortName: "Twins", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Both floors cleared, and the gap is inside the margin: the ambiguity leg
	// is the only one that can decline this.
	if gap := 100 - 95; gap >= artist.MBIDAmbiguityMargin {
		t.Fatalf("precondition failed: score gap %d is not inside the %d margin",
			gap, artist.MBIDAmbiguityMargin)
	}

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Fatal("Outcome = autoLinked; two indistinguishable identities are not evidence for either")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}
}

// TestIdentifyArtist_Tier3_DoesNotReplaceStoredMBID covers the never-replace
// invariant on the name-search path, where the candidate clears every
// confidence gate and is refused purely because something is already stored.
func TestIdentifyArtist_Tier3_DoesNotReplaceStoredMBID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, _ := identifyHistoryRouter(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:          "Keeper",
				MusicBrainzID: mbidProposed,
				Score:         100,
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		})

	a := seedIdentifiedArtist(t, artistSvc, "Keeper")

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome != outcomeQueued {
		t.Fatalf("Outcome = %v, want queued", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidStored {
		t.Errorf("stored MBID = %q, want %q; a gate-clearing candidate replaced a stored identity",
			mbid, mbidStored)
	}
}

// --- Tier 2 ---

// TestEvaluateTier2_DoesNotReplaceStoredMBID asserts on BOTH the row and the
// outcome, because deleting Tier 2's own guard produces a DIFFERENT wrong
// answer than a plain overwrite: applyIdentity's backstop refuses the write and
// the outcome becomes outcomeFailed, so the operator loses the review-queue
// entry instead of losing the ID. Both failure shapes must be caught.
func TestEvaluateTier2_DoesNotReplaceStoredMBID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, _ := identifyHistoryRouter(t, nil)

	a := seedIdentifiedArtist(t, artistSvc, "Album Keeper")

	cmp := artist.AlbumComparison{MatchPercent: 90}
	scored := []ScoredCandidate{{
		ArtistSearchResult: provider.ArtistSearchResult{Name: "Album Keeper", MusicBrainzID: mbidProposed},
		AlbumComparison:    &cmp,
	}}

	got := r.evaluateTier2(ctx, a, scored)
	if got.Outcome != outcomeQueued {
		t.Fatalf("Outcome = %v, want queued (not failed: Tier 2 must route to review itself)", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidStored {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidStored)
	}
}

// --- Provenance stamping on the operator field-edit path ---

// TestUpdateProviderField_StampsOperatorConfirmedForMBID plus its negative twin
// below: without the twin, an UNCONDITIONAL stamp would pass this test, so the
// pair is what makes the field-name condition load-bearing.
func TestUpdateProviderField_StampsOperatorConfirmedForMBID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, _, artistSvc := testRouterWithLibrary(t)

	a := &artist.Artist{Name: "Typed", SortName: "Typed", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	before, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if before.MetadataSources[artist.SourceKeyMusicBrainzID] != "" {
		t.Fatalf("precondition failed: provenance already set to %q",
			before.MetadataSources[artist.SourceKeyMusicBrainzID])
	}

	if err := artistSvc.UpdateProviderField(ctx, a.ID, "musicbrainz_id", mbidProposed); err != nil {
		t.Fatalf("UpdateProviderField: %v", err)
	}
	after, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if got := after.MetadataSources[artist.SourceKeyMusicBrainzID]; got != artist.SourceOperatorConfirmed {
		t.Errorf("provenance = %q, want %q", got, artist.SourceOperatorConfirmed)
	}
}

// TestUpdateProviderField_NonMBIDFieldDoesNotStampMBIDProvenance is that twin.
// A Spotify edit must not claim a human confirmed the MusicBrainz ID.
func TestUpdateProviderField_NonMBIDFieldDoesNotStampMBIDProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, _, artistSvc := testRouterWithLibrary(t)

	a := &artist.Artist{Name: "Spotified", SortName: "Spotified", Type: "group"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if err := artistSvc.UpdateProviderField(ctx, a.ID, "spotify_id", "7dGJo4pcD2V6oG8kP0tJRR"); err != nil {
		t.Fatalf("UpdateProviderField: %v", err)
	}
	after, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.SpotifyID != "7dGJo4pcD2V6oG8kP0tJRR" {
		t.Fatalf("precondition failed: Spotify ID = %q, the edit did not take effect", after.SpotifyID)
	}
	if got := after.MetadataSources[artist.SourceKeyMusicBrainzID]; got != "" {
		t.Errorf("MBID provenance = %q, want unset; a Spotify edit must not stamp the MusicBrainz key", got)
	}
}
