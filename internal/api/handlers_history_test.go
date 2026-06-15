package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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

// testI18nCtx adds the embedded English translator to a context so that
// templates render translated strings (e.g. "Biography") instead of raw keys.
func testI18nCtx(tb testing.TB, ctx context.Context) context.Context {
	tb.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		tb.Fatalf("loading i18n bundle: %v", err)
	}
	return i18n.WithTranslator(ctx, bundle.Translator("en"))
}

// testRouterWithHistory creates a Router wired with a real HistoryService that
// shares the same in-memory database as the artist service, so artist lookups
// and history inserts both work against the same data.
func testRouterWithHistory(t *testing.T) (*Router, *artist.Service, *artist.HistoryService) {
	t.Helper()

	db := newTestDB(t)

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

	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		HistoryService:     historySvc,
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Pagination Artist")

	// Insert 15 changes. Minimum allowed limit is PageSizeMin (10).
	for i := 0; i < 15; i++ {
		addHistoryChange(t, historySvc, a.ID, "biography", "", "value", "manual")
	}

	// Request first page of 10.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history?limit=10&offset=0", nil)
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
		t.Fatal("changes is not []any")
	}
	if len(changes) != 10 {
		t.Errorf("first page len(changes) = %d, want 10", len(changes))
	}
	if resp["total"] != float64(15) {
		t.Errorf("total = %v, want 15", resp["total"])
	}
	if resp["limit"] != float64(10) {
		t.Errorf("limit = %v, want 10", resp["limit"])
	}
	if resp["offset"] != float64(0) {
		t.Errorf("offset = %v, want 0", resp["offset"])
	}

	// Request second page.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/history?limit=10&offset=10", nil)
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
	changes2, ok2 := resp2["changes"].([]any)
	if !ok2 {
		t.Fatal("changes2 is not []any")
	}
	if len(changes2) != 5 {
		t.Errorf("second page len(changes) = %d, want 5", len(changes2))
	}
}

func TestHandleListArtistHistory_ResponseShape(t *testing.T) {
	t.Parallel()
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

	changes, ok := resp["changes"].([]any)
	if !ok {
		t.Fatal("changes is not []any")
	}
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
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "History Tab Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "", "some biography", "manual")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	ctx = testI18nCtx(t, ctx)
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
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "No History Artist")

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
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
	t.Parallel()
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

func TestHandleRevertHistory(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Revert Artist")

	// Update a field to create a history entry.
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "original bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}

	// Get the change ID.
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v, len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	t.Run("reverts field and returns JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
		req.SetPathValue("id", changeID)
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp map[string]any
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if resp["reverted"] != true {
			t.Errorf("reverted = %v, want true", resp["reverted"])
		}

		// Verify the field was reverted.
		updated, err := artistSvc.GetByID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if updated.Biography != "" {
			t.Errorf("Biography = %q, want empty (reverted)", updated.Biography)
		}

		// Verify a revert history record was created.
		allChanges, _, err := historySvc.List(context.Background(), a.ID, 10, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, c := range allChanges {
			if c.Source == "revert" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected a history entry with source 'revert'")
		}
	})

	t.Run("returns 404 for nonexistent change", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/history/nonexistent/revert", nil)
		req.SetPathValue("id", "nonexistent")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("returns 503 when history service is nil", func(t *testing.T) {
		r2, _, _ := testRouterWithHistory(t)
		r2.historyService = nil

		req := httptest.NewRequest(http.MethodPost, "/api/v1/history/any/revert", nil)
		req.SetPathValue("id", "any")
		w := httptest.NewRecorder()

		r2.handleRevertHistory(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
		}
	})
}

// TestHandleRevertHistory_HTMXActivityFragment verifies that an HTMX POST from
// the activity feed returns an HTML fragment (200, Content-Type text/html).
func TestHandleRevertHistory_HTMXActivityFragment(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "HTMX Activity Artist")
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "some bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/activity")
	w := httptest.NewRecorder()

	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

// TestHandleRevertHistory_HTMXArtistTabFragment verifies that an HTMX POST from
// the artist history tab returns an HTML fragment (200, Content-Type text/html).
func TestHandleRevertHistory_HTMXArtistTabFragment(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "HTMX Tab Artist")
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "tab bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/artists/"+a.ID)
	w := httptest.NewRecorder()

	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

