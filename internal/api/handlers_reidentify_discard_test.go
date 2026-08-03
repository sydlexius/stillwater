package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// --- #2894: the repudiated entity's secondary provider IDs -------------------
//
// The defect these tests pin is NOT "a stale row sits in a table". It is that a
// stale secondary ID STEERS the refresh that follows a re-identify:
// FetchProviderResult prefers a provider-specific ID over the MusicBrainz ID
// (provider_result.go:60-63), and AudioDB and Discogs both sit ahead of
// MusicBrainz for origin / biography / years_active. So the operator corrects
// the identity, the refresh visibly succeeds, and the REPUDIATED artist's
// metadata is fetched and written straight back.
//
// That is why every test below asserts the map handed to the scraper, not only
// the persisted rows: the rows are the state, the map is the MECHANISM. A test
// that checked rows alone would still pass against an implementation that
// cleared them after the fetch, which fixes nothing the issue reports.

// recordingScraperExecutor is stubScraperExecutor plus a record of the provider
// IDs each call was steered by.
//
// providerIDs is captured by COPY: executeRefreshCtx hands over the live map
// returned by ProviderIDMap, and asserting on a retained reference would be
// asserting on whatever the map held at assertion time rather than at call
// time.
type recordingScraperExecutor struct {
	mu     sync.Mutex
	result *provider.FetchResult
	err    error
	calls  []map[provider.ProviderName]string
}

func (s *recordingScraperExecutor) ScrapeAll(_ context.Context, _, _, _ string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	captured := make(map[provider.ProviderName]string, len(providerIDs))
	for k, v := range providerIDs {
		captured[k] = v
	}
	s.calls = append(s.calls, captured)
	return s.result, s.err
}

// callCount reports how many refreshes ran. Zero is itself an assertion in the
// locked cases: it is what proves the lock suppressed the fetch rather than the
// fetch having run and merely changed nothing.
func (s *recordingScraperExecutor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordingScraperExecutor) firstCall(t *testing.T) map[provider.ProviderName]string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatalf("the provider fetch never ran, so nothing about the re-identify refresh is under test here")
	}
	return s.calls[0]
}

const (
	// repudiatedOrigin is the origin the WRONG artist supplies. It is seeded on
	// the artist before the re-identify so the "corrected" assertion cannot
	// pass vacuously against a field that was empty all along.
	repudiatedOrigin = "Repudiated City, XX"
	// correctedOrigin is what the stubbed provider returns for the corrected
	// identity.
	correctedOrigin = "Corrected City, YY"
)

// reidentifyFetchResult is the payload the stubbed provider returns for the
// CORRECTED identity. origin and biography are both attempted and populated, so
// ApplyMetadata definitely writes them: without that, "origin was corrected"
// would be untestable and the discard's whole point unobservable.
func reidentifyFetchResult() *provider.FetchResult {
	return &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Origin:    correctedOrigin,
			Biography: lockedRefreshSentinel,
		},
		AttemptedFields: []string{"origin", "biography"},
		PopulatedFields: []string{"origin", "biography"},
	}
}

// attachRecordingOrchestrator wires a real provider.Orchestrator whose executor
// records what it was steered by, and returns the recorder.
func attachRecordingOrchestrator(t *testing.T, r *Router, result *provider.FetchResult, err error) *recordingScraperExecutor {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	rec := &recordingScraperExecutor{result: result, err: err}
	orch := provider.NewOrchestrator(nil, nil, logger, nil)
	orch.SetExecutor(rec)
	r.orchestrator = orch
	return rec
}

// seedRepudiatedOrigin stamps the wrong artist's origin onto a seeded target
// and asserts it persisted. Called after seedReidentifyTarget, which owns the
// provider-ID half of the fixture.
func seedRepudiatedOrigin(t *testing.T, svc *artist.Service, artistID string) {
	t.Helper()
	ctx := context.Background()
	a, err := svc.GetByID(ctx, artistID)
	if err != nil {
		t.Fatalf("loading seeded artist: %v", err)
	}
	a.Origin = repudiatedOrigin
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("seeding repudiated origin: %v", err)
	}
	reloaded, err := svc.GetByID(ctx, artistID)
	if err != nil {
		t.Fatalf("reloading seeded artist: %v", err)
	}
	if reloaded.Origin != repudiatedOrigin {
		t.Fatalf("precondition: origin = %q, want %q; the corrected-origin assertion would be vacuous", reloaded.Origin, repudiatedOrigin)
	}
	if reloaded.Locked {
		t.Fatalf("precondition: artist is locked, so the refresh would be skipped and this proves nothing")
	}
}

