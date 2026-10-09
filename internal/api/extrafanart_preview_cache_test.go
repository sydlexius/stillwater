package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingWarn counts the "planning failed" Warn the unplannable fixture artist
// logs once per REAL walk of the disk. It is the proof, from outside the cache's
// own bookkeeping, of how many times the disk was actually walked.
type countingWarn struct {
	slog.Handler
	walks *atomic.Int32
}

func (h countingWarn) Handle(ctx context.Context, rec slog.Record) error {
	if strings.Contains(rec.Message, "planning failed") {
		h.walks.Add(1)
	}
	return h.Handler.Handle(ctx, rec)
}

func countWalks(r *Router) *atomic.Int32 {
	n := &atomic.Int32{}
	r.logger = slog.New(countingWarn{Handler: slog.NewTextHandler(io.Discard, nil), walks: n})
	return n
}

func postDryRun(r *Router) *httptest.ResponseRecorder {
	return postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json")
}

func postLive(r *Router) *httptest.ResponseRecorder {
	return postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
}

func snapshotPresent(r *Router) bool {
	r.extraFanartPreview.mu.Lock()
	defer r.extraFanartPreview.mu.Unlock()
	return r.extraFanartPreview.snap != nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func waitParked(t *testing.T, parked chan struct{}) {
	t.Helper()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: the preview never reached the parked state")
	}
}

