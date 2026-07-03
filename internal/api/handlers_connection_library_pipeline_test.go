package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// mockImageDownloader is a directly-controllable imageDownloader for
// exercising the platformImagePipeline's branch behavior without standing up
// an HTTP fixture for every case.
type mockImageDownloader struct {
	imageCalls    int
	backdropCalls int
	imageFn       func(ctx context.Context, artistID, imageType string) ([]byte, string, error)
	backdropFn    func(ctx context.Context, artistID string, index int) ([]byte, string, error)
}

func (m *mockImageDownloader) GetArtistImage(ctx context.Context, artistID, imageType string) ([]byte, string, error) {
	m.imageCalls++
	if m.imageFn != nil {
		return m.imageFn(ctx, artistID, imageType)
	}
	return nil, "", errors.New("mockImageDownloader: GetArtistImage not configured")
}

func (m *mockImageDownloader) GetArtistBackdrop(ctx context.Context, artistID string, index int) ([]byte, string, error) {
	m.backdropCalls++
	if m.backdropFn != nil {
		return m.backdropFn(ctx, artistID, index)
	}
	return nil, "", errors.New("mockImageDownloader: GetArtistBackdrop not configured")
}

// TestNewPlatformImagePipeline_NoDirSkips covers the branch where the artist
// has neither a filesystem path nor a usable image cache directory: the
// pipeline cannot be constructed and the caller must skip the download.
func TestNewPlatformImagePipeline_NoDirSkips(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	r.imageCacheDir = "" // no cache dir configured

	a := &artist.Artist{ID: "artist-1", Name: "No Dir", Path: ""}
	p, ok := newPlatformImagePipeline(r, &mockImageDownloader{}, "platform-1", a, "emby", &populateResult{})

	if ok {
		t.Fatal("expected ok=false when the artist has no path and no cache dir")
	}
	if p != nil {
		t.Fatal("expected a nil pipeline when ok=false")
	}
}

// TestNewPlatformImagePipeline_MkdirAllFails covers the cache-directory
// creation error branch: the configured cache dir resolves to a path whose
// parent component is a regular file, so MkdirAll cannot create it.
func TestNewPlatformImagePipeline_MkdirAllFails(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	notADir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating file fixture: %v", err)
	}
	r.imageCacheDir = notADir

	a := &artist.Artist{ID: "artist-2", Name: "Bad Cache", Path: ""}
	p, ok := newPlatformImagePipeline(r, &mockImageDownloader{}, "platform-2", a, "emby", &populateResult{})

	if ok {
		t.Fatal("expected ok=false when MkdirAll fails")
	}
	if p != nil {
		t.Fatal("expected a nil pipeline when ok=false")
	}
}

// TestNewPlatformImagePipeline_PathNotAccessible covers the branch where the
// artist has a filesystem path that does not exist (e.g. removed since the
// last scan): the pipeline must not be created.
func TestNewPlatformImagePipeline_PathNotAccessible(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	a := &artist.Artist{ID: "artist-3", Name: "Missing Path", Path: missing}
	p, ok := newPlatformImagePipeline(r, &mockImageDownloader{}, "platform-3", a, "emby", &populateResult{})

	if ok {
		t.Fatal("expected ok=false when the artist path does not exist")
	}
	if p != nil {
		t.Fatal("expected a nil pipeline when ok=false")
	}
}

// TestNewPlatformImagePipeline_PathNotADirectory covers the branch where the
// artist's filesystem path resolves to a regular file rather than a
// directory.
func TestNewPlatformImagePipeline_PathNotADirectory(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	file := filepath.Join(t.TempDir(), "artist.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating file fixture: %v", err)
	}
	a := &artist.Artist{ID: "artist-4", Name: "File Path", Path: file}
	p, ok := newPlatformImagePipeline(r, &mockImageDownloader{}, "platform-4", a, "emby", &populateResult{})

	if ok {
		t.Fatal("expected ok=false when the artist path is a regular file")
	}
	if p != nil {
		t.Fatal("expected a nil pipeline when ok=false")
	}
}

// TestDownloadPlatformImages_NoDirEarlyReturn covers downloadPlatformImages'
// early return when the pipeline cannot be constructed: no downloader calls
// should happen and result.Images must stay zero.
func TestDownloadPlatformImages_NoDirEarlyReturn(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	r.imageCacheDir = ""

	dl := &mockImageDownloader{}
	a := &artist.Artist{ID: "artist-5", Name: "No Dir", Path: ""}
	var result populateResult

	r.downloadPlatformImages(context.Background(), dl, "platform-5",
		map[string]string{"Primary": "hash"}, []string{"hash1"}, a, "emby", &result)

	if dl.imageCalls != 0 || dl.backdropCalls != 0 {
		t.Errorf("expected no downloader calls, got imageCalls=%d backdropCalls=%d", dl.imageCalls, dl.backdropCalls)
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0", result.Images)
	}
}

