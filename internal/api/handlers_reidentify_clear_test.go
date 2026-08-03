package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// reidentifyProviderRow reads a single artist_provider_ids row directly from the
// database so assertions target actual persisted state, not a counter or a
// handler return value.
func reidentifyProviderRow(t *testing.T, db *sql.DB, artistID, prov string) (exists bool, providerID string) {
	t.Helper()
	var pid string
	err := db.QueryRowContext(context.Background(),
		`SELECT provider_id FROM artist_provider_ids WHERE artist_id = ? AND provider = ?`,
		artistID, prov).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		t.Fatalf("querying %s row for artist %s: %v", prov, artistID, err)
	}
	return true, pid
}

// seededSecondaryIDs is the set of provider IDs beyond the MusicBrainz
// identity itself. Whether they survive an operation depends on WHICH
// operation, and the two cases are now different (#2894):
//
//   - A plain (non-destructive) Identify must never touch them. The operator
//     linked one ID and asserted nothing about the rest, so discarding them
//     destroys correct data to fix one field. assertSecondaryIDsSurvive pins
//     that, and it is still the live contract on every non-re-identify path.
//
//   - A RE-IDENTIFY discards them. See assertSecondaryIDsDiscarded and the
//     clearing site in handleRefreshLink for the reasoning.
var seededSecondaryIDs = map[provider.ProviderName]string{
	provider.NameAudioDB:  "adb-seed",
	provider.NameWikidata: "Q-seed",
	provider.NameDeezer:   "dz-seed",
	provider.NameSpotify:  "sp-seed",
}

// assertSecondaryIDsSurvive reads every seededSecondaryIDs row back out of the
// database and requires it to still carry its seeded value. This is what turns
// a blanket "wipe every provider ID" implementation red.
//
// Scope note (#2894): this pins the NON-re-identify paths -- a plain Identify,
// an unrecognized clear_ids value, an intent with no replacement. It is
// deliberately no longer asserted on the re-identify path, where the opposite
// is now required; assertSecondaryIDsDiscarded covers that side. Both
// assertions are kept live so neither direction can regress silently: this one
// still fails a blanket wipe, and that one still fails a blanket preserve.
func assertSecondaryIDsSurvive(t *testing.T, db *sql.DB, artistID, afterWhat string) {
	t.Helper()
	for prov, want := range seededSecondaryIDs {
		exists, pid := reidentifyProviderRow(t, db, artistID, string(prov))
		if !exists || pid != want {
			t.Errorf("%s row after %s: exists=%v provider_id=%q, want exists=true provider_id=%q; a NON-destructive link discarded a provider ID the operator's choice said nothing about",
				prov, afterWhat, exists, pid, want)
		}
	}
}

// assertSecondaryIDsDiscarded is the re-identify counterpart, and the reason
// #2894 was a defect rather than a cosmetic gap.
//
// A surviving stale secondary ID does not merely sit there unused: it STEERS
// the follow-up refresh. FetchProviderResult prefers a provider-specific ID
// over the MBID (provider_result.go:60-67), so a stale AudioDB ID makes the
// refresh re-fetch the artist the operator just repudiated, and AudioDB is the
// #2 origin provider (settings.go:407). The identity is corrected, the refresh
// visibly succeeds, and the wrong origin comes straight back.
//
// Asserted against persisted rows, never a handler return value: a 200 says
// nothing about what the flow did to the artist.
func assertSecondaryIDsDiscarded(t *testing.T, db *sql.DB, artistID, afterWhat string) {
	t.Helper()
	for prov := range seededSecondaryIDs {
		exists, pid := reidentifyProviderRow(t, db, artistID, string(prov))
		if exists && pid != "" {
			t.Errorf("%s row after %s: still carries provider_id=%q; a stale secondary ID survived a re-identify and will steer the follow-up refresh back to the repudiated artist (#2894)",
				prov, afterWhat, pid)
		}
	}
}

