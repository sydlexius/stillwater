//go:build integration

package provider_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/audiodb"
	"github.com/sydlexius/stillwater/internal/provider/discogs"
	"github.com/sydlexius/stillwater/internal/provider/lastfm"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
	"github.com/sydlexius/stillwater/internal/provider/wikidata"

	_ "modernc.org/sqlite"
)

// a-ha test constants.
const (
	aHaMBID = "7364dea6-ca9a-48e3-be01-b44ad0d19897"
	aHaName = "a-ha"

	// testTimeout bounds each integration test so network stalls surface quickly.
	testTimeout = 30 * time.Second
)

// setupIntegrationSettings creates an in-memory settings service with API keys
// read from environment variables. Keys are stored in plain text for test use
// (test DB is ephemeral).
func setupIntegrationSettings(t *testing.T) *provider.SettingsService {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(context.Background(),
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`)
	if err != nil {
		t.Fatalf("creating settings table: %v", err)
	}

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	svc := provider.NewSettingsService(db, enc)

	storeKey := func(name provider.ProviderName, envVar string) {
		key := os.Getenv(envVar)
		if key == "" {
			return
		}
		if err := svc.SetAPIKey(context.Background(), name, key); err != nil {
			t.Fatalf("storing %s key: %v", name, err)
		}
	}

	storeKey(provider.NameAudioDB, "AUDIODB_API_KEY")
	storeKey(provider.NameDiscogs, "DISCOGS_TOKEN")
	storeKey(provider.NameLastFM, "LASTFM_API_KEY")

	return svc
}

func newLimiter() *provider.RateLimiterMap {
	return provider.NewRateLimiterMap()
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func TestIntegration_MusicBrainz_AHa(t *testing.T) {
	limiter := newLimiter()
	mb := musicbrainz.New(limiter, silentLogger())

	meta, err := mb.GetArtist(testCtx(t), aHaMBID)
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	if meta.Name == "" {
		t.Error("expected non-empty artist name")
	}
	if meta.MusicBrainzID != aHaMBID {
		t.Errorf("expected MBID %q, got %q", aHaMBID, meta.MusicBrainzID)
	}
	if meta.Formed == "" {
		t.Error("expected non-empty formed year")
	}
	if meta.Type == "" {
		t.Error("expected non-empty type")
	}
}

func TestIntegration_AudioDB_AHa(t *testing.T) {
	settings := setupIntegrationSettings(t)
	has, err := settings.HasAPIKey(context.Background(), provider.NameAudioDB)
	if err != nil {
		t.Fatalf("checking key: %v", err)
	}
	if !has {
		t.Skip("AUDIODB_API_KEY not set")
	}

	limiter := newLimiter()
	adb := audiodb.New(limiter, settings, silentLogger())

	meta, err := adb.GetArtist(testCtx(t), aHaMBID)
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	if meta.AudioDBID == "" {
		t.Error("expected non-empty AudioDB ID")
	}
	if meta.MusicBrainzID != aHaMBID {
		t.Errorf("expected MBID %q, got %q", aHaMBID, meta.MusicBrainzID)
	}
}

func TestIntegration_Discogs_AHa(t *testing.T) {
	settings := setupIntegrationSettings(t)
	has, err := settings.HasAPIKey(context.Background(), provider.NameDiscogs)
	if err != nil {
		t.Fatalf("checking key: %v", err)
	}
	if !has {
		t.Skip("DISCOGS_TOKEN not set")
	}

	limiter := newLimiter()
	dg := discogs.New(limiter, settings, silentLogger())

	ctx := testCtx(t)
	results, err := dg.SearchArtist(ctx, aHaName)
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one search result")
	}

	// Find an a-ha result (case-insensitive name match).
	var discogsID string
	for _, r := range results {
		if r.Name == aHaName || r.Name == "A-ha" {
			discogsID = r.ProviderID
			break
		}
	}
	if discogsID == "" {
		// Fallback: use the first result.
		discogsID = results[0].ProviderID
	}

	meta, err := dg.GetArtist(ctx, discogsID)
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.DiscogsID == "" {
		t.Error("expected non-empty Discogs ID")
	}
}

func TestIntegration_Wikidata_AHa(t *testing.T) {
	limiter := newLimiter()
	wd := wikidata.New(limiter, silentLogger())

	meta, err := wd.GetArtist(testCtx(t), aHaMBID)
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	if meta.WikidataID == "" {
		t.Error("expected non-empty Wikidata Q-item ID")
	}
	// Q-IDs start with "Q".
	if len(meta.WikidataID) < 2 || meta.WikidataID[0] != 'Q' {
		t.Errorf("expected Q-item ID, got %q", meta.WikidataID)
	}
}

func TestIntegration_LastFM_AHa(t *testing.T) {
	settings := setupIntegrationSettings(t)
	has, err := settings.HasAPIKey(context.Background(), provider.NameLastFM)
	if err != nil {
		t.Fatalf("checking key: %v", err)
	}
	if !has {
		t.Skip("LASTFM_API_KEY not set")
	}

	limiter := newLimiter()
	lfm := lastfm.New(limiter, settings, silentLogger())

	meta, err := lfm.GetArtist(testCtx(t), aHaMBID)
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	if meta.Name == "" {
		t.Error("expected non-empty artist name")
	}
	if meta.Biography == "" {
		t.Error("expected non-empty biography")
	}
	if len(meta.Genres) == 0 {
		t.Error("expected at least one tag/genre")
	}
}

func TestIntegration_Orchestrator_AHa(t *testing.T) {
	settings := setupIntegrationSettings(t)
	limiter := newLimiter()
	logger := silentLogger()

	// Register MusicBrainz only (no Discogs) so DiscogsID must come from
	// MusicBrainz URL relations via extractProviderIDsFromURLs.
	registry := provider.NewRegistry()
	registry.Register(musicbrainz.New(limiter, logger))
	registry.Register(wikidata.New(limiter, logger))

	if has, _ := settings.HasAPIKey(context.Background(), provider.NameAudioDB); has {
		registry.Register(audiodb.New(limiter, settings, logger))
	}
	if has, _ := settings.HasAPIKey(context.Background(), provider.NameLastFM); has {
		registry.Register(lastfm.New(limiter, settings, logger))
	}

	orch := provider.NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(testCtx(t), aHaMBID, aHaName)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if result.Metadata == nil {
		t.Fatal("expected non-nil metadata")
	}
	if result.Metadata.Name == "" {
		t.Error("expected non-empty name in merged result")
	}
	if result.Metadata.MusicBrainzID != aHaMBID {
		t.Errorf("expected MBID %q in merged result, got %q", aHaMBID, result.Metadata.MusicBrainzID)
	}
	if result.Metadata.Formed == "" {
		t.Error("expected non-empty formed year in merged result")
	}
	if len(result.Sources) == 0 {
		t.Error("expected at least one field source recorded")
	}
	// DiscogsID must be backfilled from MusicBrainz URL relations since
	// the Discogs provider is not registered in this test.
	if result.Metadata.DiscogsID == "" {
		t.Error("expected DiscogsID backfilled from MusicBrainz URL relations")
	}
	// DeezerID must be backfilled from the MusicBrainz "streaming music" URL relation.
	if result.Metadata.DeezerID == "" {
		t.Error("expected DeezerID backfilled from MusicBrainz URL relations")
	}
}