// Cold GET, warm GET (served from the cache) and a refreshing POST dry run write
// nothing: every file's content, size, mode and mtime and every database row
// match before and after. The warm GET is proven a hit by the walk counter (the
// unplannable artist logs once per real walk), and then by the disk itself: a file
// deleted after the cold load is still listed by the warm GET and gone only after
// the refresh.
func TestExtraFanartPreviewCache_ColdWarmAndRefreshWriteNothing(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	walks := countWalks(r)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	c := seedPlanFailArtist(t, svc, "Broken")
	ident := seedExtraFanartArtist(t, svc, "Ident", 1)
	if err := os.WriteFile(filepath.Join(ident.dir, "extrafanart", "copy.jpg"), []byte("root-Ident"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []string{filepath.Dir(a.dir), filepath.Dir(b.dir), filepath.Dir(c.dir), filepath.Dir(ident.dir)}
	time.Sleep(20 * time.Millisecond) // so a touch after this point changes an mtime
	fsBefore, dbBefore := efFSInventory(t, roots...), efDBInventory(t, r.db)
	if len(fsBefore) < 15 || len(dbBefore) < 10 {
		t.Fatalf("precondition: the inventory is too thin to prove anything (fs %d, tables %d)", len(fsBefore), len(dbBefore))
	}

	cold := getExtraFanartPage(t, r, adminContext()).Body.String()
	if n := walks.Load(); n != 1 {
		t.Fatalf("precondition: the cold load should walk the disk once, walked %d times", n)
	}
	warm := getExtraFanartPage(t, r, adminContext()).Body.String()
	if n := walks.Load(); n != 1 {
		t.Errorf("the warm load walked the disk again (%d walks): it was not served from the cache", n)
	}
	if warm != cold {
		t.Error("the cached page differs from the page it was cached from")
	}
	if !strings.Contains(cold, "img2.jpg") {
		t.Fatal("precondition: the fixture should list Alpha's img2.jpg")
	}
	w := postDryRun(r)
	if w.Code != http.StatusOK || walks.Load() != 2 {
		t.Errorf("a POST dry run must always read the disk: code %d, walks %d", w.Code, walks.Load())
	}
	planned := decodeRun(t, w).Planned
	if d := efDiff(fsBefore, efFSInventory(t, roots...)); len(d) > 0 {
		t.Errorf("cold, warm and refreshing loads changed the filesystem: %v", d)
	}
	if d := efDiff(dbBefore, efDBInventory(t, r.db)); len(d) > 0 {
		t.Errorf("cold, warm and refreshing loads changed the database: %v", d)
	}

	// The disk itself shows the warm load is a cache hit: delete a file, and the
	// next load still lists it; only a refresh drops it.
	if err := os.Remove(filepath.Join(a.dir, "extrafanart", "img2.jpg")); err != nil {
		t.Fatal(err)
	}
	if body := getExtraFanartPage(t, r, adminContext()).Body.String(); !strings.Contains(body, "img2.jpg") {
		t.Error("the warm load did not show the earlier rows after a file was deleted: it re-read the disk")
	}
	if res := decodeRun(t, postDryRun(r)); res.Planned != planned-1 {
		t.Errorf("the refresh should see the deleted file gone (want %d planned), got %d", planned-1, res.Planned)
	}
	if body := getExtraFanartPage(t, r, adminContext()).Body.String(); strings.Contains(body, "img2.jpg") {
		t.Error("after a refresh the page still shows the deleted file")
	}
}

// A cached preview can never be acted on: warm the cache, change the disk behind
// it (delete one planned file, make another an identical copy of a root file),
// run LIVE, and the moves match the DISK, not the cached counts.
func TestExtraFanartPreviewCache_StaleCacheCannotDriveALiveRun(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	if !strings.Contains(getExtraFanartPage(t, r, adminContext()).Body.String(), "img2.jpg") || !snapshotPresent(r) {
		t.Fatal("precondition: the cache should be warm with Alpha's 3 and Bravo's 2 files")
	}
	if err := os.Remove(filepath.Join(a.dir, "extrafanart", "img0.jpg")); err != nil {
		t.Fatal(err)
	}
	// Bravo's img0.jpg becomes byte-identical to Bravo's root backdrop.
	if err := os.WriteFile(filepath.Join(b.dir, "extrafanart", "img0.jpg"), []byte("root-Bravo"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := postLive(r)
	res := decodeRun(t, w)
	// Disk truth: Alpha has 2 files to move, Bravo has 1 to move and 1 identical.
	// The cache said 5 planned, 0 identical.
	if res.Moved != 3 || res.SkippedIdentical != 1 {
		t.Fatalf("the live run must act on the disk (3 moved, 1 identical), got moved=%d identical=%d: %s", res.Moved, res.SkippedIdentical, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(b.dir, "extrafanart", "img0.jpg")); err != nil {
		t.Errorf("the identical copy must be left in place: %v", err)
	}
	if snapshotPresent(r) {
		t.Error("a live run must leave no cached preview behind")
	}
	if body := getExtraFanartPage(t, r, adminContext()).Body.String(); strings.Contains(body, "img2.jpg") {
		t.Error("the page after the run still shows the pre-run rows")
	}
}

// A preview that straddles a live run is never stored, for both preview entry
// points (the page, and a refreshing POST dry run). The preview is parked mid-walk
// holding rows read BEFORE the run; the live run completes; the preview is released.
// The pre-run rows must not be resurrected. Also proves a parked dry run does not
// hold the run slot: the live run is not refused with 409.
func TestExtraFanartPreviewCache_PreviewStraddlingALiveRunIsDropped(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"page GET", "POST dry run"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, svc := testRouterForBackdrops(t)
			a := seedExtraFanartArtist(t, svc, "Alpha", 2)
			seedPlanFailArtist(t, svc, "Broken") // sorts after Alpha: Alpha's rows are read first
			parked, release := make(chan struct{}), make(chan struct{})
			r.logger = slog.New(parkOnWarn{Handler: slog.NewTextHandler(io.Discard, nil), parked: parked, release: release, first: &atomic.Bool{}})

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				if mode == "page GET" {
					done <- getExtraFanartPage(t, r, adminContext())
				} else {
					done <- postDryRun(r)
				}
			}()
			waitParked(t, parked)

			live := postLive(r)
			if live.Code == http.StatusConflict {
				t.Fatalf("a parked %s made the live run answer 409: %s", mode, live.Body.String())
			}
			if res := decodeRun(t, live); res.Moved != 2 {
				t.Fatalf("the live run should have moved Alpha's 2 files, got %+v", res)
			}
			if _, err := os.Stat(filepath.Join(a.dir, "extrafanart")); !os.IsNotExist(err) {
				t.Fatalf("precondition: the live run did not finish (err %v)", err)
			}

			close(release)
			first := <-done
			if mode == "page GET" && !strings.Contains(first.Body.String(), "img0.jpg") {
				t.Fatal("precondition: the parked preview should carry the pre-run rows")
			}
			if mode == "POST dry run" && decodeRun(t, first).Planned != 2 {
				t.Fatal("precondition: the parked preview should carry the pre-run rows")
			}
			if snapshotPresent(r) {
				t.Fatal("the preview that straddled a live run was stored: pre-run rows were resurrected")
			}
			if body := getExtraFanartPage(t, r, adminContext()).Body.String(); strings.Contains(body, "img0.jpg") {
				t.Error("the next load shows pre-run rows instead of the post-move disk")
			}
		})
	}
}

// While a live run is in progress a POST dry run reports the running notice (409).
func TestExtraFanartPreviewCache_DryRunDuringLiveRunReportsRunning(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	inApply, releaseApply := make(chan struct{}), make(chan struct{})
	r.extraFanartBeforeApply = func(string) { close(inApply); <-releaseApply }
	getExtraFanartPage(t, r, adminContext()) // warm the cache before the run
	if !snapshotPresent(r) {
		t.Fatal("precondition: the cache should be warm before the live run")
	}
	liveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { liveDone <- postLive(r) }()
	waitParked(t, inApply)

	if snapshotPresent(r) {
		t.Error("the live run must drop the cached preview when it STARTS, not only when it ends")
	}
	w := postDryRun(r)
	if res := decodeRun(t, w); w.Code != http.StatusConflict || res.Status != "running" {
		t.Errorf("a dry run during a live run must report running (409), got %d %+v", w.Code, res)
	}
	close(releaseApply)
	if res := decodeRun(t, <-liveDone); res.Moved != 2 {
		t.Errorf("the live run should have finished its 2 moves, got %+v", res)
	}
}

// The "as of" stamp is the time the preview BEGAN, not when it finished, and a
// cache hit keeps the original stamp.
func TestExtraFanartPreviewCache_AsOfIsTheBeginTime(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	seedPlanFailArtist(t, svc, "Broken")
	t1 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: t1}
	r.extraFanartNow = clk.now
	parked, release := make(chan struct{}), make(chan struct{})
	r.logger = slog.New(parkOnWarn{Handler: slog.NewTextHandler(io.Discard, nil), parked: parked, release: release, first: &atomic.Bool{}})

	type out struct {
		res *extraFanartRunResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := r.previewExtraFanartMigration(adminContext(), true)
		done <- out{res, err}
	}()
	waitParked(t, parked)
	clk.set(t1.Add(5 * time.Minute)) // the walk takes five minutes
	close(release)
	o := <-done
	if o.err != nil || !o.res.asOf.Equal(t1) {
		t.Fatalf("the stamp must be the begin time %v, got %v (err %v)", t1, o.res.asOf, o.err)
	}
	hit, err := r.previewExtraFanartMigration(adminContext(), true)
	if err != nil || !hit.asOf.Equal(t1) {
		t.Errorf("a cache hit must carry the original begin stamp %v, got %v (err %v)", t1, hit.asOf, err)
	}
}

