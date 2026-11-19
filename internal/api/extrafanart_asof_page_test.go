package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	efAsOfID    = `id="extrafanart-migration-as-of"`
	efRefreshID = `id="extrafanart-migration-refresh-button"`
)

// postDryRunHX is the Refresh button's request: the same POST dry run, as htmx
// sends it, answered with the page body as a fragment.
func postDryRunHX(t *testing.T, r *Router, ctx context.Context) *httptest.ResponseRecorder {
	req := withI18nCtx(t, httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/extrafanart-migration", strings.NewReader(`{"dry_run": true}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handleExtraFanartMigrationRun(w, req)
	return w
}

// asOfText returns the text of the as-of line, or "" when the page has none.
func asOfText(body string) string {
	_, after, ok := strings.Cut(body, efAsOfID)
	if !ok {
		return ""
	}
	_, after, _ = strings.Cut(after, ">")
	text, _, _ := strings.Cut(after, "</p>")
	return text
}

// The page dates its preview from the READ's begin stamp, on the cold load and on
// a cache hit alike (the hit keeps the original stamp, not "now").
func TestExtraFanartPage_ShowsAsOfOfTheCachedPreview(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	clk := &fakeClock{t: time.Now().Add(-4*time.Minute - 30*time.Second)}
	r.extraFanartNow = clk.now
	walks := countWalks(r)

	cold := getExtraFanartPage(t, r, adminContext()).Body.String()
	if got := asOfText(cold); got != "Preview as of 4m ago." {
		t.Fatalf("cold load as-of line = %q", got)
	}
	clk.set(clk.now().Add(2 * time.Minute)) // the hit is served 2 minutes later
	warm := getExtraFanartPage(t, r, adminContext()).Body.String()
	if got := asOfText(warm); got != "Preview as of 4m ago." {
		t.Errorf("cache hit as-of line = %q: it must keep the original begin stamp", got)
	}
	if n := walks.Load(); n > 1 {
		t.Errorf("precondition: the second load must be a cache hit, walked %d times", n)
	}
}

// Refresh is offered in every PREVIEW state, and only there: empty, planned and
// stopped previews; never the running notice or a receipt.
func TestExtraFanartPage_RefreshButtonPresentInEveryPreviewState(t *testing.T) {
	t.Parallel()
	wantWiring := []string{efRefreshID, `hx-post="/api/v1/reports/extrafanart-migration"`, `hx-vals='{"dry_run": true}'`}
	check := func(t *testing.T, name, body string, want bool) {
		t.Helper()
		for _, w := range wantWiring {
			if strings.Contains(body, w) != want {
				t.Errorf("%s: presence of %q = %v, want %v", name, w, !want, want)
			}
		}
	}

	t.Run("empty library", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouterForBackdrops(t)
		body := getExtraFanartPage(t, r, adminContext()).Body.String()
		if !strings.Contains(body, `id="extrafanart-migration-empty"`) {
			t.Fatal("precondition: want the empty state")
		}
		check(t, "empty", body, true)
	})
	t.Run("planned", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 2)
		check(t, "planned", getExtraFanartPage(t, r, adminContext()).Body.String(), true)
	})
	t.Run("stopped preview", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 2)
		if err := r.db.Close(); err != nil {
			t.Fatal(err)
		}
		body := getExtraFanartPage(t, r, adminContext()).Body.String()
		if !strings.Contains(body, `id="extrafanart-migration-error"`) {
			t.Fatal("precondition: want the error card")
		}
		check(t, "stopped", body, true)
		if strings.Contains(body, efAsOfID) {
			t.Error("a stopped preview must not carry an as-of line")
		}
	})
	t.Run("running notice", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 2)
		r.extraFanartMu.Lock()
		r.extraFanartRunning = true
		r.extraFanartMu.Unlock()
		body := getExtraFanartPage(t, r, adminContext()).Body.String()
		if !strings.Contains(body, `id="extrafanart-migration-running"`) {
			t.Fatal("precondition: want the running notice")
		}
		check(t, "running", body, false)
	})
	t.Run("receipt", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 2)
		req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/extrafanart-migration", strings.NewReader(`{"dry_run": false}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		r.handleExtraFanartMigrationRun(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `id="extrafanart-migration-receipt"`) {
			t.Fatal("precondition: want the receipt")
		}
		check(t, "receipt", body, false)
		if strings.Contains(body, efAsOfID) {
			t.Error("a receipt is not a preview and must not carry an as-of line")
		}
	})
}

// Refresh reads the disk (never the cache), re-stamps the line, takes focus, and
// the next page load is served from the refreshed result.
func TestExtraFanartPage_RefreshReadsDiskAndRestampsAsOf(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	clk := &fakeClock{t: time.Now().Add(-8 * time.Minute)}
	r.extraFanartNow = clk.now
	if body := getExtraFanartPage(t, r, adminContext()).Body.String(); !strings.Contains(body, "img2.jpg") || asOfText(body) != "Preview as of 8m ago." {
		t.Fatal("precondition: the cold load should list img2.jpg dated 8m ago")
	}
	if err := os.Remove(filepath.Join(a.dir, "extrafanart", "img2.jpg")); err != nil {
		t.Fatal(err)
	}
	clk.set(time.Now()) // the refresh begins "now"

	frag := postDryRunHX(t, r, adminContext())
	body := frag.Body.String()
	if frag.Code != http.StatusOK || strings.Contains(body, "img2.jpg") {
		t.Fatalf("the refresh must read the disk (code %d, still lists the deleted file: %v)", frag.Code, strings.Contains(body, "img2.jpg"))
	}
	if got := asOfText(body); got != "Preview as of just now." {
		t.Errorf("refreshed as-of line = %q", got)
	}
	if !strings.Contains(body, `tabindex="-1" autofocus`) {
		t.Error("the as-of line of a swapped-in fragment must take focus")
	}
	next := getExtraFanartPage(t, r, adminContext()).Body.String()
	if strings.Contains(next, "img2.jpg") || asOfText(next) != "Preview as of just now." {
		t.Error("the next page load must be served from the refreshed preview")
	}
}

// The Refresh request changes nothing on disk or in the database.
func TestExtraFanartPage_RefreshFragmentWritesNothing(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	roots := []string{filepath.Dir(a.dir), filepath.Dir(b.dir)}
	time.Sleep(20 * time.Millisecond) // so a touch after this point changes an mtime
	fsBefore, dbBefore := efFSInventory(t, roots...), efDBInventory(t, r.db)
	if len(fsBefore) < 10 || len(dbBefore) < 10 {
		t.Fatalf("precondition: the inventory is too thin to prove anything (fs %d, tables %d)", len(fsBefore), len(dbBefore))
	}
	for i := 0; i < 2; i++ {
		if w := postDryRunHX(t, r, adminContext()); !strings.Contains(w.Body.String(), "img0.jpg") {
			t.Fatalf("refresh %d did not reach its rows", i)
		}
	}
	if d := efDiff(fsBefore, efFSInventory(t, roots...)); len(d) > 0 {
		t.Errorf("refreshing changed the filesystem: %v", d)
	}
	if d := efDiff(dbBefore, efDBInventory(t, r.db)); len(d) > 0 {
		t.Errorf("refreshing changed the database: %v", d)
	}
}