// lockArtist locks a seeded artist. LockedAt is required by a schema CHECK
// constraint (locked = 0 OR (locked = 1 AND locked_at IS NOT NULL)).
func lockArtist(t *testing.T, svc *artist.Service, artistID string) {
	t.Helper()
	ctx := context.Background()
	a, err := svc.GetByID(ctx, artistID)
	if err != nil {
		t.Fatalf("loading artist to lock: %v", err)
	}
	lockedAt := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	a.Locked = true
	a.LockedAt = &lockedAt
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("locking artist: %v", err)
	}
	reloaded, err := svc.GetByID(ctx, artistID)
	if err != nil {
		t.Fatalf("reloading locked artist: %v", err)
	}
	if !reloaded.Locked {
		t.Fatalf("precondition: artist is not locked, so the lock gate is not under test")
	}
}

// startWizardOn creates a one-step wizard session for the given artist plus a
// filler step, so the accept ADVANCES rather than completing. Both shapes reach
// the same accept handler; advancing is the common case.
func startWizardOn(t *testing.T, r *Router, artistID string) *reIdentifyWizardSession {
	t.Helper()
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: artistID}, {ArtistID: "other"},
	})
	if err != nil {
		t.Fatalf("creating wizard session: %v", err)
	}
	return sess
}

// postWizardAccept drives the REAL handleReIdentifyWizardAccept with a JSON
// body, which is the shape the bulk-action JS path sends.
func postWizardAccept(t *testing.T, r *Router, sess *reIdentifyWizardSession, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	return w
}

// assertRefreshNotSteeredByRepudiatedIDs is the mechanism assertion: every
// modeled secondary ID handed to the fetch must be empty, so the providers are
// resolved from the CORRECTED MusicBrainz identity instead of from the
// repudiated entity's own IDs.
func assertRefreshNotSteeredByRepudiatedIDs(t *testing.T, ids map[provider.ProviderName]string) {
	t.Helper()
	for prov, got := range ids {
		if got != "" {
			t.Errorf("the refresh after a re-identify was steered by %s=%q; FetchProviderResult prefers a provider-specific ID over the MBID, so this fetch re-reads the artist the operator just repudiated and writes its metadata back (#2894)", prov, got)
		}
	}
}

// TestHandleReIdentifyWizardAccept_DiscardsRepudiatedIDsAndCorrectsOrigin is
// the BULK WIZARD half of #2894, and the one the first cut of this fix missed
// entirely.
//
// The first cut scoped the discard by HANDLER (handleRefreshLink) when the
// correct scope is INTENT. The wizard is a second entry point carrying the same
// "this artist is someone else" claim, and it reached the refresh through
// autoLinkAndRefresh with every stale ID intact -- reproducing the reported
// defect in full on what is the common path for a multi-artist correction.
//
// Deliberately NOT a variant of the existing accept tests: those assert the
// refresh_skipped_locked notice and pass without ever looking at a provider ID.
func TestHandleReIdentifyWizardAccept_DiscardsRepudiatedIDsAndCorrectsOrigin(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	rec := attachRecordingOrchestrator(t, r, reidentifyFetchResult(), nil)
	db := r.db

	a := seedReidentifyTarget(t, db, artistSvc, "Wizard Reidentify", "/music/wizard-reidentify")
	seedRepudiatedOrigin(t, artistSvc, a.ID)

	const replacementMBID = "mbid-wizard-corrected"
	sess := startWizardOn(t, r, a.ID)
	w := postWizardAccept(t, r, sess, `{"mbid":"`+replacementMBID+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// MECHANISM: the fetch must not have been pointed at the repudiated entity.
	if n := rec.callCount(); n != 1 {
		t.Fatalf("provider fetch ran %d times, want 1; the wizard accept did not refresh an unlocked artist", n)
	}
	assertRefreshNotSteeredByRepudiatedIDs(t, rec.firstCall(t))

	// STATE: the rows the next refresh would read are gone too.
	assertSecondaryIDsDiscarded(t, db, a.ID, "a bulk-wizard re-identify accept")
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); exists && pid != "" {
		t.Errorf("discogs row after a bulk-wizard re-identify: still carries provider_id=%q; it belongs to the repudiated entity (#2894)", pid)
	}
	// The scoped-delete boundary from #2725 is unchanged: an orphan-provider
	// fetched_at row is not a modeled identity and must survive.
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by the wizard discard; scoped delete boundary is wrong (#2725)")
	}

	// OUTCOME: the field the operator actually complained about.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.MusicBrainzID != replacementMBID {
		t.Errorf("MusicBrainzID = %q, want %q; the accept did not persist the corrected identity", reloaded.MusicBrainzID, replacementMBID)
	}
	if reloaded.Origin != correctedOrigin {
		t.Errorf("origin = %q, want %q; the corrected identity's origin did not survive the refresh -- this is the visible symptom of #2894", reloaded.Origin, correctedOrigin)
	}
}

