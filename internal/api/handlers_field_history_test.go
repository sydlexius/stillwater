package api

import (
	"context"
	"errors"
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
