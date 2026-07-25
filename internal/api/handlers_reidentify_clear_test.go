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

// seedReidentifyTarget creates an artist carrying a modeled provider ID
// (discogs, with a fetched_at stamp) plus an orphan-provider fetched_at row
// (allmusic), and asserts every one of those rows exists. Those preconditions
// are what keep the "row is gone" and "row survived" assertions below from
// passing against a database that never had the row in the first place.
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

// TestHandleRefreshLink_ClearIDsRemovesModeledPreservesOrphan is the
// over-correction guard for the test above, and the relocated home of the
// #2725 scoped-delete boundary check.
//
// "Never wipe at all" would pass TestHandleReidentify_ClearIDsPersistsNothing
// perfectly, so the suite must contain the case that over-correction gets
// wrong: a COMPLETED re-identify still has to discard the old identity. Here
// the operator picks a replacement, the link handler applies the discard and
// the replacement in one write, and afterwards:
//
//   - the stale discogs row is GONE (the discard really happened),
//   - the musicbrainz row carries the REPLACEMENT, not the old value (the
//     discard did not eat the new identity along with the old one), and
//   - the orphan allmusic row SURVIVES (#2725's scoped-delete boundary).
//
// bulkRefreshRouter supplies a stubbed orchestrator so the follow-up refresh
// really runs and the handler reaches its success response, exercising the
// whole handler rather than aborting partway.
func TestHandleRefreshLink_ClearIDsRemovesModeledPreservesOrphan(t *testing.T) {
	r, svc := bulkRefreshRouter(t, lockedRefreshFetchResult())
	db := r.db

	a := seedReidentifyTarget(t, db, svc, "Completed Reidentify", "/music/completed-reidentify")

	const replacementMBID = "mbid-replacement"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh/link",
		strings.NewReader(`{"mbid":"`+replacementMBID+`","source":"musicbrainz","clear_ids":"true"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	rec := httptest.NewRecorder()

	r.handleRefreshLink(rec, req)

	if rec.Code != 200 {
		t.Fatalf("handleRefreshLink status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Modeled rows for the DISCARDED identity must be gone.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); exists {
		t.Errorf("discogs row still present (provider_id=%q) after a completed re-identify; the old identity was not discarded", pid)
	}
	// The replacement must be what landed -- not the old value, and not nothing.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists || pid != replacementMBID {
		t.Errorf("musicbrainz row after completed re-identify: exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, replacementMBID)
	}
	// The orphan row must SURVIVE the clear (#2725).
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by re-identify clear; scoped delete boundary is wrong (#2725)")
	}

	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.MusicBrainzID != replacementMBID {
		t.Errorf("reloaded MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, replacementMBID)
	}
	if reloaded.DiscogsID != "" {
		t.Errorf("reloaded DiscogsID = %q, want empty; the discarded identity survived", reloaded.DiscogsID)
	}
}

// TestHandleRefreshLink_ClearIDsWithoutReplacementPreservesIdentity pins the
// second half of the clear predicate. The gate is a positive allow-list --
// intent AND a replacement, both asserted true -- rather than a negated
// safe-list, because it decides a DELETE. A clear_ids=true request that carries
// no mbid is exactly the shape a negated guard would wave through, and it would
// strand the artist just as the original bug did.
//
// The discogs_id in the body is what makes this non-vacuous: the request is a
// legitimate link that the handler must still honor. An implementation that
// simply rejected mbid-less requests outright would pass a weaker version of
// this test while breaking the discogs link path.
func TestHandleRefreshLink_ClearIDsWithoutReplacementPreservesIdentity(t *testing.T) {
	r, svc := bulkRefreshRouter(t, lockedRefreshFetchResult())
	db := r.db

	a := seedReidentifyTarget(t, db, svc, "No Replacement Reidentify", "/music/no-replacement-reidentify")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh/link",
		strings.NewReader(`{"discogs_id":"1234","source":"discogs","clear_ids":"true"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	rec := httptest.NewRecorder()

	r.handleRefreshLink(rec, req)

	// No replacement MBID was supplied, so nothing may be discarded.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists || pid != "mbid-seed" {
		t.Errorf("musicbrainz row after clear_ids with no replacement: exists=%v provider_id=%q, want exists=true provider_id=%q; the artist was stranded", exists, pid, "mbid-seed")
	}
	// The link the operator DID make must still be honored.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "1234" {
		t.Errorf("discogs row after link: exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "1234")
	}
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed on the no-replacement path")
	}
}
