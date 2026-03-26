package wikipedia

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestAdapter creates an adapter with all three endpoints pointing to mock servers.
// Pass "" for any endpoint to use a dummy URL (for tests that don't hit that endpoint).
func newTestAdapter(t *testing.T, actionURL, sparqlURL, wikidataAPIURL string) *Adapter {
	t.Helper()
	if actionURL == "" {
		actionURL = "http://unused-action"
	}
	if sparqlURL == "" {
		sparqlURL = "http://unused-sparql"
	}
	if wikidataAPIURL == "" {
		wikidataAPIURL = "http://unused-wdapi"
	}
	return NewWithEndpoints(provider.NewRateLimiterMap(), silentLogger(),
		actionURL, sparqlURL, wikidataAPIURL)
}

// --- SPARQL mock helpers ---

func sparqlServerReturning(t *testing.T, articleURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		if articleURL != "" {
			resp.Results.Bindings = append(resp.Results.Bindings, struct {
				Article struct {
					Value string `json:"value"`
				} `json:"article"`
			}{
				Article: struct {
					Value string `json:"value"`
				}{Value: articleURL},
			})
		}
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
}

// --- Action API mock helpers ---

func actionExtractServer(t *testing.T, title, extract string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		switch action {
		case "query":
			resp := extractResponse{}
			resp.Query.Pages = map[string]extractPage{
				"12345": {PageID: 12345, Title: title, Extract: extract},
			}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "parse":
			// Return empty wikitext (no infobox).
			resp := parseResponse{}
			resp.Parse.Title = title
			resp.Parse.PageID = 12345
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

func actionExtractAndWikitextServer(t *testing.T, title, extract, wikitext string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		switch action {
		case "query":
			meta := r.URL.Query().Get("meta")
			if meta == "siteinfo" {
				// TestConnection probe.
				json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"general": map[string]any{}}}) //nolint:errcheck
				return
			}
			resp := extractResponse{}
			resp.Query.Pages = map[string]extractPage{
				"12345": {PageID: 12345, Title: title, Extract: extract},
			}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "parse":
			resp := parseResponse{}
			resp.Parse.Title = title
			resp.Parse.PageID = 12345
			resp.Parse.Wikitext.Text = wikitext
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

// --- Wikidata entity API mock ---

func wikidataEntityServer(t *testing.T, qid, articleTitle string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := wbEntityResponse{
			Entities: map[string]wbEntity{
				qid: {
					Sitelinks: map[string]wbSitelink{
						"enwiki": {Title: articleTitle},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
}

// --- Tests ---

func TestGetArtist_ValidMBID(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Radiohead")
	defer sparqlSrv.Close()

	actionSrv := actionExtractServer(t, "Radiohead",
		"Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985.")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", meta.Name)
	}
	if meta.Biography != "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985." {
		t.Errorf("Biography = %q, want Radiohead bio", meta.Biography)
	}
	if meta.URLs["wikipedia"] == "" {
		t.Error("expected wikipedia URL in metadata")
	}
}

func TestGetArtist_NonMBID(t *testing.T) {
	// Non-UUID, non-QID string is treated as article title.
	// With no action server, it should fail.
	adapter := newTestAdapter(t, "", "", "")

	_, err := adapter.GetArtist(context.Background(), "Radiohead")
	// "Radiohead" is treated as an article title, but the action server is unreachable.
	if err == nil {
		t.Fatal("expected error for unreachable action server")
	}
}

func TestGetArtist_NotFoundInWikidata(t *testing.T) {
	// SPARQL returns empty bindings.
	sparqlSrv := sparqlServerReturning(t, "")
	defer sparqlSrv.Close()

	adapter := newTestAdapter(t, "", sparqlSrv.URL, "")

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error when MBID not found in Wikidata")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_WikidataServerError(t *testing.T) {
	sparqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sparqlSrv.Close()

	adapter := newTestAdapter(t, "", sparqlSrv.URL, "")

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error on Wikidata server error")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestGetArtist_ExtractNotFound(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Test_Artist")
	defer sparqlSrv.Close()

	// Action API returns page ID -1 (not found).
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := extractResponse{}
		resp.Query.Pages = map[string]extractPage{
			"-1": {},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error when Wikipedia article not found")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_EmptyExtract(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Empty_Article")
	defer sparqlSrv.Close()

	actionSrv := actionExtractServer(t, "Empty Article", "")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error when extract is empty")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_UnexpectedArticleURL(t *testing.T) {
	// SPARQL returns a URL without /wiki/ prefix.
	sparqlSrv := sparqlServerReturning(t, "https://www.wikidata.org/entity/Q44190")
	defer sparqlSrv.Close()

	adapter := newTestAdapter(t, "", sparqlSrv.URL, "")

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for unexpected article URL format")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable for unexpected URL, got %T: %v", err, err)
	}
}

func TestGetArtist_PercentEncodedTitle(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/AC%2FDC")
	defer sparqlSrv.Close()

	actionSrv := actionExtractServer(t, "AC/DC",
		"AC/DC are an Australian rock band formed in Sydney in 1973.")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.ProviderID != "AC/DC" {
		t.Errorf("ProviderID = %q, want %q", meta.ProviderID, "AC/DC")
	}
	if meta.Name != "AC/DC" {
		t.Errorf("Name = %q, want %q", meta.Name, "AC/DC")
	}
}

func TestGetArtist_WithInfobox(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Radiohead")
	defer sparqlSrv.Close()

	wikitext := loadFixture(t, "infobox_band.txt")
	actionSrv := actionExtractAndWikitextServer(t, "Radiohead",
		"Radiohead are an English rock band.", wikitext)
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	if meta.Country != "Abingdon, Oxfordshire, England" {
		t.Errorf("Country = %q, want %q", meta.Country, "Abingdon, Oxfordshire, England")
	}

	if len(meta.Genres) < 3 {
		t.Errorf("expected at least 3 genres, got %d: %v", len(meta.Genres), meta.Genres)
	}

	// Should have 5 active members.
	activeCount := 0
	for _, m := range meta.Members {
		if m.IsActive {
			activeCount++
		}
	}
	if activeCount != 5 {
		t.Errorf("expected 5 active members, got %d", activeCount)
	}

	if meta.YearsActive == "" {
		t.Error("expected non-empty YearsActive")
	}
}

func TestGetArtist_WithPastMembers(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Pink_Floyd")
	defer sparqlSrv.Close()

	wikitext := loadFixture(t, "infobox_with_past_members.txt")
	actionSrv := actionExtractAndWikitextServer(t, "Pink Floyd",
		"Pink Floyd were an English rock band.", wikitext)
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	activeCount := 0
	pastCount := 0
	for _, m := range meta.Members {
		if m.IsActive {
			activeCount++
		} else {
			pastCount++
		}
	}
	if activeCount != 2 {
		t.Errorf("expected 2 active members, got %d", activeCount)
	}
	if pastCount != 3 {
		t.Errorf("expected 3 past members, got %d", pastCount)
	}
}

func TestGetArtist_NoInfobox(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Some_Artist")
	defer sparqlSrv.Close()

	// Action server returns extract but no infobox in wikitext.
	actionSrv := actionExtractAndWikitextServer(t, "Some Artist",
		"Some Artist is a musician.", "This article has no infobox, just text.")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	// Biography should still be populated.
	if meta.Biography == "" {
		t.Error("expected non-empty Biography even without infobox")
	}
	// Infobox fields should be empty.
	if len(meta.Genres) != 0 {
		t.Errorf("expected no genres without infobox, got %v", meta.Genres)
	}
	if len(meta.Members) != 0 {
		t.Errorf("expected no members without infobox, got %v", meta.Members)
	}
}

func TestGetArtist_WikitextFetchFailure(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Some_Artist")
	defer sparqlSrv.Close()

	// Action server returns extract but 500 for wikitext parse.
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		switch action {
		case "query":
			resp := extractResponse{}
			resp.Query.Pages = map[string]extractPage{
				"1": {PageID: 1, Title: "Some Artist", Extract: "Some Artist is a musician."},
			}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "parse":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist should succeed even if wikitext fails: %v", err)
	}
	if meta.Biography != "Some Artist is a musician." {
		t.Errorf("Biography = %q, want %q", meta.Biography, "Some Artist is a musician.")
	}
}

func TestGetArtist_QID(t *testing.T) {
	wdSrv := wikidataEntityServer(t, "Q44190", "Radiohead")
	defer wdSrv.Close()

	actionSrv := actionExtractServer(t, "Radiohead",
		"Radiohead are an English rock band.")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, "", wdSrv.URL)

	meta, err := adapter.GetArtist(context.Background(), "Q44190")
	if err != nil {
		t.Fatalf("GetArtist with Q-ID: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", meta.Name)
	}
}

