package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/rule"
)

// detailTestRouter builds a router for the artist-detail page tests. It uses
// testRouter (which wires ConnectionService, ProviderSettings, RuleEngine, and
// the conflict no-op) so 4C integration tests can seed connections and the
// platform-state endpoint has a real connection service.
func detailTestRouter(t *testing.T) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc := testRouter(t)
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

// TestHandleNextArtistDetailPage_StableMode404 verifies that GET /next/artists/{id}
// in stable mode returns 404. The /next/* lane is gated by the UX middleware so
// it is completely unreachable when the lane is disabled.
func TestHandleNextArtistDetailPage_StableMode404(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Bbb Artist")

	w := nextDetailRequest(t, r, "stable", id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (stable mode must 404 /next/ routes): %s", w.Code, w.Body.String())
	}
}

// TestHandleNextArtistDetailPage_OptOutHeader404 verifies the handler-level
// decision-12 guard: when the lane IS enabled (next/dual mode) but the
// per-request X-Stillwater-UX: stable header opts back to the stable channel,
// the handler returns 404. The channel is injected directly via
// WithTestUXChannel, simulating the header opt-out scenario without relying on
// the middleware-level gate (which is tested by
// TestHandleNextArtistDetailPage_StableMode404).
func TestHandleNextArtistDetailPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Opt-Out Artist")

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleNextArtistDetailPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out header: status = %d, want 404 (decision 12)", w.Code)
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

// TestHandleNextArtworkModal_UnauthenticatedRedirectsToLogin closes the authz
// boundary on the editor fragment: a request without a user ID in context must
// render the login page, not the artwork editor. This covers the 12.5% of
// handleNextArtworkModal that the unauthenticated arm previously had no test for.
func TestHandleNextArtworkModal_UnauthenticatedRedirectsToLogin(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Auth Gate Artist")

	// Build a request with NO user ID in context (empty context = unauthenticated)
	// but with the UX channel set so the auth check is reached.
	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXNext)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id+"/artwork-modal?kind=primary", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleNextArtworkModal(w, req)

	body := w.Body.String()
	// The login page must be rendered, not the artwork editor fragment.
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Errorf("unauthenticated request must render the login page; auth/login form absent")
	}
	if strings.Contains(body, "artwork-modal-body") {
		t.Errorf("unauthenticated request must not render the artwork editor body")
	}
}

// TestHandleNextArtworkModal_StableChannel404 verifies the Phase 2 channel guard:
// when UXStable is in context the modal returns 404 immediately.
func TestHandleNextArtworkModal_StableChannel404(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Channel Gate Artist")

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id+"/artwork-modal?kind=primary", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleNextArtworkModal(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable channel: status = %d, want 404", w.Code)
	}
}

// seedConn creates a connection of the given type, links the platform artist ID,
// and returns the connection id. It is shared by 4C integration tests.
func seedConn(t *testing.T, r *Router, artistSvc *artist.Service, artistID, connID, connType string) {
	t.Helper()
	c := &connection.Connection{
		ID:      connID,
		Name:    connID,
		Type:    connType,
		URL:     "http://localhost:8096",
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating connection %s: %v", connID, err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), artistID, connID, "platform-"+connID); err != nil {
		t.Fatalf("setting platform ID for %s: %v", connID, err)
	}
}

