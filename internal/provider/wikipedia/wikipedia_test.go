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
	return sparqlServerReturningWithItem(t, articleURL, "")
}

// sparqlServerReturningWithItem returns a SPARQL stub that includes both the
// article URL and the Wikidata item URI (used to derive the Q-ID).
func sparqlServerReturningWithItem(t *testing.T, articleURL, itemURI string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build the response using a raw JSON literal so the test does not
		// depend on the exact anonymous struct shape of sparqlResponse.
		type binding struct {
			Item    map[string]string `json:"item,omitempty"`
			Article map[string]string `json:"article,omitempty"`
		}
		var bindings []binding
		if articleURL != "" {
			b := binding{Article: map[string]string{"value": articleURL}}
			if itemURI != "" {
				b.Item = map[string]string{"value": itemURI}
			}
			bindings = append(bindings, b)
		}
		payload := map[string]any{
			"results": map[string]any{"bindings": bindings},
		}
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(payload) //nolint:errcheck
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
	// Action server returns 500 to simulate a deterministic failure.
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer actionSrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, "", "")

	_, err := adapter.GetArtist(context.Background(), "Radiohead")
	if err == nil {
		t.Fatal("expected error when action server returns 500")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
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

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestTestConnection(t *testing.T) {
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"general": map[string]any{}}}) //nolint:errcheck
	}))
	defer actionSrv.Close()

	sparqlSrv := okServer(t)
	defer sparqlSrv.Close()

	entitySrv := okServer(t)
	defer entitySrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, entitySrv.URL)

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

func TestTestConnection_EntityAPIFailure(t *testing.T) {
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"general": map[string]any{}}}) //nolint:errcheck
	}))
	defer actionSrv.Close()

	sparqlSrv := okServer(t)
	defer sparqlSrv.Close()

	entitySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer entitySrv.Close()

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, entitySrv.URL)

	err := adapter.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected error when entity API returns 500")
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

// --- Language walk tests (issue #967) ---

// langWalkFixture wires up a mock Wikidata entity API + one mock action API
// per language so the GetArtist language-walk logic can be tested end-to-end.
type langWalkFixture struct {
	sparql  *httptest.Server
	entity  *httptest.Server
	actions map[string]*httptest.Server // keyed by lang code
}

func (f *langWalkFixture) close() {
	if f.sparql != nil {
		f.sparql.Close()
	}
	if f.entity != nil {
		f.entity.Close()
	}
	for _, s := range f.actions {
		s.Close()
	}
}

// newLangWalkAdapter builds an adapter whose actionEndpointForLang rewrites to
// per-language mock action servers. Because the real actionEndpointForLang only
// rewrites when the adapter was constructed with the default production
// endpoint, we install a custom helper via a small subclass approach: we make
// the default action server route requests based on the Host header the test
// sets. Simpler: point the adapter's actionEndpoint at a dispatcher that
// chooses the backend using a `lang` query parameter we inject.
//
// We take the simpler route of invoking the adapter with the language-prefix
// URL directly by teaching actionEndpointForLang via a per-test override: we
// install the dispatcher as actionEndpoint and give each mock server a path
// prefix that identifies the lang. The dispatcher reads the request Host or
// path and proxies accordingly.
//
// In practice, the easiest approach is to construct per-language action
// endpoints with distinct URLs and rely on the custom-endpoint branch of
// actionEndpointForLang returning a single URL. To allow multiple backends
// while using the default branch, we instead start one httptest server whose
// handler dispatches based on a "titles" query containing a lang marker.
//
// To keep tests readable we simply run one dispatcher server whose handler
// looks at an X-Test-Lang header. Since adapter code does not set such a
// header, we encode the lang into the title by prefixing it ("ja:Title")
// only in the sitelink response, so the dispatcher can route by title prefix.
// That works because Wikidata sitelinks are whatever Wikidata says they are,
// and the adapter passes them verbatim to the action API.
func buildLangWalkAdapter(t *testing.T, perLangExtracts map[string]string, enTitle, qid string) (*Adapter, *langWalkFixture) {
	t.Helper()
	// One dispatcher action server that returns extract based on title prefix.
	actionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		title := r.URL.Query().Get("titles")
		if action == "parse" {
			title = r.URL.Query().Get("page")
		}
		// Determine lang by title prefix "lang::" or default to en for bare enTitle.
		lang := "en"
		articleTitle := title
		if idx := strings.Index(title, "::"); idx > 0 {
			lang = title[:idx]
			articleTitle = title[idx+2:]
		}
		switch action {
		case "query":
			extract := perLangExtracts[lang]
			resp := extractResponse{}
			if extract == "__notfound__" {
				resp.Query.Pages = map[string]extractPage{"-1": {}}
			} else {
				resp.Query.Pages = map[string]extractPage{
					"42": {PageID: 42, Title: articleTitle, Extract: extract},
				}
			}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "parse":
			// Return empty wikitext.
			resp := parseResponse{}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))

	// Wikidata entity server: return sitelinks keyed by "{lang}wiki".
	entitySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := r.URL.Query().Get("sitefilter")
		resp := wbEntityResponse{Entities: map[string]wbEntity{
			qid: {Sitelinks: map[string]wbSitelink{}},
		}}
		// Only include the sitelink if a lang-specific extract is configured.
		if strings.HasSuffix(site, "wiki") && site != "" {
			lang := strings.TrimSuffix(site, "wiki")
			if _, ok := perLangExtracts[lang]; ok {
				resp.Entities[qid].Sitelinks[site] = wbSitelink{Title: lang + "::LocalTitle"}
			}
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))

	sparqlSrv := sparqlServerReturningWithItem(t,
		"https://en.wikipedia.org/wiki/"+enTitle,
		"https://www.wikidata.org/entity/"+qid)

	adapter := newTestAdapter(t, actionSrv.URL, sparqlSrv.URL, entitySrv.URL)
	return adapter, &langWalkFixture{
		sparql: sparqlSrv,
		entity: entitySrv,
		actions: map[string]*httptest.Server{
			"dispatcher": actionSrv,
		},
	}
}