// TestReIdentify_FailedRefreshLeavesEveryProviderIDIntact pins the ordering
// argument that makes the discard safe (#2894 C2).
//
// The clear runs in memory immediately before the fetch and is persisted only
// by that fetch's own successful write. So when the provider errors -- outage,
// rate limit, network -- nothing was saved and the artist keeps every ID it
// had. An earlier cut cleared in the SAME write that stored the new identity,
// which committed the clear before the refresh had run: the operator got a 500,
// FEWER provider IDs, and the SAME wrong metadata. Strictly worse than the bug.
//
// Both entry points are asserted because the scope of this fix is INTENT, not
// handler: the link handler surfaces the failure as 500, the wizard logs it and
// still advances, and the ID-preservation contract is identical on both.
func TestReIdentify_FailedRefreshLeavesEveryProviderIDIntact(t *testing.T) {
	t.Parallel()
	errRefreshFailed := errors.New("provider unavailable during re-identify")

	// assertNothingWasLost is the shared half: every seeded ID still on the
	// row, and the repudiated origin untouched.
	assertNothingWasLost := func(t *testing.T, r *Router, svc *artist.Service, artistID, afterWhat string) {
		t.Helper()
		assertSecondaryIDsSurvive(t, r.db, artistID, afterWhat)
		if exists, pid := reidentifyProviderRow(t, r.db, artistID, string(provider.NameDiscogs)); !exists || pid != "99" {
			t.Errorf("discogs row after %s: exists=%v provider_id=%q, want exists=true provider_id=%q; a failed refresh stranded the artist with fewer IDs than it started with (#2894)", afterWhat, exists, pid, "99")
		}
		reloaded, err := svc.GetByID(context.Background(), artistID)
		if err != nil {
			t.Fatalf("reloading artist: %v", err)
		}
		if reloaded.AudioDBID != "adb-seed" {
			t.Errorf("AudioDBID = %q, want %q after %s; AudioDB is the one ID EnrichProviderIDs cannot re-derive, so losing it on a failed refresh is unrecoverable", reloaded.AudioDBID, "adb-seed", afterWhat)
		}
		if reloaded.Origin != repudiatedOrigin {
			t.Errorf("origin = %q, want %q unchanged after %s; the refresh failed, so nothing should have been written", reloaded.Origin, repudiatedOrigin, afterWhat)
		}
	}

	t.Run("link handler returns 500", func(t *testing.T) {
		t.Parallel()
		r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
		rec := attachRecordingOrchestrator(t, r, nil, errRefreshFailed)

		a := seedReidentifyTarget(t, r.db, artistSvc, "Failed Link Reidentify", "/music/failed-link-reidentify")
		seedRepudiatedOrigin(t, artistSvc, a.ID)

		resp := postReidentifyLink(t, r, a.ID, `{"mbid":"mbid-link-corrected","source":"musicbrainz","clear_ids":"true"}`)
		if resp.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; a provider failure must be reported, not swallowed. body: %s", resp.Code, resp.Body.String())
		}
		// Precondition on the failure itself: the fetch really was attempted,
		// so "the IDs survived" is not passing because nothing ran.
		if n := rec.callCount(); n != 1 {
			t.Fatalf("provider fetch ran %d times, want 1; the 500 came from somewhere other than the refresh", n)
		}
		assertNothingWasLost(t, r, artistSvc, a.ID, "a FAILED re-identify refresh on the link handler")
	})

	t.Run("wizard advances and keeps the IDs", func(t *testing.T) {
		t.Parallel()
		r, _, artistSvc := testRouterWithIdentify(t)
		rec := attachRecordingOrchestrator(t, r, nil, errRefreshFailed)

		a := seedReidentifyTarget(t, r.db, artistSvc, "Failed Wizard Reidentify", "/music/failed-wizard-reidentify")
		seedRepudiatedOrigin(t, artistSvc, a.ID)

		sess := startWizardOn(t, r, a.ID)
		// autoLinkAndRefresh logs a refresh failure and does not propagate it,
		// so the wizard advances. That is deliberate -- one unreachable
		// provider must not abort a multi-artist run -- and it is exactly why
		// the ID-preservation contract has to hold here too.
		w := postWizardAccept(t, r, sess, `{"mbid":"mbid-wizard-corrected"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if n := rec.callCount(); n != 1 {
			t.Fatalf("provider fetch ran %d times, want 1; the refresh was never attempted", n)
		}
		assertNothingWasLost(t, r, artistSvc, a.ID, "a FAILED re-identify refresh in the wizard")
	})
}

// TestReIdentify_LockedArtistKeepsEveryProviderID pins #2894 I1.
//
// A locked artist gets its new identity persisted (a manual edit, which the
// lock permits) and its refresh suppressed (the automated change the lock
// promises to skip). The discard must sit AFTER that gate. An earlier ordering
// cleared first, so the refresh that re-derives the IDs never ran and the clear
// was PERMANENT -- and AudioDB has no branch in EnrichProviderIDs, so it does
// not come back at all until some later refresh happens to query AudioDB for a
// field it does not populate. There is no undo.
//
// Both entry points again, for the same intent-not-handler reason.
func TestReIdentify_LockedArtistKeepsEveryProviderID(t *testing.T) {
	t.Parallel()

	assertEveryIDSurvived := func(t *testing.T, r *Router, svc *artist.Service, artistID, afterWhat string) {
		t.Helper()
		assertSecondaryIDsSurvive(t, r.db, artistID, afterWhat)
		if exists, pid := reidentifyProviderRow(t, r.db, artistID, string(provider.NameDiscogs)); !exists || pid != "99" {
			t.Errorf("discogs row after %s: exists=%v provider_id=%q, want exists=true provider_id=%q", afterWhat, exists, pid, "99")
		}
		reloaded, err := svc.GetByID(context.Background(), artistID)
		if err != nil {
			t.Fatalf("reloading artist: %v", err)
		}
		if reloaded.AudioDBID != "adb-seed" {
			t.Errorf("AudioDBID = %q, want %q after %s; the refresh that would re-derive it never runs on a locked artist, so clearing it here destroys it permanently (#2894)", reloaded.AudioDBID, "adb-seed", afterWhat)
		}
		if reloaded.Origin != repudiatedOrigin {
			t.Errorf("origin = %q, want %q unchanged after %s; the lock must suppress the metadata refresh", reloaded.Origin, repudiatedOrigin, afterWhat)
		}
	}

	t.Run("link handler", func(t *testing.T) {
		t.Parallel()
		r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
		rec := attachRecordingOrchestrator(t, r, reidentifyFetchResult(), nil)

		a := seedReidentifyTarget(t, r.db, artistSvc, "Locked Link Reidentify", "/music/locked-link-reidentify")
		seedRepudiatedOrigin(t, artistSvc, a.ID)
		lockArtist(t, artistSvc, a.ID)

		resp := postReidentifyLink(t, r, a.ID, `{"mbid":"mbid-locked-link","source":"musicbrainz","clear_ids":"true"}`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.Code, resp.Body.String())
		}
		// Non-invocation is asserted directly, not inferred: with a live
		// orchestrator attached, an implementation that refreshed locked
		// artists anyway would otherwise look identical here.
		if n := rec.callCount(); n != 0 {
			t.Fatalf("provider fetch ran %d times, want 0; the artist lock did not suppress the refresh", n)
		}
		// The identity itself DID change -- the lock permits a manual edit.
		reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("reloading artist: %v", err)
		}
		if reloaded.MusicBrainzID != "mbid-locked-link" {
			t.Errorf("MusicBrainzID = %q, want %q; the lock must not block the operator's own edit", reloaded.MusicBrainzID, "mbid-locked-link")
		}
		assertEveryIDSurvived(t, r, artistSvc, a.ID, "a locked-artist re-identify on the link handler")
	})

	t.Run("wizard", func(t *testing.T) {
		t.Parallel()
		r, _, artistSvc := testRouterWithIdentify(t)
		rec := attachRecordingOrchestrator(t, r, reidentifyFetchResult(), nil)

		a := seedReidentifyTarget(t, r.db, artistSvc, "Locked Wizard Reidentify", "/music/locked-wizard-reidentify")
		seedRepudiatedOrigin(t, artistSvc, a.ID)
		lockArtist(t, artistSvc, a.ID)

		sess := startWizardOn(t, r, a.ID)
		w := postWizardAccept(t, r, sess, `{"mbid":"mbid-locked-wizard"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if n := rec.callCount(); n != 0 {
			t.Fatalf("provider fetch ran %d times, want 0; the artist lock did not suppress the wizard's refresh", n)
		}
		reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("reloading artist: %v", err)
		}
		if reloaded.MusicBrainzID != "mbid-locked-wizard" {
			t.Errorf("MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, "mbid-locked-wizard")
		}
		assertEveryIDSurvived(t, r, artistSvc, a.ID, "a locked-artist re-identify in the wizard")
	})
}

// TestAutoLinkAndRefresh_NonReidentifyCallersKeepEveryProviderID is the
// over-correction guard for #2894, and the reason the discard is gated on an
// explicit reidentify parameter rather than inferred inside the primitive.
//
// autoLinkAndRefresh is shared with the Deezer / Discogs / TheAudioDB link
// handlers and with bulk-identify. On those paths the artist's identity is NOT
// in question: the operator linked one provider ID and asserted nothing about
// the rest. Discarding there would be a fresh regression of #2714/#2725.
//
// Called directly rather than through a handler because the contract under test
// belongs to the primitive: the parameter is the whole mechanism, and routing
// through one handler would prove it for one caller.
func TestAutoLinkAndRefresh_NonReidentifyCallersKeepEveryProviderID(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
	rec := attachRecordingOrchestrator(t, r, reidentifyFetchResult(), nil)

	a := seedReidentifyTarget(t, r.db, artistSvc, "Plain Link", "/music/plain-link")
	seedRepudiatedOrigin(t, artistSvc, a.ID)

	loaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("loading artist: %v", err)
	}
	refreshSkipped, err := r.autoLinkAndRefresh(context.Background(), loaded, false, "")
	if err != nil {
		t.Fatalf("autoLinkAndRefresh: %v", err)
	}
	if refreshSkipped {
		t.Fatalf("refreshSkipped = true on an unlocked artist; the refresh path is not under test")
	}
	if n := rec.callCount(); n != 1 {
		t.Fatalf("provider fetch ran %d times, want 1", n)
	}
	// The mirror image of assertRefreshNotSteeredByRepudiatedIDs: here the IDs
	// SHOULD steer the fetch, because that is what a plain provider link is
	// for.
	steeredBy := rec.firstCall(t)
	if steeredBy[provider.NameAudioDB] != "adb-seed" {
		t.Errorf("the fetch was steered by audiodb=%q, want %q; a plain link must keep using the artist's existing provider IDs (#2714/#2725)", steeredBy[provider.NameAudioDB], "adb-seed")
	}
	assertSecondaryIDsSurvive(t, r.db, a.ID, "a NON-re-identify autoLinkAndRefresh")
	if exists, pid := reidentifyProviderRow(t, r.db, a.ID, string(provider.NameDiscogs)); !exists || pid != "99" {
		t.Errorf("discogs row after a non-re-identify link: exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "99")
	}
}

// TestAutoLinkAndRefresh_ReidentifyKeepsThisRequestsDiscogsPick pins the
// keepDiscogsID parameter (#2894).
//
// A wizard Discogs pick is the operator's own choice for the NEW identity, not
// a leftover from the repudiated one. Without this carve-out the accept assigns
// the chosen ID and its own discard clears it one line later -- the pick
// silently does nothing.
func TestAutoLinkAndRefresh_ReidentifyKeepsThisRequestsDiscogsPick(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
	rec := attachRecordingOrchestrator(t, r, reidentifyFetchResult(), nil)

	a := seedReidentifyTarget(t, r.db, artistSvc, "Discogs Pick Keep", "/music/discogs-pick-keep")
	seedRepudiatedOrigin(t, artistSvc, a.ID)

	loaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("loading artist: %v", err)
	}
	const pickedDiscogsID = "4321"
	// Precondition: the pick must DIFFER from the seeded value, or "the pick
	// survived" and "the discard never ran" are indistinguishable.
	if loaded.DiscogsID == pickedDiscogsID {
		t.Fatalf("precondition: the seeded Discogs ID already equals the pick %q", pickedDiscogsID)
	}
	loaded.DiscogsID = pickedDiscogsID
	loaded.MusicBrainzID = "mbid-pick-corrected"
	if _, err := r.autoLinkAndRefresh(context.Background(), loaded, true, pickedDiscogsID); err != nil {
		t.Fatalf("autoLinkAndRefresh: %v", err)
	}
	if n := rec.callCount(); n != 1 {
		t.Fatalf("provider fetch ran %d times, want 1", n)
	}
	// The pick steers the fetch; everything belonging to the repudiated entity
	// does not.
	steeredBy := rec.firstCall(t)
	if steeredBy[provider.NameDiscogs] != pickedDiscogsID {
		t.Errorf("the fetch was steered by discogs=%q, want the operator's own pick %q; the discard cleared the replacement it was supposed to preserve", steeredBy[provider.NameDiscogs], pickedDiscogsID)
	}
	if steeredBy[provider.NameAudioDB] != "" {
		t.Errorf("the fetch was steered by audiodb=%q; keepDiscogsID must preserve ONE ID, not disable the discard", steeredBy[provider.NameAudioDB])
	}
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.DiscogsID != pickedDiscogsID {
		t.Errorf("DiscogsID = %q, want %q; the operator's pick did not survive its own re-identify", reloaded.DiscogsID, pickedDiscogsID)
	}
	assertSecondaryIDsDiscarded(t, r.db, a.ID, "a re-identify carrying a Discogs pick")
}