// TestNextArtistDetail_ProvidersSectionLazyMounts verifies the rendered next/
// artist-detail page contains lazy-load placeholder divs for each connection in
// the Providers section, with the correct HTMX intersect once trigger.
func TestNextArtistDetail_ProvidersSectionLazyMounts(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Mount Test Artist")

	// Seed one Emby and one Lidarr connection.
	seedConn(t, r, artistSvc, id, "conn-emby", connection.TypeEmby)
	seedConn(t, r, artistSvc, id, "conn-lidarr", connection.TypeLidarr)

	w := nextDetailRequest(t, r, "next", id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The Providers section card must be present.
	if !strings.Contains(body, `id="next-providers-`+id+`"`) {
		t.Errorf("providers section card absent from page")
	}

	// Each connection gets an intersect once lazy-load mount.
	for _, connID := range []string{"conn-emby", "conn-lidarr"} {
		if !strings.Contains(body, `id="platform-state-`+connID+`"`) {
			t.Errorf("missing platform-state mount for connection %s", connID)
		}
		if !strings.Contains(body, `platform-state?connection_id=`+connID) {
			t.Errorf("platform-state hx-get for %s not present", connID)
		}
	}
	// Providers use intersect once (safer than revealed: a section visible on
	// load fires reliably even before scroll). Changed from revealed (L2 fix).
	if !strings.Contains(body, `hx-trigger="intersect once"`) {
		t.Errorf("platform-state mounts must use hx-trigger=intersect once")
	}
}

// TestNextArtistDetail_DebugSectionGating verifies the debug section rendering
// follows the ShowPlatformDebug && HasDebugConnection gate through the full
// handler + template path (not just a template unit test).
func TestNextArtistDetail_DebugSectionGating(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)

	// enableDebug sets the per-user show_platform_debug preference for the test
	// user (M55 #2060: migrated from global app setting to per-user preference).
	// The test user ID must match middleware.WithTestUserID's value ("test-user").
	enableDebug := func() {
		if _, err := r.db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO user_preferences (user_id, key, value, updated_at)
			 VALUES ('test-user', 'show_platform_debug', 'true', datetime('now'))`); err != nil {
			t.Fatalf("enabling show_platform_debug preference: %v", err)
		}
	}

	// Case (a): setting disabled, no connections -- no debug section.
	t.Run("setting_disabled", func(t *testing.T) {
		id := seedDetailArtist(t, artistSvc, "Debug Gate A")
		w := nextDetailRequest(t, r, "next", id)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `id="next-debug-`+id) {
			t.Errorf("debug section should not appear when setting is disabled")
		}
	})

	// Case (b): setting enabled, only a Lidarr connection (HasDebugConnection=false).
	t.Run("lidarr_only", func(t *testing.T) {
		enableDebug()
		id := seedDetailArtist(t, artistSvc, "Debug Gate B")
		seedConn(t, r, artistSvc, id, "dbg-lidarr", connection.TypeLidarr)
		w := nextDetailRequest(t, r, "next", id)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `id="next-debug-`+id) {
			t.Errorf("debug section should not appear with only Lidarr connections")
		}
	})

	// Case (c): setting enabled + an Emby connection -- debug section present
	// with a readonly platform-state mount.
	t.Run("emby_connection", func(t *testing.T) {
		enableDebug()
		id := seedDetailArtist(t, artistSvc, "Debug Gate C")
		seedConn(t, r, artistSvc, id, "dbg-emby", connection.TypeEmby)
		w := nextDetailRequest(t, r, "next", id)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `id="next-debug-`+id) {
			t.Errorf("debug section should appear with Emby connection + debug setting on")
		}
		if !strings.Contains(body, `id="debug-platform-state-dbg-emby"`) {
			t.Errorf("debug section missing readonly platform-state mount for Emby connection")
		}
		if !strings.Contains(body, "readonly=true") {
			t.Errorf("debug platform-state mount must carry readonly=true")
		}
	})
}

// TestNextArtistDetail_PlatformStateEndpointReachable verifies the platform-state
// endpoint (the URL the providers-section intersect once mounts fire) is registered and
// returns an HTML partial when called as an HTMX request. Without a real Emby
// server the getter fails, so the response is a PlatformStateError partial
// (200 + text/html). This confirms the endpoint is wired and returns HTML that
// HTMX can swap into the intersect once placeholder.
func TestNextArtistDetail_PlatformStateEndpointReachable(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Platform State Reach")
	seedConn(t, r, artistSvc, id, "ps-emby", connection.TypeEmby)

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/artists/"+id+"/platform-state?connection_id=ps-emby", nil)
	req.SetPathValue("id", id)
	// Mark as HTMX so the error path returns a PlatformStateError HTML partial
	// rather than a JSON error body.
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handleGetPlatformState(w, req)

	// Without a real Emby server the getter returns an error; the handler
	// renders PlatformStateError (200 + text/html) for HTMX callers.
	if w.Code != http.StatusOK {
		t.Fatalf("platform-state endpoint status = %d, want 200: %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("platform-state endpoint Content-Type = %q, want text/html", ct)
	}
}

// TestNextArtistDetail_DiscographyTabReachable verifies the discography/tab
// endpoint (the URL the discography-section mount fires) returns an HTML
// fragment for the seeded artist.
func TestNextArtistDetail_DiscographyTabReachable(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Disco Reach")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/"+id+"/discography/tab", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("discography/tab endpoint status = %d, want 200: %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("discography/tab Content-Type = %q, want text/html", ct)
	}
}
