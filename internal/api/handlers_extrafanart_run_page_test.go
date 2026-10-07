package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/i18n"
)

const (
	efPagePath = "/reports/extrafanart-migration"
	efRunPath  = "/api/v1/reports/extrafanart-migration"
	efBodyOpen = `<div id="extrafanart-migration-body"`
	efFocus    = `tabindex="-1" autofocus`
	efRunBtn   = `id="extrafanart-migration-run-button"`
)

// efMux serves requests through the REAL mux (auth, i18n and routing included),
// authenticated as an administrator by API token. hx marks the request as htmx's,
// exactly as the page's Run button sends it: a form body and HX-Request: true.
func efMux(t *testing.T, r *Router) func(method, path, body string, hx bool) *httptest.ResponseRecorder {
	t.Helper()
	if _, err := r.authService.Setup(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := r.db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	token, _, err := r.authService.CreateAPIToken(context.Background(), userID, "ef-run", string(auth.ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	// Production always has a bundle; with it the mux's own i18n middleware
	// supplies the translator, on the API route as well as the page.
	if r.i18nBundle, err = i18n.LoadEmbedded(); err != nil {
		t.Fatal(err)
	}
	hctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := r.Handler(hctx)
	return func(method, path, body string, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if hx {
			req.Header.Set("HX-Request", "true")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
}

// wantFragment asserts an htmx answer: the status code, HTML, and the page BODY
// alone (the swap target, never a whole page), carrying every want string.
func wantFragment(t *testing.T, w *httptest.ResponseRecorder, code int, want ...string) string {
	t.Helper()
	body := w.Body.String()
	if w.Code != code {
		t.Fatalf("want %d, got %d: %.300s", code, w.Code, body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("want text/html, got %q: %.200s", ct, body)
	}
	if v := w.Header().Get("Vary"); v != "HX-Request" {
		t.Errorf("want Vary: HX-Request on the negotiated answer, got %q", v)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), efBodyOpen) || strings.Contains(body, "<html") {
		t.Fatalf("want the page body fragment alone, got %.200s", body)
	}
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("fragment is missing %q", s)
		}
	}
	if strings.Contains(body, "extrafanart_migration.") {
		t.Error("a bare i18n key leaked into the fragment")
	}
	return body
}

// A clean run from the page answers 200 with the neutral receipt, listing what
// moved; the heading takes focus; a repeat run says there was nothing to do.
func TestExtraFanartRunFragment_CleanRunThenNothingToDo(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	do := efMux(t, r)

	body := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusOK,
		`id="extrafanart-migration-receipt"`, `role="status"`, efFocus, "Every file that was due to move was moved",
		"Alpha", "img0.jpg", "img1.jpg", "Moved", `href="/reports/extrafanart-migration"`)
	if got := statValue(t, body, "extrafanart-migration-moved"); got != "2" {
		t.Errorf("moved tile: got %q, want 2", got)
	}
	if strings.Contains(body, "sw-card-accent-amber") || strings.Contains(body, efRunBtn) {
		t.Error("a clean receipt has no amber accent and no Run button")
	}
	if _, extra := rootHashes(t, a.dir); len(extra) != 0 {
		t.Fatalf("the run reported success but %d files are still in extrafanart/", len(extra))
	}
	again := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusOK,
		"Nothing To Do", "No extrafanart files needed moving")
	if strings.Contains(again, "img0.jpg") {
		t.Error("the repeat run must not report the first run's files again")
	}
}

// One artist folder refuses the move: 207, the amber receipt, and the artist and
// file that failed are named with a readable reason.
func TestExtraFanartRunFragment_PartialRunIs207AndNamesTheFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	b := seedExtraFanartArtist(t, svc, "Bravo", 3)
	do := efMux(t, r)
	lockDir(t, b)

	body := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusMultiStatus,
		`id="extrafanart-migration-receipt"`, `role="alert"`, "sw-card-accent-amber", efFocus,
		"Partially Migrated", "not cleanly", "Bravo", "img2.jpg", "the move failed")
	for id, want := range map[string]string{"extrafanart-migration-moved": "2", "extrafanart-migration-failed": "3"} {
		if got := statValue(t, body, id); got != want {
			t.Errorf("tile %s: got %q, want %q", id, got, want)
		}
	}
	if strings.Contains(body, b.dir) {
		t.Error("the receipt leaked an artist path")
	}
}

