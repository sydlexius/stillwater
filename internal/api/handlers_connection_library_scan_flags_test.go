package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/library"
)

// Regression coverage for #2637: a platform library scan must never mutate the
// local-filesystem image registry (artist_images). Before the fix, all three
// scan paths derived the four *Exists flags from the platform's own image
// inventory, assigned them onto the artist, and called artistService.Update,
// which persists through UpsertAll. UpsertAll overwrites exists_flag from the
// incoming record and deletes every row whose slot is absent from it, so a
// platform that lacked artwork the local library had would:
//
//	RED   thumb/logo/banner slot 0 exists_flag -> 0, and because
//	      extractImageMetadata emits no fanart rows at all when FanartExists is
//	      false, the artist's ENTIRE fanart tail (slots 0..N) is deleted.
//	GREEN every local row and flag survives the scan untouched, while the
//	      platform ID mapping is still resolved and stored.
//
// The assertions read artist_images straight from SQLite rather than trusting a
// return value or a counter, and each test asserts the seeded precondition and
// the platform ID mapping so it cannot pass vacuously (an artist the scan never
// matched would also emerge "unmodified").

// imageRow is one artist_images row, reduced to what these tests assert on.
type imageRow struct {
	slotIndex int
	exists    bool
}

// readImageRows returns the artist_images rows of one image type for an artist,
// ordered by slot index.
func readImageRows(t *testing.T, r *Router, artistID, imageType string) []imageRow {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(), `
		SELECT slot_index, exists_flag
		FROM artist_images
		WHERE artist_id = ? AND image_type = ?
		ORDER BY slot_index
	`, artistID, imageType)
	if err != nil {
		t.Fatalf("querying artist_images (%s): %v", imageType, err)
	}
	defer func() { _ = rows.Close() }()

	var out []imageRow
	for rows.Next() {
		var got imageRow
		if err := rows.Scan(&got.slotIndex, &got.exists); err != nil {
			t.Fatalf("scanning artist_images row (%s): %v", imageType, err)
		}
		out = append(out, got)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating artist_images rows (%s): %v", imageType, err)
	}
	return out
}

// assertFullyStockedRegistry asserts the local registry holds exactly the rows
// seedArtistWithLocalArtwork creates: thumb/logo/banner at slot 0 and fanart at
// slots 0..fanartCount-1, every one of them existing. Used both as the seeded
// precondition (so a later identical assertion cannot pass vacuously) and as
// the post-scan expectation.
func assertFullyStockedRegistry(t *testing.T, r *Router, artistID, stage string, fanartCount int) {
	t.Helper()

	for _, imageType := range []string{"thumb", "logo", "banner"} {
		got := readImageRows(t, r, artistID, imageType)
		if len(got) != 1 {
			t.Fatalf("%s: %s rows = %d (%v), want exactly 1", stage, imageType, len(got), got)
		}
		if got[0].slotIndex != 0 {
			t.Fatalf("%s: %s row slot_index = %d, want 0", stage, imageType, got[0].slotIndex)
		}
		if !got[0].exists {
			t.Fatalf("%s: %s slot 0 exists_flag = false, want true "+
				"(the local file is still on disk; the scan must not clear it)", stage, imageType)
		}
	}

	fanart := readImageRows(t, r, artistID, "fanart")
	if len(fanart) != fanartCount {
		t.Fatalf("%s: fanart rows = %d (%v), want %d "+
			"(a platform without backdrops must not truncate the local fanart tail)",
			stage, len(fanart), fanart, fanartCount)
	}
	for i, row := range fanart {
		if row.slotIndex != i {
			t.Fatalf("%s: fanart row %d has slot_index %d, want %d", stage, i, row.slotIndex, i)
		}
		if !row.exists {
			t.Fatalf("%s: fanart slot %d exists_flag = false, want true", stage, row.slotIndex)
		}
	}
}

// seedArtistWithLocalArtwork creates an artist whose local image registry is
// fully stocked: a thumb, a logo, a banner, and fanartCount fanart slots, as if
// folder.jpg, logo.png, banner.jpg and backdrop*.jpg all sit on disk. It
// asserts the seed actually landed, so a test built on it cannot pass because
// the rows were never there.
func seedArtistWithLocalArtwork(t *testing.T, r *Router, lib *library.Library, name, mbid, dir string, fanartCount int) *artist.Artist {
	t.Helper()

	a := &artist.Artist{
		Name:          name,
		SortName:      name,
		MusicBrainzID: mbid,
		LibraryID:     lib.ID,
		Path:          dir,
		ThumbExists:   true,
		FanartExists:  true,
		FanartCount:   fanartCount,
		LogoExists:    true,
		BannerExists:  true,
	}
	if err := r.artistService.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist %q: %v", name, err)
	}
	assertFullyStockedRegistry(t, r, a.ID, "precondition", fanartCount)
	return a
}