func TestGetArtist_LangWalk(t *testing.T) {
	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"

	tests := []struct {
		name          string
		prefs         []string
		extracts      map[string]string // lang -> extract (or "__notfound__" for -1 page)
		wantBioPrefix string
		wantErr       bool
	}{
		{
			name:          "hit on first preference",
			prefs:         []string{"ja", "de", "en"},
			extracts:      map[string]string{"ja": "Japanese biography.", "en": "English biography."},
			wantBioPrefix: "Japanese biography",
		},
		{
			name:          "fall through to second preference",
			prefs:         []string{"ja", "de", "en"},
			extracts:      map[string]string{"de": "German biography.", "en": "English biography."},
			wantBioPrefix: "German biography",
		},
		{
			name:          "fall through to English fallback",
			prefs:         []string{"ja", "de"},
			extracts:      map[string]string{"en": "English biography."},
			wantBioPrefix: "English biography",
		},
		{
			name:     "all languages miss",
			prefs:    []string{"ja", "de"},
			extracts: map[string]string{"en": "__notfound__"},
			wantErr:  true,
		},
		{
			name:          "no prefs uses English only",
			prefs:         nil,
			extracts:      map[string]string{"en": "English biography."},
			wantBioPrefix: "English biography",
		},
		{
			name:          "duplicate and empty prefs are normalized",
			prefs:         []string{"ja-JP", "ja", "", "en-US"},
			extracts:      map[string]string{"ja": "Japanese biography.", "en": "English biography."},
			wantBioPrefix: "Japanese biography",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, fx := buildLangWalkAdapter(t, tt.extracts, "Test_Artist", "Q12345")
			defer fx.close()

			ctx := context.Background()
			if tt.prefs != nil {
				ctx = provider.WithMetadataLanguages(ctx, tt.prefs)
			}
			meta, err := adapter.GetArtist(ctx, mbid)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got meta=%+v", meta)
				}
				var notFound *provider.ErrNotFound
				if !errors.As(err, &notFound) {
					t.Errorf("expected ErrNotFound after walking all languages, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetArtist: %v", err)
			}
			if !strings.HasPrefix(meta.Biography, tt.wantBioPrefix) {
				t.Errorf("Biography = %q, want prefix %q", meta.Biography, tt.wantBioPrefix)
			}
		})
	}
}

// TestGetArtist_LangWalk_RateLimitedContext verifies that a canceled context
// mid-walk surfaces as an ErrProviderUnavailable wrapping the cancellation
// error (the rate limiter returns ctx.Err()).
func TestGetArtist_LangWalk_RateLimitedContext(t *testing.T) {
	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	adapter, fx := buildLangWalkAdapter(t,
		map[string]string{"en": "English biography."},
		"Test_Artist", "Q12345")
	defer fx.close()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = provider.WithMetadataLanguages(ctx, []string{"ja", "de", "en"})
	cancel() // pre-cancel; the first limiter.Wait will return immediately.

	_, err := adapter.GetArtist(ctx, mbid)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	// Either provider.ErrProviderUnavailable (wrapping ctx.Err) or ctx.Err itself is acceptable.
	if !errors.Is(err, context.Canceled) {
		var unavail *provider.ErrProviderUnavailable
		if !errors.As(err, &unavail) {
			t.Errorf("expected context.Canceled or ErrProviderUnavailable, got %T: %v", err, err)
		}
	}
}

func TestOrderedLanguages(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil prefs yields en only", nil, []string{"en"}},
		{"single non-en adds en fallback", []string{"ja"}, []string{"ja", "en"}},
		{"explicit en stays sole", []string{"en"}, []string{"en"}},
		{"prefs preserved in order", []string{"ja", "de", "fr"}, []string{"ja", "de", "fr", "en"}},
		{"duplicates removed", []string{"ja", "ja", "de"}, []string{"ja", "de", "en"}},
		{"locale tags trimmed", []string{"ja-JP", "de-DE"}, []string{"ja", "de", "en"}},
		{"invalid entries skipped", []string{"zzzz", "x", "ja"}, []string{"ja", "en"}},
		{"whitespace trimmed", []string{"  ja ", "de"}, []string{"ja", "de", "en"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := orderedLanguages(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("orderedLanguages(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("orderedLanguages(%v) = %v, want %v", tt.in, got, tt.want)
				}
			}
		})
	}
}

func TestExtractQID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"http://www.wikidata.org/entity/Q44190", "Q44190"},
		{"https://www.wikidata.org/entity/Q1", "Q1"},
		{"https://www.wikidata.org/entity/q7", "Q7"},
		{"", ""},
		{"https://www.wikidata.org/entity/", ""},
		{"https://www.wikidata.org/entity/NotAQID", ""},
	}
	for _, tt := range tests {
		if got := extractQID(tt.in); got != tt.want {
			t.Errorf("extractQID(%q) = %q, want %q", tt.in, got, tt.want)
		}
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