func TestHandleListGlobalHistory(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Global History Artist")
	b := addTestArtist(t, artistSvc, "Second Global Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "", "bio text", "manual")
	addHistoryChange(t, historySvc, a.ID, "genres", "", "Rock", "scan")
	addHistoryChange(t, historySvc, b.ID, "biography", "", "second bio", "manual")

	t.Run("returns all changes with JSON shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil)
		w := httptest.NewRecorder()

		r.handleListGlobalHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp map[string]any
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decoding: %v", err)
		}

		changes, ok := resp["changes"].([]any)
		if !ok {
			t.Fatalf("changes is not []any: %T", resp["changes"])
		}
		if len(changes) != 3 {
			t.Errorf("len(changes) = %d, want 3", len(changes))
		}
		if resp["total"] != float64(3) {
			t.Errorf("total = %v, want 3", resp["total"])
		}

		// Verify artist_name is present and cross-artist results are included.
		artistNames := map[string]bool{}
		for _, entry := range changes {
			c, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if name, exists := c["artist_name"]; exists {
				if n, ok := name.(string); ok {
					artistNames[n] = true
				}
			}
		}
		if !artistNames["Global History Artist"] {
			t.Error("expected changes from 'Global History Artist'")
		}
		if !artistNames["Second Global Artist"] {
			t.Error("expected changes from 'Second Global Artist'")
		}
	})

	t.Run("filters by source", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/history?source=scan", nil)
		w := httptest.NewRecorder()

		r.handleListGlobalHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp map[string]any
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if resp["total"] != float64(1) {
			t.Errorf("total = %v, want 1", resp["total"])
		}
	})

	t.Run("returns 503 when nil", func(t *testing.T) {
		r2, _, _ := testRouterWithHistory(t)
		r2.historyService = nil

		req := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil)
		w := httptest.NewRecorder()

		r2.handleListGlobalHistory(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
		}
	})
}

func TestHandleListGlobalHistory_WildcardSource(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Wildcard Source Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "", "bio", "provider:musicbrainz")
	addHistoryChange(t, historySvc, a.ID, "genres", "", "Rock", "provider:audiodb")
	addHistoryChange(t, historySvc, a.ID, "moods", "", "Happy", "manual")

	// The wildcard "provider:*" should match both provider sources.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/history?source=provider:*", nil)
	w := httptest.NewRecorder()

	r.handleListGlobalHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["total"] != float64(2) {
		t.Errorf("total = %v, want 2", resp["total"])
	}
}

func TestHandleListGlobalHistory_DateRange(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "Date Range Artist")
	addHistoryChange(t, historySvc, a.ID, "biography", "", "bio", "manual")

	// Use a time range that spans now to include the just-inserted change.
	now := time.Now().UTC()
	from := now.Add(-1 * time.Minute).Format(time.RFC3339)
	to := now.Add(1 * time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/history?from="+from+"&to="+to, nil)
	w := httptest.NewRecorder()

	r.handleListGlobalHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["total"] != float64(1) {
		t.Errorf("total = %v, want 1", resp["total"])
	}

	// Use a range entirely in the past to exclude the change.
	pastFrom := "2020-01-01T00:00:00Z"
	pastTo := "2020-01-01T01:00:00Z"
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/history?from="+pastFrom+"&to="+pastTo, nil)
	w2 := httptest.NewRecorder()

	r.handleListGlobalHistory(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w2.Code, http.StatusOK)
	}

	var resp2 map[string]any
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp2["total"] != float64(0) {
		t.Errorf("total = %v, want 0", resp2["total"])
	}

	// Plain YYYY-MM-DD bounds: the date input on the activity page emits
	// these values, and the parser must treat from=YYYY-MM-DD as UTC midnight
	// and to=YYYY-MM-DD as end-of-day UTC so the full day is included. Using
	// today's UTC date guarantees the just-inserted change falls within the
	// window regardless of the test machine's wall clock.
	today := now.Format(time.DateOnly)
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/history?from="+today+"&to="+today, nil)
	w3 := httptest.NewRecorder()

	r.handleListGlobalHistory(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w3.Code, http.StatusOK, w3.Body.String())
	}

	var resp3 map[string]any
	if err := json.NewDecoder(w3.Body).Decode(&resp3); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp3["total"] != float64(1) {
		t.Errorf("plain-date total = %v, want 1", resp3["total"])
	}
}

func TestHandleActivityPage(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/activity", nil)
	w := httptest.NewRecorder()

	r.handleActivityPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Activity") {
		t.Error("response body missing 'Activity' heading")
	}
}