func TestGetArtist_QID_NotFound(t *testing.T) {
	// Wikidata entity API returns an entity without enwiki sitelink.
	wdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := wbEntityResponse{
			Entities: map[string]wbEntity{
				"Q99999": {
					Sitelinks: map[string]wbSitelink{},
				},
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer wdSrv.Close()

	adapter := newTestAdapter(t, "", "", wdSrv.URL)

	_, err := adapter.GetArtist(context.Background(), "Q99999")
	if err == nil {
		t.Fatal("expected error for Q-ID without enwiki sitelink")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_ArticleTitle(t *testing.T) {
	actionSrv := actionExtractServer(t, "Radiohead",
		"Radiohead are an English rock band.")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, "", "")

	meta, err := adapter.GetArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("GetArtist with article title: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", meta.Name)
	}
	if meta.ProviderID != "Radiohead" {
		t.Errorf("ProviderID = %q, want Radiohead", meta.ProviderID)
	}
}

func TestTestConnection(t *testing.T) {
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"general": map[string]any{}}}) //nolint:errcheck
	}))
	defer actionSrv.Close()

	sparqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer sparqlSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	if err := adapter.TestConnection(context.Background()); err != nil {
		t.Errorf("TestConnection: %v", err)
	}
}

func TestName(t *testing.T) {
	adapter := New(provider.NewRateLimiterMap(), silentLogger())
	if adapter.Name() != provider.NameWikipedia {
		t.Errorf("Name() = %q, want %q", adapter.Name(), provider.NameWikipedia)
	}
}

