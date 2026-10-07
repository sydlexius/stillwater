package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
)

func getExtraFanartPage(t *testing.T, r *Router, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := withI18nCtx(t, httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/extrafanart-migration", nil))
	w := httptest.NewRecorder()
	r.handleExtraFanartMigrationPage(w, req)
	return w
}

// statValue returns the text of a summary tile by its id.
func statValue(t *testing.T, body, id string) string {
	t.Helper()
	m := regexp.MustCompile(`id="` + id + `">([^<]*)<`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no summary tile %q", id)
	}
	return m[1]
}

const efRowMarker = "border-t border-gray-100" // one per table row

// An admin sees one row per seeded file, naming its artist and file, and the
// one-way / nothing-deleted paragraph sits above the table.
func TestExtraFanartPage_AdminSeesOneRowPerFile(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	// Precondition from the POST engine, not from the page under test.
	pre := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if pre.Planned != 5 || pre.ArtistsWithFiles != 2 {
		t.Fatalf("precondition: want 5 planned files across 2 artists, got %d across %d", pre.Planned, pre.ArtistsWithFiles)
	}

	w := getExtraFanartPage(t, r, adminContext())
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if n := strings.Count(body, efRowMarker); n != 5 {
		t.Errorf("want 5 table rows, got %d", n)
	}
	for _, want := range []string{"Alpha", "Bravo", "img0.jpg", "img1.jpg", "img2.jpg", "Will Move"} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q", want)
		}
	}
	if strings.Contains(body, "extrafanart-migration-empty") || strings.Contains(body, "extrafanart-migration-error") {
		t.Error("a populated preview must not render the empty or error state")
	}
	// The tiles carry the engine's own counts (2 artists, 5 files), so a swap shows.
	for id, want := range map[string]string{"extrafanart-migration-artists": "2", "extrafanart-migration-moves": "5", "extrafanart-migration-identical": "0"} {
		if got := statValue(t, body, id); got != want {
			t.Errorf("tile %s: got %q, want %q", id, got, want)
		}
	}
	// Every file's destination from the engine's own preview appears in its row.
	dests := 0
	for _, art := range pre.Artists {
		for _, f := range art.Files {
			if f.Destination == "" {
				t.Fatalf("precondition: the preview gave %s no destination", f.File)
			}
			dests++
			if !strings.Contains(body, `<td class="px-3 py-2">`+f.Destination+`</td>`) {
				t.Errorf("the table does not show destination %q", f.Destination)
			}
		}
	}
	if dests != 5 {
		t.Fatalf("precondition: want 5 destinations, got %d", dests)
	}
	warn, table := strings.Index(body, "extrafanart-migration-warning"), strings.Index(body, `id="extrafanart-migration-table"`)
	if warn < 0 || table < 0 || warn > table {
		t.Errorf("the one-way paragraph must come before the table (warning at %d, table at %d)", warn, table)
	}
	if !strings.Contains(body, "one-way operation") || !strings.Contains(body, "Nothing is deleted") {
		t.Error("the one-way and nothing-deleted copy is missing")
	}
	if strings.Contains(body, a.dir) || strings.Contains(body, b.dir) {
		t.Error("the page leaked an artist path")
	}
}

// efFSInventory records mode, size, mtime and a content hash of every path under
// roots, so a chmod, a touch or a rewrite shows even when the content is unchanged.
func efFSInventory(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			sum := ""
			if fi.Mode().IsRegular() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				h := sha256.Sum256(b)
				sum = hex.EncodeToString(h[:8])
			}
			out[p] = fmt.Sprintf("%v|%d|%d|%s", fi.Mode(), fi.Size(), fi.ModTime().UnixNano(), sum)
			return nil
		})
		if err != nil {
			t.Fatalf("inventorying %s: %v", root, err)
		}
	}
	return out
}

// efDBInventory records a row count and a hash of every row of every table.
func efDBInventory(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	var tables []string
	func() {
		rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, n)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}()
	out := map[string]string{}
	for _, tb := range tables {
		func() {
			rs, err := db.Query(`SELECT * FROM "` + tb + `"`)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rs.Close() }()
			cols, _ := rs.Columns()
			var lines []string
			for rs.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rs.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				lines = append(lines, fmt.Sprintf("%v", vals))
			}
			sort.Strings(lines)
			h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
			out[tb] = fmt.Sprintf("%d rows %s", len(lines), hex.EncodeToString(h[:8]))
		}()
	}
	return out
}