// TestHandleActivityContent_RendersRow verifies that handleActivityContent
// renders activityRow entries including the old/new value blocks.
// This covers the whitespace-pre-wrap change in the generated activity_templ.go.
func TestHandleActivityContent_RendersRow(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	a := addTestArtist(t, artistSvc, "Activity Row Artist")

	// Use long (>300 char) multi-line values so this test catches regressions
	// in either 300-char truncation or newline rendering (whitespace-pre-wrap).
	oldVal := "old biography line 1\n" + strings.Repeat("A", 340)
	newVal := "new biography line 1\nnew biography line 2\n" + strings.Repeat("B", 340)
	addHistoryChange(t, historySvc, a.ID, "biography", oldVal, newVal, "manual")

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/activity/content", nil)
	w := httptest.NewRecorder()

	r.handleActivityContent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, oldVal) {
		t.Error("response missing full old value in activity row (truncation regression?)")
	}
	if !strings.Contains(body, newVal) {
		t.Error("response missing full new value in activity row (truncation regression?)")
	}
	if !strings.Contains(body, "whitespace-pre-wrap") {
		t.Error("response missing whitespace-pre-wrap class (multiline rendering regression?)")
	}
}

// htmxRevertRequest builds a POST request that mimics the HTMX undo button
// click. It sets HX-Request, HX-Current-URL, the form-encoded showing hint
// when non-empty, and the fromActivity routing tag so the handler chooses
// the correct rendering branch.
func htmxRevertRequest(t *testing.T, ctx context.Context, changeID, currentURL, showingHint string) *http.Request {
	t.Helper()
	body := url.Values{}
	if showingHint != "" {
		body.Set("showing", showingHint)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/history/"+changeID+"/revert",
		strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", currentURL)
	req.SetPathValue("id", changeID)
	return req
}

// seedHistoryChanges inserts n biography history rows for the given artist.
// Each row stores OldValue="some-value-N" (a non-empty value distinct from
// the field's actual current state) so the revert handler can attempt to
// restore something concrete and successfully record a new revert row. It
// returns the change IDs ordered most-recent-first (matching List() order).
func seedHistoryChanges(t *testing.T, svc *artist.HistoryService, artistID string, n int) []string {
	t.Helper()
	for i := 0; i < n; i++ {
		// OldValue is non-empty so reverting will call UpdateField with that
		// value, which writes through (the artist's biography starts empty)
		// and creates a new revert history row.
		oldVal := "old-bio-" + strconv.Itoa(i)
		newVal := "new-bio-" + strconv.Itoa(i)
		if err := svc.Record(context.Background(), artistID, "biography", oldVal, newVal, "manual"); err != nil {
			t.Fatalf("seed history %d: %v", i, err)
		}
	}
	all, _, err := svc.List(context.Background(), artistID, n+10, 0)
	if err != nil {
		t.Fatalf("list seeded changes: %v", err)
	}
	ids := make([]string, 0, len(all))
	for _, c := range all {
		ids = append(ids, c.ID)
	}
	return ids
}

// TestHandleRevertHistory_ActivityShowingCounter_LoadMoreHint locks in the
// load-more counter regression: after Load-more on the activity feed, the
// revert handler must use the client-supplied "showing" hint (DOM row count)
// rather than activeFilter.Offset+limit, because HX-Current-URL still has
// offset=0 (the load-more button does not push the URL). With 47 entries
// seeded and 40 rows rendered (DOM count from a hypothetical second
// load-more), the OOB counter fragment must reflect 40, not the page-1 size.
func TestHandleRevertHistory_ActivityShowingCounter_LoadMoreHint(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Activity LoadMore Artist")
	// Seed 47 rows so that 40 visible (after load-more rounds) is plausibly
	// less than total. The revert will add one more (making total 48), so
	// the rendered counter should be "40 of 48" with the hint.
	ids := seedHistoryChanges(t, historySvc, a.ID, 47)
	if len(ids) != 47 {
		t.Fatalf("seeded count = %d, want 47", len(ids))
	}

	// Pick a change ID in the middle of the list. The undo button on the
	// activity feed posts here.
	target := ids[10]

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))

	t.Run("uses showing hint over offset+limit when load-more state is implied", func(t *testing.T) {
		// Browser URL never has offset because load-more does not push it.
		// hx-vals carries the actual DOM row count: 40.
		req := htmxRevertRequest(t, ctx, target, "http://localhost:8080/activity", "40")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		// Counter must reflect the hint (40), not the page-1 size (typically the
		// configured page size). Total grows by 1 due to the inserted revert row.
		if !strings.Contains(body, "Showing 40 of 48") {
			t.Errorf("counter regressed: body missing 'Showing 40 of 48'\nbody: %s", body)
		}
	})

	t.Run("falls back to offset+limit when no hint is supplied", func(t *testing.T) {
		// Re-seed because the previous revert mutated state.
		a2 := addTestArtist(t, artistSvc, "Activity NoHint Artist")
		_ = seedHistoryChanges(t, historySvc, a2.ID, 47)
		// Find a change for a2.
		all, _, err := historySvc.List(context.Background(), a2.ID, 47, 0)
		if err != nil || len(all) == 0 {
			t.Fatalf("List a2: err=%v len=%d", err, len(all))
		}
		target2 := all[10].ID

		req := htmxRevertRequest(t, ctx, target2, "http://localhost:8080/activity?artist_id="+a2.ID, "")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		// Without the hint, fallback uses offset+limit which is the page size
		// (default 50, but min(showing, total) caps at total). The artist
		// filter restricts results to a2 only, so the rendered total is 48 and
		// the fallback showing should equal min(0+50, 48) = 48.
		if !strings.Contains(body, "Showing 48 of 48") {
			t.Errorf("fallback counter wrong: body missing 'Showing 48 of 48'\nbody: %s", body)
		}
	})

	t.Run("rejects hint when greater than total", func(t *testing.T) {
		a3 := addTestArtist(t, artistSvc, "Activity HintCap Artist")
		_ = seedHistoryChanges(t, historySvc, a3.ID, 5)
		all, _, err := historySvc.List(context.Background(), a3.ID, 5, 0)
		if err != nil || len(all) == 0 {
			t.Fatalf("List a3: err=%v len=%d", err, len(all))
		}
		target3 := all[0].ID

		// Hint claims 9999 visible rows. Server should reject it.
		req := htmxRevertRequest(t, ctx, target3,
			"http://localhost:8080/activity?artist_id="+a3.ID, "9999")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		// Hint rejected. Fallback applies: min(0+pageSize, 6) = 6.
		if !strings.Contains(body, "Showing 6 of 6") {
			t.Errorf("hint cap not enforced: body missing 'Showing 6 of 6'\nbody: %s", body)
		}
		// Inversely, the rejected hint must NOT appear.
		if strings.Contains(body, "Showing 9999") {
			t.Errorf("body contains rejected hint: %s", body)
		}
	})

	t.Run("rejects non-numeric hint", func(t *testing.T) {
		a4 := addTestArtist(t, artistSvc, "Activity BadHint Artist")
		_ = seedHistoryChanges(t, historySvc, a4.ID, 5)
		all, _, err := historySvc.List(context.Background(), a4.ID, 5, 0)
		if err != nil || len(all) == 0 {
			t.Fatalf("List a4: err=%v len=%d", err, len(all))
		}
		target4 := all[0].ID

		req := htmxRevertRequest(t, ctx, target4,
			"http://localhost:8080/activity?artist_id="+a4.ID, "not-a-number")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		body := w.Body.String()
		// Non-numeric hint silently ignored; fallback applies.
		if !strings.Contains(body, "Showing 6 of 6") {
			t.Errorf("non-numeric hint not rejected: body missing 'Showing 6 of 6'\nbody: %s", body)
		}
	})
}