// The store decision itself, with a clock that never moves, so an invalidation
// lands at the SAME instant as the begin (the tie a timestamp comparison cannot
// order and the generation counter exists to handle). Also every not-cached reason.
func TestExtraFanartPreviewCache_StoreDecision(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	frozen := func() time.Time { return at }
	clean := func() *extraFanartRunResult {
		return &extraFanartRunResult{DryRun: true, Planned: 1, Artists: []extraFanartArtistResult{{
			ArtistID: "a", Name: "A", Files: []extraFanartFileResult{{File: "x.jpg", Outcome: "planned"}}}}}
	}
	tests := []struct {
		name   string
		mutate func(c *extraFanartPreviewCache, res *extraFanartRunResult)
		want   string
	}{
		{"unchanged generation stores", func(*extraFanartPreviewCache, *extraFanartRunResult) {}, ""},
		{"invalidation at the same instant as begin drops", func(c *extraFanartPreviewCache, _ *extraFanartRunResult) { c.Invalidate() }, notCachedSuperseded},
		{"aborted run", func(_ *extraFanartPreviewCache, res *extraFanartRunResult) { res.aborted = true }, notCachedAborted},
		{"skipped missing folders", func(_ *extraFanartPreviewCache, res *extraFanartRunResult) { res.ArtistsSkippedMissing = 1 }, notCachedSkippedMissed},
		{"over the row bound", func(_ *extraFanartPreviewCache, res *extraFanartRunResult) {
			res.Artists[0].Files = make([]extraFanartFileResult, extraFanartPreviewMaxRows)
		}, notCachedTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var c extraFanartPreviewCache
			gen, begin := c.begin(frozen)
			res := clean()
			tc.mutate(&c, res)
			if got := c.store(gen, "k", begin, res); got != tc.want {
				t.Fatalf("store = %q, want %q", got, tc.want)
			}
			hit, ok := c.lookup("k")
			if ok != (tc.want == "") {
				t.Fatalf("lookup hit = %v, want %v", ok, tc.want == "")
			}
			if ok && !hit.asOf.Equal(at) {
				t.Errorf("stored as-of %v, want the begin time %v", hit.asOf, at)
			}
		})
	}

	// A stored snapshot is private: editing the caller's result, or a copy a reader
	// got, must not change what the next reader sees.
	var c extraFanartPreviewCache
	gen, begin := c.begin(frozen)
	res := clean()
	if why := c.store(gen, "k", begin, res); why != "" {
		t.Fatal(why)
	}
	res.Artists[0].Files[0].Outcome = "edited-by-producer"
	first, _ := c.lookup("k")
	first.Artists[0].Files[0].Outcome = "edited-by-reader"
	second, _ := c.lookup("k")
	if got := second.Artists[0].Files[0].Outcome; got != "planned" {
		t.Errorf("the snapshot shares a slice with a caller: next reader sees %q", got)
	}
	if _, ok := c.lookup("other"); ok {
		t.Error("a different convention key must miss")
	}
}

