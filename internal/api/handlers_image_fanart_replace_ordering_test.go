package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// TestHandleImageCrop_FanartReplace_DoesNotAppendOnPlatform is the #3125 C1
// regression test: it drives the handler's REAL save-then-sync ordering --
// finalizeImageSave records provenance from the JUST-WRITTEN file (line
// ~171 of handlers_image.go, inside updateArtistImageFlag) BEFORE it calls
// SyncImageToPlatforms (line ~257) -- and proves the platform sync still
// resolves an in-place replace rather than falling back to append.
//
// This is the ordering a round-1 fixture COULD NOT exercise: a fixture that
// hand-seeds "the previous primary's hash" into a fake ArtistService
// encodes what the author believes finalizeImageSave does, not what it
// actually does. Driving the real handler removes that gap -- the backup
// this test relies on is written by the SAME saveFanartSlotProtected call
// the crop handler always makes, at the SAME point in the real request
// flow, and the provenance-recording step that comes after it (and BEFORE
// sync) is exercised exactly as production runs it.
//
// Must fail before the C1 fix: reverting previousFanartPrimaryData to read
// artist_images.PHash (the round-1 design) makes this go red, because that
// column is stamped from the NEW file by this same request, so the
// resolver's previous-primary branch becomes unreachable and every replace
// silently appends -- see the round-2 report for the paste of that run.
func TestHandleImageCrop_FanartReplace_DoesNotAppendOnPlatform(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var posts []string
	backdrops := map[int][]byte{} // in-memory fake Emby backdrop store
	nextIndex := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/Images/Backdrop/"):
			posts = append(posts, req.URL.Path)
			// Parse the trailing index and REPLACE in place, mirroring
			// Emby's measured semantics (the #3125 issue's wire capture).
			var idx int
			if _, err := fmt.Sscanf(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], "%d", &idx); err == nil {
				if raw, err := io.ReadAll(req.Body); err == nil {
					if decoded, decErr := base64.StdEncoding.DecodeString(string(raw)); decErr == nil {
						backdrops[idx] = decoded
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/Images/Backdrop"):
			// Non-indexed: APPEND at the next index.
			posts = append(posts, req.URL.Path)
			if raw, err := io.ReadAll(req.Body); err == nil {
				if decoded, decErr := base64.StdEncoding.DecodeString(string(raw)); decErr == nil {
					backdrops[nextIndex] = decoded
				}
			}
			nextIndex++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(req.URL.Path, "/Users/"):
			// GetArtistDetail: report the CURRENT backdrop count.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"BackdropImageTags":[%s]}`, tagsFor(len(backdrops)))
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/Images/Backdrop/"):
			// GetArtistBackdrop: return the stored bytes for this index, so
			// the resolver's content-hash comparison sees the REAL current
			// state rather than an empty body (which would hash to a fixed
			// value and could spuriously "match" every request).
			var idx int
			_, _ = fmt.Sscanf(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], "%d", &idx)
			data, ok := backdrops[idx]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Replace Ordering Artist", SortName: "Crop Replace Ordering Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	conn := &connection.Connection{
		ID:      "conn-emby",
		Name:    "Emby",
		Type:    connection.TypeEmby,
		URL:     srv.URL,
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
		Emby:    &connection.EmbyConfig{PlatformUserID: "test-user-1"},
	}
	if err := r.connectionService.Create(context.Background(), conn); err != nil {
		t.Fatalf("creating test connection: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Seed the ORIGINAL primary on disk AND on the fake platform, exactly
	// as if a prior successful sync had already run.
	original := jpegBytes(t, 1920, 1080)
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), original, 0o644); err != nil {
		t.Fatalf("seeding original primary: %v", err)
	}
	mu.Lock()
	backdrops[0] = original
	nextIndex = 1
	mu.Unlock()
	r.updateArtistImageFlag(context.Background(), a, "fanart")

	// PRECONDITION: exactly one backdrop before the replace.
	mu.Lock()
	preCount := len(backdrops)
	mu.Unlock()
	if preCount != 1 {
		t.Fatalf("precondition failed: platform backdrop count = %d, want 1", preCount)
	}

	// Now perform the crop-replace through the REAL handler: recrop the
	// primary (no "append"), the exact request shape that drove the
	// #3125 defect.
	var imgBuf bytes.Buffer
	if err := jpeg.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatalf("encoding crop input: %v", err)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(imgBuf.Bytes()),
		"type":       "fanart",
		"x":          0, "y": 0, "width": 1280, "height": 720,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleImageCrop(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if warnings, _ := resp["sync_warnings"].([]any); len(warnings) != 0 {
		t.Fatalf("sync_warnings = %v, want none", warnings)
	}

	// THE C1 ASSERTION: the platform backdrop count must be UNCHANGED (an
	// in-place replace), not incremented (an append). Before the C1 fix
	// this reads 2: the DB-sourced previous-primary hash always equals the
	// NEW file's hash (both are read from the same just-written file, by
	// the same request), so the resolver's previous-primary branch was
	// unreachable and this fell back to append every time.
	mu.Lock()
	postCount := len(backdrops)
	postPrimary := backdrops[0]
	mu.Unlock()
	if postCount != 1 {
		t.Errorf("platform backdrop count after replace = %d, want 1 (in-place replace) -- an append means the C1 fix did not resolve the previous primary's true bytes", postCount)
	}
	if sha256Hex(postPrimary) == sha256Hex(original) {
		t.Error("backdrop 0 still holds the ORIGINAL bytes after a replace -- the write never reached the primary slot")
	}
}

func tagsFor(n int) string {
	tags := make([]string, n)
	for i := range tags {
		tags[i] = fmt.Sprintf("%q", fmt.Sprintf("tag%d", i))
	}
	return strings.Join(tags, ",")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
