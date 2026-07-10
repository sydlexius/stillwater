package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
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
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// init registers a text/html body decoder for the kin-openapi validator. The
// image search / info / websearch endpoints negotiate JSON vs HTML responses
// via the HX-Request header, and several tests exercise the HTML arm so that
// regressions flipping the arm (or dropping the Content-Type) surface here.
// kin-openapi ships decoders only for application/json, text/plain, etc.;
// without a text/html decoder ValidateResponse fatals with "unsupported
// content type" before checking the schema. Registering PlainBodyDecoder
// treats the body as an opaque string, which matches how the spec declares
// these responses (`text/html: schema: type: string`).
func init() {
	openapi3filter.RegisterBodyDecoder("text/html", openapi3filter.PlainBodyDecoder)
}

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
	r.orchestrator = provider.NewOrchestrator(emptyRegistry, settings, r.logger, nil)
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

// testPNG encodes a fully-opaque white PNG of the given dimensions. Used by
// handleLogoTrim tests where the alpha channel determines whether trimming
// is a no-op. image.NewRGBA zero-initializes the Pix slice (A=0), which would
// produce a transparent canvas; we explicitly Draw a uniform opaque white so
// the fixture matches the docstring and the trim contract has real pixels to
// operate on.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 255, G: 255, B: 255, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	// Pattern B: spec's request body declares `type` enum [thumb,fanart,logo,banner].
	// Sending `poster` would be rejected by the request validator before
	// reaching the handler; this test exercises the handler's own
	// defense-in-depth rejection, so call the handler directly.
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify the append side-effect on disk: the seed dropped one fanart.jpg,
	// the handler should have created a second indexed file (e.g. fanart1.jpg).
	// Without this assertion, an append regression that overwrites the
	// original or no-ops entirely would still pass the 200 check.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	jpgCount := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".jpg") {
			jpgCount++
		}
	}
	if jpgCount < 2 {
		t.Fatalf("jpg files on disk = %d, want >= 2 (original + appended); entries=%v", jpgCount, entries)
	}
}