// TestValidateRevertable exercises the eligibility checks in isolation.
// These rules must be enforced at the validate stage before any mutation.
func TestValidateRevertable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		field   string
		source  string
		wantErr error
	}{
		{"trackable field, non-revert source", "biography", "manual", nil},
		{"trackable field, provider source", "genres", "provider:musicbrainz", nil},
		{"untrackable field", "unknown_field_xyz", "manual", errRevertNotTrackable},
		{"revert source is rejected", "biography", "revert", errRevertOfRevert},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			change := &artist.MetadataChange{
				Field:  tc.field,
				Source: tc.source,
			}
			err := validateRevertable(change)
			if tc.wantErr == nil && err != nil {
				t.Errorf("validateRevertable() = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("validateRevertable() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestPerformRevert_ClearBranch verifies that performRevert calls ClearField
// when change.OldValue is empty and records a new history entry with source
// "revert". The returned revertChangeID must match the row written to the DB.
func TestPerformRevert_ClearBranch(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "PerformRevert Clear Artist")

	// Set biography to non-empty so ClearField has something to clear.
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "some bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}

	change := &artist.MetadataChange{
		ArtistID: a.ID,
		Field:    "biography",
		OldValue: "", // triggers ClearField
		NewValue: "some bio",
		Source:   "manual",
	}

	revertChangeID, _, err := r.performRevert(context.Background(), change)
	if err != nil {
		t.Fatalf("performRevert: %v", err)
	}
	if revertChangeID == "" {
		t.Fatal("revertChangeID is empty")
	}

	// The artist's biography should now be cleared.
	updated, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if updated.Biography != "" {
		t.Errorf("Biography = %q, want empty after clear revert", updated.Biography)
	}

	// A history row with source "revert" and the pre-assigned ID must exist.
	row, err := historySvc.GetByID(context.Background(), revertChangeID)
	if err != nil {
		t.Fatalf("GetByID(revert row): %v", err)
	}
	if row.Source != "revert" {
		t.Errorf("row.Source = %q, want revert", row.Source)
	}
	if row.ID != revertChangeID {
		t.Errorf("row.ID = %q, want %q", row.ID, revertChangeID)
	}
}

// TestPerformRevert_UpdateBranch verifies that performRevert calls UpdateField
// when change.OldValue is non-empty, restoring the old value.
func TestPerformRevert_UpdateBranch(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "PerformRevert Update Artist")

	change := &artist.MetadataChange{
		ArtistID: a.ID,
		Field:    "biography",
		OldValue: "original bio", // triggers UpdateField
		NewValue: "changed bio",
		Source:   "manual",
	}

	revertChangeID, _, err := r.performRevert(context.Background(), change)
	if err != nil {
		t.Fatalf("performRevert: %v", err)
	}

	// The artist's biography should be restored to the old value.
	updated, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if updated.Biography != "original bio" {
		t.Errorf("Biography = %q, want 'original bio'", updated.Biography)
	}

	// Verify the revert history row was recorded with the deterministic ID.
	row, err := historySvc.GetByID(context.Background(), revertChangeID)
	if err != nil {
		t.Fatalf("GetByID(revert row): %v", err)
	}
	if row.Source != "revert" {
		t.Errorf("row.Source = %q, want revert", row.Source)
	}
}

// TestHandleRevertHistory_ArtistTabShowingCounter_LoadMoreHint covers the same
// regression as the activity test but for the artist detail page's history
// tab. The fallback path uses len(changes)-1 from a List(limit, 0) call which
// regresses to the first-page count after the user has loaded additional pages.
// The hint must override that fallback.
func TestHandleRevertHistory_ArtistTabShowingCounter_LoadMoreHint(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Artist Tab LoadMore Artist")
	_ = seedHistoryChanges(t, historySvc, a.ID, 47)
	all, _, err := historySvc.List(context.Background(), a.ID, 47, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	target := all[10].ID

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))

	t.Run("uses showing hint over len(changes)-1 fallback", func(t *testing.T) {
		// HX-Current-URL points to the artist detail page (not /activity), so
		// the fromActivity branch is false and the artist-tab branch runs.
		req := htmxRevertRequest(t, ctx, target,
			"http://localhost:8080/artists/"+a.ID, "40")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "Showing 40 of 48") {
			t.Errorf("artist tab counter regressed: body missing 'Showing 40 of 48'\nbody: %s", body)
		}
	})

	t.Run("falls back to len(changes)-1 when no hint", func(t *testing.T) {
		a2 := addTestArtist(t, artistSvc, "Artist Tab NoHint Artist")
		_ = seedHistoryChanges(t, historySvc, a2.ID, 5)
		all2, _, err := historySvc.List(context.Background(), a2.ID, 5, 0)
		if err != nil || len(all2) == 0 {
			t.Fatalf("List: err=%v len=%d", err, len(all2))
		}
		target2 := all2[0].ID

		req := htmxRevertRequest(t, ctx, target2,
			"http://localhost:8080/artists/"+a2.ID, "")
		w := httptest.NewRecorder()

		r.handleRevertHistory(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		// total = 6, len(changes) on first page = 6 (fits on default page size),
		// fallback = len-1 = 5 (the pre-revert visible count).
		if !strings.Contains(body, "Showing 5 of 6") {
			t.Errorf("fallback counter wrong: body missing 'Showing 5 of 6'\nbody: %s", body)
		}
	})
}

