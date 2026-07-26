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

// seededSecondaryIDs is the set of provider IDs that re-identify must NOT
// touch. The operator's assertion is "this artist is someone else", which
// repudiates the MusicBrainz identity that assertion is about; it says nothing
// about an AudioDB or Wikidata ID, and a wrong MBID does not make a correct
// AudioDB ID wrong. Wiping these as a unit destroys correct data to fix one
// incorrect field, so every re-identify test asserts they survive.
var seededSecondaryIDs = map[provider.ProviderName]string{
	provider.NameAudioDB:  "adb-seed",
	provider.NameWikidata: "Q-seed",
	provider.NameDeezer:   "dz-seed",
	provider.NameSpotify:  "sp-seed",
}

// assertSecondaryIDsSurvive reads every seededSecondaryIDs row back out of the
// database and requires it to still carry its seeded value. This is what turns
// a blanket "wipe every provider ID" implementation red.
func assertSecondaryIDsSurvive(t *testing.T, db *sql.DB, artistID, afterWhat string) {
	t.Helper()
	for prov, want := range seededSecondaryIDs {
		exists, pid := reidentifyProviderRow(t, db, artistID, string(prov))
		if !exists || pid != want {
			t.Errorf("%s row after %s: exists=%v provider_id=%q, want exists=true provider_id=%q; re-identify discarded a provider ID the operator's choice said nothing about",
				prov, afterWhat, exists, pid, want)
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

// TestHandleRefreshLink_MusicBrainzPickReplacesMBIDKeepsTheRest covers the
// ordinary completed re-identify: the operator picks a MusicBrainz candidate.
//
// The replacement supplies a new MBID, so the repudiated one is overwritten in
// the same write. Nothing else is: the discard is PER-PROVIDER, keyed to what
// the chosen candidate actually supplies. The operator asserted the artist is
// someone else, which is a statement about the MusicBrainz identity; it is not
// evidence that a correct Discogs, AudioDB, Wikidata, Deezer or Spotify ID has
// become wrong. Discarding those would destroy correct data to fix one
// incorrect field.
//
// bulkRefreshRouter supplies a stubbed orchestrator so the follow-up refresh
// really runs and the handler reaches its success response, exercising the
// whole handler rather than aborting partway.
func TestHandleRefreshLink_MusicBrainzPickReplacesMBIDKeepsTheRest(t *testing.T) {
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
	// The Discogs ID the chosen candidate said nothing about must survive.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "99" {
		t.Errorf("discogs row after a MusicBrainz re-identify pick: exists=%v provider_id=%q, want exists=true provider_id=%q; a wrong MusicBrainz ID does not make a correct Discogs ID wrong", exists, pid, "99")
	}
	assertSecondaryIDsSurvive(t, db, a.ID, "a MusicBrainz re-identify pick")
	// The orphan row must SURVIVE the write (#2725).
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
	// The discard is scoped to MusicBrainz, not a blanket wipe.
	assertSecondaryIDsSurvive(t, db, a.ID, "a Discogs re-identify pick")
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
