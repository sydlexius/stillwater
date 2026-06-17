package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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

// alwaysErrHistoryRepo is a stub HistoryRepository whose ListGlobal always
// fails. The other methods return harmless zero values; they are not exercised
// in the handler tests that use this repo.
type alwaysErrHistoryRepo struct{}

func (alwaysErrHistoryRepo) Record(_ context.Context, _ *artist.MetadataChange) error {
	return nil
}

func (alwaysErrHistoryRepo) GetByID(_ context.Context, _ string) (*artist.MetadataChange, error) {
	return nil, errors.New("not found")
}

func (alwaysErrHistoryRepo) List(_ context.Context, _ string, _, _ int) ([]artist.MetadataChange, int, error) {
	return nil, 0, nil
}

func (alwaysErrHistoryRepo) ListGlobal(_ context.Context, _ artist.GlobalHistoryFilter) ([]artist.MetadataChangeWithArtist, int, error) {
	return nil, 0, errors.New("simulated history store failure")
}

// testRouterWithErrHistoryService creates a Router wired with a HistoryService
// whose ListGlobal always errors. This is used to verify that handleFieldEdit
// degrades gracefully when the history pre-load step fails.
func testRouterWithErrHistoryService(t *testing.T) (*Router, *artist.Service) {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	errHistSvc := artist.NewHistoryServiceWithRepo(alwaysErrHistoryRepo{})
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
		SessionSecret:      testSessionSecret,
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		HistoryService:     errHistSvc,
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

	return r, artistSvc
}

// makeFieldEditRequest builds an HTMX GET request for handleFieldEdit.
func makeFieldEditRequest(t *testing.T, artistID, field string) *http.Request {
	t.Helper()
	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/artists/"+artistID+"/fields/"+field+"/edit", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", artistID)
	req.SetPathValue("field", field)
	return req
}

// TestHandleFieldEdit_HistoryClockShownWhenHistoryExists verifies that when the
// history service has prior values for the requested field, the rendered edit
// fragment includes the clock/undo affordance (fieldHistoryMenu).
func TestHandleFieldEdit_HistoryClockShownWhenHistoryExists(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "History Clock Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "", "original bio", "manual")

	req := makeFieldEditRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()

	r.handleFieldEdit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// fieldHistoryMenu renders data-context-menu on its outer wrapper div and
	// data-sw-stage-value on each history entry button.
	if !strings.Contains(body, "data-context-menu") {
		t.Errorf("expected clock/undo affordance (data-context-menu) in edit fragment when history exists; body excerpt: %.300s", body)
	}
	if !strings.Contains(body, "data-sw-stage-value") {
		t.Errorf("expected data-sw-stage-value in edit fragment when history exists; body excerpt: %.300s", body)
	}
}

// TestHandleFieldEdit_HistoryClockAbsentWhenEmpty verifies that when there are
// no prior values for the field, the edit fragment does not render the
// clock/undo affordance.
func TestHandleFieldEdit_HistoryClockAbsentWhenEmpty(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "No History Artist")

	req := makeFieldEditRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()

	r.handleFieldEdit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "data-context-menu") {
		t.Errorf("expected no clock/undo affordance in edit fragment when history is empty; body excerpt: %.300s", body)
	}
	if strings.Contains(body, "data-sw-stage-value") {
		t.Errorf("expected no data-sw-stage-value in edit fragment when history is empty; body excerpt: %.300s", body)
	}
}

// TestHandleFieldEdit_HistoryListGlobalError_GracefulDegrade verifies that when
// historyService.ListGlobal returns an error the handler still renders the
// field edit fragment with status 200 (graceful degradation, no 500).
func TestHandleFieldEdit_HistoryListGlobalError_GracefulDegrade(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithErrHistoryService(t)

	a := addTestArtist(t, artistSvc, "Error Degrade Artist")

	req := makeFieldEditRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()

	r.handleFieldEdit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// The edit fragment itself must still render (field wrapper div is present).
	if !strings.Contains(body, "field-biography-"+a.ID) {
		t.Errorf("expected field edit wrapper in body on history error; body excerpt: %.300s", body)
	}
	// No history entries should appear since loading failed.
	if strings.Contains(body, "data-sw-stage-value") {
		t.Errorf("expected no staged-value entries in body when history load errored; body excerpt: %.300s", body)
	}
}