// TestResolveShowingCount exercises the hint-vs-fallback decision in isolation.
// This is the helper both render branches (activity feed and artist history
// tab) call to honor the hx-vals "showing" hint while degrading gracefully
// when the hint is missing, malformed, or out of range.
func TestResolveShowingCount(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// includeShowing distinguishes "no showing key in body" from
	// "showing key present with empty value": both render to the handler
	// as FormValue("showing") == "" but they exercise different branches
	// at the HTTP layer, so the table below covers each explicitly.
	mkReq := func(showing string, includeShowing bool) *http.Request {
		body := url.Values{}
		if includeShowing {
			body.Set("showing", showing)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/history/x/revert",
			strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	cases := []struct {
		name     string
		hint     string
		include  bool
		fallback int
		total    int
		want     int
	}{
		{"hint preferred when in range", "40", true, 20, 48, 40},
		{"hint accepts equal-to-total", "48", true, 20, 48, 48},
		{"hint accepts boundary 1", "1", true, 20, 48, 1},
		{"missing hint uses fallback", "", false, 20, 48, 20},
		{"empty hint uses fallback", "", true, 20, 48, 20},
		{"non-numeric hint uses fallback", "not-a-number", true, 20, 48, 20},
		{"zero hint uses fallback", "0", true, 20, 48, 20},
		{"negative hint uses fallback", "-5", true, 20, 48, 20},
		{"hint over total uses fallback", "9999", true, 20, 48, 20},
		{"hint just over total uses fallback", "49", true, 20, 48, 20},
		{"negative fallback clamped to zero", "", false, -3, 48, 0},
		{"fallback over total clamped to total", "", false, 99, 48, 48},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveShowingCount(mkReq(tc.hint, tc.include), tc.fallback, tc.total, logger)
			if got != tc.want {
				t.Errorf("hint=%q fallback=%d total=%d: got %d, want %d",
					tc.hint, tc.fallback, tc.total, got, tc.want)
			}
		})
	}

	t.Run("nil logger does not panic on bad hint", func(t *testing.T) {
		// Defensive: callers may pass a nil logger in tests or stripped
		// builds. The helper must not panic when logging the bad hint.
		got := resolveShowingCount(mkReq("garbage", true), 20, 48, nil)
		if got != 20 {
			t.Errorf("nil-logger fallback: got %d, want 20", got)
		}
	})
}