// A run refused because another holds the slot: 409 with the running notice,
// which takes focus; there is no receipt, because nothing ran.
func TestExtraFanartRunFragment_ConflictIs409RunningNotice(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	do := efMux(t, r)
	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()

	body := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusConflict,
		`id="extrafanart-migration-running"`, "A Migration Is Already Running", efFocus)
	if strings.Contains(body, "extrafanart-migration-receipt") || strings.Contains(body, "Alpha") {
		t.Error("a refused run has no receipt and lists nothing")
	}
	if _, extra := rootHashes(t, a.dir); len(extra) != 2 {
		t.Errorf("a refused run moved files: %d left in extrafanart/", len(extra))
	}
}

// A run stopped early (shutdown reaches it at the second artist): 500, and the
// receipt says it stopped, names the stranded files, and never shows the
// "nothing to migrate" empty state.
func TestExtraFanartRunFragment_StoppedRunIs500WithReceipt(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	b := seedExtraFanartArtist(t, svc, "Bravo", 3)
	do := efMux(t, r)
	r.extraFanartBeforeApply = func(id string) {
		if id == b.id {
			r.webhookShutdownCancel()
		}
	}

	body := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusInternalServerError,
		`id="extrafanart-migration-receipt"`, `role="alert"`, "The Run Stopped Early", efFocus,
		"Files already moved stay moved", "Bravo", "the run stopped before this file")
	if got := statValue(t, body, "extrafanart-migration-moved"); got != "2" {
		t.Errorf("moved tile: got %q, want 2 (Alpha finished before the stop)", got)
	}
	if strings.Contains(body, "extrafanart-migration-empty") || strings.Contains(body, "The Preview Could Not Finish") {
		t.Error("a stopped RUN must not read as an empty library or a failed preview")
	}
}

