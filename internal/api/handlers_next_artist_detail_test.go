package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// detailTestRouter builds a router for the artist-detail page tests. It wires a
// provider SettingsService onto the library router (testRouterWithLibrary leaves
// it nil) because buildArtistDetailData reads the provider priorities to build
// the per-field providers map.
func detailTestRouter(t *testing.T) (*Router, *artist.Service) {
	t.Helper()
	r, _, artistSvc := testRouterWithLibrary(t)
	r.providerSettings = provider.NewSettingsService(r.db, nil)
	return r, artistSvc
}

// seedDetailArtist creates an artist with the given name and returns its id.
// Mirrors the create-then-read pattern in the next/ artists tests; Create
// populates a.ID in place.
func seedDetailArtist(t *testing.T, svc *artist.Service, name string) string {
	t.Helper()
	// SortName is set explicitly so ListIDs (ORDER BY sort_name, id) yields a
	// deterministic order for the neighbor assertions; without it sort_name is
	// empty and the ordering degrades to random UUID order.
	a := &artist.Artist{Name: name, SortName: name}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist %q: %v", name, err)
	}
	return a.ID
}

// nextDetailRequest builds a GET /next/artists/{id} request with an authed user
// context and the path value set, wrapped in the UX middleware for the given
// channel ("next" or "stable").
func nextDetailRequest(t *testing.T, r *Router, channel, id string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	middleware.UX(channel, "")(http.HandlerFunc(r.handleNextArtistDetailPage)).ServeHTTP(w, req)
	return w
}

// TestHandleNextArtistDetailPage_NextChannel verifies the next channel renders
// the single-scroll next/ detail page (not the stable tab bar).
func TestHandleNextArtistDetailPage_NextChannel(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Aaa Artist")

	w := nextDetailRequest(t, r, "next", id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-artist-detail") {
		t.Errorf("next page missing the next-channel marker class")
	}
	if strings.Contains(body, `role="tablist"`) {
		t.Errorf("next page must be single-scroll, not the stable tab bar")
	}
}

// TestHandleNextArtistDetailPage_StableFallback verifies the stable channel
// delegates to the existing tabbed stable detail page so /next/artists/{id}
// never dead-ends.
func TestHandleNextArtistDetailPage_StableFallback(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Bbb Artist")

	w := nextDetailRequest(t, r, "stable", id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `role="tablist"`) {
		t.Errorf("stable fallback must render the tabbed stable detail page")
	}
}

// TestHandleNextArtistDetailPage_Neighbors verifies prev/next-artist neighbor
// ids are resolved from the sort_name-ascending ListIDs order and linked in the
// hero's h/l navigation.
func TestHandleNextArtistDetailPage_Neighbors(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	a := seedDetailArtist(t, artistSvc, "Aaa")
	b := seedDetailArtist(t, artistSvc, "Bbb")
	c := seedDetailArtist(t, artistSvc, "Ccc")

	w := nextDetailRequest(t, r, "next", b)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The middle artist (default name-ASC order Aaa < Bbb < Ccc) must link
	// prev=Aaa, next=Ccc via the hero nav shortcuts (data-sw-shortcut h/l).
	if !strings.Contains(body, "/next/artists/"+a) {
		t.Errorf("missing prev-artist link to %s", a)
	}
	if !strings.Contains(body, "/next/artists/"+c) {
		t.Errorf("missing next-artist link to %s", c)
	}
}

// TestResolveArtistNeighbors_ListIDsErrorDegrades verifies the neighbor resolver
// degrades cleanly (returns empty prev/next so the h/l shortcuts no-op) when the
// underlying ListIDs query errors, rather than panicking or surfacing the error.
// The error is logged, not swallowed (handlers_next_artist_detail.go). A closed
// DB forces ListIDs to error deterministically.
func TestResolveArtistNeighbors_ListIDsErrorDegrades(t *testing.T) {
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Lonely")

	// Close the DB so the next ListIDs call errors.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id, nil)
	prev, next := r.resolveArtistNeighbors(req, id)
	if prev != "" || next != "" {
		t.Errorf("resolveArtistNeighbors on DB error = (%q, %q), want empty pair", prev, next)
	}
}

// TestHandleNextArtistDetailPage_FieldFindingChips locks the field-tag-on-rule
// feature (M55 #1336) end to end at the handler level: a field-tagged open
// violation (bio_exists -> biography) renders an inline finding chip on that
// field's row, while an untagged whole-record rule (nfo_exists) produces NO
// inline chip -- it surfaces only in the lazily-loaded Open Findings list, which
// is not part of the initial page HTML.
func TestHandleNextArtistDetailPage_FieldFindingChips(t *testing.T) {
	t.Parallel()
	// testRouterWithPipelineFull wires both the rule service (with SeedDefaults,
	// so the GetViolationsForArtists JOIN has rule rows) and provider settings
	// (buildArtistDetailData needs it).
	r, artistSvc, ruleSvc := testRouterWithPipelineFull(t)
	id := seedDetailArtist(t, artistSvc, "Chip Probe")

	// Distinctive messages so presence/absence is assertable by substring.
	const bioProbe = "BIO_CHIP_PROBE_MESSAGE"
	const nfoProbe = "NFO_CHIP_PROBE_MESSAGE"
	for _, v := range []*rule.RuleViolation{
		{RuleID: rule.RuleBioExists, ArtistID: id, ArtistName: "Chip Probe", Severity: "warning", Message: bioProbe, Status: rule.ViolationStatusOpen},
		{RuleID: rule.RuleNFOExists, ArtistID: id, ArtistName: "Chip Probe", Severity: "error", Message: nfoProbe, Status: rule.ViolationStatusOpen},
	} {
		if err := ruleSvc.UpsertViolation(t.Context(), v); err != nil {
			t.Fatalf("UpsertViolation %s: %v", v.RuleID, err)
		}
	}

	w := nextDetailRequest(t, r, "next", id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The field-tagged rule renders as an inline field chip on the biography row.
	if !strings.Contains(body, "sw-field-chip") {
		t.Errorf("expected an inline field chip in the rendered page; none present")
	}
	if !strings.Contains(body, bioProbe) {
		t.Errorf("biography row missing its field-tagged finding chip (%q absent)", bioProbe)
	}
	if !strings.Contains(body, "field-biography-"+id) {
		t.Errorf("biography field container not rendered")
	}
	// The untagged whole-record rule must NOT inline a chip (it lives only in the
	// lazily-loaded Open Findings list, absent from the initial page HTML).
	if strings.Contains(body, nfoProbe) {
		t.Errorf("untagged rule (nfo_exists) leaked an inline chip: %q present", nfoProbe)
	}
}

// TestHandleNextArtistDetailPage_NotFound verifies an unknown id returns 404.
func TestHandleNextArtistDetailPage_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := detailTestRouter(t)

	w := nextDetailRequest(t, r, "next", "nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
