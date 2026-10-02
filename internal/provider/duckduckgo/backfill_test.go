package duckduckgo

// Tests for #3309: filter a whole i.js page before the 30-cap, and follow
// `next` (at most 2 extra pages) only when too few survive. Every test runs
// against an httptest fake; none touches a real DuckDuckGo host.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// fakePage describes one i.js response: how many hits, which of them are on
// the blocked host, the next cursor, and an optional failing status.
type fakePage struct {
	hits    int
	blocked func(i int) bool
	next    string
	status  int
}

// pagedServer serves the vqd page (setting a cookie) and i.js pages keyed by
// the `s` offset, recording every request.
func pagedServer(t *testing.T, pages map[string]fakePage) (*httptest.Server, func() []capturedRequest, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedRequest
	var cookies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, capturedRequest{path: r.URL.Path, header: r.Header.Clone(), query: r.URL.Query()})
		if r.URL.Path == "/i.js" {
			c, _ := r.Cookie("ddg_session")
			if c != nil {
				cookies = append(cookies, c.Value)
			} else {
				cookies = append(cookies, "")
			}
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "ddg_session", Value: "sess1"})
			_, _ = w.Write([]byte(`<script>vqd='4-111'</script>`))
		case "/i.js":
			pg, ok := pages[r.URL.Query().Get("s")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if pg.status != 0 {
				w.WriteHeader(pg.status)
				return
			}
			var resp imageSearchResponse
			s := r.URL.Query().Get("s")
			for i := 0; i < pg.hits; i++ {
				host := "good.example"
				if pg.blocked != nil && pg.blocked(i) {
					host = "blocked.example"
				}
				resp.Results = append(resp.Results, imageHit{Image: fmt.Sprintf("https://%s/s%s-%d.jpg", host, s, i)})
			}
			resp.Next = pg.next
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedRequest {
			mu.Lock()
			defer mu.Unlock()
			return append([]capturedRequest(nil), reqs...)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), cookies...)
		}
}

// blockHost is a stand-in for the AI matcher: drops blocked.example hits.
func blockHost(in []provider.ImageResult) ([]provider.ImageResult, int) {
	var kept []provider.ImageResult
	for _, r := range in {
		if !strings.Contains(r.URL, "blocked.example") {
			kept = append(kept, r)
		}
	}
	return kept, len(in) - len(kept)
}

func iJSRequests(reqs []capturedRequest) []capturedRequest {
	var out []capturedRequest
	for _, r := range reqs {
		if r.path == "/i.js" {
			out = append(out, r)
		}
	}
	return out
}

func first40Blocked(i int) bool { return i < 40 }

func TestFilteredSearchFiltersWholePageBeforeCap(t *testing.T) {
	srv, captured, _ := pagedServer(t, map[string]fakePage{
		"0": {hits: 100, blocked: first40Blocked, next: "i.js?s=100"},
	})
	a := newTestAdapter(t, srv.URL)

	res, removed, err := a.SearchImagesFiltered(t.Context(), "Artist", provider.ImageThumb, blockHost)
	if err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the fixture really has 40 blocked hits in the first 30
	// slots' neighborhood; without filtering first, the cap would keep
	// hits 0-29 which are ALL blocked.
	if len(res) != 30 {
		t.Fatalf("results = %d, want 30", len(res))
	}
	for _, r := range res {
		if strings.Contains(r.URL, "blocked.example") {
			t.Fatalf("blocked result survived: %s", r.URL)
		}
	}
	if removed != 40 {
		t.Errorf("removed = %d, want 40", removed)
	}
	if n := len(iJSRequests(captured())); n != 1 {
		t.Errorf("i.js requests = %d, want exactly 1", n)
	}
}

func TestFilteredSearchPagesOnShortfallAndCapsExtraPages(t *testing.T) {
	// Every page: 10 hits, 8 blocked -> 2 survivors per page. 3 pages total
	// (first + 2 extra) gives 6; a 4th page exists but must never be fetched.
	allBut2 := func(i int) bool { return i >= 2 }
	srv, captured, _ := pagedServer(t, map[string]fakePage{
		"0":  {hits: 10, blocked: allBut2, next: "i.js?s=10"},
		"10": {hits: 10, blocked: allBut2, next: "i.js?s=20"},
		"20": {hits: 10, blocked: allBut2, next: "i.js?s=30"},
		"30": {hits: 10, next: "i.js?s=40"},
	})
	a := newTestAdapter(t, srv.URL)

	res, removed, err := a.SearchImagesFiltered(t.Context(), "Artist", provider.ImageThumb, blockHost)
	if err != nil {
		t.Fatal(err)
	}
	reqs := iJSRequests(captured())
	if len(reqs) != 3 {
		t.Fatalf("i.js requests = %d, want 3 (1 + at most 2 extra)", len(reqs))
	}
	if len(res) != 6 {
		t.Errorf("results = %d, want 6", len(res))
	}
	// (d) removed sums across pages.
	if removed != 24 {
		t.Errorf("removed = %d, want 24 (8 per page x 3 pages)", removed)
	}
	for i, want := range []string{"0", "10", "20"} {
		if got := reqs[i].query.Get("s"); got != want {
			t.Errorf("request %d s = %q, want %q", i, got, want)
		}
	}
}