func efDiff(before, after map[string]string) []string {
	var d []string
	for k, v := range before {
		if after[k] != v {
			d = append(d, fmt.Sprintf("%s: %q -> %q", k, v, after[k]))
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			d = append(d, fmt.Sprintf("%s: new %q", k, v))
		}
	}
	sort.Strings(d)
	return d
}

// Loading the page writes nothing, on every branch it can take. The library is
// mixed (movable files, an unplannable artist, an artist whose folder is gone,
// an identical copy), and the inventory covers every path's content, size, mode
// and mtime plus every row of every table, before and after three loads.
func TestExtraFanartPage_LoadWritesNothing(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	c := seedPlanFailArtist(t, svc, "Broken")
	gone := seedExtraFanartArtist(t, svc, "Gone", 1)
	if err := os.RemoveAll(gone.dir); err != nil {
		t.Fatal(err)
	}
	ident := seedExtraFanartArtist(t, svc, "Ident", 1)
	if err := os.WriteFile(filepath.Join(ident.dir, "extrafanart", "copy.jpg"), []byte("root-Ident"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []string{filepath.Dir(a.dir), filepath.Dir(b.dir), filepath.Dir(c.dir), filepath.Dir(ident.dir)}
	time.Sleep(20 * time.Millisecond) // so a touch after this point changes an mtime
	fsBefore, dbBefore := efFSInventory(t, roots...), efDBInventory(t, r.db)
	if len(fsBefore) < 20 || len(dbBefore) < 10 {
		t.Fatalf("precondition: the inventory is too thin to prove anything (fs %d, tables %d)", len(fsBefore), len(dbBefore))
	}
	for i := 0; i < 3; i++ {
		body := getExtraFanartPage(t, r, adminContext()).Body.String()
		for _, want := range []string{"img0.jpg", `id="extrafanart-migration-skipped"`, `id="extrafanart-migration-problems"`, "identical copy of an existing image"} {
			if !strings.Contains(body, want) {
				t.Fatalf("load %d: the mixed library did not reach its branches, missing %q", i, want)
			}
		}
	}
	if d := efDiff(fsBefore, efFSInventory(t, roots...)); len(d) > 0 {
		t.Errorf("loading the page changed the filesystem: %v", d)
	}
	if d := efDiff(dbBefore, efDBInventory(t, r.db)); len(d) > 0 {
		t.Errorf("loading the page changed the database: %v", d)
	}
}

// The skipped-artists notice reaches the page, as the amber notice card, with
// the right plural form: artists whose folders are gone were not checked.
func TestExtraFanartPage_SkippedArtistsNoticeReachesPage(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	for _, n := range []string{"Gone1", "Gone2"} {
		g := seedExtraFanartArtist(t, svc, n, 1)
		if err := os.RemoveAll(g.dir); err != nil {
			t.Fatal(err)
		}
	}
	if pre := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json")); pre.ArtistsSkippedMissing != 2 {
		t.Fatalf("precondition: want 2 skipped artists, got %d", pre.ArtistsSkippedMissing)
	}
	body := getExtraFanartPage(t, r, adminContext()).Body.String()
	m := regexp.MustCompile(`<div id="extrafanart-migration-skipped" class="([^"]*)" role="alert">`).FindStringSubmatch(body)
	if m == nil || m[1] != "sw-card rounded-lg px-3 py-2 border-l-4 sw-card-accent-amber" {
		t.Fatalf("want the skipped notice as the amber alert card, got %v", m)
	}
	if !strings.Contains(body, "2 artists were skipped") {
		t.Error("the skipped count (plural) is missing")
	}
}

// A non-admin gets a 403 with no plan; an anonymous visitor gets no plan either.
func TestExtraFanartPage_GatedToAdmins(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)

	operator := middleware.WithTestRole(middleware.WithTestUserID(context.Background(), "u1"), "operator")
	w := getExtraFanartPage(t, r, operator)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin: want 403, got %d", w.Code)
	}
	for name, resp := range map[string]*httptest.ResponseRecorder{"operator": w, "anonymous": getExtraFanartPage(t, r, context.Background())} {
		body := resp.Body.String()
		if strings.Contains(body, "Alpha") || strings.Contains(body, "extrafanart-migration-table") || strings.Contains(body, "img0.jpg") {
			t.Errorf("%s response carries the plan", name)
		}
	}
}

