package audiodb

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

func setupTest(t *testing.T) (*provider.RateLimiterMap, *provider.SettingsService) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`)
	if err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, _ := encryption.NewEncryptor("")
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)
	if err := settings.SetAPIKey(context.Background(), provider.NameAudioDB, "test-premium-key"); err != nil {
		t.Fatalf("setting test key: %v", err)
	}
	return limiter, settings
}

func setupFreeTest(t *testing.T) (*provider.RateLimiterMap, *provider.SettingsService) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`)
	if err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, _ := encryption.NewEncryptor("")
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)
	// No API key stored -- adapter should use free key 123.
	return limiter, settings
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loading fixture %s: %v", name, err)
	}
	return data
}

// newTestServer creates a test server that handles both v1 and v2 path patterns.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerCapturing(t, nil, nil)
}

// newTestServerCapturing creates a test server that also records the API key header and request path.
func newTestServerCapturing(t *testing.T, capturedKey *string, capturedPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if capturedKey != nil {
			*capturedKey = r.Header.Get("X-API-KEY")
		}
		if capturedPath != nil {
			*capturedPath = r.URL.Path
		}

		switch {
		// v2 paths
		case strings.Contains(r.URL.Path, "/search/artist/"):
			// v2 search response uses the "search" top-level key
			v2Data := []byte(strings.Replace(string(loadFixture(t, "search_radiohead.json")), `"artists"`, `"search"`, 1))
			w.Write(v2Data)
		case strings.Contains(r.URL.Path, "/lookup/artist_mb/not-found"):
			w.Write([]byte(`{"lookup":null}`))
		case strings.Contains(r.URL.Path, "/lookup/artist_mb/"):
			w.Write(loadFixture(t, "lookup_radiohead.json"))
		case strings.Contains(r.URL.Path, "/lookup/artist/"):
			// v2 direct numeric ID lookup
			w.Write(loadFixture(t, "lookup_radiohead.json"))
		// v1 paths
		case strings.Contains(r.URL.Path, "/search.php"):
			w.Write(loadFixture(t, "search_radiohead.json"))
		case strings.Contains(r.URL.Path, "/artist.php"):
			// v1 direct numeric ID lookup
			w.Write(loadFixture(t, "search_radiohead.json"))
		case strings.Contains(r.URL.Path, "/artist-mb.php"):
			if r.URL.Query().Get("i") == "not-found" {
				w.Write([]byte(`{"artists":null}`))
			} else {
				w.Write(loadFixture(t, "search_radiohead.json"))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestSearchArtist(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	results, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", results[0].Name)
	}
	if results[0].ProviderID != "111239" {
		t.Errorf("expected provider ID 111239, got %s", results[0].ProviderID)
	}
	// Score should be computed via NameSimilarity, not hard-coded to 100.
	// "Radiohead" vs "Radiohead" is an exact match = 100.
	if results[0].Score != 100 {
		t.Errorf("expected score 100 for exact match, got %d", results[0].Score)
	}
}

func TestGetArtist(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	meta, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", meta.Name)
	}
	if meta.AudioDBID != "111239" {
		t.Errorf("expected AudioDB ID 111239, got %s", meta.AudioDBID)
	}
	if meta.Biography == "" {
		t.Error("expected non-empty biography")
	}
	if len(meta.Genres) == 0 {
		t.Error("expected genres")
	}
	if meta.Formed != "1985" {
		t.Errorf("expected formed 1985, got %s", meta.Formed)
	}
}

func TestMapArtist_GroupExcludesBorn(t *testing.T) {
	// When FormedYear is set (indicating a group), BornYear and DiedYear
	// should not be mapped to Born/Died to avoid cross-contamination.
	art := &AudioDBArtist{
		IDArtist:   "12345",
		Artist:     "a-ha",
		FormedYear: "1985",
		BornYear:   "1982",
		DiedYear:   "2010",
		Disbanded:  "2010",
	}
	meta := mapArtist(context.Background(), art)
	if meta.Formed != "1985" {
		t.Errorf("Formed = %q, want 1985", meta.Formed)
	}
	if meta.Born != "" {
		t.Errorf("Born = %q, want empty (group should not have Born)", meta.Born)
	}
	if meta.Died != "" {
		t.Errorf("Died = %q, want empty (group should not have Died)", meta.Died)
	}
	if meta.Disbanded != "2010" {
		t.Errorf("Disbanded = %q, want 2010", meta.Disbanded)
	}
}

func TestMapArtist_PersonGetsBorn(t *testing.T) {
	// When only BornYear is set (no FormedYear), it maps to Born (person).
	art := &AudioDBArtist{
		IDArtist:   "67890",
		Artist:     "Bjork",
		FormedYear: "0",
		BornYear:   "1965",
		DiedYear:   "0",
	}
	meta := mapArtist(context.Background(), art)
	if meta.Born != "1965" {
		t.Errorf("Born = %q, want 1965", meta.Born)
	}
	if meta.Formed != "" {
		t.Errorf("Formed = %q, want empty", meta.Formed)
	}
}

func TestMapArtist_BiographyLocalization(t *testing.T) {
	tests := []struct {
		name      string
		bio       string // strBiography (localized default)
		bioEN     string // strBiographyEN
		langPrefs []string
		wantBio   string
	}{
		{
			name:      "English preference selects BiographyEN",
			bio:       "Biographie auf Deutsch.",
			bioEN:     "English biography.",
			langPrefs: []string{"en"},
			wantBio:   "English biography.",
		},
		{
			name:      "Non-English preference falls back to default bio",
			bio:       "Biographie auf Deutsch.",
			bioEN:     "English biography.",
			langPrefs: []string{"de"},
			wantBio:   "Biographie auf Deutsch.",
		},
		{
			name:      "Empty English bio falls back to default bio",
			bio:       "Biographie par default.",
			bioEN:     "",
			langPrefs: []string{"en"},
			wantBio:   "Biographie par default.",
		},
		{
			name:      "No preferences uses fallback",
			bio:       "Default bio.",
			bioEN:     "English bio.",
			langPrefs: nil,
			wantBio:   "Default bio.",
		},
		{
			name:      "Regional fallback uses base language in preference order",
			bio:       "Biographie par défaut.",
			bioEN:     "English biography.",
			langPrefs: []string{"fr-CA", "en-GB"},
			wantBio:   "English biography.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			art := &AudioDBArtist{
				IDArtist:    "99999",
				Artist:      "Test Artist",
				Biography:   tt.bio,
				BiographyEN: tt.bioEN,
			}
			ctx := context.Background()
			if len(tt.langPrefs) > 0 {
				ctx = provider.WithMetadataLanguages(ctx, tt.langPrefs)
			}
			meta := mapArtist(ctx, art)
			if meta.Biography != tt.wantBio {
				t.Errorf("Biography = %q, want %q", meta.Biography, tt.wantBio)
			}
		})
	}
}