func TestFilteredSearchStopsAt30AcrossPages(t *testing.T) {
	srv, captured, _ := pagedServer(t, map[string]fakePage{
		"0":  {hits: 20, blocked: func(i int) bool { return i >= 10 }, next: "i.js?s=20"}, // 10 survive
		"20": {hits: 40, next: "i.js?s=60"},                                               // 40 survive -> cap
		"60": {hits: 40},
	})
	a := newTestAdapter(t, srv.URL)

	res, removed, err := a.SearchImagesFiltered(t.Context(), "Artist", provider.ImageThumb, blockHost)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 30 {
		t.Errorf("results = %d, want 30", len(res))
	}
	if removed != 10 {
		t.Errorf("removed = %d, want 10", removed)
	}
	if n := len(iJSRequests(captured())); n != 2 {
		t.Errorf("i.js requests = %d, want 2", n)
	}
}

func TestExtraPagesCarryIdenticalHeadersCookieAndVQD(t *testing.T) {
	srv, captured, cookies := pagedServer(t, map[string]fakePage{
		"0":  {hits: 5, blocked: func(int) bool { return true }, next: "i.js?s=5"},
		"5":  {hits: 5, blocked: func(int) bool { return true }, next: "i.js?s=10"},
		"10": {hits: 5},
	})
	a := newTestAdapter(t, srv.URL)
	if _, _, err := a.SearchImagesFiltered(t.Context(), "Artist", provider.ImageThumb, blockHost); err != nil {
		t.Fatal(err)
	}
	reqs := iJSRequests(captured())
	if len(reqs) != 3 {
		t.Fatalf("i.js requests = %d, want 3", len(reqs))
	}
	for i, r := range reqs {
		if got := r.header.Get("Accept-Language"); got != acceptLanguage {
			t.Errorf("request %d Accept-Language = %q, want %q", i, got, acceptLanguage)
		}
		for _, h := range []string{"User-Agent", "Accept", "Accept-Encoding", "Referer", "X-Requested-With"} {
			if r.header.Get(h) != reqs[0].header.Get(h) {
				t.Errorf("request %d header %s = %q, differs from first %q", i, h, r.header.Get(h), reqs[0].header.Get(h))
			}
		}
		if r.header.Get("Referer") == "" {
			t.Errorf("request %d has no Referer", i)
		}
		if r.query.Get("vqd") != "4-111" {
			t.Errorf("request %d vqd = %q, want the first request's token", i, r.query.Get("vqd"))
		}
		// Params other than the offset are unchanged from the first request.
		for _, k := range []string{"q", "o", "p", "f", "l"} {
			if r.query.Get(k) != reqs[0].query.Get(k) {
				t.Errorf("request %d param %s = %q, differs from first", i, k, r.query.Get(k))
			}
		}
	}
	for i, c := range cookies() {
		if c != "sess1" {
			t.Errorf("request %d cookie = %q, want the per-search jar's sess1", i, c)
		}
	}
}

func TestNoFilterIsOneRequestFirst30(t *testing.T) {
	srv, captured, _ := pagedServer(t, map[string]fakePage{
		"0": {hits: 100, blocked: first40Blocked, next: "i.js?s=100"},
	})
	a := newTestAdapter(t, srv.URL)

	res, err := a.SearchImages(t.Context(), "Artist", provider.ImageThumb)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 30 {
		t.Fatalf("results = %d, want 30", len(res))
	}
	// Filter off: the first 30 hits, blocked or not, exactly as before.
	if !strings.Contains(res[0].URL, "blocked.example") {
		t.Errorf("first result %s should be hit 0 (unfiltered)", res[0].URL)
	}
	if n := len(iJSRequests(captured())); n != 1 {
		t.Errorf("i.js requests = %d, want 1", n)
	}
}

func TestExtraPageChallengeIsNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusAccepted} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv, captured, _ := pagedServer(t, map[string]fakePage{
				"0":  {hits: 10, blocked: func(i int) bool { return i >= 4 }, next: "i.js?s=10"},
				"10": {status: status},
			})
			a := newTestAdapter(t, srv.URL)
			res, removed, err := a.SearchImagesFiltered(t.Context(), "Artist", provider.ImageThumb, blockHost)
			if err != nil {
				t.Fatalf("a failed extra page must not fail the search: %v", err)
			}
			if len(res) != 4 || removed != 6 {
				t.Errorf("results/removed = %d/%d, want 4/6 (what page 1 produced)", len(res), removed)
			}
			if n := len(iJSRequests(captured())); n != 2 {
				t.Errorf("i.js requests = %d, want 2 (first + one failed extra, no retry)", n)
			}
		})
	}
}

func TestNextOffsetRejectsUnusableCursors(t *testing.T) {
	for name, next := range map[string]string{
		"empty": "", "no s": "i.js?q=x", "non numeric": "i.js?s=abc",
		"not advancing": "i.js?s=0", "bad url": "http://[::1",
	} {
		if n, ok := nextOffset(next, 0); ok {
			t.Errorf("%s: nextOffset(%q) = %d, true; want false", name, next, n)
		}
	}
	if n, ok := nextOffset("i.js?q=x&s=100", 0); !ok || n != 100 {
		t.Errorf("valid cursor = %d, %v; want 100, true", n, ok)
	}
}

// A short first page with a next cursor must NOT page when no filter is
// supplied: filter off is exactly the old one-request behavior.
func TestNoFilterShortPageDoesNotPage(t *testing.T) {
	srv, captured, _ := pagedServer(t, map[string]fakePage{
		"0":  {hits: 10, next: "i.js?s=10"},
		"10": {hits: 10},
	})
	a := newTestAdapter(t, srv.URL)
	res, err := a.SearchImages(t.Context(), "Artist", provider.ImageThumb)
	if err != nil || len(res) != 10 {
		t.Fatalf("results = %d, err = %v; want 10, nil", len(res), err)
	}
	if n := len(iJSRequests(captured())); n != 1 {
		t.Errorf("i.js requests = %d, want 1", n)
	}
}
