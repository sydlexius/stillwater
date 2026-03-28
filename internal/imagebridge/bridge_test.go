package imagebridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// stubPlatformIDProvider returns a fixed set of platform IDs.
type stubPlatformIDProvider struct {
	ids []artist.PlatformID
	err error
}

func (s *stubPlatformIDProvider) GetPlatformIDs(_ context.Context, _ string) ([]artist.PlatformID, error) {
	return s.ids, s.err
}

// setupTestConnService creates a connection.Service backed by an in-memory DB.
func setupTestConnService(t *testing.T) *connection.Service {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	return connection.NewService(db, enc)
}

func TestFetchArtistImage_Success(t *testing.T) {
	// Stand up a fake Emby server that returns image bytes.
	fakeBody := []byte("fake-png-data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/Images/") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fakeBody)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	connSvc := setupTestConnService(t)
	ctx := context.Background()

	// Create a connection pointed at our test server.
	conn := &connection.Connection{
		Name:              "Test Emby",
		Type:              connection.TypeEmby,
		URL:               srv.URL,
		APIKey:            "test-key",
		Enabled:           true,
		FeatureImageWrite: true,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	provider := &stubPlatformIDProvider{
		ids: []artist.PlatformID{
			{
				ArtistID:         "artist-001",
				ConnectionID:     conn.ID,
				PlatformArtistID: "emby-artist-123",
			},
		},
	}

	bridge := New(connSvc, provider, slog.Default())

	data, contentType, err := bridge.FetchArtistImage(ctx, "artist-001", "logo")
	if err != nil {
		t.Fatalf("FetchArtistImage: %v", err)
	}
	if string(data) != string(fakeBody) {
		t.Errorf("data = %q, want %q", string(data), string(fakeBody))
	}
	if contentType != "image/png" {
		t.Errorf("contentType = %q, want image/png", contentType)
	}
}

func TestFetchArtistImage_NoPlatformIDs(t *testing.T) {
	connSvc := setupTestConnService(t)
	provider := &stubPlatformIDProvider{ids: nil}
	bridge := New(connSvc, provider, slog.Default())

	_, _, err := bridge.FetchArtistImage(context.Background(), "artist-001", "logo")
	if err == nil {
		t.Fatal("expected error for artist with no platform IDs")
	}
	if !strings.Contains(err.Error(), "no platform ID mappings") {
		t.Errorf("error = %q, want 'no platform ID mappings'", err.Error())
	}
}

func TestFetchArtistImage_PlatformIDLookupError(t *testing.T) {
	connSvc := setupTestConnService(t)
	provider := &stubPlatformIDProvider{err: fmt.Errorf("db error")}
	bridge := New(connSvc, provider, slog.Default())

	_, _, err := bridge.FetchArtistImage(context.Background(), "artist-001", "logo")
	if err == nil {
		t.Fatal("expected error when platform ID lookup fails")
	}
	if !strings.Contains(err.Error(), "resolving platform IDs") {
		t.Errorf("error = %q, want 'resolving platform IDs'", err.Error())
	}
}

