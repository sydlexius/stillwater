package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// testRouterWithPlatformPublisher builds a Router with a real *publish.Publisher
// wired with ArtistLister set (so ScanPlatformBackdropDuplicates is fully
// wired and can be exercised end to end), mirroring testRouter in
// handlers_report_test.go. Static assets are wired so assetsFor/renderTempl
// work for full-page renders.
func testRouterWithPlatformPublisher(t *testing.T) *Router {
	t.Helper()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	pub := publish.New(publish.Deps{
		ArtistService:     artistSvc,
		ArtistLister:      artistSvc,
		ConnectionService: connSvc,
		Logger:            logger,
	})

	r := NewRouter(RouterDeps{
		SessionSecret:     testSessionSecret,
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
		Publisher:         pub,
	})

	return r
}

// TestPlatformBackdropDuplicatesPage_RequiresAdmin pins the admin gate: a
// non-admin GET must be refused (403 or a redirect), never the rendered
// report (#2540 Task 6).
func TestPlatformBackdropDuplicatesPage_RequiresAdmin(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/platform-backdrop-duplicates", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusForbidden && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 403/redirect for non-admin", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPage_UnauthRendersLoginPage mirrors the
// sibling local-report test: an unauthenticated GET must render the login
// page (200), not a bare 401, because the route uses wrapOptionalAuth.
func TestPlatformBackdropDuplicatesPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "platform-backdrop-duplicates-table") {
		t.Error("unauthenticated visitor must not see the platform-backdrop-duplicates table")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
}

// TestPlatformBackdropDuplicatesPage_PublisherNilFailsLoud pins the fail-loud
// contract: r.publisher == nil must return 500 with a logged error, never a
// silent empty report.
func TestPlatformBackdropDuplicatesPage_PublisherNilFailsLoud(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.publisher = nil

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when publisher is nil", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPage_ScanError pins the report page's error
// path: if the scan itself fails, the handler returns 500 rather than a
// misleading empty report. A Publisher wired without an ArtistLister
// (below) makes ScanPlatformBackdropDuplicates return its "not fully wired"
// error, which is the cheapest way to exercise this path without a fake.
func TestPlatformBackdropDuplicatesPage_ScanError(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := newTestDB(t)
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Deliberately omit ArtistLister so the scan is not fully wired and
	// returns an error.
	pub := publish.New(publish.Deps{
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		Logger:            logger,
	})

	r := NewRouter(RouterDeps{
		SessionSecret:     testSessionSecret,
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
		Publisher:         pub,
	})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the scan errors; body: %s", w.Code, w.Body.String())
	}
}

// TestPlatformBackdropDuplicatesPage_AuthenticatedRendersPage is the
// authenticated-path regression test: an admin request against a fully
// wired publisher with no artists must render an empty, clean report.
func TestPlatformBackdropDuplicatesPage_AuthenticatedRendersPage(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated admin request should get 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "platform-backdrop-duplicates-table") {
		t.Error("authenticated admin must see the platform-backdrop-duplicates-table in the response")
	}
	// No artists means RedundantBackdrops == 0, so the prune button is
	// correctly withheld (mirrors the sibling report's ExactRedundantSlots
	// gate); assert the empty-state message renders instead.
	if !strings.Contains(body, `id="platform-backdrop-duplicates-empty"`) {
		t.Error("a clean, empty scan must render the empty-state message")
	}
}

// TestPlatformBackdropDuplicatesPage_PruneButtonPostsToPruneEndpoint asserts
// that once the scan reports redundant backdrops, the prune button renders
// and posts to the Task 7 prune endpoint.
func TestPlatformBackdropDuplicatesPage_PruneButtonPostsToPruneEndpoint(t *testing.T) {
	t.Parallel()
	view := buildPlatformBackdropDuplicatesView(publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     1,
		RedundantBackdrops:  2,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", Name: "Artist One", ConnectionID: "c-emby", Connection: "emby", Backdrops: 4, Redundant: 2},
		},
	})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	rec := httptest.NewRecorder()
	renderTempl(rec, req, templates.PlatformBackdropDuplicatesPage(templates.AssetPaths{}, view))

	body := rec.Body.String()
	if !strings.Contains(body, `id="platform-backdrop-duplicates-prune-button"`) {
		t.Error("report must expose the Prune Platform Duplicates action when RedundantBackdrops > 0")
	}
	if !strings.Contains(body, "/api/v1/reports/platform-backdrop-duplicates/prune") {
		t.Error("prune button must post to /api/v1/reports/platform-backdrop-duplicates/prune")
	}
	if !strings.Contains(body, "Artist One") {
		t.Errorf("report table must list the affected artist; body: %s", body)
	}
}