// TestHandleFieldEdit_ShowOlderAffordanceRenderedWhenMoreThan5Entries verifies
// that when >5 prior values exist the edit fragment includes the "Show older"
// affordance and the HTMX fragment URL, and renders exactly 5 item buttons
// (not the 6th "detection" entry).
func TestHandleFieldEdit_ShowOlderAffordanceRenderedWhenMoreThan5Entries(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Many History Artist")
	for i := range 6 {
		addHistoryChange(t, historySvc, a.ID, "biography", "", "bio value "+strconv.Itoa(i), "manual")
	}

	req := makeFieldEditRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()
	r.handleFieldEdit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()

	// "Show older" affordance must be present.
	if !strings.Contains(body, "history/fragment") {
		t.Errorf("expected 'history/fragment' HTMX URL in edit fragment when >5 history entries; body excerpt: %.400s", body)
	}

	// Only 5 item buttons should be in the initial render (not 6).
	count := strings.Count(body, "data-sw-stage-value")
	// Desktop panel + mobile sheet each render 5 entries = 10 total.
	if count != 10 {
		t.Errorf("expected 10 data-sw-stage-value occurrences (5 desktop + 5 mobile), got %d; body excerpt: %.500s", count, body)
	}
}

// TestHandleFieldEdit_ShowOlderAffordanceAbsentWhenFiveOrFewerEntries verifies
// that the "Show older" button is NOT rendered when exactly 5 or fewer entries
// exist (the cap+1 probe did not overflow).
func TestHandleFieldEdit_ShowOlderAffordanceAbsentWhenFiveOrFewerEntries(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Exact 5 History Artist")
	for i := range 5 {
		addHistoryChange(t, historySvc, a.ID, "origin", "", "city "+strconv.Itoa(i), "manual")
	}

	req := makeFieldEditRequest(t, a.ID, "origin")
	w := httptest.NewRecorder()
	r.handleFieldEdit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, "history/fragment") {
		t.Errorf("expected no 'history/fragment' URL when <=5 history entries; body excerpt: %.400s", body)
	}
}

// makeFieldHistoryFragmentRequest builds a GET request for handleFieldHistoryFragment.
func makeFieldHistoryFragmentRequest(t *testing.T, artistID, field string) *http.Request {
	t.Helper()
	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/artists/"+artistID+"/fields/"+field+"/history/fragment", nil)
	req.SetPathValue("id", artistID)
	req.SetPathValue("field", field)
	return req
}

// TestHandleFieldHistoryFragment_Returns200WithAllItems verifies that the
// fragment endpoint returns a 200 with the full field history rendered as item
// buttons when history exists.
func TestHandleFieldHistoryFragment_Returns200WithAllItems(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Fragment Full Artist")
	for i := range 7 {
		addHistoryChange(t, historySvc, a.ID, "origin", "", "city "+strconv.Itoa(i), "manual")
	}

	req := makeFieldHistoryFragmentRequest(t, a.ID, "origin")
	w := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()

	// All 7 entries must be rendered (no 5-cap on the fragment endpoint).
	count := strings.Count(body, "data-sw-stage-value")
	if count != 7 {
		t.Errorf("expected 7 data-sw-stage-value entries from fragment endpoint, got %d; body excerpt: %.500s", count, body)
	}
	// 7-entry fixture (< pageSize): fragment renders all entries with no continuation button.
	if strings.Contains(body, "history/fragment") {
		t.Errorf("fragment response must not contain nested 'history/fragment' URL; body excerpt: %.300s", body)
	}
}

// TestHandleFieldHistoryFragment_ExactlyPageSizeNoContinuation verifies the
// boundary condition where the total entry count equals the page size (50).
// The cap+1 detection fetches pageSize+1 rows to decide whether more pages
// exist; with exactly 50 entries the probe returns 50 rows (not 51), so no
// continuation affordance should be rendered.
func TestHandleFieldHistoryFragment_ExactlyPageSizeNoContinuation(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Exact PageSize History Artist")
	for i := range 50 {
		addHistoryChange(t, historySvc, a.ID, "biography", "", "bio value "+strconv.Itoa(i), "manual")
	}

	req := makeFieldHistoryFragmentRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()

	// All 50 entries must be rendered.
	count := strings.Count(body, "data-sw-stage-value")
	if count != 50 {
		t.Errorf("expected 50 data-sw-stage-value entries for exactly-50 fixture, got %d; body excerpt: %.500s", count, body)
	}

	// No continuation affordance: the cap+1 probe returned exactly 50 rows
	// (not 51), so no next-page offset URL should appear.
	if strings.Contains(body, "history/fragment?offset=") {
		t.Errorf("expected no 'history/fragment?offset=' continuation URL for exactly-50 fixture; body excerpt: %.500s", body)
	}
}