// TestHandleRevertHistory_ActivityFromArtistPageRoute covers the routing
// decision in handleRevertHistory: when HX-Current-URL contains "/activity"
// the activity-feed fragment is rendered, otherwise the artist-tab fragment.
// This is the contract the hint relies on to drive the right rendering branch.
func TestHandleRevertHistory_ActivityFromArtistPageRoute(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Route Test Artist")
	_ = seedHistoryChanges(t, historySvc, a.ID, 3)
	all, _, err := historySvc.List(context.Background(), a.ID, 3, 0)
	if err != nil || len(all) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(all))
	}
	target := all[0].ID

	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))

	// /activity route -> activity fragment id
	reqAct := htmxRevertRequest(t, ctx, target, "http://localhost:8080/activity", "")
	wAct := httptest.NewRecorder()
	r.handleRevertHistory(wAct, reqAct)
	bodyAct := wAct.Body.String()
	// Activity fragment uses #activity-entries, artist tab uses #history-change-list.
	if !strings.Contains(bodyAct, `id="activity-entries"`) {
		t.Errorf("activity route did not render activity fragment\nbody: %s", bodyAct)
	}

	// Reset state for a second revert.
	a2 := addTestArtist(t, artistSvc, "Route Test Artist 2")
	_ = seedHistoryChanges(t, historySvc, a2.ID, 3)
	all2, _, err := historySvc.List(context.Background(), a2.ID, 3, 0)
	if err != nil || len(all2) == 0 {
		t.Fatalf("List a2: err=%v len=%d", err, len(all2))
	}
	target2 := all2[0].ID

	// /artists/{id} route -> artist-tab fragment id
	reqArt := htmxRevertRequest(t, ctx, target2, "http://localhost:8080/artists/"+a2.ID, "")
	wArt := httptest.NewRecorder()
	r.handleRevertHistory(wArt, reqArt)
	bodyArt := wArt.Body.String()
	if !strings.Contains(bodyArt, `id="history-change-list"`) {
		t.Errorf("artist-page route did not render history tab fragment\nbody: %s", bodyArt)
	}
}

func TestParseFilterValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"manual", "+scan", "-provider:mb"}, []string{"manual", "scan"}},
		{[]string{"", "  ", "+good"}, []string{"good"}},
		{[]string{"-only-excludes"}, nil},
		{[]string{}, nil},
	}
	for _, tc := range cases {
		got := parseFilterValues(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("input %v: got %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("input %v index %d: got %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestSliceContains(t *testing.T) {
	t.Parallel()
	if !sliceContains([]string{"a", "b", "c"}, "b") {
		t.Error("expected true for present element")
	}
	if sliceContains([]string{"a", "b", "c"}, "d") {
		t.Error("expected false for absent element")
	}
	if sliceContains(nil, "a") {
		t.Error("expected false for nil slice")
	}
}

func TestIsPlainDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		want bool
	}{
		{"2024-01-15", true},
		{"2024-1-15", false},  // too short
		{"2024-01-1", false},  // too short
		{"2024/01/15", false}, // wrong separator
		{"20240115", false},   // no separators
		{"abcd-ef-gh", false}, // non-digit body
		{"", false},
	}
	for _, tc := range cases {
		got := isPlainDate(tc.s)
		if got != tc.want {
			t.Errorf("isPlainDate(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestParseTimeValue(t *testing.T) {
	t.Parallel()

	t.Run("empty returns zero", func(t *testing.T) {
		if !parseTimeValue("", "from").IsZero() {
			t.Error("expected zero time for empty input")
		}
	})

	t.Run("unparsable returns zero", func(t *testing.T) {
		if !parseTimeValue("not-a-date", "from").IsZero() {
			t.Error("expected zero time for unparsable input")
		}
	})

	t.Run("RFC3339 parsed correctly", func(t *testing.T) {
		got := parseTimeValue("2024-06-01T12:00:00Z", "from")
		if got.IsZero() {
			t.Error("expected non-zero time for RFC3339 input")
		}
	})

	t.Run("plain date from is midnight UTC", func(t *testing.T) {
		got := parseTimeValue("2024-06-01", "from")
		if got.Hour() != 0 || got.Minute() != 0 {
			t.Errorf("from date: expected midnight UTC, got %v", got)
		}
	})

	t.Run("plain date to is end-of-day UTC", func(t *testing.T) {
		got := parseTimeValue("2024-06-01", "to")
		if got.Hour() != 23 || got.Minute() != 59 {
			t.Errorf("to date: expected end-of-day UTC, got %v", got)
		}
	})
}

func TestBuildGlobalFilterFromURL(t *testing.T) {
	t.Parallel()

	t.Run("invalid URL returns empty filter", func(t *testing.T) {
		f := buildGlobalFilterFromURL("://not-a-url")
		if f.ArtistID != "" || f.Offset != 0 {
			t.Errorf("expected empty filter for invalid URL, got %+v", f)
		}
	})

	t.Run("negative offset clamped to zero", func(t *testing.T) {
		f := buildGlobalFilterFromURL("http://localhost/activity?offset=-5")
		if f.Offset != 0 {
			t.Errorf("expected offset=0 for negative input, got %d", f.Offset)
		}
	})

	t.Run("source prefix extracted", func(t *testing.T) {
		f := buildGlobalFilterFromURL("http://localhost/activity?source=provider:*&artist_id=abc")
		if f.ArtistID != "abc" {
			t.Errorf("ArtistID = %q, want abc", f.ArtistID)
		}
		if len(f.SourcePrefixes) != 1 || f.SourcePrefixes[0] != "provider:" {
			t.Errorf("SourcePrefixes = %v, want [provider:]", f.SourcePrefixes)
		}
	})
}

// TestHandleRevertHistory_MissingPathParam verifies that the handler writes a
// 400 response when the "id" path parameter is absent (RequirePathParam fails).
func TestHandleRevertHistory_MissingPathParam(t *testing.T) {
	t.Parallel()
	r, _, historySvc := testRouterWithHistory(t)
	r.historyService = historySvc

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history//revert", nil)
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleRevertHistory_UntrackableField verifies that the handler returns
// 400 when the change's field is not in the revertable field set.
func TestHandleRevertHistory_UntrackableField(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Untrackable Field Artist")
	addHistoryChange(t, historySvc, a.ID, "unknown_field_xyz", "old", "new", "manual")
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changes[0].ID+"/revert", nil)
	req.SetPathValue("id", changes[0].ID)
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleRevertHistory_ActivitySourceFilter verifies that when the active
// activity feed filter restricts sources and excludes "revert", the handler
// suppresses the rich HTMX fragment and writes the plain fallback instead.
func TestHandleRevertHistory_ActivitySourceFilter(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Source Filter Artist")
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "filter bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	// HX-Current-URL carries source=manual; "revert" is excluded, so the rich
	// fragment must be suppressed and the plain amber fallback written instead.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/activity?source=manual")
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `id="activity-entries"`) {
		t.Errorf("expected fallback fragment, got rich activity fragment; body: %s", body)
	}
	if !strings.Contains(body, "border-amber") {
		t.Errorf("expected fallback amber div in body: %s", body)
	}
}

// TestHandleRevertHistory_ActivityDateRangeFilter verifies that when the
// activity feed has a date-range upper bound in the past, the revert row
// (created now) falls outside the window and the fallback fragment is written.
func TestHandleRevertHistory_ActivityDateRangeFilter(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Date Range Artist")
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "range bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	// "to=2020-01-01" places the upper bound before now; the revert row is
	// created at the current time and therefore falls outside the filter.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/activity?to=2020-01-01")
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `id="activity-entries"`) {
		t.Errorf("expected fallback fragment, got rich activity fragment")
	}
}

// TestHandleRevertHistory_NoopRevertFallback verifies the Warn-level fallback
// branch: the revert mutation succeeds but the history service skips recording
// (oldValue == newValue), so GetByID(revertChangeID) returns ErrChangeNotFound
// and the handler falls through to the amber warning fragment.
func TestHandleRevertHistory_NoopRevertFallback(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Noop Revert Artist")
	// Record a change whose OldValue ("noop-bio") we will pre-load into the
	// artist so the revert UpdateField call is a no-op.
	addHistoryChange(t, historySvc, a.ID, "biography", "noop-bio", "current-bio", "manual")
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	// Set biography to "noop-bio" directly so UpdateField("biography","noop-bio")
	// sees oldValue==newValue and skips history recording. revertChangeID is
	// therefore not found by GetByID, leaving revertChange nil.
	if _, err := r.db.ExecContext(context.Background(),
		"UPDATE artists SET biography = ? WHERE id = ?", "noop-bio", a.ID); err != nil {
		t.Fatalf("pre-loading biography: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/artists/"+a.ID)
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `id="history-change-list"`) {
		t.Errorf("expected fallback fragment, got rich artist-tab fragment; body: %s", body)
	}
	if !strings.Contains(body, "border-amber") {
		t.Errorf("expected fallback amber div in body: %s", body)
	}
}

// TestHandleRevertHistory_NoopRendersHonestMessage verifies that when a revert
// is a no-op (the field already equals OldValue), the HTMX fallback fragment
// contains the honest "already at the reverted value" message rather than the
// generic "Change reverted. Refresh..." message.
func TestHandleRevertHistory_NoopRendersHonestMessage(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Noop Revert Artist")

	// Record a history entry directly (source="manual", field changes from ""
	// to "some bio"). OldValue="" means a revert would call ClearField.
	changeID := "test-noop-change-" + a.ID
	ctx := context.Background()
	err := historySvc.Repo().Record(ctx, &artist.MetadataChange{
		ID:       changeID,
		ArtistID: a.ID,
		Field:    "biography",
		OldValue: "",
		NewValue: "some bio",
		Source:   "manual",
	})
	if err != nil {
		t.Fatalf("inserting history entry: %v", err)
	}

	// The artist's biography is already "" (empty, same as OldValue), so the
	// revert is a no-op: ClearField will see oldValue="" and skip the write.
	// The HTMX response should carry the honest no-op message.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/artists/"+a.ID)
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "already at the reverted value") {
		t.Errorf("expected honest no-op message in body; got: %s", body)
	}
	if strings.Contains(body, "Refresh the page") {
		t.Errorf("got generic 'Refresh the page' message for a no-op revert; want honest message; body: %s", body)
	}
}

// TestPlatformPullNoopFieldNotInUpdated verifies the service-level contract
// underlying the platform-pull handler: UpdateField returns changed=false when
// the field already matches the incoming platform value, so callers must gate
// the append to `updated` on changed==true (not nil error).
//
// Note: end-to-end testing of handlePullMetadata requires a real Emby/Jellyfin
// server. This test covers the service contract that the handler relies on.
func TestPlatformPullNoopFieldNotInUpdated(t *testing.T) {
	t.Parallel()
	_, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Platform Pull Noop Artist")
	ctx := context.Background()

	// Set a biography on the artist.
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "existing bio"); err != nil {
		t.Fatalf("setting initial biography: %v", err)
	}

	// Simulate what handlePullMetadata does: try to apply the same value the
	// platform returned. Should be a no-op (changed=false).
	var updated []string
	changed, err := artistSvc.UpdateField(ctx, a.ID, "biography", "existing bio")
	if err != nil {
		t.Fatalf("UpdateField same value: %v", err)
	}
	if changed {
		updated = append(updated, "biography")
	}

	if len(updated) != 0 {
		t.Errorf("updated = %v, want [] (matching field must not appear in updated)", updated)
	}

	// Now apply a different value; it must be in updated.
	changed, err = artistSvc.UpdateField(ctx, a.ID, "biography", "new bio")
	if err != nil {
		t.Fatalf("UpdateField new value: %v", err)
	}
	if changed {
		updated = append(updated, "biography")
	}
	if len(updated) != 1 || updated[0] != "biography" {
		t.Errorf("updated = %v, want [biography] after a real change", updated)
	}
}