// seedReidentifyTarget creates an artist carrying a full provider identity --
// a MusicBrainz ID, a Discogs ID with a fetched_at stamp, and every secondary
// modeled ID -- plus an orphan-provider fetched_at row (allmusic), and asserts
// every one of those rows exists. Those preconditions are what keep the "row
// is gone" and "row survived" assertions below from passing against a database
// that never had the row in the first place.
func seedReidentifyTarget(t *testing.T, db *sql.DB, svc *artist.Service, name, path string) *artist.Artist {
	t.Helper()
	ctx := context.Background()
	a := &artist.Artist{
		Name:      name,
		SortName:  name,
		Type:      "group",
		Path:      path,
		DiscogsID: "99",
	}
	fetched := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	a.DiscogsIDFetchedAt = &fetched
	a.MusicBrainzID = "mbid-seed"
	a.AudioDBID = seededSecondaryIDs[provider.NameAudioDB]
	a.WikidataID = seededSecondaryIDs[provider.NameWikidata]
	a.DeezerID = seededSecondaryIDs[provider.NameDeezer]
	a.SpotifyID = seededSecondaryIDs[provider.NameSpotify]
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.UpdateProviderFetchedAt(ctx, a.ID, string(provider.NameAllMusic)); err != nil {
		t.Fatalf("stamp allmusic orphan: %v", err)
	}

	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "99" {
		t.Fatalf("precondition: discogs row exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "99")
	}
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists || pid != "mbid-seed" {
		t.Fatalf("precondition: musicbrainz row exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "mbid-seed")
	}
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Fatalf("precondition: allmusic orphan row missing before the flow starts")
	}
	for prov, want := range seededSecondaryIDs {
		if exists, pid := reidentifyProviderRow(t, db, a.ID, string(prov)); !exists || pid != want {
			t.Fatalf("precondition: %s row exists=%v provider_id=%q, want exists=true provider_id=%q", prov, exists, pid, want)
		}
	}
	return a
}

// postReidentify drives the REAL handleReidentify with a form body. Non-HTMX so
// the JSON branch answers and no templ rendering is needed.
func postReidentify(t *testing.T, r *Router, artistID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+artistID+"/reidentify",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", artistID)
	rec := httptest.NewRecorder()
	r.handleReidentify(rec, req)
	return rec
}

// TestHandleReidentify_ClearIDsPersistsNothing is issue #2714 stated as an
// assertion: the destructive re-identify entry point must not commit the wipe.
//
// Before the fix, handleReidentify blanked the seven struct-modeled provider
// fields and called Update immediately, so an operator who opened the
// re-identify flow and then abandoned it -- closed the tab, got no usable
// search results, changed their mind -- was left with an artist carrying NO
// provider IDs at all. That is strictly worse than the state they started in
// and there is no undo. This test simulates exactly that abandonment: start the
// flow, never link a replacement, and read the provider IDs back OUT OF THE
// DATABASE.
//
// Every assertion targets a persisted row, never the handler's status code: a
// 200 tells you nothing about what the flow did to the artist, and asserting on
// it is how this class of bug survives a test suite.
func TestHandleReidentify_ClearIDsPersistsNothing(t *testing.T) {
	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := artist.NewService(db)
	r := &Router{
		logger:        logger,
		artistService: svc,
		db:            db,
	}

	a := seedReidentifyTarget(t, db, svc, "Abandoned Reidentify", "/music/abandoned-reidentify")

	// Start the destructive flow -- and stop there. This is the whole bug.
	rec := postReidentify(t, r, a.ID, "clear_ids=true")
	if rec.Code != 200 {
		t.Fatalf("handleReidentify status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// The artist must be exactly as it was. Read back from the DB.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists || pid != "mbid-seed" {
		t.Errorf("musicbrainz row after abandoned re-identify: exists=%v provider_id=%q, want exists=true provider_id=%q; the artist was stranded with no identity (#2714)", exists, pid, "mbid-seed")
	}
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "99" {
		t.Errorf("discogs row after abandoned re-identify: exists=%v provider_id=%q, want exists=true provider_id=%q (#2714)", exists, pid, "99")
	}
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by an abandoned re-identify")
	}
	assertSecondaryIDsSurvive(t, db, a.ID, "an abandoned re-identify")

	// The struct the rest of the app reads must agree with the rows.
	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.MusicBrainzID != "mbid-seed" {
		t.Errorf("reloaded MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, "mbid-seed")
	}
	if reloaded.DiscogsID != "99" {
		t.Errorf("reloaded DiscogsID = %q, want %q", reloaded.DiscogsID, "99")
	}
}

// postReidentifyLink drives the REAL handleRefreshLink with a JSON body.
func postReidentifyLink(t *testing.T, r *Router, artistID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+artistID+"/refresh/link",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", artistID)
	rec := httptest.NewRecorder()
	r.handleRefreshLink(rec, req)
	return rec
}

// TestHandleRefreshLink_MusicBrainzPickReplacesMBIDAndDiscardsStaleIDs covers
// the ordinary completed re-identify: the operator picks a MusicBrainz
// candidate.
//
// This test previously asserted the OPPOSITE for the secondary IDs, under the
// name ...KeepsTheRest, on the reasoning that "a wrong MusicBrainz ID does not
// make a correct Discogs ID wrong". #2894 showed that reasoning does not hold
// on THIS path, and the inversion is deliberate rather than a regression:
//
//   - #2714/#2725 were about repairing a bad MBID on an artist that was
//     otherwise correctly identified. Preserving the other IDs was right there,
//     and assertSecondaryIDsSurvive still pins it on every such path.
//   - A re-identify is a different claim. The operator is declaring the whole
//     ENTITY wrong, and the secondary IDs were harvested from the repudiated
//     entity's own provider responses in the first place (EnrichProviderIDs).
//     They are not independent facts about a correct artist; they are more of
//     the same wrong answer.
//
// The concrete harm is that a surviving stale ID STEERS the follow-up refresh:
// FetchProviderResult prefers a provider-specific ID over the MBID, so the
// refresh re-fetches the repudiated artist and writes its origin and biography
// straight back over the corrected identity.
//
// bulkRefreshRouter supplies a stubbed orchestrator so the follow-up refresh
// really runs and the handler reaches its success response, exercising the
// whole handler rather than aborting partway.
func TestHandleRefreshLink_MusicBrainzPickReplacesMBIDAndDiscardsStaleIDs(t *testing.T) {
	r, svc := bulkRefreshRouter(t, lockedRefreshFetchResult())
	db := r.db

	a := seedReidentifyTarget(t, db, svc, "Completed Reidentify", "/music/completed-reidentify")

	const replacementMBID = "mbid-replacement"
	rec := postReidentifyLink(t, r, a.ID, `{"mbid":"`+replacementMBID+`","source":"musicbrainz","clear_ids":"true"}`)
	if rec.Code != 200 {
		t.Fatalf("handleRefreshLink status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// The replacement must be what landed -- not the old value, and not nothing.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists || pid != replacementMBID {
		t.Errorf("musicbrainz row after completed re-identify: exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, replacementMBID)
	}
	// Discogs is discarded along with the rest. This request supplied no
	// Discogs ID, so there is nothing to keep, and the seeded one belongs to
	// the repudiated entity. Discogs supplies `biography` (provider.go:88), one
	// of the exact fields #2894 reports surviving a re-identify.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); exists && pid != "" {
		t.Errorf("discogs row after a MusicBrainz re-identify pick: still carries provider_id=%q; it belongs to the repudiated entity and will steer the refresh's biography lookup back to it (#2894)", pid)
	}
	assertSecondaryIDsDiscarded(t, db, a.ID, "a MusicBrainz re-identify pick")
	// The orphan row must SURVIVE the write (#2725). The discard is scoped to
	// modeled provider identities; it is not a blanket delete of every row for
	// this artist, and that boundary is unchanged by #2894.
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by re-identify; scoped delete boundary is wrong (#2725)")
	}

	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.MusicBrainzID != replacementMBID {
		t.Errorf("reloaded MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, replacementMBID)
	}
}

// TestHandleRefreshLink_DiscogsPickDiscardsRepudiatedMBID is the case the
// per-provider model exists for, and the guard against the "never discard
// anything" over-correction.
//
// Disambiguation search returns MusicBrainz AND Discogs candidates. A Discogs
// card carries no MusicBrainz ID, so it supplies no replacement MBID. Without
// an explicit discard the artist keeps its OLD, operator-repudiated MusicBrainz
// ID sitting alongside the new Discogs one: the operator said "this artist is
// someone else", and the field that identity claim is about survives untouched
// and known-wrong. Deferring the wipe (#2714) must not reintroduce that, and it
// must not degrade into "re-identify writes nothing away, ever".
//
// The secondary IDs are asserted present afterwards so the fix cannot be a
// blanket wipe wearing a different name.
func TestHandleRefreshLink_DiscogsPickDiscardsRepudiatedMBID(t *testing.T) {
	r, svc := bulkRefreshRouter(t, lockedRefreshFetchResult())
	db := r.db

	a := seedReidentifyTarget(t, db, svc, "Discogs Pick Reidentify", "/music/discogs-pick-reidentify")

	rec := postReidentifyLink(t, r, a.ID, `{"discogs_id":"1234","source":"discogs","clear_ids":"true"}`)
	if rec.Code != 200 {
		t.Fatalf("handleRefreshLink status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// The repudiated MusicBrainz identity must be gone -- not still "mbid-seed".
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); exists {
		t.Errorf("musicbrainz row still present (provider_id=%q) after a Discogs re-identify pick; the operator declared this artist misidentified and the wrong MBID survived", pid)
	}
	// The link the operator DID make must be honored.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "1234" {
		t.Errorf("discogs row after link: exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "1234")
	}
	// The stale secondaries go too (#2894). The scoping that matters is no
	// longer "MusicBrainz only" but "this request's replacements are kept, the
	// repudiated entity's IDs are not" -- the Discogs assertion just above is
	// the kept half, and this is the discarded half.
	assertSecondaryIDsDiscarded(t, db, a.ID, "a Discogs re-identify pick")
	// The orphan row must SURVIVE the delete (#2725). This is the path where a
	// modeled row really is deleted, so the boundary is live here.
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by the re-identify discard; scoped delete boundary is wrong (#2725)")
	}

	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.MusicBrainzID != "" {
		t.Errorf("reloaded MusicBrainzID = %q, want empty; the repudiated identity survived a Discogs re-identify pick", reloaded.MusicBrainzID)
	}
	if reloaded.DiscogsID != "1234" {
		t.Errorf("reloaded DiscogsID = %q, want %q", reloaded.DiscogsID, "1234")
	}
}

// TestHandleRefreshLink_DiscardRequiresIntentAndReplacement is the
// over-correction guard in the other direction: "always discard" must be as
// red as "never discard".
//
// The gate is a positive allow-list -- the re-identify intent AND a replacement
// identity, both asserted true -- rather than a negated safe-list, because it
// decides a DELETE. Each subtest is a request shape that a negated guard, or a
// discard hoisted out of its condition, would wave through:
//
//   - intent with no replacement at all: nothing to replace the MBID with, so
//     discarding it strands the artist exactly as the original bug did;
//   - a plain (non-destructive) Identify that links a Discogs ID: this is the
//     concurrent-tab case the request-scoped intent exists to protect. No
//     operator asserted misidentification, so no identity may be discarded;
//   - an unrecognized clear_ids value: "1" is not the sanctioned "true", and an
//     ambiguous signal must fall through to the non-destructive path.
//
// Every subtest also asserts the link the operator DID make still landed, so
// none of them can pass by way of a handler that rejects the request outright.
func TestHandleRefreshLink_DiscardRequiresIntentAndReplacement(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantDiscogsID string // "" means no discogs link was requested
	}{
		{
			name: "intent but no replacement identity",
			body: `{"source":"musicbrainz","clear_ids":"true"}`,
		},
		{
			name:          "plain identify linking a discogs id",
			body:          `{"discogs_id":"1234","source":"discogs"}`,
			wantDiscogsID: "1234",
		},
		{
			name:          "unrecognized clear_ids value",
			body:          `{"discogs_id":"1234","source":"discogs","clear_ids":"1"}`,
			wantDiscogsID: "1234",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, svc := bulkRefreshRouter(t, lockedRefreshFetchResult())
			db := r.db

			a := seedReidentifyTarget(t, db, svc, "Preserve "+tc.name, "/music/preserve-"+tc.name)

			postReidentifyLink(t, r, a.ID, tc.body)

			// The seeded identity must be untouched.
			if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists || pid != "mbid-seed" {
				t.Errorf("musicbrainz row: exists=%v provider_id=%q, want exists=true provider_id=%q; an identity was discarded without both the re-identify intent and a replacement", exists, pid, "mbid-seed")
			}
			assertSecondaryIDsSurvive(t, db, a.ID, "a non-destructive link")
			if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
				t.Errorf("allmusic orphan row destroyed on a non-destructive link")
			}

			// The link the operator DID make must still be honored, so this
			// cannot pass against a handler that simply refused the request.
			wantDiscogs := tc.wantDiscogsID
			if wantDiscogs == "" {
				wantDiscogs = "99" // the seeded value, untouched
			}
			if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != wantDiscogs {
				t.Errorf("discogs row: exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, wantDiscogs)
			}

			reloaded, err := svc.GetByID(context.Background(), a.ID)
			if err != nil {
				t.Fatalf("reloading artist: %v", err)
			}
			if reloaded.MusicBrainzID != "mbid-seed" {
				t.Errorf("reloaded MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, "mbid-seed")
			}
		})
	}
}

// TestHandleReidentify_RendersClearIDsIntoTheForm pins HOP 1 of the
// request-scoped intent chain.
//
// The intent lives in the request, not on the artist row, so it survives only
// as long as every hop forwards it: handleReidentify renders the hidden
// clear_ids field into the disambiguation form -> the search POST carries it ->
// the candidate card's hx-vals carry it -> handleRefreshLink reads it back.
// Drop it at this first hop and NOTHING fails loudly: the search still runs,
// the operator still links a candidate, and re-identify silently degrades into
// plain identify, leaving the repudiated MusicBrainz ID in place. The rest of
// the suite starts downstream at the search handler with the field injected by
// hand, so this is the only place that failure is visible.
//
// The rendered HTML is the subject because the hidden input IS the contract
// with the browser. Both directions are asserted: a form that emits the field
// unconditionally would arm every plain Identify with destructive intent.
func TestHandleReidentify_RendersClearIDsIntoTheForm(t *testing.T) {
	renderForm := func(t *testing.T, body string) string {
		t.Helper()
		db := newTestDB(t)
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		svc := artist.NewService(db)
		r := &Router{logger: logger, artistService: svc, db: db}

		a := seedReidentifyTarget(t, db, svc, "Form Hop "+body, "/music/form-hop-"+body)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/reidentify",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.SetPathValue("id", a.ID)
		req = req.WithContext(testI18nCtx(t, req.Context()))
		rec := httptest.NewRecorder()
		r.handleReidentify(rec, req)

		if rec.Code != 200 {
			t.Fatalf("handleReidentify status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		html := rec.Body.String()
		// Precondition: the disambiguation form really rendered. Without it an
		// "the field is absent" assertion would pass against empty output.
		if !strings.Contains(html, `name="query"`) {
			t.Fatalf("precondition: no disambiguation form rendered; body: %s", html)
		}
		return html
	}

	t.Run("re-identify renders the hidden field", func(t *testing.T) {
		html := renderForm(t, "clear_ids=true")
		if !strings.Contains(html, `name="clear_ids"`) {
			t.Errorf("disambiguation form omits the clear_ids hidden field; the re-identify intent is dropped at the first hop and the flow silently degrades to plain identify (#2714). body: %s", html)
		}
	})

	t.Run("plain identify does not", func(t *testing.T) {
		html := renderForm(t, "")
		if strings.Contains(html, `name="clear_ids"`) {
			t.Errorf("disambiguation form carries clear_ids on the NON-destructive identify flow; linking would discard an identity the operator never repudiated. body: %s", html)
		}
	})
}

// steeringRecorder is a ScrapeAll stub that records the identity it was steered
// by and answers with a DIFFERENT origin depending on that identity.
//
// This is the machinery that would misbehave, wired live. A test that only
// asserts "the stale rows are gone" passes against a fix that clears IDs
// without changing what the refresh actually fetches; this one fails unless the
// corrected identity is what reaches the provider layer.
// The mutex matches recordingScraperExecutor in handlers_reidentify_discard_test.go
// deliberately. Today every refresh here runs inline on the handler goroutine, so
// there is no race to detect; the synchronization is for the change that has not
// happened yet. Two recorders in one package with opposite contracts is a trap --
// adding t.Parallel() to a test here, or moving the refresh onto a goroutine, would
// turn this into a silent data race surfacing as an unrelated -race failure.
type steeringRecorder struct {
	mu          sync.Mutex
	calls       int
	gotMBID     string
	gotProvider map[provider.ProviderName]string
	// originFor maps the steering ID actually used to the origin that identity
	// returns, mirroring FetchProviderResult's "provider ID beats MBID" rule.
	staleOrigin     string
	correctedOrigin string
}

func (s *steeringRecorder) ScrapeAll(_ context.Context, mbid, _, _ string, ids map[provider.ProviderName]string) (*provider.FetchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.gotMBID = mbid
	s.gotProvider = map[provider.ProviderName]string{}
	for k, v := range ids {
		s.gotProvider[k] = v
	}
	// Reproduce the production steering rule: a non-empty provider-specific ID
	// wins over the MBID (provider_result.go:60-67). AudioDB is the #2 origin
	// provider (settings.go:407), so its stale ID is what decides the answer.
	origin := s.correctedOrigin
	if ids[provider.NameAudioDB] != "" {
		origin = s.staleOrigin
	}
	return &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Origin: origin, YearsActive: "1990-present"},
		AttemptedFields: []string{"origin", "years_active"},
		PopulatedFields: []string{"origin", "years_active"},
	}, nil
}

// callCount, steeringMBID and steeringIDs read the recorded state under the same
// lock ScrapeAll writes it with. Locking only the writer leaves the read side
// unsynchronized, which is the same race with an extra step.
func (s *steeringRecorder) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *steeringRecorder) steeringMBID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotMBID
}

// steeringIDs returns a copy, so an assertion cannot observe the map mutating
// under it if a later change starts issuing concurrent fetches.
func (s *steeringRecorder) steeringIDs() map[provider.ProviderName]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[provider.ProviderName]string, len(s.gotProvider))
	for k, v := range s.gotProvider {
		out[k] = v
	}
	return out
}

