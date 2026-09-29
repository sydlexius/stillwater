package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

func cullTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// cullFixtureServer serves a mix of good and broken image URLs over TLS
// (ensureHTTPS in the handler leaves https URLs alone) and counts hits per path.
func cullFixtureServer(t *testing.T) (*httptest.Server, *[6]atomic.Int32) {
	t.Helper()
	good := cullTestPNG(t, 40, 30)
	var hits [6]atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/good.png", func(w http.ResponseWriter, _ *http.Request) {
		hits[0].Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(good)
	})
	mux.HandleFunc("/404.png", func(w http.ResponseWriter, _ *http.Request) {
		hits[1].Add(1)
		http.NotFound(w, nil)
	})
	mux.HandleFunc("/page.png", func(w http.ResponseWriter, _ *http.Request) {
		hits[2].Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	})
	mux.HandleFunc("/truncated.png", func(w http.ResponseWriter, _ *http.Request) {
		hits[3].Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(good[:10])
	})
	mux.HandleFunc("/slow.png", func(w http.ResponseWriter, req *http.Request) {
		hits[4].Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-req.Context().Done():
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(good)
	})
	mux.HandleFunc("/partial.png", func(w http.ResponseWriter, _ *http.Request) {
		// Typical production shape: 206 with the leading bytes only.
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Range", "bytes 0-"+strconv.Itoa(len(good)-1)+"/999999")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(good)
	})
	mux.HandleFunc("/err500.png", func(w http.ResponseWriter, _ *http.Request) {
		// Error status but a valid image body AND an image content-type, so
		// only the status check can reject it.
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(good)
	})
	mux.HandleFunc("/endless.jpg", func(w http.ResponseWriter, req *http.Request) {
		// Ignores Range; a JPEG that never reaches its frame header (endless
		// APP1 segments), so only the read cap ends the probe.
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8})
		seg := append([]byte{0xFF, 0xE1, 0xFF, 0xFF}, make([]byte, 65533)...)
		for req.Context().Err() == nil {
			if _, err := w.Write(seg); err != nil {
				return
			}
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestCullBrokenImageResults_DropsBrokenKeepsGood(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	srv, hits := cullFixtureServer(t)
	r.ssrfClient = srv.Client()

	in := []provider.ImageResult{
		{URL: srv.URL + "/404.png", Likes: 5},
		{URL: srv.URL + "/good.png", Likes: 4},
		{URL: srv.URL + "/page.png"},
		{URL: srv.URL + "/truncated.png"},
		{URL: srv.URL + "/slow.png"},
		{URL: srv.URL + "/good.png", Likes: 3, Width: 999, Height: 888}, // duplicate URL
	}
	out := r.cullBrokenImageResultsWith(context.Background(), in, cullConfig{maxConcurrent: 3, probeTimeout: 300 * time.Millisecond, passBudget: 5 * time.Second})

	if len(out) != 2 {
		t.Fatalf("kept %d results, want 2 (both good.png entries): %+v", len(out), out)
	}
	for _, o := range out {
		if o.URL != srv.URL+"/good.png" {
			t.Errorf("unexpected survivor %q", o.URL)
		}
	}
	if out[0].Width != 40 || out[0].Height != 30 {
		t.Errorf("zero dims not filled from probe: %dx%d", out[0].Width, out[0].Height)
	}
	if out[1].Width != 999 || out[1].Height != 888 {
		t.Errorf("provider dims overwritten: %dx%d", out[1].Width, out[1].Height)
	}
	if out[0].Likes != 4 || out[1].Likes != 3 {
		t.Errorf("order not preserved: %+v", out)
	}
	if n := hits[0].Load(); n != 1 {
		t.Errorf("good.png probed %d times, want exactly 1 (dedup)", n)
	}
}

func TestCullBrokenImageResults_PassBudgetKeepsUnverified(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	// A host that accepts the request and never answers.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	t.Cleanup(srv.Close)
	r.ssrfClient = srv.Client()

	// probeTimeout far exceeds the budget: only the pass budget can end this,
	// and its abort is not a verdict, so the result is kept.
	start := time.Now()
	out := r.cullBrokenImageResultsWith(context.Background(),
		[]provider.ImageResult{{URL: srv.URL + "/hang.png"}},
		cullConfig{maxConcurrent: 1, probeTimeout: 30 * time.Second, passBudget: 300 * time.Millisecond})
	elapsed := time.Since(start)
	if len(out) != 1 {
		t.Fatalf("budget-expired result dropped; got %d, want 1 kept unverified", len(out))
	}
	if elapsed < 250*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("pass took %v, want about the 300ms budget", elapsed)
	}
}

// The client cancels after a real 404 verdict has already landed for another
// result. Without the post-wait guard the 404 would be applied and the result
// list silently shrunk for a client that is gone.
func TestCullBrokenImageResults_ClientCancelAfterVerdictKeepsAll(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/second.png" {
			cancel()
			time.Sleep(50 * time.Millisecond)
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	r.ssrfClient = srv.Client()

	// maxConcurrent 1 serializes the probes: /first.png's 404 verdict is
	// recorded before /second.png cancels the context.
	in := []provider.ImageResult{{URL: srv.URL + "/first.png"}, {URL: srv.URL + "/second.png"}}
	out := r.cullBrokenImageResultsWith(ctx, in, cullConfig{maxConcurrent: 1, probeTimeout: time.Second, passBudget: 5 * time.Second})
	if len(out) != 2 {
		t.Fatalf("canceled client shrank the results: kept %d, want 2", len(out))
	}
}