// TestHandleFieldHistoryFragment_Returns404ForUnknownArtist verifies that the
// fragment endpoint returns 404 when the artist does not exist.
func TestHandleFieldHistoryFragment_Returns404ForUnknownArtist(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	req := makeFieldHistoryFragmentRequest(t, "nonexistent-artist-id", "biography")
	w := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown artist, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleFieldHistoryFragment_Returns400ForInvalidField verifies that the
// fragment endpoint returns 400 when the requested field is not a known
// editable field (IsEditableField gate). This prevents authenticated users
// from probing metadata_changes for arbitrary internal columns.
func TestHandleFieldHistoryFragment_Returns400ForInvalidField(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	req := makeFieldHistoryFragmentRequest(t, "any-artist-id", "library_id")
	w := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-editable field, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleFieldHistoryFragment_DegradedWhenHistoryErrors verifies that the
// fragment endpoint renders an empty list (no items, no 500) when the history
// service fails, preserving the graceful-degrade contract.
func TestHandleFieldHistoryFragment_DegradedWhenHistoryErrors(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithErrHistoryService(t)

	a := addTestArtist(t, artistSvc, "Fragment Err Artist")

	req := makeFieldHistoryFragmentRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on history error (graceful degrade), got %d; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "data-sw-stage-value") {
		t.Errorf("expected no items in fragment when history service errors; body: %s", w.Body.String())
	}
}

// TestHandleFieldHistoryFragment_DeepHistoryCapPlusOneDetection verifies that
// the fragment endpoint applies cap+1 detection for deep histories: when 51
// entries exist the response renders exactly 50 items and includes a "Show older"
// continuation affordance with the next-page offset URL (?offset=50), making
// entries beyond the first page reachable. A second request with ?offset=50
// returns the remaining 1 entry with no further continuation.
func TestHandleFieldHistoryFragment_DeepHistoryCapPlusOneDetection(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Deep History Artist")
	for i := range 51 {
		addHistoryChange(t, historySvc, a.ID, "biography", "", "bio value "+strconv.Itoa(i), "manual")
	}

	// --- First page (offset=0): expect 50 items + continuation affordance ---
	req := makeFieldHistoryFragmentRequest(t, a.ID, "biography")
	w := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("page 1: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()

	// Exactly 50 item buttons (not 51 - the detection entry must be trimmed).
	count := strings.Count(body, "data-sw-stage-value")
	if count != 50 {
		t.Errorf("page 1: expected 50 data-sw-stage-value entries for 51-entry deep history, got %d; body excerpt: %.500s", count, body)
	}

	// A continuation affordance must appear pointing to offset=50.
	if !strings.Contains(body, "history/fragment?offset=50") {
		t.Errorf("page 1: expected 'history/fragment?offset=50' continuation URL; body excerpt: %.500s", body)
	}

	// --- Second page (offset=50): expect 1 remaining item, no continuation ---
	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req2 := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/artists/"+a.ID+"/fields/biography/history/fragment?offset=50", nil)
	req2.SetPathValue("id", a.ID)
	req2.SetPathValue("field", "biography")
	w2 := httptest.NewRecorder()
	r.handleFieldHistoryFragment(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("page 2: status = %d, want %d; body: %s", w2.Code, http.StatusOK, w2.Body.String())
	}
	body2 := w2.Body.String()

	count2 := strings.Count(body2, "data-sw-stage-value")
	if count2 != 1 {
		t.Errorf("page 2: expected 1 data-sw-stage-value entry at offset=50, got %d; body excerpt: %.500s", count2, body2)
	}

	if strings.Contains(body2, "history/fragment?offset=") {
		t.Errorf("page 2: expected no further continuation URL when all entries exhausted; body excerpt: %.500s", body2)
	}
}