// steeringRouter wires a steeringRecorder in place of the scraper executor.
func steeringRouter(t *testing.T, rec *steeringRecorder) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger, nil)
	orch.SetExecutor(rec)
	r.orchestrator = orch
	return r, artistSvc
}

// TestHandleRefreshLink_ReidentifyRefreshResolvesFromCorrectedIdentity is the
// test with teeth for #2894, and the one that reproduces the production symptom
// on demand rather than merely failing.
//
// The reported symptom was not "an ID survived". It was: the operator
// re-identifies, the refresh visibly SUCCEEDS, and the wrong artist's origin is
// still there afterwards with nothing on screen saying so. Reverting the fix
// must reproduce exactly that, which is what the origin assertion below pins.
//
// Mechanism under test, end to end: handleRefreshLink persists the corrected
// identity, then executeRefresh passes a.ProviderIDMap() to the orchestrator,
// and FetchProviderResult prefers any non-empty provider-specific ID over the
// MBID. A surviving stale AudioDB ID therefore re-fetches the repudiated
// artist, and AudioDB is the #2 origin provider, so its answer wins the
// first-match-wins priority loop before MusicBrainz is ever consulted.
func TestHandleRefreshLink_ReidentifyRefreshResolvesFromCorrectedIdentity(t *testing.T) {
	const (
		staleOrigin     = "Wrong Country"
		correctedOrigin = "Correct City, Correct Country"
	)
	rec := &steeringRecorder{staleOrigin: staleOrigin, correctedOrigin: correctedOrigin}
	r, svc := steeringRouter(t, rec)
	db := r.db

	a := seedReidentifyTarget(t, db, svc, "Steering Reidentify", "/music/steering-reidentify")

	// Precondition: the artist really starts out carrying the stale AudioDB ID
	// that would steer the refresh. Without this the whole test could pass
	// against a fixture that never had one.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameAudioDB)); !exists || pid == "" {
		t.Fatalf("precondition: audiodb row exists=%v provider_id=%q, want a non-empty stale ID to steer the refresh", exists, pid)
	}

	const replacementMBID = "mbid-corrected"
	resp := postReidentifyLink(t, r, a.ID, `{"mbid":"`+replacementMBID+`","source":"musicbrainz","clear_ids":"true"}`)
	if resp.Code != 200 {
		t.Fatalf("handleRefreshLink status = %d, want 200; body: %s", resp.Code, resp.Body.String())
	}

	// Precondition: the refresh actually ran. A refresh that never happened
	// must not read as a pass -- "origin unchanged" would be trivially true.
	if got := rec.callCount(); got != 1 {
		t.Fatalf("precondition: orchestrator called %d times, want exactly 1; the follow-up refresh did not run, so every assertion below is vacuous", got)
	}

	// The provider layer must have been steered by the CORRECTED identity.
	if got := rec.steeringMBID(); got != replacementMBID {
		t.Errorf("refresh was steered by mbid %q, want %q", got, replacementMBID)
	}
	steered := rec.steeringIDs()
	for _, prov := range []provider.ProviderName{provider.NameAudioDB, provider.NameDeezer, provider.NameSpotify} {
		if got := steered[prov]; got != "" {
			t.Errorf("refresh was handed stale %s id %q; FetchProviderResult prefers a provider-specific ID over the MBID, so this re-fetches the artist the operator just repudiated (#2894)", prov, got)
		}
	}

	// The payoff, and the production symptom stated as an assertion.
	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.Origin != correctedOrigin {
		t.Errorf("origin after re-identify = %q, want %q; the identity was corrected and the refresh SUCCEEDED, yet the repudiated artist's origin survived -- this is #2894 exactly", reloaded.Origin, correctedOrigin)
	}
}

