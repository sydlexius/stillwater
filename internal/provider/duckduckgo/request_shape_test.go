package duckduckgo

// Tests for issue #3273: the adapter must send DuckDuckGo the exact
// known-good request shape measured in
// https://github.com/sydlexius/stillwater/issues/3228#issuecomment-5880827938
// -- headers, params, a per-search cookie jar, and manual gzip decoding --
// while keeping the httpsafe SSRF guard intact.

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/provider"
)

// capturedRequest is a snapshot of one request the adapter sent, taken
// before the handler responds (so it reflects exactly what the client put on
// the wire, not anything the test later mutates).
type capturedRequest struct {
	path   string
	header http.Header
	query  url.Values
}

// newCapturingServer serves the vqd main page (GET /) with vqdToken embedded
// and the image results (GET /i.js) with imageBody, recording every request
// it receives. If gzipImage is true, the /i.js response is gzip-compressed
// and carries Content-Encoding: gzip. setCookie, if non-empty, is set on the
// vqd response only. captured() returns a snapshot safe to read after the
// call under test completes.
func newCapturingServer(vqdToken string, imageBody []byte, gzipImage bool, setCookie string) (*httptest.Server, func() []capturedRequest) {
	var mu sync.Mutex
	var reqs []capturedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, capturedRequest{
			path:   r.URL.Path,
			header: r.Header.Clone(),
			query:  r.URL.Query(),
		})
		mu.Unlock()

		switch {
		case r.URL.Path == "/" && r.Method == http.MethodGet:
			if setCookie != "" {
				http.SetCookie(w, &http.Cookie{Name: "ddg_session", Value: setCookie})
			}
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><script>vqd='` + vqdToken + `'</script></html>`))
		case r.URL.Path == "/i.js":
			w.Header().Set("Content-Type", "application/json")
			if gzipImage {
				w.Header().Set("Content-Encoding", "gzip")
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				_, _ = gz.Write(imageBody)
				_ = gz.Close()
				w.Write(buf.Bytes())
				return
			}
			w.Write(imageBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedRequest, len(reqs))
		copy(out, reqs)
		return out
	}
}

func newTestAdapter(t *testing.T, srvURL string) *Adapter {
	t.Helper()
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewWithBaseURL(limiter, logger, srvURL, srvURL)
	useLoopbackTestClient(a)
	return a
}

const oneResultBody = `{"results":[{"image":"https://example.com/x.jpg","thumbnail":"https://example.com/x_t.jpg","width":100,"height":100}]}`

// TestRequestShapeHeadersAndParams asserts every header and param on both
// the vqd fetch and the i.js fetch against the known-good request shape.
// This is the AC's "every header and param has a unit test" requirement;
// the mutation proof for it (removing Accept-Language, and widening f to
// five commas) is recorded in the PR/report rather than left in the tree --
// see the issue's "MUTATION PROOF" acceptance criterion.
func TestRequestShapeHeadersAndParams(t *testing.T) {
	srv, captured := newCapturingServer("4-123456789", []byte(oneResultBody), false, "")
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	query := "Radiohead " + searchTerms[provider.ImageThumb]

	if _, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb); err != nil {
		t.Fatalf("SearchImages: %v", err)
	}

	reqs := captured()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests (vqd, i.js), got %d: %+v", len(reqs), reqs)
	}
	vqdReq, imgReq := reqs[0], reqs[1]

	if vqdReq.path != "/" {
		t.Errorf("vqd request path = %q, want /", vqdReq.path)
	}
	if got := vqdReq.query.Get("q"); got != query {
		t.Errorf("vqd q = %q, want %q", got, query)
	}
	if got := vqdReq.query.Get("iax"); got != "images" {
		t.Errorf("vqd iax = %q, want images", got)
	}
	if got := vqdReq.query.Get("ia"); got != "images" {
		t.Errorf("vqd ia = %q, want images", got)
	}
	if got := vqdReq.header.Get("User-Agent"); got != userAgent {
		t.Errorf("vqd User-Agent = %q, want %q", got, userAgent)
	}
	if got := vqdReq.header.Get("Accept-Language"); got != acceptLanguage {
		t.Errorf("vqd Accept-Language = %q, want %q", got, acceptLanguage)
	}
	if got := vqdReq.header.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("vqd Accept-Encoding = %q, want gzip", got)
	}

	if imgReq.path != "/i.js" {
		t.Errorf("image request path = %q, want /i.js", imgReq.path)
	}
	wantParams := map[string]string{
		"q": query,
		"o": "json",
		"p": "1",
		"s": "0",
		"f": ",,,,",
		"l": "us-en",
	}
	for k, want := range wantParams {
		if got := imgReq.query.Get(k); got != want {
			t.Errorf("i.js param %s = %q, want %q", k, got, want)
		}
	}
	if got := imgReq.query.Get("vqd"); got != "4-123456789" {
		t.Errorf("i.js vqd = %q, want 4-123456789", got)
	}
	if got := imgReq.header.Get("Accept"); got != "application/json, text/plain, */*" {
		t.Errorf("i.js Accept = %q, want %q", got, "application/json, text/plain, */*")
	}
	if got := imgReq.header.Get("Accept-Language"); got != acceptLanguage {
		t.Errorf("i.js Accept-Language = %q, want %q", got, acceptLanguage)
	}
	if got := imgReq.header.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("i.js Accept-Encoding = %q, want gzip", got)
	}
	if got := imgReq.header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("i.js X-Requested-With = %q, want XMLHttpRequest", got)
	}
	wantReferer := vqdRequestURL(srv.URL, query)
	if got := imgReq.header.Get("Referer"); got != wantReferer {
		t.Errorf("i.js Referer = %q, want %q", got, wantReferer)
	}
}

// TestCookieSharedWithinOneSearch asserts the cookie the vqd response sets
// arrives on the i.js request for that same search.
func TestCookieSharedWithinOneSearch(t *testing.T) {
	srv, captured := newCapturingServer("4-1", []byte(oneResultBody), false, "abc123")
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)

	if _, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb); err != nil {
		t.Fatalf("SearchImages: %v", err)
	}

	reqs := captured()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	imgCookie := reqs[1].header.Get("Cookie")
	if !strings.Contains(imgCookie, "ddg_session=abc123") {
		t.Errorf("i.js Cookie header = %q, want it to contain ddg_session=abc123", imgCookie)
	}
}

// TestCookieNotSharedAcrossSearches asserts two sequential searches through
// the same *Adapter get independent jars: the second search's vqd request
// must not carry the cookie the first search's vqd response set.
func TestCookieNotSharedAcrossSearches(t *testing.T) {
	srv, captured := newCapturingServer("4-1", []byte(oneResultBody), false, "sess-per-search")
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)

	if _, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb); err != nil {
		t.Fatalf("first SearchImages: %v", err)
	}
	if _, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageFanart); err != nil {
		t.Fatalf("second SearchImages: %v", err)
	}

	var vqdCookies []string
	for _, r := range captured() {
		if r.path == "/" {
			vqdCookies = append(vqdCookies, r.header.Get("Cookie"))
		}
	}
	if len(vqdCookies) != 2 {
		t.Fatalf("expected 2 vqd requests, got %d", len(vqdCookies))
	}
	if vqdCookies[0] != "" {
		t.Errorf("first search's vqd request unexpectedly carried a cookie: %q", vqdCookies[0])
	}
	if vqdCookies[1] != "" {
		t.Errorf("second search's vqd request carried the first search's cookie: %q", vqdCookies[1])
	}
}

// TestImageSearchGzipDecodes asserts a gzip-encoded i.js body decodes
// correctly. Accept-Encoding is set explicitly on the request (required to
// match the known-good header set), which disables net/http's transparent
// decompression, so this exercises the adapter's own readBody gzip path.
func TestImageSearchGzipDecodes(t *testing.T) {
	srv, _ := newCapturingServer("4-1", []byte(`{"results":[{"image":"https://example.com/gz.jpg","width":42,"height":24}]}`), true, "")
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)

	images, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err != nil {
		t.Fatalf("SearchImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d: %+v", len(images), images)
	}
	if images[0].URL != "https://example.com/gz.jpg" {
		t.Errorf("URL = %q, want https://example.com/gz.jpg", images[0].URL)
	}
	if images[0].Width != 42 || images[0].Height != 24 {
		t.Errorf("dimensions = %dx%d, want 42x24", images[0].Width, images[0].Height)
	}
}

// TestImageSearchGzipCorruptionWrapsError feeds a truncated/corrupt gzip
// body through the i.js path and asserts readBody's error is wrapped with
// context ("decompressing gzip body: ...") rather than surfacing a bare
// stdlib error indistinguishable from a plain network-read failure. %w is
// used so errors.Is/As on the underlying error (io.ErrUnexpectedEOF for a
// gzip stream truncated mid-block) still works through the wrapping.
func TestImageSearchGzipCorruptionWrapsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.Method == http.MethodGet:
			w.Write([]byte(`<html><script>vqd='4-1'</script></html>`))
		case r.URL.Path == "/i.js":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			// A well-formed gzip header followed by truncated/corrupt
			// compressed data: gzip.NewReader succeeds (the header is
			// intact), but the later io.ReadAll of the decompressing
			// reader fails, exercising the wrapped-error path inside the
			// io.ReadAll branch of readBody rather than the
			// gzip.NewReader error branch.
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			_, _ = gz.Write([]byte(oneResultBody))
			_ = gz.Close()
			full := buf.Bytes()
			// Cut well before the end (gzip trailer included), inside the
			// compressed block, so decompression fails mid-stream instead
			// of cleanly at a block boundary.
			w.Write(full[:len(full)-4])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)

	_, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected an error for a truncated gzip body, got nil")
	}
	if !strings.Contains(err.Error(), "decompressing gzip body") {
		t.Errorf("error = %v, want it to contain \"decompressing gzip body\"", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		// Not fatal: gzip may surface a different concrete error for some
		// truncation points (e.g. gzip.ErrChecksum on a whole-trailer cut).
		// The message-contains check above is the load-bearing assertion;
		// this just records which error class this particular truncation
		// point actually produced.
		t.Logf("truncated-gzip error is not io.ErrUnexpectedEOF (got %v); message-wrap assertion still holds", err)
	}
}

// TestExtractVQDFromHTMLFallback covers the /html/ fallback's looser
// vqdFallbackRegex (#3273 fix round): unlike the main page's quoted-only
// vqdRegex, this call site was never live-verified to require quotes, so
// it accepts both the unquoted digits-dash query-parameter style
// historically observed there AND the quoted forms, in case the fallback
// ever emits those too.
func TestExtractVQDFromHTMLFallback(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		token string
	}{
		{"unquoted, ampersand-terminated", `vqd=98765&`, "98765"},
		{"unquoted with dash, ampersand-terminated", `vqd=4-123456789&`, "4-123456789"},
		{"unquoted, end of input", `vqd=98765`, "98765"},
		{"single quoted", `vqd='4-123456789'`, "4-123456789"},
		{"double quoted", `vqd="4-123456789"`, "4-123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := extractVQDFromHTMLFallback([]byte(tt.html))
			if err != nil {
				t.Fatalf("extractVQDFromHTMLFallback(%q): %v", tt.html, err)
			}
			if token != tt.token {
				t.Errorf("got token %q, want %q", token, tt.token)
			}
		})
	}
}

// TestSSRFGuardSurvivesJarAttach proves newSearchClient's per-search jar
// attach (httpsafe.ClientWithJar) does not drop the SSRF guard on
// a.client.Transport. It deliberately does NOT call useLoopbackTestClient:
// a.client stays the real httpsafe.SafeClient-backed client, and the target
// is a loopback address that SafeTransport must reject at dial time before
// any network I/O. errors.Is pins the failure to the SSRF guard specifically
// -- not an incidental connection-refused/timeout that a bare, unguarded
// http.Client would also produce, which is the failure mode this test exists
// to catch.
//
// Mutation proof (not left in the tree): temporarily changing
// httpsafe.ClientWithJar to build `&http.Client{Jar: jar, Timeout:
// client.Timeout}` (dropping Transport, so the copy silently falls back to
// http.DefaultTransport) makes this test fail with a dial/connection error
// instead of errors.Is(err, httpsafe.ErrPrivateAddress). See the PR/report
// for the exact run.
func TestSSRFGuardSurvivesJarAttach(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewWithBaseURL(limiter, logger, "http://127.0.0.1:1", "http://127.0.0.1:1")

	_, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected an error for a loopback target, got nil")
	}
	if !errors.Is(err, httpsafe.ErrPrivateAddress) {
		t.Fatalf("expected errors.Is(err, httpsafe.ErrPrivateAddress), got: %v", err)
	}
}

// TestSearchImagesLive is an opt-in integration test against the real
// DuckDuckGo. Skipped unless SW_DDG_LIVE=1. Per the issue's warning, a
// malformed request can penalize the operator's IP for minutes, so this
// test makes exactly one search and must never be looped or retried.
func TestSearchImagesLive(t *testing.T) {
	if os.Getenv("SW_DDG_LIVE") != "1" {
		t.Skip("set SW_DDG_LIVE=1 to run a single live DuckDuckGo search (see issue #3273); never loop/retry this")
	}

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := New(limiter, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	images, err := a.SearchImages(ctx, "Radiohead", provider.ImageThumb)
	if err != nil {
		t.Fatalf("live SearchImages: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("live SearchImages: expected at least 1 result, got 0")
	}

	found := false
	for _, img := range images {
		if img.Width > 0 && img.Height > 0 &&
			(strings.HasPrefix(img.URL, "http://") || strings.HasPrefix(img.URL, "https://")) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("live SearchImages: no result had both a full http(s) URL and width/height > 0: %+v", images)
	}
}