// While the singleton is held (a parked run), the page shows the running notice and no table.
func TestExtraFanartPage_RunningNoticeWhenSingletonHeld(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()

	body := getExtraFanartPage(t, r, adminContext()).Body.String()
	if !strings.Contains(body, `id="extrafanart-migration-running"`) {
		t.Error("want the running notice")
	}
	if !strings.Contains(body, "migration is running") {
		t.Error("the running notice must say that a migration is running")
	}
	if strings.Contains(body, "extrafanart-migration-table") || strings.Contains(body, "Alpha") {
		t.Error("the running notice must not come with a plan")
	}
}

// A preview that cannot start (here: the database is gone, so the profile lookup
// fails) shows the error card and no rows, never a "nothing to migrate" page.
func TestExtraFanartPage_PreviewFailureShowsErrorCard(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	if err := r.db.Close(); err != nil {
		t.Fatal(err)
	}
	body := getExtraFanartPage(t, r, adminContext()).Body.String()
	if !strings.Contains(body, `id="extrafanart-migration-error"`) {
		t.Fatal("want the error card")
	}
	if strings.Contains(body, efRowMarker) || strings.Contains(body, "extrafanart-migration-empty") {
		t.Error("a failed preview must show neither rows nor the empty state")
	}
}

// An artist whose files cannot be planned is a named row with a readable reason
// and the problems card, not a bare key.
func TestExtraFanartPage_PlanFailureRowIsLabelled(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedPlanFailArtist(t, svc, "Broken")
	body := getExtraFanartPage(t, r, adminContext()).Body.String()
	for _, want := range []string{"Broken", "the files could not be listed", `id="extrafanart-migration-problems"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q", want)
		}
	}
	if strings.Contains(body, "extrafanart_migration.") {
		t.Error("a bare i18n key leaked into the page")
	}
}

// The stable route is registered and not swallowed by GET /reports/{name}, and
// the /next twin re-dispatches to the same page.
func TestExtraFanartPage_RoutedAndNextTwin(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	if _, err := r.authService.Setup(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := r.db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	token, _, err := r.authService.CreateAPIToken(context.Background(), userID, "ef-page", string(auth.ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	r.ux = "next" // the UX middleware serves /next/* only in the next or dual channel
	hctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := r.Handler(hctx)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/reports/extrafanart-migration", "/next/reports/extrafanart-migration"} {
		w := get(path)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, `id="extrafanart-migration-table"`) || !strings.Contains(body, "img0.jpg") {
			t.Errorf("%s: want the preview page, got %d %.200s", path, w.Code, body)
		}
	}
}

// closeDBOnWarn closes the router's database when the unplannable artist's
// planning failure is logged, so the NEXT artist-list page fails.
type closeDBOnWarn struct {
	slog.Handler
	db   *sql.DB
	once *bool
}

func (h closeDBOnWarn) Handle(ctx context.Context, rec slog.Record) error {
	if strings.Contains(rec.Message, "planning failed") && !*h.once {
		*h.once = true
		_ = h.db.Close()
	}
	return h.Handler.Handle(ctx, rec)
}

// A preview that stops partway (the artist list fails after page 1) still shows
// the rows it reached, and the error card says they are not a complete plan.
func TestExtraFanartPage_StoppedPreviewWithPartialRowsShowsErrorCard(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	seedPlanFailArtist(t, svc, "Broken")
	filler := seedExtraFanartArtist(t, svc, "Zfiller0", 0)
	// More than one list page (200) so a second page is requested, and fails.
	for i := 1; i < 201; i++ {
		n := fmt.Sprintf("Zfiller%03d", i)
		if err := svc.Create(context.Background(), &artist.Artist{Name: n, SortName: n, Path: filler.dir}); err != nil {
			t.Fatal(err)
		}
	}
	var once bool
	prev := r.logger
	r.logger = slog.New(closeDBOnWarn{Handler: slog.NewTextHandler(&bytes.Buffer{}, nil), db: r.db, once: &once})
	t.Cleanup(func() { r.logger = prev })

	body := getExtraFanartPage(t, r, adminContext()).Body.String()
	if !once {
		t.Fatal("precondition: the planning failure never fired the database close")
	}
	if !strings.Contains(body, "img0.jpg") || !strings.Contains(body, `id="extrafanart-migration-table"`) {
		t.Fatal("precondition: the stopped preview should still show the rows it reached")
	}
	if !strings.Contains(body, `id="extrafanart-migration-error"`) {
		t.Error("a stopped preview with partial rows must show the error card")
	}
}

// The page extends the write deadline like the POST handler: a preview that
// outlasts the server's WriteTimeout still delivers its page. The handler is
// wrapped so it starts after the server's 300ms deadline has already passed.
func TestExtraFanartPage_ExtendsWriteDeadline(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(600 * time.Millisecond)
		req = withI18nCtx(t, req.WithContext(adminContext()))
		r.handleExtraFanartMigrationPage(w, req)
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("the page was lost to the server WriteTimeout: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil || !strings.Contains(buf.String(), "img0.jpg") {
		t.Fatalf("the page body was cut off (err %v)", err)
	}
}

// When the deadline cannot be extended the page says so in the log, never silently.
func TestExtraFanartPage_WarnsWhenDeadlineCannotBeExtended(t *testing.T) {
	t.Parallel()
	r, _ := testRouterForBackdrops(t)
	var buf bytes.Buffer
	prev := r.logger
	r.logger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { r.logger = prev })
	getExtraFanartPage(t, r, adminContext()) // a ResponseRecorder has no write deadline
	if !strings.Contains(buf.String(), "could not extend the write deadline") {
		t.Errorf("want a Warn about the write deadline, log was %q", buf.String())
	}
}

// A closed tab (canceled request context) logs one Info line and nothing else:
// no ERROR lines and no attempt to render a page or a 500 to nobody.
func TestExtraFanartPage_CanceledPreviewLogsInfoAndStops(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	var buf bytes.Buffer
	prev := r.logger
	r.logger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { r.logger = prev })
	ctx, cancel := context.WithCancel(adminContext())
	cancel()

	w := getExtraFanartPage(t, r, ctx)
	if !strings.Contains(buf.String(), "canceled by the client") {
		t.Errorf("want the Info line, log was %q", buf.String())
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("a closed tab must not log an ERROR, log was %q", buf.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("nothing should be rendered for a canceled request, got %d bytes", w.Body.Len())
	}
}

// parkOnWarn blocks the first "planning failed" log (the unplannable artist) until
// released, which parks a preview in the middle of its dry run.
type parkOnWarn struct {
	slog.Handler
	parked  chan struct{}
	release chan struct{}
	first   *atomic.Bool
}

func (h parkOnWarn) Handle(ctx context.Context, rec slog.Record) error {
	if strings.Contains(rec.Message, "planning failed") {
		// Only the first caller parks; a sync.Once would block the others too.
		if h.first.CompareAndSwap(false, true) {
			close(h.parked)
			<-h.release
		}
	}
	return h.Handler.Handle(ctx, rec)
}

// A preview that is open (parked mid-dry-run) never makes a live run answer 409,
// and a second page load meanwhile shows the running notice instead of a second
// preview. Both the POST run and the second load must finish while the first
// preview is still parked.
func TestExtraFanartPage_OpenPreviewDoesNotBlockALiveRun(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedPlanFailArtist(t, svc, "Broken")
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	parked, release := make(chan struct{}), make(chan struct{})
	r.logger = slog.New(parkOnWarn{Handler: slog.NewTextHandler(&bytes.Buffer{}, nil), parked: parked, release: release, first: &atomic.Bool{}})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- getExtraFanartPage(t, r, adminContext()) }()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: the preview never reached the parked state")
	}
	r.extraFanartMu.Lock()
	previewing := r.extraFanartPreviewing
	r.extraFanartMu.Unlock()
	if !previewing {
		t.Fatal("precondition: a parked preview must hold the preview guard")
	}

	second := getExtraFanartPage(t, r, adminContext()).Body.String()
	if !strings.Contains(second, `id="extrafanart-migration-running"`) || strings.Contains(second, "extrafanart-migration-table") {
		t.Error("a second preview while one is open must show the running notice and no table")
	}
	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	if w.Code == http.StatusConflict {
		t.Fatalf("an open preview made a live run answer 409: %s", w.Body.String())
	}
	if res := decodeRun(t, w); res.Moved != 2 {
		t.Errorf("the live run should have moved Alpha's 2 files, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "extrafanart")); !os.IsNotExist(err) {
		t.Errorf("the live run did not complete while the preview was open (err %v)", err)
	}

	close(release)
	first := (<-done).Body.String()
	if strings.Contains(first, "extrafanart_migration.") {
		t.Error("the parked preview rendered a bare key")
	}
	r.extraFanartMu.Lock()
	still := r.extraFanartPreviewing || r.extraFanartRunning
	r.extraFanartMu.Unlock()
	if still {
		t.Error("a guard was left held after both finished")
	}
}