// TestHandleRefreshLink_ReidentifyHonorsFieldLocks is acceptance criterion 2
// asserted rather than assumed: clearing provider IDs must not become a back
// door around a per-field lock.
//
// The lock is enforced inside artist.ApplyMetadata, which reads a.LockedFields
// off the artist on every call (#2749), so this is a guard against a future
// change to the refresh path rather than to the merge itself. It is worth
// having precisely because #2894's fix makes the refresh MORE effective: a
// refresh that now genuinely overwrites origin is exactly the one that would
// trample a pinned origin if the lock were ever bypassed.
//
// The fixture differs along BOTH axes on purpose. Asserting only "origin
// unchanged" would pass against a refresh that wrote nothing at all, so an
// UNLOCKED field is asserted to have changed in the same call. That is what
// makes "unchanged" a real result instead of a coincidence.
func TestHandleRefreshLink_ReidentifyHonorsFieldLocks(t *testing.T) {
	const (
		pinnedOrigin    = "Operator Pinned Origin"
		correctedOrigin = "Provider Supplied Origin"
	)
	rec := &steeringRecorder{staleOrigin: correctedOrigin, correctedOrigin: correctedOrigin}
	r, svc := steeringRouter(t, rec)
	db := r.db

	a := seedReidentifyTarget(t, db, svc, "Locked Field Reidentify", "/music/locked-field-reidentify")
	ctx := context.Background()
	a.Origin = pinnedOrigin
	a.YearsActive = "1970-1980"
	a.LockedFields = []string{"origin"}
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("seeding locked origin: %v", err)
	}

	// Precondition: the lock and the pinned value really persisted. A lock that
	// silently failed to save would make the whole test vacuous.
	seeded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seeded artist: %v", err)
	}
	if len(seeded.LockedFields) == 0 || seeded.Origin != pinnedOrigin {
		t.Fatalf("precondition: locked_fields=%v origin=%q, want a persisted origin lock pinning %q", seeded.LockedFields, seeded.Origin, pinnedOrigin)
	}
	// Precondition: the provider answers with a DIFFERENT origin, so "unchanged"
	// cannot pass by both sides being equal.
	if correctedOrigin == pinnedOrigin {
		t.Fatalf("precondition: fixture origins coincide, the lock assertion would be vacuous")
	}

	resp := postReidentifyLink(t, r, a.ID, `{"mbid":"mbid-corrected","source":"musicbrainz","clear_ids":"true"}`)
	if resp.Code != 200 {
		t.Fatalf("handleRefreshLink status = %d, want 200; body: %s", resp.Code, resp.Body.String())
	}
	if got := rec.callCount(); got != 1 {
		t.Fatalf("precondition: orchestrator called %d times, want exactly 1", got)
	}

	reloaded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.Origin != pinnedOrigin {
		t.Errorf("locked origin = %q, want %q; re-identify overwrote a field the operator pinned", reloaded.Origin, pinnedOrigin)
	}
	// The differing axis: an UNLOCKED field must have moved in the same call.
	if reloaded.YearsActive != "1990-present" {
		t.Errorf("unlocked years_active = %q, want %q; nothing was written at all, so the locked-origin assertion above proves nothing", reloaded.YearsActive, "1990-present")
	}
}
