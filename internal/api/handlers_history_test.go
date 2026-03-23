package api

import (
	"context"
	"encoding/json"
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
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouterWithHistory creates a Router wired with a real HistoryService that
// shares the same in-memory database as the artist service, so artist lookups
// and history inserts both work against the same data.
func testRouterWithHistory(t *testing.T) (*Router, *artist.Service, *artist.HistoryService) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	historySvc := artist.NewHistoryService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	ruleEngine := rule.NewEngine(ruleSvc, db, nil, nil, logger)
	nfoSnapSvc := nfo.NewSnapshotService(db)
	providerSettings := provider.NewSettingsService(db, nil)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		HistoryService:     historySvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		RuleEngine:         ruleEngine,
		NFOSnapshotService: nfoSnapSvc,
		ProviderSettings:   providerSettings,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
	})

	return r, artistSvc, historySvc
}

// addHistoryChange records a single metadata change using the given HistoryService.
func addHistoryChange(t *testing.T, svc *artist.HistoryService, artistID, field, oldVal, newVal, source string) {
	t.Helper()
	if err := svc.Record(context.Background(), artistID, field, oldVal, newVal, source); err != nil {
		t.Fatalf("recording history change: %v", err)
	}
}

func TestHandleListArtistHistory_NotFound(t *testing.T) {
	r, _, _ := testRouterWithHistory(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/no-such-artist/history", nil)
	req.SetPathValue("id", "no-such-artist")
	w := httptest.NewRecorder()

	r.handleListArtistHistory(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleListArtistHistory_Empty(t *testing.T) {
	r, artistSvc, _ := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Empty History Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleListArtistHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	changes, ok := resp["changes"].([]any)
	if !ok {
		t.Fatalf("changes field missing or wrong type: %T", resp["changes"])
	}
	if len(changes) != 0 {
		t.Errorf("len(changes) = %d, want 0", len(changes))
	}
	if resp["total"] != float64(0) {
		t.Errorf("total = %v, want 0", resp["total"])
	}
}

func TestHandleListArtistHistory_WithChanges(t *testing.T) {
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "History Artist")

	addHistoryChange(t, historySvc, a.ID, "biography", "", "New bio", "manual")
	addHistoryChange(t, historySvc, a.ID, "genres", "", "Rock, Pop", "provider:musicbrainz")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleListArtistHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	changes, ok := resp["changes"].([]any)
	if !ok {
		t.Fatalf("changes field missing or wrong type: %T", resp["changes"])
	}
	if len(changes) != 2 {
		t.Errorf("len(changes) = %d, want 2", len(changes))
	}
	if resp["total"] != float64(2) {
		t.Errorf("total = %v, want 2", resp["total"])
	}
}

func TestHandleListArtistHistory_Pagination(t *testing.T) {
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Pagination Artist")

	// Insert 5 changes.
	for i := 0; i < 5; i++ {
		addHistoryChange(t, historySvc, a.ID, "biography", "", "value", "manual")
	}

	// Request first page of 3.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history?limit=3&offset=0", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleListArtistHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	changes, _ := resp["changes"].([]any)
	if len(changes) != 3 {
		t.Errorf("first page len(changes) = %d, want 3", len(changes))
	}
	if resp["total"] != float64(5) {
		t.Errorf("total = %v, want 5", resp["total"])
	}
	if resp["limit"] != float64(3) {
		t.Errorf("limit = %v, want 3", resp["limit"])
	}
	if resp["offset"] != float64(0) {
		t.Errorf("offset = %v, want 0", resp["offset"])
	}

	// Request second page.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history?limit=3&offset=3", nil)
	req2.SetPathValue("id", a.ID)
	w2 := httptest.NewRecorder()
	r.handleListArtistHistory(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w2.Code, http.StatusOK)
	}

	var resp2 map[string]any
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decoding response2: %v", err)
	}
	changes2, _ := resp2["changes"].([]any)
	if len(changes2) != 2 {
		t.Errorf("second page len(changes) = %d, want 2", len(changes2))
	}
}

func TestHandleListArtistHistory_ResponseShape(t *testing.T) {
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Shape Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "old", "new", "manual")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleListArtistHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	changes, _ := resp["changes"].([]any)
	if len(changes) == 0 {
		t.Fatal("expected at least one change")
	}

	c, ok := changes[0].(map[string]any)
	if !ok {
		t.Fatalf("change entry is not an object: %T", changes[0])
	}

	// Verify required fields are present.
	for _, field := range []string{"id", "artist_id", "field", "old_value", "new_value", "source", "created_at"} {
		if _, exists := c[field]; !exists {
			t.Errorf("change missing field %q", field)
		}
	}

	if c["field"] != "biography" {
		t.Errorf("field = %v, want biography", c["field"])
	}
	if c["old_value"] != "old" {
		t.Errorf("old_value = %v, want old", c["old_value"])
	}
	if c["new_value"] != "new" {
		t.Errorf("new_value = %v, want new", c["new_value"])
	}
	if c["source"] != "manual" {
		t.Errorf("source = %v, want manual", c["source"])
	}
	if c["artist_id"] != a.ID {
		t.Errorf("artist_id = %v, want %s", c["artist_id"], a.ID)
	}
}

func TestHandleArtistHistoryTab_HTML(t *testing.T) {
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "History Tab Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "", "some biography", "manual")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists/"+a.ID+"/history/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleArtistHistoryTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if body == "" {
		t.Error("response body is empty")
	}
	// The rendered HTML should include the field label for "biography".
	if !strings.Contains(body, "Biography") {
		t.Errorf("response body missing 'Biography' label")
	}
}

func TestHandleArtistHistoryTab_Empty(t *testing.T) {
	r, artistSvc, _ := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "No History Artist")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists/"+a.ID+"/history/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleArtistHistoryTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No metadata changes") {
		t.Errorf("expected empty state message in body: %s", body[:min(200, len(body))])
	}
}

func TestHandleArtistHistoryTab_NilHistoryService(t *testing.T) {
	r, artistSvc, _ := testRouterWithHistory(t)
	r.historyService = nil // simulate unconfigured service

	a := addTestArtist(t, artistSvc, "Nil History Artist")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists/"+a.ID+"/history/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleArtistHistoryTab(w, req)

	// Should render empty state gracefully without panicking.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
