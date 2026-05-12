package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
)

// newImageHandlerTestServer builds a Router with the platform service wired,
// an empty orchestrator + web-search registry (both required by the search
// handlers), and a stub SSRF client (so fetch tests stay offline). The
// returned Router exposes everything image handler tests need; per-test setup
// only has to seed artists and adjust the round-tripper if a fetch path is
// exercised.
func newImageHandlerTestServer(t *testing.T) (*Router, *artist.Service) {
	t.Helper()

	r, artistSvc := testRouterWithPlatform(t)

	// Wire an Orchestrator backed by an empty Registry so handleImageSearch
	// (which dereferences r.orchestrator) is non-nil. With no registered
	// providers, FetchImages returns an empty result, which is the simplest
	// non-error path for the test.
	emptyRegistry := provider.NewRegistry()
	settings := provider.NewSettingsService(r.db, nil)
	r.orchestrator = provider.NewOrchestrator(emptyRegistry, settings, r.logger)
	r.providerSettings = settings

	// handleWebImageSearch ranges r.webSearchRegistry.All(); a nil registry
	// would panic. An empty registry yields the same "no results" branch.
	r.webSearchRegistry = provider.NewWebSearchRegistry()

	// Replace the SSRF client transport so fetch tests never go to the
	// network. Per-test cases reassign r.ssrfClient to install a custom
	// round-tripper.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 500, 500)}}

	return r, artistSvc
}

// testJPEG encodes a JPEG of the given dimensions and returns its bytes. Used
// to feed stubRoundTripper for fetch-path tests.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

// testPNG encodes a PNG of the given dimensions with full opacity. Used by
// handleLogoTrim tests where the alpha channel determines whether trimming
// is a no-op.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encoding PNG: %v", err)
	}
	return buf.Bytes()
}

// ----------------------------------------------------------------------------
// handleImageFetch
// ----------------------------------------------------------------------------

func TestHandleImageFetch_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	body := strings.NewReader(`{"url":"https://example.com/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/missing/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleImageFetch_InvalidImageType(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "BadType", SortName: "BadType", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"https://8.8.8.8/x.jpg","type":"poster"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageFetch_MissingURL(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NoURL", SortName: "NoURL", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleImageFetch_NonHTTPScheme(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "FTPArtist", SortName: "FTPArtist", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"ftp://example.com/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "http://") {
		t.Errorf("body should mention required scheme; got %s", w.Body.String())
	}
}

func TestHandleImageFetch_PrivateURL_Rejected(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "PrivURL", SortName: "PrivURL", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"http://127.0.0.1/secret.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "private") {
		t.Errorf("body should mention private/reserved; got %s", w.Body.String())
	}
}

func TestHandleImageFetch_FetchFails_BadGateway(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "FetchFail", SortName: "FetchFail", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Replace the round-tripper with one that always errors.
	r.ssrfClient = &http.Client{Transport: errorRoundTripper{}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
}

// errorRoundTripper returns an error for every request so the fetch path
// exercises its bad-gateway branch.
type errorRoundTripper struct{}

func (errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestHandleImageFetch_NeedsCrop(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NeedsCrop", SortName: "NeedsCrop", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Serve a wide image: thumb wants square; a 1000x100 image will trigger
	// the needs_crop branch.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 1000, 100)}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/wide.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got, _ := resp["needs_crop"].(bool); !got {
		t.Errorf("needs_crop = %v, want true", resp["needs_crop"])
	}
}

func TestHandleImageFetch_FormEncoded_Success(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "FormFetch", SortName: "FormFetch", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Square image so skip_crop is not required.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 500, 500)}}

	form := "url=https%3A%2F%2F8.8.8.8%2Fx.jpg&type=thumb"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true",
		strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageFetch_FanartAppend(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "FanArt", SortName: "FanArt", Path: dir, FanartExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Drop an initial fanart.jpg so MaxFanartIndex finds an existing primary.
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 1920, 1080)}}
	body := strings.NewReader(`{"url":"https://8.8.8.8/bg.jpg","type":"fanart"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageFetch_InvalidJSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "BadJSON", SortName: "BadJSON", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{not valid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleImageFetch_HTMXSuccess(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "HTMXFetch", SortName: "HTMXFetch", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 500, 500)}}

	form := "url=https%3A%2F%2F8.8.8.8%2Fx.jpg&type=thumb"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch",
		strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", w.Code, w.Body.String())
	}
	if hx := w.Header().Get("HX-Refresh"); hx != "true" {
		t.Errorf("HX-Refresh = %q, want true", hx)
	}
}