func TestRequiresAuth(t *testing.T) {
	adapter := New(provider.NewRateLimiterMap(), silentLogger())
	if adapter.RequiresAuth() {
		t.Error("RequiresAuth() should be false")
	}
}

func TestSupportsNameLookup(t *testing.T) {
	adapter := New(provider.NewRateLimiterMap(), silentLogger())
	if adapter.SupportsNameLookup() {
		t.Error("SupportsNameLookup() should be false")
	}
}

func TestIsQID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"valid Q-ID", "Q44190", true},
		{"lowercase q", "q44190", true},
		{"single digit", "Q1", true},
		{"empty string", "", false},
		{"just Q", "Q", false},
		{"Q with letters", "Q44abc", false},
		{"UUID", "a74b1b7f-71a5-4011-9441-d0b5e4122711", false},
		{"plain text", "Radiohead", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isQID(tt.id)
			if got != tt.want {
				t.Errorf("isQID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "valid lowercase", id: "a74b1b7f-71a5-4011-9441-d0b5e4122711", want: true},
		{name: "valid uppercase", id: "A74B1B7F-71A5-4011-9441-D0B5E4122711", want: true},
		{name: "valid mixed case", id: "a74B1b7F-71a5-4011-9441-d0b5E4122711", want: true},
		{name: "empty string", id: "", want: false},
		{name: "too short", id: "a74b1b7f-71a5-4011-9441", want: false},
		{name: "too long", id: "a74b1b7f-71a5-4011-9441-d0b5e41227110", want: false},
		{name: "missing hyphens", id: "a74b1b7f71a540119441d0b5e4122711xxxx", want: false},
		{name: "wrong hyphen positions", id: "a74b1b7f071a5-4011-9441-d0b5e412271", want: false},
		{name: "non-hex characters", id: "g74b1b7f-71a5-4011-9441-d0b5e4122711", want: false},
		{name: "plain artist name", id: "Radiohead", want: false},
		{name: "numeric ID", id: "12345", want: false},
		{name: "36 chars but not UUID", id: "not-a-uuid-at-all-but-right-length!!", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.IsUUID(tt.id)
			if got != tt.want {
				t.Errorf("provider.IsUUID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestGetArtist_TitleWithUnderscore(t *testing.T) {
	actionSrv := actionExtractServer(t, "Some_Band",
		"Some Band is a band.")
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, "", "")

	meta, err := adapter.GetArtist(context.Background(), "Some_Band")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	// Underscores in titles should be replaced with spaces.
	if strings.Contains(meta.Name, "_") {
		t.Errorf("Name = %q, should not contain underscores", meta.Name)
	}
}

func TestTestConnection_ActionAPIFailure(t *testing.T) {
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer actionSrv.Close()

	sparqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer sparqlSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	err := adapter.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected error when Action API returns 500")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestTestConnection_SPARQLFailure(t *testing.T) {
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"general": map[string]any{}}}) //nolint:errcheck
	}))
	defer actionSrv.Close()

	sparqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sparqlSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	err := adapter.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected error when SPARQL returns 500")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestGetArtist_QID_ServerError(t *testing.T) {
	wdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer wdSrv.Close()

	adapter := newTestAdapter(t, "", "", wdSrv.URL)

	_, err := adapter.GetArtist(context.Background(), "Q44190")
	if err == nil {
		t.Fatal("expected error when Wikidata API returns 500")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestGetArtist_EmptyPagesMap(t *testing.T) {
	sparqlSrv := sparqlServerReturning(t, "https://en.wikipedia.org/wiki/Ghost_Article")
	defer sparqlSrv.Close()

	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty pages map.
		resp := extractResponse{}
		resp.Query.Pages = map[string]extractPage{}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, "")

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for empty pages map")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestSearchArtist_ReturnsNil(t *testing.T) {
	adapter := New(provider.NewRateLimiterMap(), silentLogger())
	results, err := adapter.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Errorf("SearchArtist: unexpected error %v", err)
	}
	if results != nil {
		t.Errorf("SearchArtist: expected nil, got %v", results)
	}
}

func TestGetImages_ReturnsNil(t *testing.T) {
	adapter := New(provider.NewRateLimiterMap(), silentLogger())
	results, err := adapter.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Errorf("GetImages: unexpected error %v", err)
	}
	if results != nil {
		t.Errorf("GetImages: expected nil, got %v", results)
	}
}

// isErrNotFound checks if err (or any wrapped error) is an *provider.ErrNotFound.
func isErrNotFound(err error, target **provider.ErrNotFound) bool {
	return errors.As(err, target)
}

// isErrUnavailable checks if err (or any wrapped error) is an *provider.ErrProviderUnavailable.
func isErrUnavailable(err error, target **provider.ErrProviderUnavailable) bool {
	return errors.As(err, target)
}