// A platform-profile change changes the convention key, so the next page load
// walks the disk again instead of showing a plan for the old naming.
func TestExtraFanartPreviewCache_ProfileChangeMisses(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	walks := countWalks(r)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	seedPlanFailArtist(t, svc, "Broken")
	getExtraFanartPage(t, r, adminContext())
	getExtraFanartPage(t, r, adminContext())
	if n := walks.Load(); n != 1 {
		t.Fatalf("precondition: the second load should be a hit, walks = %d", n)
	}
	activateFanartProfile(t, r.platformService, "backdrop.jpg", "fanart.jpg")
	getExtraFanartPage(t, r, adminContext())
	if n := walks.Load(); n != 2 {
		t.Errorf("a profile change must miss the cache (want 2 walks), got %d", n)
	}
}

// An artist whose folder is gone makes the result not cacheable (the notice says
// remount and reload, and a cached copy would make that false): the next load
// sees the disk as it is then.
func TestExtraFanartPreviewCache_SkippedFoldersAreNotCached(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	walks := countWalks(r)
	seedPlanFailArtist(t, svc, "Broken")
	g := seedExtraFanartArtist(t, svc, "Gone", 1)
	if err := os.RemoveAll(g.dir); err != nil {
		t.Fatal(err)
	}
	getExtraFanartPage(t, r, adminContext())
	getExtraFanartPage(t, r, adminContext())
	if n := walks.Load(); n != 2 || snapshotPresent(r) {
		t.Errorf("a preview with skipped folders must not be cached: walks %d, snapshot present %v", n, snapshotPresent(r))
	}
}