// TestDownloadNamedImages_SkipsEmptyAndUnknownTags covers both continue
// branches in downloadNamedImages: an empty tag value and a platform key
// with no known Stillwater image-type mapping must not trigger a download.
func TestDownloadNamedImages_SkipsEmptyAndUnknownTags(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	dl := &mockImageDownloader{}
	a := &artist.Artist{ID: "artist-6", Name: "Skip Tags", Path: t.TempDir()}
	var result populateResult

	imageTags := map[string]string{
		"Primary":   "",         // empty value -> skip
		"NotMapped": "somehash", // unknown platform key -> skip
	}
	r.downloadPlatformImages(context.Background(), dl, "platform-6", imageTags, nil, a, "emby", &result)

	if dl.imageCalls != 0 {
		t.Errorf("expected no image download calls, got %d", dl.imageCalls)
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0", result.Images)
	}
}

// TestDownloadNamedImage_DownloadErrorSkips covers the GetArtistImage error
// branch: a failed download is logged and skipped, not counted or persisted.
func TestDownloadNamedImage_DownloadErrorSkips(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	dl := &mockImageDownloader{
		imageFn: func(ctx context.Context, artistID, imageType string) ([]byte, string, error) {
			return nil, "", errors.New("platform unavailable")
		},
	}
	dir := t.TempDir()
	a := &artist.Artist{ID: "artist-7", Name: "Download Fails", Path: dir}
	var result populateResult

	r.downloadPlatformImages(context.Background(), dl, "platform-7",
		map[string]string{"Primary": "hash"}, nil, a, "emby", &result)

	if dl.imageCalls != 1 {
		t.Errorf("expected exactly 1 image download attempt, got %d", dl.imageCalls)
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0 after a failed download", result.Images)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files written after a failed download, got %v", entries)
	}
}

// TestDownloadBackdrops_SkipsEmptyTag covers the empty-tag continue branch in
// downloadBackdrops: an empty backdrop tag must not trigger a download.
func TestDownloadBackdrops_SkipsEmptyTag(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	dl := &mockImageDownloader{}
	a := &artist.Artist{ID: "artist-8", Name: "Empty Backdrop Tag", Path: t.TempDir()}
	var result populateResult

	r.downloadPlatformImages(context.Background(), dl, "platform-8", nil, []string{""}, a, "emby", &result)

	if dl.backdropCalls != 0 {
		t.Errorf("expected no backdrop download calls, got %d", dl.backdropCalls)
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0", result.Images)
	}
}

// TestDownloadBackdrop_EmptyResponseSkips covers the len(data)==0 branch: a
// platform that returns a zero-length backdrop body must not count or save
// anything.
func TestDownloadBackdrop_EmptyResponseSkips(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	dl := &mockImageDownloader{
		backdropFn: func(ctx context.Context, artistID string, index int) ([]byte, string, error) {
			return []byte{}, "", nil
		},
	}
	dir := t.TempDir()
	a := &artist.Artist{ID: "artist-9", Name: "Empty Backdrop", Path: dir}
	var result populateResult

	r.downloadPlatformImages(context.Background(), dl, "platform-9", nil, []string{"hash1"}, a, "emby", &result)

	if dl.backdropCalls != 1 {
		t.Errorf("expected exactly 1 backdrop download attempt, got %d", dl.backdropCalls)
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0 for an empty backdrop response", result.Images)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files written for an empty backdrop response, got %v", entries)
	}
}

// TestDownloadBackdrop_ConvertFormatErrorSkips covers the image-conversion
// error branch: a platform response that isn't decodable image data must be
// skipped rather than saved or counted.
func TestDownloadBackdrop_ConvertFormatErrorSkips(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	dl := &mockImageDownloader{
		backdropFn: func(ctx context.Context, artistID string, index int) ([]byte, string, error) {
			return []byte("not an image"), "", nil
		},
	}
	dir := t.TempDir()
	a := &artist.Artist{ID: "artist-10", Name: "Bad Backdrop Data", Path: dir}
	var result populateResult

	r.downloadPlatformImages(context.Background(), dl, "platform-10", nil, []string{"hash1"}, a, "emby", &result)

	if dl.backdropCalls != 1 {
		t.Errorf("expected exactly 1 backdrop download attempt, got %d", dl.backdropCalls)
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0 for undecodable backdrop data", result.Images)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files written for undecodable backdrop data, got %v", entries)
	}
}