// A run that stops before it reaches any artist (here the settings lookup fails)
// has no rows. Its receipt must not show the table's empty state, which would
// say there is nothing to migrate. Called directly: the mux cannot authenticate
// once the database is closed.
func TestExtraFanartRunFragment_StoppedBeforeAnyArtistShowsNoEmptyState(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	if err := r.db.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, efRunPath, strings.NewReader("dry_run=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handleExtraFanartMigrationRun(w, withI18nCtx(t, req.WithContext(adminContext())))
	body := wantFragment(t, w, http.StatusInternalServerError, "The Run Stopped Early", efFocus)
	if strings.Contains(body, "extrafanart-migration-empty") || strings.Contains(body, `id="extrafanart-migration-table"`) {
		t.Error("a run that reached nothing must not show the table or its empty state")
	}
}

// The page's client gives up mid-run (a proxy timeout): the run still finishes,
// and the undeliverable receipt is logged as the client leaving, not as a fault.
func TestExtraFanartRunFragment_ClientGoneIsNotAServerError(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	var logs bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(adminContext())
	r.extraFanartBeforeApply = func(string) { cancel() }
	req := httptest.NewRequest(http.MethodPost, efRunPath, strings.NewReader("dry_run=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	r.handleExtraFanartMigrationRun(httptest.NewRecorder(), withI18nCtx(t, req.WithContext(ctx)))
	if _, extra := rootHashes(t, a.dir); len(extra) != 0 {
		t.Fatalf("the run must finish without its client; %d files left in extrafanart/", len(extra))
	}
	if out := logs.String(); !strings.Contains(out, "the client went away") || strings.Contains(out, "level=ERROR") {
		t.Errorf("want the client's departure logged at info and no error, got:\n%s", out)
	}
}

// Without HX-Request the endpoint answers exactly as before: the JSON bytes are
// pinned for the two outcomes that are fully deterministic, and a form-encoded
// post (a non-htmx browser form) still gets JSON.
func TestExtraFanartRun_JSONUnchangedWithoutHX(t *testing.T) {
	t.Parallel()
	r, _ := testRouterForBackdrops(t)
	do := efMux(t, r)
	const empty = `{"dry_run":%t,"status":"nothing_to_do","artists_scanned":0,"artists_skipped_missing":0,"artists_with_files":0,` +
		`"planned":0,"skipped_identical":0,"problems":0,"moved":0,"failed":0,"artists":[]}` + "\n"
	check := func(name string, w *httptest.ResponseRecorder, code int, want string) {
		t.Helper()
		if v := w.Header().Get("Vary"); v != "HX-Request" {
			t.Errorf("%s: want Vary: HX-Request, got %q", name, v)
		}
		if ct := w.Header().Get("Content-Type"); w.Code != code || ct != "application/json" || w.Body.String() != want {
			t.Errorf("%s: want %d application/json %q, got %d %q %q", name, code, want, w.Code, ct, w.Body.String())
		}
	}
	check("preview", do(http.MethodPost, efRunPath, `{"dry_run": true}`, false), http.StatusOK, fmt.Sprintf(empty, true))
	check("live", do(http.MethodPost, efRunPath, `{"dry_run": false}`, false), http.StatusOK, fmt.Sprintf(empty, false))

	form := httptest.NewRequest(http.MethodPost, efRunPath, strings.NewReader("dry_run=false"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fw := httptest.NewRecorder()
	r.handleExtraFanartMigrationRun(fw, form.WithContext(adminContext()))
	check("form without HX", fw, http.StatusOK, fmt.Sprintf(empty, false))

	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()
	check("conflict", do(http.MethodPost, efRunPath, `{"dry_run": false}`, false), http.StatusConflict,
		`{"dry_run":false,"status":"running","artists_scanned":0,"artists_skipped_missing":0,"artists_with_files":0,`+
			`"planned":0,"skipped_identical":0,"problems":0,"moved":0,"failed":0,"artists":[],`+
			`"error":"an extrafanart migration is already in progress"}`+"\n")
}

// A live run where the only artist's folder is missing checked nothing: 200,
// the neutral receipt that says so, never "nothing to do".
func TestExtraFanartRunFragment_MissingFolderIsNothingChecked(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	g := &artist.Artist{Name: "Ghost", SortName: "Ghost", Path: filepath.Join(t.TempDir(), "unmounted", "Ghost")}
	if err := svc.Create(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	do := efMux(t, r)
	// The skipped-artists notice is its own amber card; the receipt itself is the
	// neutral role=status one (its accent per status is the view test's job).
	wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusOK,
		"Nothing Checked", "some artist folders were not found", `role="status"`, `id="extrafanart-migration-skipped"`)
}

// A live run where every move is blocked (a symlink in extrafanart/) moves
// nothing and answers 207 failed: the amber receipt names the file and shows
// the problem count, though no file counts as "failed".
func TestExtraFanartRunFragment_AllBlockedIs207Failed(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 0)
	if err := os.Symlink(filepath.Join(a.dir, "backdrop.jpg"), filepath.Join(a.dir, "extrafanart", "img0.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	do := efMux(t, r)
	body := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusMultiStatus,
		`role="alert"`, "sw-card-accent-amber", "no file was moved", "Alpha", "img0.jpg", `id="extrafanart-migration-problems"`)
	if got := statValue(t, body, "extrafanart-migration-moved"); got != "0" {
		t.Errorf("moved tile: got %q, want 0", got)
	}
}

// A live run that only hit a planning error ends failed with Moved == 0 and
// Failed == 0. The receipt must still show the problem count, or it reads as
// "0 files not moved" under a failed heading.
func TestExtraFanartRunFragment_PlanErrorOnlyShowsProblemCount(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedPlanFailArtist(t, svc, "Broken")
	do := efMux(t, r)
	body := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusMultiStatus,
		"no file was moved", `id="extrafanart-migration-problems"`, "1 file or artist has a problem")
	if got := statValue(t, body, "extrafanart-migration-failed"); got != "0" {
		t.Errorf("precondition: failed tile %q, want 0 (the planning-error-only state)", got)
	}
}

// The Run button is offered only for a finished preview with something to move.
// When offered it posts a LIVE run to the API route and swaps the page body.
func TestExtraFanartPage_RunButtonOnlyForAFinishedPlan(t *testing.T) {
	t.Parallel()
	t.Run("planned", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 2)
		body := efMux(t, r)(http.MethodGet, efPagePath, "", false).Body.String()
		for _, want := range []string{
			efRunBtn, `hx-post="` + efRunPath + `"`, `hx-vals='{"dry_run": false}'`, `hx-target="#extrafanart-migration-body"`,
			`hx-swap="outerHTML"`, `hx-confirm="Move the extrafanart files now?`, `hx-disabled-elt="this"`,
			`hx-on::before-swap="swExtraFanartRun.beforeSwap(event)"`, "window.swExtraFanartRun",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page is missing %q", want)
			}
		}
		if strings.Contains(body, efFocus) || strings.Contains(body, "extrafanart-migration-receipt") {
			t.Error("a page load is a preview: no receipt, and nothing may take focus")
		}
	})
	t.Run("nothing planned", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 0)
		body := efMux(t, r)(http.MethodGet, efPagePath, "", false).Body.String()
		if !strings.Contains(body, `id="extrafanart-migration-empty"`) {
			t.Fatal("precondition: want the empty preview")
		}
		if strings.Contains(body, efRunBtn) {
			t.Error("nothing is planned, so there must be no Run button")
		}
	})
	t.Run("preview aborted with rows", func(t *testing.T) {
		t.Parallel()
		r, svc := testRouterForBackdrops(t)
		seedExtraFanartArtist(t, svc, "Alpha", 2)
		seedPlanFailArtist(t, svc, "Broken")
		filler := seedExtraFanartArtist(t, svc, "Zfiller0", 0)
		for i := 1; i < 201; i++ { // a second list page, which fails once the database closes
			n := fmt.Sprintf("Zfiller%03d", i)
			if err := svc.Create(context.Background(), &artist.Artist{Name: n, SortName: n, Path: filler.dir}); err != nil {
				t.Fatal(err)
			}
		}
		do := efMux(t, r)
		var once bool
		r.logger = slog.New(closeDBOnWarn{Handler: slog.NewTextHandler(&bytes.Buffer{}, nil), db: r.db, once: &once})
		body := do(http.MethodGet, efPagePath, "", false).Body.String()
		if !once || !strings.Contains(body, "Will Move") || !strings.Contains(body, `id="extrafanart-migration-error"`) {
			t.Fatalf("precondition: want a stopped preview that still lists planned rows (closed=%t)", once)
		}
		if strings.Contains(body, efRunBtn) {
			t.Error("a stopped preview is not a complete plan, so there must be no Run button")
		}
	})
}