// The structural guarantee: the cache is touched ONLY by the preview path (which
// serves the page and the POST dry run) and, for Invalidate, by the live run. Any
// other reference, for example the live path reading it, fails here. The scan
// reads the production sources of this package, not just one file.
func TestExtraFanartPreviewCacheIsOnlyTouchedByThePreviewPath(t *testing.T) {
	t.Parallel()
	// function -> the cache methods it may call.
	allowed := map[string]map[string]bool{
		"previewExtraFanartMigration": {"begin": true, "lookup": true, "store": true},
		"runExtraFanartMigration":     {"Invalidate": true},
	}
	// Files that own the type: the cache itself, and the router field declaration.
	owners := map[string]bool{"extrafanart_preview_cache.go": true, "router.go": true}

	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	parsed := map[string]*ast.File{} // every non-test source file of the package
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		parsed[name] = f
	}
	seen := map[string]bool{} // "func.method" actually found, to rule out a vacuous pass
	for _, pkg := range []struct{ Files map[string]*ast.File }{{parsed}} {
		for fname, file := range pkg.Files {
			base := filepath.Base(fname)
			for _, decl := range file.Decls {
				fn, _ := decl.(*ast.FuncDecl)
				// First mark which selector is the receiver of a method call.
				methodOf := map[ast.Node]string{}
				ast.Inspect(decl, func(n ast.Node) bool {
					if outer, ok := n.(*ast.SelectorExpr); ok {
						if inner, ok := outer.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "extraFanartPreview" {
							methodOf[inner] = outer.Sel.Name
						}
					}
					return true
				})
				ast.Inspect(decl, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						if x.Sel.Name != "extraFanartPreview" || owners[base] {
							return true
						}
						owner := "<package level>"
						if fn != nil {
							owner = fn.Name.Name
						}
						m := methodOf[x]
						if !allowed[owner][m] {
							t.Errorf("%s: %s uses the extrafanart preview cache (%q); only the preview path and the live run's Invalidate may", base, owner, m)
						}
						seen[owner+"."+m] = true
					case *ast.Ident:
						if (x.Name == "extraFanartPreviewCache" || x.Name == "extraFanartPreviewSnapshot") && !owners[base] {
							t.Errorf("%s: references the cache type %s outside its owner files", base, x.Name)
						}
					}
					return true
				})
			}
		}
	}
	for _, want := range []string{"previewExtraFanartMigration.begin", "previewExtraFanartMigration.lookup", "previewExtraFanartMigration.store", "runExtraFanartMigration.Invalidate"} {
		if !seen[want] {
			t.Errorf("scan precondition: expected to find %s, so the scan is not looking at the right code", want)
		}
	}
}

// Timing, as #3434 asks. Skipped unless SW_PREVIEW_TIMING is set, so CI and the gate
// do not pay for it. Generates real files of realistic size on LOCAL temp storage,
// then times a cold preview, a warm cache hit and a forced refresh. Local SSD with a
// warm OS page cache understates a network share: treat the numbers as a floor.
//
//	SW_PREVIEW_TIMING=1 [SW_PREVIEW_ARTISTS=120] [SW_PREVIEW_FILES=5] [SW_PREVIEW_KB=1024] \
//	  go test -count=1 -run PreviewTiming -v ./internal/api/
func TestExtraFanartPreviewCache_Timing(t *testing.T) {
	if os.Getenv("SW_PREVIEW_TIMING") == "" {
		t.Skip("set SW_PREVIEW_TIMING=1 to run the preview timing measurement")
	}
	envInt := func(name string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
			return v
		}
		return def
	}
	artists, files, kb := envInt("SW_PREVIEW_ARTISTS", 120), envInt("SW_PREVIEW_FILES", 5), envInt("SW_PREVIEW_KB", 1024)
	r, svc := testRouterForBackdrops(t)
	for i := 0; i < artists; i++ {
		a := seedExtraFanartArtist(t, svc, fmt.Sprintf("Artist%03d", i), 0)
		for j := 0; j < files; j++ {
			f, err := os.Create(filepath.Join(a.dir, "extrafanart", fmt.Sprintf("img%d.jpg", j)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(f, rand.Reader, int64(kb)*1024); err != nil { // random: every file distinct
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	timed := func(useCache bool) (time.Duration, *extraFanartRunResult) {
		start := time.Now()
		res, err := r.previewExtraFanartMigration(context.Background(), useCache)
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(start), res
	}
	cold, res := timed(true)
	if res.Planned != artists*files {
		t.Fatalf("precondition: want %d planned files, got %d", artists*files, res.Planned)
	}
	warm, _ := timed(true)
	refresh, _ := timed(false)
	t.Logf("%d artists x %d files x %d KB (%.1f MB total): cold %s, warm hit %s, forced refresh %s",
		artists, files, kb, float64(artists*files*kb)/1024, cold, warm, refresh)
	if warm >= cold {
		t.Errorf("a cache hit (%s) should be faster than a cold preview (%s)", warm, cold)
	}
}