// assertPlatformIDStored proves the scan really processed this artist. Without
// it, every "the registry was not modified" assertion below would also pass if
// the scan silently matched nothing at all.
func assertPlatformIDStored(t *testing.T, r *Router, artistID, connectionID, want string) {
	t.Helper()
	got, err := r.artistService.GetPlatformID(context.Background(), artistID, connectionID)
	if err != nil {
		t.Fatalf("GetPlatformID: %v", err)
	}
	if got != want {
		t.Fatalf("platform ID = %q, want %q (the scan did not process this artist, "+
			"so the registry assertions would pass vacuously)", got, want)
	}
}

// artistlessEmbyResponse serves one Emby artist that has no images at all: no
// Primary, no Logo, no Banner, no backdrops.
func embyServerWithoutImages(t *testing.T, name, id, mbid, path string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Artists/AlbumArtists" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"Items":[{
				"Name":%q,
				"SortName":%q,
				"Id":%q,
				"Path":%q,
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":%q},
				"ImageTags":{},
				"BackdropImageTags":[]
			}],
			"TotalRecordCount":1
		}`, name, name, id, path, mbid)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestScanFromEmby_DoesNotClearLocalImageRegistry is the direct #2637
// regression: Emby holds no artwork for an artist whose local library holds a
// full set. Against the unfixed code this cleared thumb/logo/banner and deleted
// all three fanart rows.
func TestScanFromEmby_DoesNotClearLocalImageRegistry(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	srv := embyServerWithoutImages(t, "Portishead", "emby-2637-001", "mbid-2637-emby", artistDir)

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-emby-2637", "Emby Server", "emby")

	lib := &library.Library{
		Name:         "Emby Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       connection.TypeEmby,
		ConnectionID: "conn-emby-2637",
		ExternalID:   "emby-2637-lib",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	const fanartSlots = 3
	a := seedArtistWithLocalArtwork(t, router, lib, "Portishead", "mbid-2637-emby", artistDir, fanartSlots)

	client := emby.NewWithHTTPClient(srv.URL, "key", "", srv.Client(), quietTestLogger())
	matched, err := router.scanFromEmby(ctx, client, lib)
	if err != nil {
		t.Fatalf("scanFromEmby: %v", err)
	}
	if matched != 1 {
		t.Fatalf("matched = %d, want 1", matched)
	}

	assertPlatformIDStored(t, router, a.ID, "conn-emby-2637", "emby-2637-001")
	assertFullyStockedRegistry(t, router, a.ID, "after scan", fanartSlots)

	// The in-memory projection must agree with the rows, so the UI and rule
	// engine do not see a phantom "no artwork" artist either.
	got, err := router.artistService.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if !got.ThumbExists || !got.FanartExists || !got.LogoExists || !got.BannerExists {
		t.Errorf("reloaded flags thumb=%v fanart=%v logo=%v banner=%v, want all true",
			got.ThumbExists, got.FanartExists, got.LogoExists, got.BannerExists)
	}
	if got.FanartCount != fanartSlots {
		t.Errorf("reloaded FanartCount = %d, want %d", got.FanartCount, fanartSlots)
	}
}

// TestScanFromEmby_PrimaryWithoutBackdropsKeepsFanartTail isolates the slot-0
// amplification. Emby reports a Primary image, so the thumb flag alone would
// have compared equal; only the fanart flag flips. That single slot-0 flip was
// enough to make extractImageMetadata emit zero fanart rows and UpsertAll
// delete the whole tail, which is why every artist in the incident came out of
// it holding exactly one fanart row or none.
func TestScanFromEmby_PrimaryWithoutBackdropsKeepsFanartTail(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Artists/AlbumArtists" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Primary, Logo and Banner all present on the platform; only the
		// backdrops are missing.
		fmt.Fprintf(w, `{
			"Items":[{
				"Name":"Tricky",
				"SortName":"Tricky",
				"Id":"emby-2637-002",
				"Path":%q,
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"mbid-2637-slot0"},
				"ImageTags":{"Primary":"tag-p","Logo":"tag-l","Banner":"tag-b"},
				"BackdropImageTags":[]
			}],
			"TotalRecordCount":1
		}`, artistDir)
	}))
	defer srv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-emby-slot0", "Emby Server", "emby")

	lib := &library.Library{
		Name:         "Emby Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       connection.TypeEmby,
		ConnectionID: "conn-emby-slot0",
		ExternalID:   "emby-slot0-lib",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	const fanartSlots = 4
	a := seedArtistWithLocalArtwork(t, router, lib, "Tricky", "mbid-2637-slot0", artistDir, fanartSlots)

	client := emby.NewWithHTTPClient(srv.URL, "key", "", srv.Client(), quietTestLogger())
	if _, err := router.scanFromEmby(ctx, client, lib); err != nil {
		t.Fatalf("scanFromEmby: %v", err)
	}

	assertPlatformIDStored(t, router, a.ID, "conn-emby-slot0", "emby-2637-002")

	// All four fanart slots must survive. Against the unfixed code this came
	// back empty, taking slots 1..3 down with slot 0.
	fanart := readImageRows(t, router, a.ID, "fanart")
	if len(fanart) != fanartSlots {
		t.Fatalf("fanart rows after scan = %d (%v), want %d: one cleared slot-0 flag "+
			"truncated the whole fanart tail", len(fanart), fanart, fanartSlots)
	}
	for i, row := range fanart {
		if row.slotIndex != i || !row.exists {
			t.Errorf("fanart row %d = {slot %d, exists %v}, want {slot %d, exists true}",
				i, row.slotIndex, row.exists, i)
		}
	}
}

// TestScanFromEmby_PlatformImagesDoNotFabricateLocalRows covers the opposite
// direction. Emby holds artwork the local library does not. A monotone
// false->true guard would have written those flags through, inventing
// artist_images rows for files that are not on disk: the serve-image endpoint
// would 404 on them and the rule engine would stop reporting the real gap. The
// platform inventory is not the local inventory in either direction, so the
// scan records neither.
func TestScanFromEmby_PlatformImagesDoNotFabricateLocalRows(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Artists/AlbumArtists" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"Items":[{
				"Name":"Massive Attack",
				"SortName":"Massive Attack",
				"Id":"emby-2637-003",
				"Path":%q,
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"mbid-2637-fabricate"},
				"ImageTags":{"Primary":"tag-p","Logo":"tag-l","Banner":"tag-b"},
				"BackdropImageTags":["hash1","hash2"]
			}],
			"TotalRecordCount":1
		}`, artistDir)
	}))
	defer srv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-emby-fab", "Emby Server", "emby")

	lib := &library.Library{
		Name:         "Emby Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       connection.TypeEmby,
		ConnectionID: "conn-emby-fab",
		ExternalID:   "emby-fab-lib",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Nothing on disk, so nothing in the registry.
	bare := &artist.Artist{
		Name:          "Massive Attack",
		SortName:      "Massive Attack",
		MusicBrainzID: "mbid-2637-fabricate",
		LibraryID:     lib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, bare); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	for _, imageType := range []string{"thumb", "fanart", "logo", "banner"} {
		if rows := readImageRows(t, router, bare.ID, imageType); len(rows) != 0 {
			t.Fatalf("precondition: %s rows = %v, want none", imageType, rows)
		}
	}

	client := emby.NewWithHTTPClient(srv.URL, "key", "", srv.Client(), quietTestLogger())
	if _, err := router.scanFromEmby(ctx, client, lib); err != nil {
		t.Fatalf("scanFromEmby: %v", err)
	}

	assertPlatformIDStored(t, router, bare.ID, "conn-emby-fab", "emby-2637-003")

	for _, imageType := range []string{"thumb", "fanart", "logo", "banner"} {
		if rows := readImageRows(t, router, bare.ID, imageType); len(rows) != 0 {
			t.Errorf("%s rows after scan = %v, want none: the platform's inventory "+
				"must not create local-filesystem rows", imageType, rows)
		}
	}
}