// TestReIdentifyWizard_LockedNoRefreshNoticeSurvivesTheJourney closes the gap
// between "the flag reaches the session" and "the operator sees it".
//
// Three tests already touch this notice and none of them proves it: the accept
// tests assert the session field and the JSON flag, and
// TestWizardLockedNoRefreshNoticeRenders calls the templ component with
// hand-built data. Nothing connects them. A flag threaded correctly into a
// fragment that the handler never populates renders nothing, and every one of
// those tests stays green -- which is the same silent half-completion #2894 is
// about, one layer up.
//
// So this drives the REAL handlers end to end and reads the rendered HTML: lock
// an artist, accept it in the wizard, then fetch the next step the way the
// operator's browser does. The step fetch is the part that matters -- it is
// Back and reload, a path that reaches the notice through
// handleReIdentifyWizardStep rather than through the accept's own response, and
// losing it there drops the only mid-run record of artists still holding the
// previous match's metadata.
func TestReIdentifyWizard_LockedNoRefreshNoticeSurvivesTheJourney(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	rec := attachRecordingOrchestrator(t, r, reidentifyFetchResult(), nil)
	ctx := context.Background()

	const lockedName = "Locked Journey Artist"
	a := &artist.Artist{ID: "wizJourney1", Name: lockedName, AudioDBID: "adb-journey"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	lockArtist(t, artistSvc, a.ID)

	sess := startWizardOn(t, r, a.ID)
	if w := postWizardAccept(t, r, sess, `{"mbid":"mbid-journey"}`); w.Code != http.StatusOK {
		t.Fatalf("accept status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// Precondition: the lock really suppressed the refresh, so the notice is
	// warranted rather than being rendered on every run.
	if n := rec.callCount(); n != 0 {
		t.Fatalf("provider fetch ran %d times, want 0; the lock did not suppress the refresh, so the notice would be a lie", n)
	}

	// Now the browser fetches step 1 -- Back, or a reload. HX-Request so
	// renderTempl emits the fragment rather than the full page wrapper.
	// testI18nCtx is required, not decoration: this rig wires no i18n bundle, so
	// without it every t() call renders its KEY and the copy assertion below
	// fails against a notice that is in fact present.
	req := httptest.NewRequestWithContext(
		testI18nCtx(t, middleware.WithTestUserID(ctx, "test-user")),
		http.MethodGet, "/artists/re-identify/wizard/"+sess.ID+"/step/1", nil)
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "1")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Accept-Language", "en")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardStep(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("step status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	// Asserted on the localized copy and role="status", not on Tailwind
	// classes, matching TestWizardLockedNoRefreshNoticeRenders's contract so a
	// restyle does not break this.
	if !strings.Contains(body, "still reflects the previous match") {
		t.Errorf("the locked-no-refresh notice is absent from the step fragment; the operator navigated and lost the only record that %q still holds the previous match's metadata (#2894)", lockedName)
	}
	if !strings.Contains(body, `role="status"`) {
		t.Errorf("the notice rendered without role=\"status\"; a screen-reader user is told nothing")
	}
	if !strings.Contains(body, lockedName) {
		t.Errorf("the notice does not name %q; an operator cannot act on a count", lockedName)
	}
}