// TestBuildPlatformBackdropDuplicatesView pins the report-to-view-model
// conversion: every PerArtist entry becomes a row and the totals pass
// through unchanged.
func TestBuildPlatformBackdropDuplicatesView(t *testing.T) {
	report := publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     1,
		RedundantBackdrops:  2,
		ScanErrors:          1,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", Name: "Artist One", ConnectionID: "c-emby", Connection: "emby", Backdrops: 4, Redundant: 2},
		},
	}
	view := buildPlatformBackdropDuplicatesView(report)

	if view.ConnectionsAffected != 1 || view.ArtistsAffected != 1 || view.RedundantBackdrops != 2 || view.ScanErrors != 1 {
		t.Fatalf("totals did not pass through: %+v", view)
	}
	if len(view.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(view.Rows))
	}
	row := view.Rows[0]
	if row.ArtistID != "a1" || row.Name != "Artist One" || row.Connection != "emby" || row.Backdrops != 4 || row.Redundant != 2 {
		t.Errorf("row mismatch: %+v", row)
	}
}

// keep an explicit reference to the templates package view type so a future
// refactor of the view struct's field set fails to compile here rather than
// silently drifting from the templ template's expectations.
var _ = templates.PlatformBackdropDuplicatesPageView{}

// TestPlatformBackdropDuplicatesPrune_ConflictWhenRunning asserts a request
// returns 409 while the singleton slot is already claimed, mirroring
// TestBackdropDuplicatesRemediate_ConflictWhenRunning (#2540 Task 7).
func TestPlatformBackdropDuplicatesPrune_ConflictWhenRunning(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	r.platformPruneMu.Lock()
	r.platformPruneRunning = true
	r.platformPruneMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}

	// The rejected concurrent request must not clobber the real prune's claim
	// on the singleton slot: guards against a future defer-misplacement
	// regression that clears platformPruneRunning on the 409 path.
	r.platformPruneMu.Lock()
	stillRunning := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if !stillRunning {
		t.Fatal("platformPruneRunning = false after a rejected concurrent request, want true (still claimed by the running prune)")
	}
}

// TestPlatformBackdropDuplicatesPrune_NonAdminForbidden mirrors
// TestBackdropDuplicatesRemediate_NonAdminForbidden: the prune endpoint is
// admin-only via requireForeignAdmin, so an authenticated non-admin must get
// 403 rather than triggering the prune.
func TestPlatformBackdropDuplicatesPrune_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPrune_PublisherNilFailsLoud pins the
// fail-loud contract: r.publisher == nil must return 500, never a silent
// empty 200 (this repo forbids silent-failure capability guards).
func TestPlatformBackdropDuplicatesPrune_PublisherNilFailsLoud(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.publisher = nil

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when publisher is nil", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPrune_Error pins the prune endpoint's error
// path: if PrunePlatformBackdropDuplicates fails (here, a Publisher wired
// without an ArtistLister, so it is not fully wired), the handler returns
// 500 and releases the singleton so a later request is not permanently
// blocked.
func TestPlatformBackdropDuplicatesPrune_Error(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := newTestDB(t)
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Deliberately omit ArtistLister so the prune is not fully wired and
	// returns an error.
	pub := publish.New(publish.Deps{
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		Logger:            logger,
	})

	r := NewRouter(RouterDeps{
		SessionSecret:     testSessionSecret,
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
		Publisher:         pub,
	})

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the prune errors; body: %s", w.Code, w.Body.String())
	}

	r.platformPruneMu.Lock()
	running := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if running {
		t.Error("platformPruneRunning must be released after a failed prune")
	}
}

// TestPlatformBackdropDuplicatesPrune_Success asserts the happy path: an
// admin POST against a fully wired publisher with no artists reaches
// PrunePlatformBackdropDuplicates and the JSON body reports its (empty)
// result.
func TestPlatformBackdropDuplicatesPrune_Success(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"artists_processed":0`) {
		t.Errorf("expected artists_processed = 0 in body: %s", body)
	}
	if !strings.Contains(body, `"backdrops_removed":0`) {
		t.Errorf("expected backdrops_removed = 0 in body: %s", body)
	}
	if !strings.Contains(body, `"skipped_changed":0`) {
		t.Errorf("expected skipped_changed = 0 in body: %s", body)
	}
	if !strings.Contains(body, `"failures":0`) {
		t.Errorf("expected failures = 0 in body: %s", body)
	}

	r.platformPruneMu.Lock()
	running := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if running {
		t.Error("platformPruneRunning must be released after a successful prune")
	}
}