// The receipt and the running notice hold a focused heading, so the live-region
// role must sit on the paragraph BESIDE it, never on a card that contains it: a
// focused element inside a live region is announced twice.
func TestExtraFanartRunFragment_LiveRegionNeverContainsTheFocusedHeading(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	do := efMux(t, r)

	receipt := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusOK, efFocus)
	if card := tagOf(t, receipt, `id="extrafanart-migration-receipt"`); strings.Contains(card, "role=") {
		t.Errorf("the receipt card holds the focused heading and must not be a live region: %s", card)
	}
	if !strings.Contains(tagOf(t, receipt, `id="extrafanart-migration-receipt-body"`), `role="status"`) {
		t.Error("the receipt body paragraph must carry the live-region role")
	}

	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()
	running := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusConflict, efFocus)
	if card := tagOf(t, running, `id="extrafanart-migration-running"`); strings.Contains(card, "role=") {
		t.Errorf("the running notice holds the focused heading and must not be a live region: %s", card)
	}
	if !strings.Contains(running, `role="alert">An extrafanart migration is running`) {
		t.Error("the running notice's body paragraph must carry the live-region role")
	}
}

// tagOf returns the opening tag (through its closing ">") that contains marker.
func tagOf(t *testing.T, html, marker string) string {
	t.Helper()
	at := strings.Index(html, marker)
	if at < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	start := strings.LastIndex(html[:at], "<")
	end := strings.Index(html[at:], ">")
	if start < 0 || end < 0 {
		t.Fatalf("no tag around %q", marker)
	}
	return html[start : at+end+1]
}

// The browser spec embeds the running notice as a fixture file. The real handler
// must render exactly that, so the fixture cannot drift from production.
func TestExtraFanartRunFragment_RunningNoticeMatchesTheSpecFixture(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(filepath.Join("..", "..", "tests", "a11y", "fixtures", "extrafanart-running-notice.html"))
	if err != nil {
		t.Fatal(err)
	}
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	do := efMux(t, r)
	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()
	got := wantFragment(t, do(http.MethodPost, efRunPath, "dry_run=false", true), http.StatusConflict)
	if strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
		t.Errorf("the running notice drifted from tests/a11y/fixtures/extrafanart-running-notice.html\n got: %s\nwant: %s", got, want)
	}
}