// TestArtistServiceUpdate_StillMovesImageFlagsFalseToTrue pins that #2637 only
// removed the platform-driven write, not the ability to record newly-found
// local artwork. The scanner and the rule fixers reach artist_images through
// exactly this Update path; if the fix had frozen flag updates in general, this
// would fail.
func TestArtistServiceUpdate_StillMovesImageFlagsFalseToTrue(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	artistDir := t.TempDir()

	lib := &library.Library{
		Name:   "Filesystem",
		Path:   filepath.Dir(artistDir),
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	a := &artist.Artist{
		Name:          "Goldfrapp",
		SortName:      "Goldfrapp",
		MusicBrainzID: "mbid-2637-falsetrue",
		LibraryID:     lib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if rows := readImageRows(t, router, a.ID, "thumb"); len(rows) != 0 {
		t.Fatalf("precondition: thumb rows = %v, want none", rows)
	}

	// The local scanner found folder.jpg and two backdrops.
	a.ThumbExists = true
	a.FanartExists = true
	a.FanartCount = 2
	if err := router.artistService.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	thumb := readImageRows(t, router, a.ID, "thumb")
	if len(thumb) != 1 || !thumb[0].exists {
		t.Errorf("thumb rows = %v, want one existing slot-0 row", thumb)
	}
	fanart := readImageRows(t, router, a.ID, "fanart")
	if len(fanart) != 2 {
		t.Fatalf("fanart rows = %v, want 2", fanart)
	}
	for i, row := range fanart {
		if row.slotIndex != i || !row.exists {
			t.Errorf("fanart row %d = {slot %d, exists %v}, want {slot %d, exists true}",
				i, row.slotIndex, row.exists, i)
		}
	}
}

// TestScanFromJellyfin_DoesNotClearLocalImageRegistry mirrors the Emby case;
// scanFromJellyfin carried an identical copy of the destructive block.
func TestScanFromJellyfin_DoesNotClearLocalImageRegistry(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Artists/AlbumArtists" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"Items":[{
				"Name":"Zero 7",
				"SortName":"Zero 7",
				"Id":"jf-2637-001",
				"Path":%q,
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"mbid-2637-jf"},
				"ImageTags":{},
				"BackdropImageTags":[]
			}],
			"TotalRecordCount":1
		}`, artistDir)
	}))
	defer srv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-jf-2637", "Jellyfin Server", "jellyfin")

	lib := &library.Library{
		Name:         "Jellyfin Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       connection.TypeJellyfin,
		ConnectionID: "conn-jf-2637",
		ExternalID:   "jf-2637-lib",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	const fanartSlots = 3
	a := seedArtistWithLocalArtwork(t, router, lib, "Zero 7", "mbid-2637-jf", artistDir, fanartSlots)

	client := jellyfin.NewWithHTTPClient(srv.URL, "key", "", srv.Client(), quietTestLogger())
	matched, err := router.scanFromJellyfin(ctx, client, lib)
	if err != nil {
		t.Fatalf("scanFromJellyfin: %v", err)
	}
	if matched != 1 {
		t.Fatalf("matched = %d, want 1", matched)
	}

	assertPlatformIDStored(t, router, a.ID, "conn-jf-2637", "jf-2637-001")
	assertFullyStockedRegistry(t, router, a.ID, "after scan", fanartSlots)
}

// TestScanFromLidarr_DoesNotClearLocalImageRegistry covers the third copy of
// the block, which derived the same flags from Lidarr's Images[].CoverType.
func TestScanFromLidarr_DoesNotClearLocalImageRegistry(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/artist" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Lidarr knows the artist but carries no images for it.
		_, _ = w.Write([]byte(`[
			{"id":77,"artistName":"Boards of Canada","foreignArtistId":"mbid-2637-lidarr",
			 "path":"/music/Boards of Canada","monitored":true,"metadataProfileId":1,"images":[]}
		]`))
	}))
	defer srv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-lidarr-2637", "Lidarr Server", "lidarr")

	lib := &library.Library{
		Name:         "Lidarr Music",
		Path:         filepath.Dir(artistDir),
		Type:         library.TypeRegular,
		Source:       library.SourceLidarr,
		ConnectionID: "conn-lidarr-2637",
		ExternalID:   "lidarr",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	const fanartSlots = 2
	a := seedArtistWithLocalArtwork(t, router, lib, "Boards of Canada", "mbid-2637-lidarr", artistDir, fanartSlots)

	client := lidarr.NewWithHTTPClient(srv.URL, "key", srv.Client(), quietTestLogger())
	matched, err := router.scanFromLidarr(ctx, client, lib)
	if err != nil {
		t.Fatalf("scanFromLidarr: %v", err)
	}
	if matched != 1 {
		t.Fatalf("matched = %d, want 1", matched)
	}

	assertPlatformIDStored(t, router, a.ID, "conn-lidarr-2637", "77")
	assertFullyStockedRegistry(t, router, a.ID, "after scan", fanartSlots)
}
