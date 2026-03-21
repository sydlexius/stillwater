package rule

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

// TestBulkExecutor_SaveBestImage_PlatformNaming verifies that saveBestImage
// uses the active platform profile's filename conventions rather than the
// hardcoded defaults when writing images to disk.
//
// Kodi naming conventions (from CLAUDE.md):
//
//	thumb=folder.jpg, fanart=fanart.jpg, logo=logo.png, banner=banner.jpg
//
// The default naming for "thumb" is also folder.jpg, so this test uses "fanart"
// to exercise a case where the primary name is unambiguously platform-specific.
// Kodi's primary fanart name is "fanart.jpg", which also matches the default
// ("fanart.jpg"), so we use a custom profile with a distinctive primary name
// ("backdrop.jpg") to make the assertion unambiguous.
func TestBulkExecutor_SaveBestImage_PlatformNaming(t *testing.T) {
	// Serve a valid JPEG from a local HTTP test server.
	testJPEG := makeTestJPEG(t, 800, 600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testJPEG)
	}))
	defer srv.Close()

	db := setupTestDB(t)
	ctx := context.Background()

	// Create a platform service and insert a custom profile whose fanart primary
	// name is "backdrop.jpg" -- this differs from the default "fanart.jpg" and
	// makes it straightforward to assert that the platform profile was used.
	platformSvc := platform.NewService(db)
	customProfile := &platform.Profile{
		Name:       "test-kodi-naming",
		NFOEnabled: false,
		NFOFormat:  "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb:  []string{"folder.jpg"},
			Fanart: []string{"backdrop.jpg"},
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
		IsActive: true,
	}
	if err := platformSvc.Create(ctx, customProfile); err != nil {
		t.Fatalf("creating platform profile: %v", err)
	}
	if err := platformSvc.SetActive(ctx, customProfile.ID); err != nil {
		t.Fatalf("setting profile active: %v", err)
	}

	// Confirm the active profile is now our custom one.
	active, err := platformSvc.GetActive(ctx)
	if err != nil || active == nil || active.ID != customProfile.ID {
		t.Fatalf("expected custom profile to be active; got %v (err %v)", active, err)
	}

	artistDir := t.TempDir()
	a := &artist.Artist{
		ID:            "test-bulk-artist",
		Name:          "Bulk Test Artist",
		SortName:      "Bulk Test Artist",
		MusicBrainzID: "fake-mbid-bulk",
		Path:          artistDir,
	}

	// Build a minimal FetchResult with a single fanart candidate pointing to
	// our HTTP test server.
	fetchResult := &provider.FetchResult{
		Images: []provider.ImageResult{
			{
				URL:    srv.URL + "/backdrop.jpg",
				Type:   provider.ImageFanart,
				Width:  800,
				Height: 600,
				Source: "test",
			},
		},
	}

	// Construct a BulkExecutor with only the dependencies saveBestImage needs.
	// bulkService, artistService, orchestrator, pipeline, and snapshotService
	// are all nil because saveBestImage does not call them directly.
	executor := &BulkExecutor{
		platformService: platformSvc,
		logger:          testLogger(),
	}

	saved := executor.saveBestImage(ctx, a, "fanart", fetchResult)
	if !saved {
		t.Fatal("saveBestImage returned false; expected the image to be saved successfully")
	}

	// The platform profile specifies "backdrop.jpg" as the primary fanart name.
	// Verify that this file was written to the artist directory.
	expectedPath := filepath.Join(artistDir, "backdrop.jpg")
	if _, err := os.Stat(expectedPath); err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("expected file %q to exist (platform-aware naming), but it was not found", expectedPath)
		}
		t.Fatalf("failed to stat expected file %q: %v", expectedPath, err)
	}

	// Also verify the default fanart name ("fanart.jpg") was NOT written,
	// confirming the platform profile was used instead of hardcoded defaults.
	defaultPath := filepath.Join(artistDir, "fanart.jpg")
	if _, err := os.Stat(defaultPath); err == nil {
		t.Errorf("unexpected file %q exists; expected platform-specific %q instead", defaultPath, expectedPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("failed to stat %q: %v", defaultPath, err)
	}

	// Confirm the artist's in-memory FanartExists flag was set.
	if !a.FanartExists {
		t.Error("a.FanartExists should be true after saveBestImage succeeds")
	}
}