// ----------------------------------------------------------------------------
// handleImageSearch
// ----------------------------------------------------------------------------

func TestHandleImageSearch_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/missing/images/search", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()

	r.handleImageSearch(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleImageSearch_NoMBID(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NoMBID", SortName: "NoMBID", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageSearch_InvalidSort(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "BadSort", SortName: "BadSort", Path: t.TempDir(), MusicBrainzID: "abc"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?sort=evil", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleImageSearch_EmptyResults_JSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "EmptyRes", SortName: "EmptyRes", Path: t.TempDir(), MusicBrainzID: "abc-mbid"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?type=thumb", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Images []provider.ImageResult `json:"images"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Images) != 0 {
		t.Errorf("images = %d, want 0", len(resp.Images))
	}
}

func TestHandleImageSearch_HTMXFanart(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "HTMXSearch", SortName: "HTMXSearch", Path: t.TempDir(), MusicBrainzID: "x"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?type=fanart", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestHandleImageSearch_HTMXNonFanart(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "HTMXThumb", SortName: "HTMXThumb", Path: t.TempDir(), MusicBrainzID: "x"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?type=thumb", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// ----------------------------------------------------------------------------
// handleWebImageSearch
// ----------------------------------------------------------------------------

func TestHandleWebImageSearch_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/artists/missing/images/websearch?type=thumb", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()

	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleWebImageSearch_MissingType(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NoType", SortName: "NoType", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/websearch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebImageSearch_InvalidType(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "InvType", SortName: "InvType", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/artists/"+a.ID+"/images/websearch?type=poster", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebImageSearch_InvalidSort(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "SortBad", SortName: "SortBad", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/artists/"+a.ID+"/images/websearch?type=thumb&sort=evil", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebImageSearch_EmptyResults_JSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "EmptyWeb", SortName: "EmptyWeb", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/artists/"+a.ID+"/images/websearch?type=thumb", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleWebImageSearch_HTMX(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "HTMXWeb", SortName: "HTMXWeb", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/artists/"+a.ID+"/images/websearch?type=thumb", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// ----------------------------------------------------------------------------
// handleImageInfo
// ----------------------------------------------------------------------------

func TestHandleImageInfo_InvalidType(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "InfoBadType", SortName: "InfoBadType", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/poster/info", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "poster")
	w := httptest.NewRecorder()

	r.handleImageInfo(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleImageInfo_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/missing/images/thumb/info", nil)
	req.SetPathValue("id", "missing")
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleImageInfo(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleImageInfo_NoImageDir(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	// Artist with no path and no cache dir set -> imageDir returns "".
	r.imageCacheDir = ""
	a := &artist.Artist{Name: "NoDir", SortName: "NoDir"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/thumb/info", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleImageInfo(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleImageInfo_FileMissing(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "Missing", SortName: "Missing", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/thumb/info", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleImageInfo(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleImageInfo_Success_JSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "InfoOK", SortName: "InfoOK", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Active platform profile (Kodi) writes folder.jpg for thumb.
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 800, 800)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/thumb/info", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleImageInfo(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Type     string `json:"type"`
		Filename string `json:"filename"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.Type != "thumb" {
		t.Errorf("Type = %q, want thumb", resp.Type)
	}
	if resp.Width != 800 || resp.Height != 800 {
		t.Errorf("dimensions = %dx%d, want 800x800", resp.Width, resp.Height)
	}
	if resp.Size <= 0 {
		t.Errorf("Size = %d, want > 0", resp.Size)
	}
}

func TestHandleImageInfo_Success_HTMX(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "InfoHTMX", SortName: "InfoHTMX", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 400, 400)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/thumb/info", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleImageInfo(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// ----------------------------------------------------------------------------
// handleLogoTrim
// ----------------------------------------------------------------------------

func TestHandleLogoTrim_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/missing/images/logo/trim", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()

	r.handleLogoTrim(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleLogoTrim_LogoNotFound(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NoLogo", SortName: "NoLogo", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleLogoTrim(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// logoFilename returns the active platform profile's primary logo filename.
// Kodi defaults: clearlogo.png. Look it up at runtime instead of hard-coding.
func logoFilename(t *testing.T, r *Router) string {
	t.Helper()
	names := r.getActiveNamingConfig(context.Background(), "logo")
	if len(names) == 0 {
		t.Fatalf("no logo naming config available")
	}
	return names[0]
}

func TestHandleLogoTrim_Success_JSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "TrimOK", SortName: "TrimOK", Path: dir, LogoExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	logoPath := filepath.Join(dir, logoFilename(t, r))
	if err := os.WriteFile(logoPath, testPNG(t, 200, 100), 0o644); err != nil {
		t.Fatalf("writing logo: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleLogoTrim(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLogoTrim_HTMX_Success(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "TrimHTMX", SortName: "TrimHTMX", Path: dir, LogoExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	logoPath := filepath.Join(dir, logoFilename(t, r))
	if err := os.WriteFile(logoPath, testPNG(t, 150, 80), 0o644); err != nil {
		t.Fatalf("writing logo: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleLogoTrim(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", w.Code, w.Body.String())
	}
}

// ----------------------------------------------------------------------------
// probeImageDimensions
// ----------------------------------------------------------------------------

func TestProbeImageDimensions_NoOpWhenAllPopulated(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	in := []provider.ImageResult{
		{URL: "https://example.com/a.jpg", Width: 100, Height: 100},
		{URL: "https://example.com/b.jpg", Width: 200, Height: 200},
	}
	out := r.probeImageDimensions(context.Background(), in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Width != 100 || out[1].Width != 200 {
		t.Errorf("dims clobbered: %+v", out)
	}
}

func TestProbeImageDimensions_EmptyInput(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	out := r.probeImageDimensions(context.Background(), nil)
	if len(out) != 0 {
		t.Fatalf("len = %d, want 0", len(out))
	}
}

func TestProbeImageDimensions_ProbesMissingDims(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	// Spin up a server that serves a JPEG so ProbeRemoteImage can decode
	// real dimensions. This exercises the goroutine fan-out path end-to-end.
	body := testJPEG(t, 640, 480)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	in := []provider.ImageResult{
		{URL: srv.URL + "/x.jpg"}, // missing dims, will be probed
		{URL: "https://example.com/already.jpg", Width: 50, Height: 50},
	}
	out := r.probeImageDimensions(context.Background(), in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	// The already-populated entry should be untouched.
	if out[1].Width != 50 || out[1].Height != 50 {
		t.Errorf("already-populated entry was clobbered: %+v", out[1])
	}
}

func TestProbeImageDimensions_FailedProbeLeavesZero(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	// Server returns 500 -- ProbeRemoteImage fails, dims stay zero.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	in := []provider.ImageResult{{URL: srv.URL + "/x.jpg"}}
	out := r.probeImageDimensions(context.Background(), in)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Width != 0 || out[0].Height != 0 {
		t.Errorf("expected dims unchanged on probe failure, got %dx%d", out[0].Width, out[0].Height)
	}
}

// ----------------------------------------------------------------------------
// clearImageFlagAsync (C11 panic recovery)
// ----------------------------------------------------------------------------

// TestClearImageFlagAsync_RecoversFromPanic injects a panic by nilling out
// r.artistService before calling the helper. The dereference inside the
// goroutine triggers a runtime panic; the deferred recover() must catch it,
// emit a structured slog.Error log, and return without crashing the process.
// We capture the logger handler output and assert that the panic key appears.
func TestClearImageFlagAsync_RecoversFromPanic(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)

	// Build a custom logger that writes to a buffer so we can assert on
	// the structured log emission. Drop the existing one so the recover()
	// branch emits to our recorder.
	var logBuf safeBuffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))

	a := &artist.Artist{Name: "PanicArt", SortName: "PanicArt", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Nil the artistService so calling its method panics with a nil deref.
	r.artistService = nil

	// Run synchronously by calling the helper directly (the production call
	// site spawns a goroutine; calling directly lets us deterministically
	// observe the recovery without test-side timing flakes).
	r.clearImageFlagAsync(a.ID, "thumb")

	got := logBuf.String()
	if !strings.Contains(got, "panic in clearImageFlagAsync") {
		t.Errorf("expected panic-recovery log entry; got %q", got)
	}
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("expected ERROR level log; got %q", got)
	}
	if !strings.Contains(got, "artist_id=") || !strings.Contains(got, "image_type=thumb") {
		t.Errorf("expected structured artist_id+image_type attrs; got %q", got)
	}
}

// TestClearImageFlagAsync_LogsWarnOnError closes the underlying DB so the
// ClearImageFlag service call returns an error. The helper must log a Warn
// entry (not panic) and return cleanly. This is the only branch in the
// helper not exercised by the panic-injection / happy-path tests.
func TestClearImageFlagAsync_LogsWarnOnError(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)

	var logBuf safeBuffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	a := &artist.Artist{Name: "DBClosed", SortName: "DBClosed", Path: t.TempDir(), ThumbExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Force the next DB call to fail by closing the connection pool.
	// Subsequent service calls then return a "database is closed" error,
	// taking the helper into its Warn branch.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	r.clearImageFlagAsync(a.ID, "thumb")

	got := logBuf.String()
	if !strings.Contains(got, "failed to clear stale image flag") {
		t.Errorf("expected warn log on error path; got %q", got)
	}
	if strings.Contains(got, "panic in clearImageFlagAsync") {
		t.Errorf("unexpected panic log on plain-error path; got %q", got)
	}
}

// TestClearImageFlagAsync_HappyPath confirms the non-panic branch still emits
// the cleared-flag info log and exits cleanly. Together with the panic-
// injection test this proves recover() does not swallow normal operation.
func TestClearImageFlagAsync_HappyPath(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)

	var logBuf safeBuffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	a := &artist.Artist{Name: "HappyClear", SortName: "HappyClear", Path: t.TempDir(), ThumbExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	r.clearImageFlagAsync(a.ID, "thumb")

	got := logBuf.String()
	if !strings.Contains(got, "cleared stale image flag") {
		t.Errorf("expected info log on happy path; got %q", got)
	}
	if strings.Contains(got, "panic in clearImageFlagAsync") {
		t.Errorf("unexpected panic log on happy path; got %q", got)
	}
}

// safeBuffer wraps bytes.Buffer with a mutex so concurrent slog handlers
// (the production code path spawns a goroutine for stale-flag cleanup) cannot
// race when we capture log output in tests.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ----------------------------------------------------------------------------
// suppress unused import lint when none of the helpers in this file end up
// touching every imported package directly. atomic + time + publish + platform
// are pulled in via helpers shared with the existing handlers_image_test.go;
// keep blank references so go vet stays quiet if a test is later trimmed.
// ----------------------------------------------------------------------------

var (
	_ = atomic.LoadInt32
	_ = time.Now
	_ = publish.New
	_ = platform.NewService
)