// TestHandleImageFetch_ExplicitSlot_ReplacesThatSlotOnly covers #2281 QOL #48
// on the fetch path: an explicit slot must overwrite only that numbered
// fanart file, leaving the primary untouched.
func TestHandleImageFetch_ExplicitSlot_ReplacesThatSlotOnly(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "FetchSlot", SortName: "FetchSlot", Path: dir, FanartExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	names := seedFanartSlots(t, r, dir, 2) // slots 0, 1
	primaryBefore, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("reading seed primary: %v", err)
	}

	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 1920, 1080)}}
	body := strings.NewReader(`{"url":"https://8.8.8.8/bg.jpg","type":"fanart","slot":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	saved, _ := resp["saved"].([]any)
	if len(saved) != 1 || saved[0] != names[1] {
		t.Errorf("saved = %v, want [%q] (slot 1 only)", saved, names[1])
	}

	primaryAfter, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("reading primary after slot fetch: %v", err)
	}
	if !bytes.Equal(primaryBefore, primaryAfter) {
		t.Error("slot=1 fetch must not touch the primary (slot 0)")
	}
}

// TestHandleImageFetch_ExplicitSlot_OutOfRangeRejected covers the no-gaps
// validation on the fetch path: a slot at/beyond the current count is
// rejected with a generic 400. The default stub transport from
// newImageHandlerTestServer would happily "succeed" a fetch if the handler
// incorrectly reached the network path, so a 400 here also pins that the
// out-of-range check runs before any save.
func TestHandleImageFetch_ExplicitSlot_OutOfRangeRejected(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "FetchSlotOOR", SortName: "FetchSlotOOR", Path: dir, FanartExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	seedFanartSlots(t, r, dir, 1) // slot 0 only

	body := strings.NewReader(`{"url":"https://8.8.8.8/bg.jpg","type":"fanart","slot":5}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory entries = %d, want 1 (only the seeded slot 0, nothing saved)", len(entries))
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

	// Pattern B: malformed JSON is rejected by the wrapper's request validator
	// before the handler sees it; this test exercises the handler's own
	// decode-error path, so call the handler directly.
	r.handleImageFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleImageStage_Success covers #2281 QOL #47: a successful stage
// returns a base64 data: URI and, critically, saves nothing, publishes no
// event, and never touches the pipeline -- it is a pure read.
func TestHandleImageStage_Success(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "StageOK", SortName: "StageOK", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	called := make(chan struct{}, 1)
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			select {
			case called <- struct{}{}:
			default:
			}
			return &rule.RunResult{}, nil
		},
	}
	r.pipeline = stub

	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 640, 480)}}
	body := strings.NewReader(`{"url":"https://8.8.8.8/discogs.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageStage), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	dataURI, _ := resp["image_data"].(string)
	if !strings.HasPrefix(dataURI, "data:image/jpeg;base64,") {
		t.Errorf("image_data = %q, want a data:image/jpeg;base64,... URI", dataURI)
	}
	if resp["type"] != "thumb" {
		t.Errorf("type = %v, want %q", resp["type"], "thumb")
	}

	// Nothing saved to disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("directory entries = %d, want 0 (staging must not save to disk); entries=%v", len(entries), entries)
	}
	// Pipeline never invoked (staging runs no rules).
	select {
	case <-called:
		t.Error("staging must not invoke the rule pipeline")
	default:
	}
}

// TestHandleImageStage_RejectsPrivateURL mirrors the fetch endpoint's SSRF
// guard: staging reuses the same isPrivateURL check.
func TestHandleImageStage_RejectsPrivateURL(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "StagePriv", SortName: "StagePriv", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"http://127.0.0.1/secret.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageStage), req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleImageStage_InvalidType rejects an unsupported type before any
// network fetch.
func TestHandleImageStage_InvalidType(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "StageBadType", SortName: "StageBadType", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"https://8.8.8.8/x.jpg","type":"poster"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	// Pattern B (see TestHandleImageFetch_InvalidJSON): "poster" fails the
	// OpenAPI enum before reaching the handler, so exercise the handler's own
	// invalid-type rejection directly rather than through serveValidated.
	r.handleImageStage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleImageStage_NonImageBytesRejected covers the img.DetectFormat
// validation: a successful HTTP fetch that is not actually image data must be
// rejected rather than staged. fetchImageFromURL runs DetectFormat itself and
// returns an error before handleImageStage ever sees the bytes, so this is
// deterministically a 502 (upstream fetch/decode failure), not a 400 --
// handleImageStage's OWN post-fetch DetectFormat call can never fail on the
// same data (see the comment at that call site), so there is no separate
// reachable 400 case to also exercise here.
func TestHandleImageStage_NonImageBytesRejected(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "StageNotImage", SortName: "StageNotImage", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: []byte("<html>not an image</html>")}}
	body := strings.NewReader(`{"url":"https://8.8.8.8/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageStage), req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for non-image bytes; body: %s", w.Code, w.Body.String())
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
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

	// Pattern B: spec declares sort enum [likes,resolution]; the wrapper would
	// reject `evil` before the handler runs. This test exercises the handler's
	// own enum check, so call directly.
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleWebImageSearch), req)
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

	// Pattern B: spec marks `type` query param required. Wrapper rejects the
	// missing param before the handler runs; this test exercises the handler's
	// own check, so call directly.
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

	// Pattern B: spec declares type enum [thumb,fanart,logo,banner]. Wrapper
	// rejects `poster`; this test exercises the handler's own enum check.
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

	// Pattern B: spec declares sort enum [likes,resolution]. Wrapper rejects
	// `evil`; this test exercises the handler's own enum check.
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

	w := serveValidated(t, http.HandlerFunc(r.handleWebImageSearch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// JSON arm: Content-Type must be application/json and the body must be
	// the documented {"images": [...]} shape. A regression that flips the
	// arm to text/html or returns a bare array would still pass a 200-only
	// check but fail here.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
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

	w := serveValidated(t, http.HandlerFunc(r.handleWebImageSearch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// HTMX arm: must render a text/html fragment, not the JSON envelope.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
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

	// Pattern B: spec restricts the path `type` to enum [thumb,fanart,logo,banner].
	// Wrapper rejects `poster`; this test exercises the handler's own check.
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageInfo), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageInfo), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageInfo), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageInfo), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleImageInfo), req)
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

	// Sentinel direct call: TestOperationIDCoverage scans test files for
	// CallExprs whose Fun is `r.handleX`; the other LogoTrim cases go through
	// serveValidated(http.HandlerFunc(r.handleLogoTrim), ...), which the AST
	// walker reads as a HandlerFunc CallExpr (not a handleLogoTrim CallExpr)
	// and so doesn't count toward `trimLogo` operationId coverage. Keep one
	// raw call so the ratchet sees the reference.
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

	w := serveValidated(t, http.HandlerFunc(r.handleLogoTrim), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleLogoTrim), req)
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

	w := serveValidated(t, http.HandlerFunc(r.handleLogoTrim), req)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Guard the inbound request contract so a regression that changes
		// the probe to a different verb or omits the body surfaces here
		// rather than as a silent "probed dims didn't change" result.
		if req.Method != http.MethodGet {
			t.Errorf("probe mock: method = %s, want GET", req.Method)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Install the test server's own client on the router so probeImageDimensions
	// can reach the loopback address without being blocked by the SSRF-safe transport.
	r.ssrfClient = srv.Client()

	in := []provider.ImageResult{
		{URL: srv.URL + "/x.jpg"}, // missing dims, will be probed
		{URL: "https://example.com/already.jpg", Width: 50, Height: 50},
	}
	out := r.probeImageDimensions(context.Background(), in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	// The probed entry should now carry the JPEG's real dimensions; without
	// this assertion the test would pass even if the probe goroutine never
	// wrote back into the slice.
	if out[0].Width != 640 || out[0].Height != 480 {
		t.Errorf("probed entry: got %dx%d, want 640x480", out[0].Width, out[0].Height)
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

	// Install the test server's client so the loopback address is reachable.
	r.ssrfClient = srv.Client()

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
	r.clearImageFlagAsync(context.Background(), a.ID, "thumb")

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

	r.clearImageFlagAsync(context.Background(), a.ID, "thumb")

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

	r.clearImageFlagAsync(context.Background(), a.ID, "thumb")

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
// handleImageStage -- gaps in the existing coverage above: the pre-fetch
// rejection branches other than "private URL" and "invalid type" were never
// exercised (#2323 patch-coverage fix-round).
// ----------------------------------------------------------------------------

func TestHandleImageStage_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	body := strings.NewReader(`{"url":"https://8.8.8.8/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/missing/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "missing")

	w := serveValidated(t, http.HandlerFunc(r.handleImageStage), req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageStage_MissingPathParam(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	body := strings.NewReader(`{"url":"https://8.8.8.8/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists//images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no SetPathValue("id", ...): RequirePathParam must reject an
	// empty id before touching the artist service.
	w := httptest.NewRecorder()

	r.handleImageStage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageStage_InvalidJSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "StageBadJSON", SortName: "StageBadJSON", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{not valid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	// Pattern B (see TestHandleImageFetch_InvalidJSON): malformed JSON would be
	// rejected by the request validator before reaching the handler; this
	// exercises the handler's own decode-error path directly.
	r.handleImageStage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageStage_MissingURL(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "StageNoURL", SortName: "StageNoURL", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageStage), req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageStage_NonHTTPScheme(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "StageFTP", SortName: "StageFTP", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	body := strings.NewReader(`{"url":"ftp://example.com/x.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/stage", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageStage), req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "http://") {
		t.Errorf("body should mention the required scheme; got %s", w.Body.String())
	}
}

// ----------------------------------------------------------------------------
// fetchRespondIfNeedsCrop -- gaps: the existing TestHandleImageFetch_NeedsCrop
// only exercises the JPEG needs-crop-true branch. The no-crop-needed
// (fall-through to a normal save), PNG mime-type, and explicit-slot-echo
// branches were never hit (#2323 patch-coverage fix-round).
// ----------------------------------------------------------------------------

// TestHandleImageFetch_NoCropNeeded_JSON covers geo.NeedsCrop == false: a
// square image against thumb's square requirement must fall through to a
// normal save (still 200, but WITHOUT needs_crop in the response), not the
// needs_crop branch. isHTMXRequest must be false and skip_crop absent so
// fetchRespondIfNeedsCrop is actually called (matching the guard in
// handleImageFetch); its own internal check must be what returns false here.
func TestHandleImageFetch_NoCropNeeded_JSON(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NoCropNeeded", SortName: "NoCropNeeded", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 500, 500)}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/square.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["needs_crop"] == true {
		t.Errorf("needs_crop = true for a 500x500 image against thumb's square requirement; want false/absent")
	}
}

// TestHandleImageFetch_NeedsCrop_PNGFormat covers the img.FormatPNG branch of
// fetchRespondIfNeedsCrop's mime-type switch (the existing NeedsCrop test only
// exercises the JPEG default case).
func TestHandleImageFetch_NeedsCrop_PNGFormat(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a := &artist.Artist{Name: "NeedsCropPNG", SortName: "NeedsCropPNG", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Wide PNG against thumb's square requirement forces needs_crop.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testPNG(t, 1000, 100)}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/wide.png","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got, _ := resp["needs_crop"].(bool); !got {
		t.Fatalf("needs_crop = %v, want true", resp["needs_crop"])
	}
	dataURI, _ := resp["image_data"].(string)
	if !strings.HasPrefix(dataURI, "data:image/png;base64,") {
		t.Errorf("image_data = %q, want a data:image/png;base64,... URI", dataURI)
	}
}

// TestHandleImageFetch_NeedsCrop_EchoesSlot verifies fetchRespondIfNeedsCrop
// threads an explicit fanart slot through the needs_crop response (#2281 QOL
// #48), so the client's follow-up crop POST persists to the same slot this
// fetch originated from -- without this, "slot" is silently absent from the
// response and the crop would fall back to the primary/append path instead.
func TestHandleImageFetch_NeedsCrop_EchoesSlot(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "NeedsCropSlot", SortName: "NeedsCropSlot", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	seedFanartSlots(t, r, dir, 3)
	r.updateArtistImageFlag(context.Background(), a, "fanart")
	// Square image against fanart's wide-aspect requirement forces needs_crop.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 500, 500)}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/mismatched.jpg","type":"fanart","slot":2}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got, _ := resp["needs_crop"].(bool); !got {
		t.Fatalf("needs_crop = %v, want true (500x500 square vs fanart's wide aspect requirement)", resp["needs_crop"])
	}
	gotSlot, ok := resp["slot"].(float64)
	if !ok || int(gotSlot) != 2 {
		t.Errorf("slot field = %v (present=%v), want 2", resp["slot"], ok)
	}
}

// TestHandleImageFetch_FormEncoded_ExplicitSlot covers extractImageFetchParams'
// form-encoded slot-parsing branch (the existing FormEncoded_Success test and
// the JSON-bodied ExplicitSlot tests never exercise the form path WITH a
// slot value together).
func TestHandleImageFetch_FormEncoded_ExplicitSlot(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "FormFetchSlot", SortName: "FormFetchSlot", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	names := seedFanartSlots(t, r, dir, 3) // slots 0, 1, 2
	r.updateArtistImageFlag(context.Background(), a, "fanart")
	primaryBefore, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("reading seed primary: %v", err)
	}
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 1920, 1080)}}

	form := "url=https%3A%2F%2F8.8.8.8%2Freplacement.jpg&type=fanart&slot=1"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true",
		strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	primaryAfter, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("reading primary after form-encoded slot fetch: %v", err)
	}
	if !bytes.Equal(primaryBefore, primaryAfter) {
		t.Error("a form-encoded slot=1 fetch must not touch the primary (slot 0)")
	}
	slot1After, err := os.ReadFile(filepath.Join(dir, names[1]))
	if err != nil {
		t.Fatalf("reading slot 1 after form-encoded fetch: %v", err)
	}
	if bytes.Equal(slot1After, primaryBefore) {
		t.Error("slot 1 must actually have changed after the form-encoded fetch")
	}
}