func TestMapArtist_BiographyExtendedLanguages(t *testing.T) {
	// Verify that biography fields beyond EN are selected when the user
	// prefers those languages. AudioDB provides per-language biography
	// fields for a set of fixed languages.
	tests := []struct {
		name      string
		art       AudioDBArtist
		langPrefs []string
		wantBio   string
	}{
		{
			name: "German preference selects BiographyDE",
			art: AudioDBArtist{
				IDArtist:    "1",
				Artist:      "A",
				BiographyDE: "Deutsche Biographie.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"de"},
			wantBio:   "Deutsche Biographie.",
		},
		{
			name: "French preference selects BiographyFR",
			art: AudioDBArtist{
				IDArtist:    "2",
				Artist:      "B",
				BiographyFR: "Biographie en francais.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"fr"},
			wantBio:   "Biographie en francais.",
		},
		{
			name: "Japanese preference selects BiographyJA",
			art: AudioDBArtist{
				IDArtist:    "3",
				Artist:      "C",
				BiographyJA: "日本語バイオグラフィー。",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"ja"},
			wantBio:   "日本語バイオグラフィー。",
		},
		{
			name: "Chinese preference selects BiographyCN",
			art: AudioDBArtist{
				IDArtist:    "4",
				Artist:      "D",
				BiographyCN: "中文简介。",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"zh"},
			wantBio:   "中文简介。",
		},
		{
			name: "Spanish preference selects BiographyES",
			art: AudioDBArtist{
				IDArtist:    "5",
				Artist:      "E",
				BiographyES: "Biografia en espanol.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"es"},
			wantBio:   "Biografia en espanol.",
		},
		{
			name: "Russian preference selects BiographyRU",
			art: AudioDBArtist{
				IDArtist:    "6",
				Artist:      "F",
				BiographyRU: "Биография на русском.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"ru"},
			wantBio:   "Биография на русском.",
		},
		{
			name: "Italian preference selects BiographyIT",
			art: AudioDBArtist{
				IDArtist:    "7",
				Artist:      "G",
				BiographyIT: "Biografia in italiano.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"it"},
			wantBio:   "Biografia in italiano.",
		},
		{
			name: "Portuguese preference selects BiographyPT",
			art: AudioDBArtist{
				IDArtist:    "8",
				Artist:      "H",
				BiographyPT: "Biografia em portuguese.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"pt"},
			wantBio:   "Biografia em portuguese.",
		},
		{
			name: "First preference wins when both available",
			art: AudioDBArtist{
				IDArtist:    "9",
				Artist:      "I",
				BiographyDE: "Deutsche Biographie.",
				BiographyFR: "Biographie en francais.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"fr", "de", "en"},
			wantBio:   "Biographie en francais.",
		},
		{
			name: "Falls back to second preference when first empty",
			art: AudioDBArtist{
				IDArtist:    "10",
				Artist:      "J",
				BiographyDE: "",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"de", "en"},
			wantBio:   "English biography.",
		},
		{
			name: "Regional tag zh-Hant matches zh (BiographyCN)",
			art: AudioDBArtist{
				IDArtist:    "11",
				Artist:      "K",
				BiographyCN: "中文简介。",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"zh-Hant"},
			wantBio:   "中文简介。",
		},
		{
			name: "pt-BR matches pt (BiographyPT)",
			art: AudioDBArtist{
				IDArtist:    "12",
				Artist:      "L",
				BiographyPT: "Biografia em portuguese.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"pt-BR"},
			wantBio:   "Biografia em portuguese.",
		},
		{
			name: "Norwegian preference selects BiographyNO",
			art: AudioDBArtist{
				IDArtist:    "13",
				Artist:      "M",
				BiographyNO: "Norsk biografi.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"no"},
			wantBio:   "Norsk biografi.",
		},
		{
			name: "Swedish preference selects BiographySE",
			art: AudioDBArtist{
				IDArtist:    "14",
				Artist:      "N",
				BiographySE: "Svensk biografi.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"sv"},
			wantBio:   "Svensk biografi.",
		},
		{
			name: "Dutch preference selects BiographyNL",
			art: AudioDBArtist{
				IDArtist:    "15",
				Artist:      "O",
				BiographyNL: "Nederlandse biografie.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"nl"},
			wantBio:   "Nederlandse biografie.",
		},
		{
			name: "Polish preference selects BiographyPL",
			art: AudioDBArtist{
				IDArtist:    "16",
				Artist:      "P",
				BiographyPL: "Polska biografia.",
				BiographyEN: "English biography.",
			},
			langPrefs: []string{"pl"},
			wantBio:   "Polska biografia.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			art := tt.art
			ctx := provider.WithMetadataLanguages(context.Background(), tt.langPrefs)
			meta := mapArtist(ctx, &art)
			if meta.Biography != tt.wantBio {
				t.Errorf("Biography = %q, want %q", meta.Biography, tt.wantBio)
			}
		})
	}
}

func TestGetArtistNotFound(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetArtist(context.Background(), "not-found")
	if err == nil {
		t.Fatal("expected error for not-found")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestGetImages(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	images, err := a.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 6 {
		t.Fatalf("expected 6 images, got %d", len(images))
	}
}

func TestPremiumKeyUsesV2Header(t *testing.T) {
	limiter, settings := setupTest(t)
	var capturedKey, capturedPath string
	srv := newTestServerCapturing(t, &capturedKey, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if capturedKey != "test-premium-key" {
		t.Errorf("expected X-API-KEY header %q, got %q", "test-premium-key", capturedKey)
	}
	if !strings.Contains(capturedPath, "/search/artist/") {
		t.Errorf("expected v2 search path, got %q", capturedPath)
	}
}

func TestFreeKeyUsesV1URL(t *testing.T) {
	limiter, settings := setupFreeTest(t)
	var capturedKey, capturedPath string
	srv := newTestServerCapturing(t, &capturedKey, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if capturedKey != "" {
		t.Errorf("expected no X-API-KEY header for free tier, got %q", capturedKey)
	}
	if !strings.Contains(capturedPath, "/search.php") {
		t.Errorf("expected v1 search.php path, got %q", capturedPath)
	}
}

func TestRequiresAuth(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://unused")

	if a.RequiresAuth() {
		t.Error("expected RequiresAuth to return false (free tier available)")
	}
}

func TestNumericIDRoutesToDirectEndpoint(t *testing.T) {
	limiter, settings := setupTest(t)
	var capturedPath string
	srv := newTestServerCapturing(t, nil, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	// Numeric ID should route to the direct lookup endpoint (artist.php / lookup/artist/)
	_, err := a.GetArtist(context.Background(), "111239")
	if err != nil {
		t.Fatalf("GetArtist with numeric ID: %v", err)
	}
	if !strings.Contains(capturedPath, "/lookup/artist/") {
		t.Errorf("numeric ID should route to /lookup/artist/, got %q", capturedPath)
	}
}

func TestUUIDRoutesToMBIDEndpoint(t *testing.T) {
	limiter, settings := setupTest(t)
	var capturedPath string
	srv := newTestServerCapturing(t, nil, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	// UUID should route to the MBID lookup endpoint (artist-mb.php / lookup/artist_mb/)
	_, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist with UUID: %v", err)
	}
	if !strings.Contains(capturedPath, "/lookup/artist_mb/") {
		t.Errorf("UUID should route to /lookup/artist_mb/, got %q", capturedPath)
	}
}

func TestNumericIDRoutesImages(t *testing.T) {
	limiter, settings := setupTest(t)
	var capturedPath string
	srv := newTestServerCapturing(t, nil, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetImages(context.Background(), "111239")
	if err != nil {
		t.Fatalf("GetImages with numeric ID: %v", err)
	}
	if !strings.Contains(capturedPath, "/lookup/artist/") {
		t.Errorf("numeric ID for images should route to /lookup/artist/, got %q", capturedPath)
	}
}

func TestFreeKeyNumericIDRoutesToV1Direct(t *testing.T) {
	limiter, settings := setupFreeTest(t)
	var capturedPath string
	srv := newTestServerCapturing(t, nil, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetArtist(context.Background(), "111239")
	if err != nil {
		t.Fatalf("GetArtist with numeric ID (free key): %v", err)
	}
	if !strings.Contains(capturedPath, "/artist.php") {
		t.Errorf("numeric ID with free key should route to /artist.php, got %q", capturedPath)
	}
}

func TestIsNumericID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"111239", true},
		{"0", true},
		{"999999999", true},
		{"a74b1b7f-71a5-4011-9441-d0b5e4122711", false},
		{"abc123", false},
		{"", false},
		{"12 34", false},
	}
	for _, tt := range tests {
		if got := isNumericID(tt.id); got != tt.want {
			t.Errorf("isNumericID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}
