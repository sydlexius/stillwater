package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// spyHistoryRepo wraps a HistoryRepository and counts calls to ListGlobal.
// Used to assert that handleFieldsEditAll issues exactly one query regardless
// of how many editable fields are rendered (N+1 fix, #1919).
type spyHistoryRepo struct {
	delegate        artist.HistoryRepository
	listGlobalCalls atomic.Int32
}

func (s *spyHistoryRepo) Record(ctx context.Context, c *artist.MetadataChange) error {
	return s.delegate.Record(ctx, c)
}

func (s *spyHistoryRepo) GetByID(ctx context.Context, id string) (*artist.MetadataChange, error) {
	return s.delegate.GetByID(ctx, id)
}

func (s *spyHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) ([]artist.MetadataChange, int, error) {
	return s.delegate.List(ctx, artistID, limit, offset)
}

func (s *spyHistoryRepo) ListGlobal(ctx context.Context, filter artist.GlobalHistoryFilter) ([]artist.MetadataChangeWithArtist, int, error) {
	s.listGlobalCalls.Add(1)
	return s.delegate.ListGlobal(ctx, filter)
}

// testRouterWithSpyHistory creates a Router whose history service is backed by
// a spyHistoryRepo that counts ListGlobal calls while delegating to the real
// SQLite-backed repository. Returns the spy so tests can assert call counts.
func testRouterWithSpyHistory(t *testing.T) (*Router, *artist.Service, *artist.HistoryService, *spyHistoryRepo) {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)

	// Build a real history service so we have a proper HistoryRepository, then
	// wrap its repo in a spy before injecting into the router.
	realHistSvc := artist.NewHistoryService(db)
	spy := &spyHistoryRepo{delegate: realHistSvc.Repo()}
	spySvc := artist.NewHistoryServiceWithRepo(spy)

	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	ruleEngine := rule.NewEngine(ruleSvc, db, nil, nil, logger)
	nfoSnapSvc := nfo.NewSnapshotService(db)
	providerSettings := provider.NewSettingsService(db, nil)

	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		HistoryService:     spySvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		RuleEngine:         ruleEngine,
		NFOSnapshotService: nfoSnapSvc,
		ProviderSettings:   providerSettings,
		I18nBundle:         i18nBundle,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})

	return r, artistSvc, spySvc, spy
}

func makeFieldsEditAllRequest(t *testing.T, artistID string) *http.Request {
	t.Helper()
	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/artists/"+artistID+"/fields/edit-all", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", artistID)
	return req
}

// TestHandleFieldsEditAll_SingleListGlobalCall asserts that handling the
// edit-all endpoint issues exactly one ListGlobal call regardless of how many
// editable fields are rendered (the N+1 fix).
func TestHandleFieldsEditAll_SingleListGlobalCall(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, spy := testRouterWithSpyHistory(t)

	a := addTestArtist(t, artistSvc, "Edit All N+1 Artist")

	req := makeFieldsEditAllRequest(t, a.ID)
	w := httptest.NewRecorder()

	r.handleFieldsEditAll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	got := spy.listGlobalCalls.Load()
	if got != 1 {
		t.Errorf("ListGlobal called %d times, want exactly 1 (N+1 fix)", got)
	}
}

// TestHandleFieldsEditAll_AllFieldsRendered asserts that the response contains
// an edit fragment for every type-applicable field. Uses an artist with an
// unknown type (empty string) so all 20 editable fields apply and none are
// skipped by the type-applicability guard (F2).
func TestHandleFieldsEditAll_AllFieldsRendered(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, _ := testRouterWithSpyHistory(t)

	a := &artist.Artist{
		Name:     "Edit All Fields Artist",
		SortName: "Edit All Fields Artist",
		Type:     "", // unknown type: all 20 editable fields apply
		Path:     "/music/edit-all-fields",
		Genres:   []string{"Rock"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := makeFieldsEditAllRequest(t, a.ID)
	w := httptest.NewRecorder()

	r.handleFieldsEditAll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, field := range artist.EditableFieldsList() {
		wantID := `id="field-` + field + `-` + a.ID + `"`
		if !strings.Contains(body, wantID) {
			t.Errorf("response missing edit fragment for field %q (expected id %s in body)", field, wantID)
		}
	}
}

// TestHandleFieldsEditAll_OOBAttributePresent asserts that each type-applicable
// fragment in the batch response carries hx-swap-oob="outerHTML" so htmx v2
// distributes them to their individual DOM targets without a targeted main swap.
// Uses an unknown type so all 20 editable fields apply (no type-based skip).
func TestHandleFieldsEditAll_OOBAttributePresent(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, _ := testRouterWithSpyHistory(t)

	a := &artist.Artist{
		Name:     "Edit All OOB Artist",
		SortName: "Edit All OOB Artist",
		Type:     "", // unknown type: all 20 editable fields apply
		Path:     "/music/edit-all-oob",
		Genres:   []string{"Rock"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := makeFieldsEditAllRequest(t, a.ID)
	w := httptest.NewRecorder()

	r.handleFieldsEditAll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	fieldCount := len(artist.EditableFieldsList())
	oobCount := strings.Count(body, `hx-swap-oob="outerHTML"`)
	if oobCount != fieldCount {
		t.Errorf("found %d hx-swap-oob markers, want %d (one per field)", oobCount, fieldCount)
	}
}

// TestHandleFieldsEditAll_PerFieldHistoryCap asserts that a field with > 6
// history entries triggers the "Show older" affordance and a field with fewer
// than 6 entries does not -- verifying the windowed query rather than just the
// outer cap guard.
func TestHandleFieldsEditAll_PerFieldHistoryCap(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc, _ := testRouterWithSpyHistory(t)

	a := addTestArtist(t, artistSvc, "Edit All History Cap Artist")

	// biography: 7 entries -> windowed query returns 6 -> hasMore=true -> "Show older" rendered.
	for i := 0; i < 7; i++ {
		addHistoryChange(t, historySvc, a.ID, "biography", "", "bio value", "manual")
	}
	// genres: 2 entries -> windowed query returns 2 -> hasMore=false -> no "Show older".
	addHistoryChange(t, historySvc, a.ID, "genres", "", "Rock", "manual")
	addHistoryChange(t, historySvc, a.ID, "genres", "", "Pop", "manual")

	req := makeFieldsEditAllRequest(t, a.ID)
	w := httptest.NewRecorder()

	r.handleFieldsEditAll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// biography: 6 entries returned -> hasMore -> "Show older" affordance URL present.
	if !strings.Contains(body, "/fields/biography/history/fragment") {
		t.Errorf("expected 'biography/history/fragment' URL in response (biography has >6 entries); body excerpt: %.500s", body)
	}

	// genres: 2 entries returned -> !hasMore -> no "Show older" affordance.
	if strings.Contains(body, "/fields/genres/history/fragment") {
		t.Errorf("expected no 'genres/history/fragment' URL (genres has <6 entries); body excerpt: %.500s", body)
	}
}

// TestHandleFieldsEditAll_NonHTMXRequest asserts that the endpoint rejects
// non-HTMX requests with 400 (it is an HTMX-only fragment endpoint).
func TestHandleFieldsEditAll_NonHTMXRequest(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, _ := testRouterWithSpyHistory(t)

	a := addTestArtist(t, artistSvc, "Edit All Non-HTMX Artist")

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/artists/"+a.ID+"/fields/edit-all", nil)
	req.SetPathValue("id", a.ID)
	// Intentionally no HX-Request header.

	w := httptest.NewRecorder()
	r.handleFieldsEditAll(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-HTMX request", w.Code)
	}
}