func TestCullBrokenImageResults_StatusAndRangeShapes(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	srv, _ := cullFixtureServer(t)
	r.ssrfClient = srv.Client()

	start := time.Now()
	out := r.cullBrokenImageResultsWith(context.Background(), []provider.ImageResult{
		{URL: srv.URL + "/partial.png"},
		{URL: srv.URL + "/err500.png"},
		{URL: srv.URL + "/endless.jpg"},
	}, cullConfig{maxConcurrent: 3, probeTimeout: 5 * time.Second, passBudget: 10 * time.Second})
	if len(out) != 1 || out[0].URL != srv.URL+"/partial.png" {
		t.Fatalf("got %+v, want only the 206 result kept", out)
	}
	if e := time.Since(start); e > 3*time.Second {
		t.Errorf("endless Range-ignoring body took %v; the read cap should end it in well under a second", e)
	}
}

func TestCullBrokenImageResults_PeakConcurrencyBounded(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	good := cullTestPNG(t, 8, 8)
	var inflight, peak atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(good)
	}))
	t.Cleanup(srv.Close)
	r.ssrfClient = srv.Client()

	var in []provider.ImageResult
	for i := range 12 {
		in = append(in, provider.ImageResult{URL: srv.URL + "/" + strconv.Itoa(i) + ".png"})
	}
	out := r.cullBrokenImageResultsWith(context.Background(), in, cullConfig{maxConcurrent: 3, probeTimeout: 5 * time.Second, passBudget: 10 * time.Second})
	if len(out) != 12 {
		t.Fatalf("kept %d, want 12", len(out))
	}
	if p := peak.Load(); p > 3 || p < 2 {
		t.Errorf("peak in-flight probes = %d, want 2..3 (cap 3, and >1 proves the fixture ran in parallel)", p)
	}
}

func TestCullBrokenImageResults_Empty(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)
	if out := r.cullBrokenImageResults(context.Background(), nil); len(out) != 0 {
		t.Fatalf("len = %d, want 0", len(out))
	}
}

// TestHandleWebImageSearch_AllCulledStaysOK pins the design decision: every
// result culled from a provider that ANSWERED is an empty "ok" outcome, never
// "unavailable" (which is reserved for provider errors, #3229), with images a
// non-null array and the provider not listed as unavailable.
func TestHandleWebImageSearch_AllCulledStaysOK(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	srv, hits := cullFixtureServer(t)
	r.ssrfClient = srv.Client()

	stub := &stubWebImageProvider{name: provider.NameDuckDuckGo, results: []provider.ImageResult{
		{URL: srv.URL + "/404.png", Type: provider.ImageThumb},
		{URL: srv.URL + "/page.png", Type: provider.ImageThumb},
	}}
	a := setUpWebSearchTest(t, r, svc, stub)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/websearch?type=thumb", nil)
	req.SetPathValue("id", a.ID)
	w := serveValidated(t, http.HandlerFunc(r.handleWebImageSearch), req)

	if hits[1].Load() == 0 || hits[2].Load() == 0 {
		t.Fatal("precondition failed: broken hosts were never probed")
	}
	var resp struct {
		Images               []provider.ImageResult `json:"images"`
		Status               string                 `json:"status"`
		UnavailableProviders []string               `json:"unavailable_providers"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.Images == nil || len(resp.Images) != 0 {
		t.Errorf("images = %#v, want empty non-nil array", resp.Images)
	}
	if len(resp.UnavailableProviders) != 0 {
		t.Errorf("unavailable_providers = %v, want empty", resp.UnavailableProviders)
	}
}

// TestHandleWebImageSearch_CullsBrokenKeepsGood proves the wiring: without the
// handler's cull call the 404 result would still be returned.
func TestHandleWebImageSearch_CullsBrokenKeepsGood(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	srv, _ := cullFixtureServer(t)
	r.ssrfClient = srv.Client()

	stub := &stubWebImageProvider{name: provider.NameDuckDuckGo, results: []provider.ImageResult{
		{URL: srv.URL + "/404.png", Type: provider.ImageThumb},
		{URL: srv.URL + "/good.png", Type: provider.ImageThumb},
	}}
	a := setUpWebSearchTest(t, r, svc, stub)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/websearch?type=thumb", nil)
	req.SetPathValue("id", a.ID)
	w := serveValidated(t, http.HandlerFunc(r.handleWebImageSearch), req)

	var resp struct {
		Images []provider.ImageResult `json:"images"`
		Status string                 `json:"status"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 1 || resp.Images[0].URL != srv.URL+"/good.png" || resp.Status != "ok" {
		t.Fatalf("got %+v status %q, want only good.png / ok", resp.Images, resp.Status)
	}
}