func TestFetchArtistImage_DisabledConnection(t *testing.T) {
	connSvc := setupTestConnService(t)
	ctx := context.Background()

	conn := &connection.Connection{
		Name:    "Disabled Emby",
		Type:    connection.TypeEmby,
		URL:     "http://localhost:9999",
		APIKey:  "test-key",
		Enabled: false,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	provider := &stubPlatformIDProvider{
		ids: []artist.PlatformID{
			{
				ArtistID:         "artist-002",
				ConnectionID:     conn.ID,
				PlatformArtistID: "emby-002",
			},
		},
	}
	bridge := New(connSvc, provider, slog.Default())

	_, _, err := bridge.FetchArtistImage(ctx, "artist-002", "logo")
	if err == nil {
		t.Fatal("expected error for disabled connection")
	}
	if !strings.Contains(err.Error(), "no enabled platform connections") {
		t.Errorf("error = %q, want 'no enabled platform connections'", err.Error())
	}
}

func TestUploadArtistImage_Success(t *testing.T) {
	expectedData := []byte("trimmed-png")
	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/Images/") {
			ct := r.Header.Get("Content-Type")
			if ct != "image/png" {
				t.Errorf("Content-Type = %q, want image/png", ct)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading request body: %v", err)
			}
			received <- body
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	connSvc := setupTestConnService(t)
	ctx := context.Background()

	conn := &connection.Connection{
		Name:              "Upload Emby",
		Type:              connection.TypeEmby,
		URL:               srv.URL,
		APIKey:            "test-key",
		Enabled:           true,
		FeatureImageWrite: true,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	provider := &stubPlatformIDProvider{
		ids: []artist.PlatformID{
			{
				ArtistID:         "artist-001",
				ConnectionID:     conn.ID,
				PlatformArtistID: "emby-001",
			},
		},
	}
	bridge := New(connSvc, provider, slog.Default())

	err := bridge.UploadArtistImage(ctx, "artist-001", "logo", expectedData, "image/png")
	if err != nil {
		t.Fatalf("UploadArtistImage: %v", err)
	}

	select {
	case body := <-received:
		// Emby client base64-encodes image data before POSTing, so decode
		// the received body before comparing to the original bytes.
		decoded, decErr := base64.StdEncoding.DecodeString(string(body))
		if decErr != nil {
			t.Fatalf("decoding base64 body: %v", decErr)
		}
		if string(decoded) != string(expectedData) {
			t.Errorf("uploaded body = %q, want %q", decoded, expectedData)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the upload request within timeout")
	}
}

func TestUploadArtistImage_NoImageWriteFeature(t *testing.T) {
	connSvc := setupTestConnService(t)
	ctx := context.Background()

	// Create with library_import enabled to prevent the "all false -> all true"
	// default, then explicitly disable image_write via UpdateFeatures.
	conn := &connection.Connection{
		Name:                 "No Write",
		Type:                 connection.TypeEmby,
		URL:                  "http://localhost:9999",
		APIKey:               "test-key",
		Enabled:              true,
		FeatureLibraryImport: true,
		FeatureNFOWrite:      false,
		FeatureImageWrite:    true, // will be disabled below
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	// Disable image_write after creation.
	if err := connSvc.UpdateFeatures(ctx, conn.ID, true, false, false); err != nil {
		t.Fatalf("disabling image_write: %v", err)
	}

	provider := &stubPlatformIDProvider{
		ids: []artist.PlatformID{
			{
				ArtistID:         "artist-003",
				ConnectionID:     conn.ID,
				PlatformArtistID: "emby-003",
			},
		},
	}
	bridge := New(connSvc, provider, slog.Default())

	// No connections have image_write enabled. Upload returns an error
	// because the caller needs to know that nothing was uploaded.
	err := bridge.UploadArtistImage(ctx, "artist-003", "logo", []byte("data"), "image/png")
	if err == nil {
		t.Fatal("expected error when no connections have image_write enabled")
	}
	if !strings.Contains(err.Error(), "no enabled platform connections with image_write") {
		t.Errorf("error = %q, want 'no enabled platform connections with image_write'", err.Error())
	}
}

func TestFetchArtistImage_JellyfinConnection(t *testing.T) {
	fakeBody := []byte("jellyfin-logo-data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/Images/") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fakeBody)
			return
		}
		// Return empty JSON array for any other requests.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]interface{}{})
	}))
	defer srv.Close()

	connSvc := setupTestConnService(t)
	ctx := context.Background()

	conn := &connection.Connection{
		Name:              "Test Jellyfin",
		Type:              connection.TypeJellyfin,
		URL:               srv.URL,
		APIKey:            "jf-key",
		Enabled:           true,
		FeatureImageWrite: true,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	provider := &stubPlatformIDProvider{
		ids: []artist.PlatformID{
			{
				ArtistID:         "artist-jf",
				ConnectionID:     conn.ID,
				PlatformArtistID: "jf-artist-456",
			},
		},
	}
	bridge := New(connSvc, provider, slog.Default())

	data, contentType, err := bridge.FetchArtistImage(ctx, "artist-jf", "logo")
	if err != nil {
		t.Fatalf("FetchArtistImage (jellyfin): %v", err)
	}
	if string(data) != string(fakeBody) {
		t.Errorf("data = %q, want %q", string(data), string(fakeBody))
	}
	if contentType != "image/png" {
		t.Errorf("contentType = %q, want image/png", contentType)
	}
}

func TestFetchArtistImage_UnsupportedConnectionType(t *testing.T) {
	connSvc := setupTestConnService(t)
	ctx := context.Background()

	conn := &connection.Connection{
		Name:    "Lidarr",
		Type:    connection.TypeLidarr,
		URL:     "http://localhost:8686",
		APIKey:  "lidarr-key",
		Enabled: true,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	provider := &stubPlatformIDProvider{
		ids: []artist.PlatformID{
			{
				ArtistID:         "artist-lidarr",
				ConnectionID:     conn.ID,
				PlatformArtistID: "lidarr-artist",
			},
		},
	}
	bridge := New(connSvc, provider, slog.Default())

	_, _, err := bridge.FetchArtistImage(ctx, "artist-lidarr", "logo")
	if err == nil {
		t.Fatal("expected error for unsupported connection type")
	}
	if !strings.Contains(err.Error(), "does not support image fetch") {
		t.Errorf("error = %q, want 'does not support image fetch'", err.Error())
	}
}
