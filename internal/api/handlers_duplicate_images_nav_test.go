package api

// handlers_duplicate_images_nav_test.go -- the sidebar's Images section,
// GET /api/v1/reports/duplicate-images/nav (#2608).
//
// Two requirements dominate the assertions here.
//
// First, "the serving path reads a cache and NEVER scans" is the requirement
// most likely to be silently violated, so it is asserted directly: the tests
// install counting scan sources and require the counter to stay at zero across
// the request. A regression that put a scan back on the render path would make
// the 60s sidebar poll re-hash the whole library from disk.
//
// Second, this endpoint OWNS the section's existence. Unlike the count
// endpoints it sits beside, an empty body here means "render no section at
// all" -- header included -- because a section without violations hides. The
// accepted consequence is that at a zero unmatched count the foreign-file
// allowlist is reachable only by direct URL.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/dupimages"
	"github.com/sydlexius/stillwater/internal/foreign"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
)

// dupNavRouter builds the minimum Router surface the handler needs and resets
// the process-wide count cache so tests do not bleed into one another.
//
// foreignRepo is left nil, so foreignSummaryForBanner reports 0 unmatched
// images. Tests that need a non-zero unmatched count use dupNavRouterWithForeign.
//
// These tests are NOT parallel: dupimages.Shared() is process-wide state.
func dupNavRouter(t *testing.T) *Router {
	t.Helper()
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	return &Router{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

// dupNavRouterWithForeign is dupNavRouter plus a real foreign-file repository,
// so the Unmatched half of the section can be driven from actual rows rather
// than a stub. Returns the router and the db for seeding.
func dupNavRouterWithForeign(t *testing.T) (*Router, *sql.DB) {
	t.Helper()
	r := dupNavRouter(t)
	db := newTestDB(t)
	r.foreignRepo = foreign.NewRepository(db)
	return r, db
}

// seedUnmatched inserts n foreign-file rows for one artist and asserts the
// count endpoint's data source actually sees them. Without this precondition
// an "Unmatched row rendered" assertion could pass vacuously against a repo
// that silently returned 0.
func seedUnmatched(t *testing.T, r *Router, db *sql.DB, n int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x','/x')`)
	for i := range n {
		name := "stray" + string(rune('a'+i)) + ".jpg"
		if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
			ArtistID: "a1", FilePath: "/x/" + name, FileName: name,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if got := r.foreignSummaryForBanner(context.Background()); got != n {
		t.Fatalf("precondition: unmatched count = %d, want %d", got, n)
	}
}

// dupNavReq attaches the REAL embedded English translator, not a stub, so the
// assertions below double as proof that the nav.images.* keys actually exist in
// internal/i18n/locales/en.json. With a stub translator a missing key would
// echo back as its own name and every label assertion would pass vacuously
// against text like "nav.images.library_duplicates".
func dupNavReq(t *testing.T, role string) *http.Request {
	t.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading embedded locales: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/duplicate-images/nav?ch=next", nil)
	ctx := middleware.WithTestUserID(req.Context(), "user-1")
	ctx = middleware.WithTestRole(ctx, role)
	ctx = i18n.WithTranslator(ctx, bundle.Translator("en"))
	return req.WithContext(ctx)
}

func embyCount(n int) dupimages.PlatformCount {
	return dupimages.PlatformCount{Type: "emby", Label: "Emby", Count: n}
}

func jellyfinCount(n int) dupimages.PlatformCount {
	return dupimages.PlatformCount{Type: "jellyfin", Label: "Jellyfin", Count: n}
}

// dupNavStubSources installs stub count sources that SURVIVE the handler.
//
// Necessary because the handler calls r.dupImageCache(), which re-installs the
// ROUTER's own sources (r.libraryDupCount / r.platformDupCounts) on first use,
// guarded by r.dupImageOnce. A test that only calls dupimages.Shared().SetSources
// has its stubs silently replaced on the first request, so the stub can never
// be invoked -- which makes an assertion that the stub was NOT called pass
// vacuously, and an assertion that it WAS called impossible.
//
// Consuming the Once with a no-op first leaves the stubs in place.
func dupNavStubSources(t *testing.T, r *Router, library dupimages.LibraryCountFn, platform dupimages.PlatformCountFn) {
	t.Helper()
	r.dupImageOnce.Do(func() {})
	dupimages.Shared().SetSources(library, platform)
}

// seedCounts primes the cache exactly as a completed background refresh would.
func seedCounts(library int, platforms ...dupimages.PlatformCount) {
	dupimages.Shared().Set(dupimages.Counts{Library: library, Platforms: platforms})
}

// THE HIDE BEHAVIOR at the handler seam (#2608, maintainer's spec). All three
// counts zero -> an EMPTY body, so the container renders nothing and the whole
// section, header included, disappears. Assert the chrome's absence by name so
// a regression that emits a bare header fails legibly here.
func TestDupImagesNav_HidesEntireSectionWhenAllCountsZero(t *testing.T) {
	r, _ := dupNavRouterWithForeign(t)
	seedCounts(0) // no duplicates; no foreign rows seeded, so unmatched is 0 too

	w := httptest.NewRecorder()
	r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if body != "" {
		t.Fatalf("body = %q, want empty so the whole section hides", body)
	}
	for _, forbidden := range []string{"sw-sidebar-section", ">Images<", "<ul", "sidebar-foreign-next"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("all-zero response emitted %q; the section must not render at all", forbidden)
		}
	}
}

// The counterpart: a single non-zero count brings the section BACK, header
// included, carrying only the offending row. Covers each of the three rows
// independently, so no one count is silently doing all the work.
func TestDupImagesNav_SectionReturnsWithOnlyTheOffendingRow(t *testing.T) {
	t.Run("unmatched only", func(t *testing.T) {
		r, db := dupNavRouterWithForeign(t)
		seedCounts(0)
		seedUnmatched(t, r, db, 3)

		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		body := w.Body.String()

		if !strings.Contains(body, ">Images<") {
			t.Errorf("section header missing though unmatched > 0:\n%s", body)
		}
		if !strings.Contains(body, `id="sidebar-foreign-next"`) {
			t.Errorf("Unmatched row missing:\n%s", body)
		}
		if !strings.Contains(body, `aria-label="3 unrecognized images in your library"`) {
			t.Errorf("count-bearing accessible name missing:\n%s", body)
		}
		if strings.Contains(body, "sidebar-image-duplicates-") {
			t.Errorf("a duplicate row rendered at zero count:\n%s", body)
		}
	})

	t.Run("library only", func(t *testing.T) {
		r := dupNavRouter(t)
		seedCounts(5)

		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		body := w.Body.String()

		if !strings.Contains(body, ">Images<") {
			t.Errorf("section header missing though library > 0:\n%s", body)
		}
		if !strings.Contains(body, `id="sidebar-image-duplicates-library"`) {
			t.Errorf("Library row missing:\n%s", body)
		}
		if strings.Contains(body, `id="sidebar-foreign-next"`) {
			t.Errorf("Unmatched row rendered at zero count:\n%s", body)
		}
		if strings.Contains(body, "sidebar-image-duplicates-emby") || strings.Contains(body, "sidebar-image-duplicates-jellyfin") {
			t.Errorf("a platform row rendered with no platform offenders:\n%s", body)
		}
	})

	t.Run("one platform only", func(t *testing.T) {
		r := dupNavRouter(t)
		seedCounts(0, jellyfinCount(3))

		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		body := w.Body.String()

		if !strings.Contains(body, ">Images<") {
			t.Errorf("section header missing though a platform > 0:\n%s", body)
		}
		if !strings.Contains(body, `id="sidebar-image-duplicates-jellyfin"`) {
			t.Errorf("Jellyfin row missing:\n%s", body)
		}
		if strings.Contains(body, `id="sidebar-image-duplicates-library"`) {
			t.Errorf("Library row rendered at zero count:\n%s", body)
		}
		if strings.Contains(body, `id="sidebar-image-duplicates-emby"`) {
			t.Errorf("Emby row rendered though Emby has no offenders:\n%s", body)
		}
	})
}

// THE MAINTAINER'S UAT JOURNEY (#2608). On the archive build, allow-listing
// every unmatched image left the Unmatched row VISIBLE with an empty pill,
// because the row was server-rendered and only its pill was swapped. That
// pinning is gone: the row lives in this fragment now, so clearing the ledger
// must make the ROW disappear -- and with the duplicate counts also zero, the
// IMAGES header must go with it.
//
// Asserting the populated state FIRST is what makes the empty state meaningful;
// without it a handler that always returned "" would pass the second half.
func TestDupImagesNav_AllowlistingEverythingRemovesRowThenSection(t *testing.T) {
	r, db := dupNavRouterWithForeign(t)
	seedCounts(0) // no duplicate offenders, so unmatched alone holds the section up
	seedUnmatched(t, r, db, 3)

	render := func() string {
		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		return w.Body.String()
	}

	// Precondition: the row and the section are actually there to be removed.
	before := render()
	if !strings.Contains(before, `id="sidebar-foreign-next"`) {
		t.Fatalf("precondition: Unmatched row missing before allowlisting:\n%s", before)
	}
	if !strings.Contains(before, ">Images<") {
		t.Fatalf("precondition: IMAGES header missing before allowlisting:\n%s", before)
	}

	// Allow-list everything: the app suppresses re-detection and drops the
	// ledger rows, which is what the count reads.
	for i := range 3 {
		name := "stray" + string(rune('a'+i)) + ".jpg"
		if err := r.foreignRepo.DeleteByPath(context.Background(), "a1", "/x/"+name); err != nil {
			t.Fatalf("allowlist %s: %v", name, err)
		}
	}
	if got := r.foreignSummaryForBanner(context.Background()); got != 0 {
		t.Fatalf("precondition: unmatched count = %d after allowlisting all, want 0", got)
	}

	// The row must be GONE -- not present with an empty pill, which is exactly
	// the archive behavior the maintainer rejected.
	after := render()
	if after != "" {
		t.Fatalf("after allowlisting everything the whole section must vanish, got:\n%s", after)
	}
	for _, forbidden := range []string{`id="sidebar-foreign-next"`, ">Images<", "sw-sidebar-count-pill"} {
		if strings.Contains(after, forbidden) {
			t.Errorf("allowlisted-clean render still contains %q; the row must not survive with an empty pill", forbidden)
		}
	}
}

func TestDupImagesNav_AllRowsWhenPopulated(t *testing.T) {
	r, db := dupNavRouterWithForeign(t)
	seedCounts(12, embyCount(4), jellyfinCount(2))
	seedUnmatched(t, r, db, 2)

	w := httptest.NewRecorder()
	r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
	body := w.Body.String()

	for _, want := range []string{
		// Section chrome, now owned by this response.
		`class="sw-sidebar-section"`,
		">Images<",
		// Rows.
		`id="sidebar-foreign-next"`,
		`id="sidebar-image-duplicates-library"`,
		`id="sidebar-image-duplicates-emby"`,
		`id="sidebar-image-duplicates-jellyfin"`,
		"/reports/foreign-files",
		"/reports/backdrop-duplicates",
		"/reports/platform-backdrop-duplicates",
		">Unmatched<",
		">Library Duplicates<",
		">Emby Duplicates<",
		">Jellyfin Duplicates<",
		">12<",
		">4<",
		">2<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
}

// Rows are named for the platform explicitly, and the accessible name says
// which platform is dirty.
func TestDupImagesNav_RowsNamePlatformExplicitly(t *testing.T) {
	r := dupNavRouter(t)
	seedCounts(0, embyCount(7))

	w := httptest.NewRecorder()
	r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
	body := w.Body.String()

	if !strings.Contains(body, ">Emby Duplicates<") {
		t.Errorf("row not named for the platform:\n%s", body)
	}
	if !strings.Contains(body, `aria-label="7 duplicate images on Emby"`) {
		t.Errorf("descriptive aria-label missing:\n%s", body)
	}
	// The old generic wording must not come back.
	if strings.Contains(body, ">Platform Duplicates<") || strings.Contains(body, ">Platforms<") {
		t.Errorf("generic platform wording rendered instead of the platform name:\n%s", body)
	}
}

// Visible labels stay TERSE so they cannot truncate in the sidebar; the
// count-bearing description lives on aria-label. This is the whole point of
// #2608 -- "Platform Backdrop Duplicates" truncated -- so pin it.
func TestDupImagesNav_VisibleLabelsStayTerse(t *testing.T) {
	r, db := dupNavRouterWithForeign(t)
	seedCounts(3, embyCount(2))
	seedUnmatched(t, r, db, 1)

	w := httptest.NewRecorder()
	r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
	body := w.Body.String()

	// The old long labels must not be visible text anymore.
	for _, tooLong := range []string{">Platform Backdrop Duplicates<", ">Backdrop Duplicates<", ">Unmatched Images<"} {
		if strings.Contains(body, tooLong) {
			t.Errorf("long label %q is back as visible text; it truncates in the sidebar", tooLong)
		}
	}
	// ...while the descriptive text is still reachable via the accessible name.
	for _, aria := range []string{
		`aria-label="1 unrecognized images in your library"`,
		`aria-label="3 duplicate backdrops in your libraries"`,
		`aria-label="2 duplicate images on Emby"`,
	} {
		if !strings.Contains(body, aria) {
			t.Errorf("descriptive accessible name %s missing:\n%s", aria, body)
		}
	}
}

// THE LOAD-BEARING TEST: serving the fragment must not invoke either scan.
func TestDupImagesNav_ServesCachedValueWithoutScanning(t *testing.T) {
	r := dupNavRouter(t)

	var libCalls, platCalls atomic.Int32
	dupimages.Shared().SetSources(
		func(context.Context) (int, error) { libCalls.Add(1); return 99, nil },
		func(context.Context) ([]dupimages.PlatformCount, error) {
			platCalls.Add(1)
			return []dupimages.PlatformCount{embyCount(99)}, nil
		},
	)
	seedCounts(12, embyCount(4))

	for range 10 {
		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		// Served numbers are the CACHED ones, not the sources' 99.
		if !strings.Contains(w.Body.String(), ">12<") {
			t.Fatalf("served a value other than the cached 12:\n%s", w.Body.String())
		}
	}

	if n := libCalls.Load(); n != 0 {
		t.Errorf("library scan ran %d times on the serving path; must be 0", n)
	}
	if n := platCalls.Load(); n != 0 {
		t.Errorf("platform scan ran %d times on the serving path; must be 0", n)
	}
}

// A warm-but-clean cache must NOT re-trigger a scan on every poll either:
// Computed=true is what distinguishes "known clean" from "never computed".
func TestDupImagesNav_CleanCacheDoesNotRetriggerScan(t *testing.T) {
	r := dupNavRouter(t)

	var libCalls atomic.Int32
	dupimages.Shared().SetSources(
		func(context.Context) (int, error) { libCalls.Add(1); return 0, nil },
		nil,
	)
	seedCounts(0)

	for range 10 {
		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
	}
	if n := libCalls.Load(); n != 0 {
		t.Errorf("a computed-clean cache triggered %d refreshes; want 0", n)
	}
}

func TestDupImagesNav_AdminOnly(t *testing.T) {
	for _, role := range []string{"user", "operator", ""} {
		t.Run("role="+role, func(t *testing.T) {
			r := dupNavRouter(t)
			seedCounts(12, embyCount(4))

			w := httptest.NewRecorder()
			r.handleDuplicateImagesNav(w, dupNavReq(t, role))

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for role %q", w.Code, role)
			}
			// No section markup may leak -- not a row, and not the header
			// either. A non-admin must get no Images section, not an empty one.
			for _, forbidden := range []string{"Duplicates<", ">Images<", "sw-sidebar-section"} {
				if strings.Contains(w.Body.String(), forbidden) {
					t.Fatalf("section markup %q leaked to a non-admin: %s", forbidden, w.Body.String())
				}
			}
		})
	}
}

// A cold cache answers immediately with an empty body (nothing known yet) and
// kicks the scan into the background rather than blocking the render.
func TestDupImagesNav_ColdCacheAnswersEmptyAndDoesNotBlock(t *testing.T) {
	r := dupNavRouter(t)

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	dupimages.Shared().SetSources(
		func(context.Context) (int, error) { <-block; return 5, nil },
		nil,
	)

	done := make(chan string, 1)
	go func() {
		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		done <- w.Body.String()
	}()

	select {
	case body := <-done:
		if body != "" {
			t.Fatalf("cold cache served %q, want empty", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler blocked on the background scan; the render path must never wait for it")
	}
}

// THE POSITIVE HALF of the lazy-refresh contract: a cold cache must actually
// KICK the background scan, exactly once.
//
// This is deliberately separate from the two tests above, neither of which
// pins it. ColdCacheAnswersEmptyAndDoesNotBlock asserts only that the body is
// empty and the handler does not wait -- a handler that never called
// TriggerRefresh at all would satisfy both of those identically.
// CleanCacheDoesNotRetriggerScan asserts the NEGATIVE (zero refreshes once the
// cache is computed). Without this test, deleting cache.TriggerRefresh() from
// the handler leaves the whole package suite green.
//
// What that regression would cost: TriggerRefresh is the ONLY thing that fills
// the duplicate rows on a fresh boot. Lose it and the section stays empty until
// the 12h maintenance refresh lands.
//
// "Exactly once" is the other half of the contract: TriggerRefresh is
// single-flight, so a burst of 60s sidebar polls against a still-cold cache
// must produce ONE scan, not one per poll.
func TestDupImagesNav_ColdCacheTriggersExactlyOneBackgroundRefresh(t *testing.T) {
	r := dupNavRouter(t)

	var libCalls atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	dupNavStubSources(t, r,
		func(context.Context) (int, error) {
			libCalls.Add(1)
			started <- struct{}{}
			// Hold the scan open so every poll below sees a cache that is
			// still un-computed. Otherwise a fast first refresh would flip
			// Computed and the single-flight claim would go untested.
			<-release
			return 7, nil
		},
		nil,
	)

	// Precondition: the cache really is cold. Without this the assertions
	// below could pass vacuously against an already-computed snapshot.
	if dupimages.Shared().Get().Computed {
		t.Fatal("precondition: the cache must start un-computed")
	}

	const polls = 5
	for range polls {
		w := httptest.NewRecorder()
		r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("a cold cache never invoked the count source: the lazy refresh did not fire, " +
			"so the duplicate rows would stay empty until the 12h maintenance refresh")
	}
	if n := libCalls.Load(); n != 1 {
		t.Fatalf("count source invoked %d times across %d cold polls; want exactly 1 (TriggerRefresh is single-flight)", n, polls)
	}

	// Drain the background refresh before returning so no straggler goroutine
	// writes into the process-wide cache during a later test.
	releaseOnce()
	deadline := time.Now().Add(5 * time.Second)
	for !dupimages.Shared().Get().Computed {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never completed after release")
		}
		time.Sleep(time.Millisecond)
	}
}

// ACCEPTED FAILURE MODE (#2608, maintainer's explicit call). When the
// unmatched count cannot be read, foreignSummaryForBanner logs a Warn and
// returns 0; with the duplicate counts also zero the view is Empty and the
// handler emits an empty body, so the ENTIRE Images section disappears --
// visually identical to "everything is clean".
//
// That is the direct, unavoidable consequence of the hide-when-zero spec:
// hiding at a zero count necessarily means hiding when the count is unknown.
// It is not a defect to fix here; the Warn is the operator's only signal.
//
// This test exists so the accepted behavior cannot drift silently -- it is the
// nav endpoint's counterpart to TestHandleForeignFilesCount_CountError, which
// pins the same fail-safe on the older count endpoint.
func TestDupImagesNav_UnmatchedCountFailureHidesSectionAndWarns(t *testing.T) {
	r, db := dupNavRouterWithForeign(t)
	readLogs := captureLogs(t, r)

	// Seed real rows FIRST. seedUnmatched asserts the repo genuinely reports
	// them, so an empty body below can only come from the injected failure and
	// never from a repo that was vacuously empty all along.
	seedUnmatched(t, r, db, 3)
	// A computed-clean duplicate cache: the duplicate rows are legitimately
	// zero, leaving the unmatched count as the only thing that could keep the
	// section alive.
	seedCounts(0)

	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}

	w := httptest.NewRecorder()
	r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the failure degrades to a hidden section, not an error)", w.Code)
	}
	if got := w.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty: a failed unmatched count renders as an ABSENT section", got)
	}

	var warned bool
	for _, rec := range readLogs() {
		if rec["level"] == "WARN" && rec["msg"] == "foreign-file banner count failed" {
			warned = true
			break
		}
	}
	if !warned {
		t.Error("no warning logged for the failed unmatched count; the Warn is the operator's " +
			"ONLY signal that the section is hidden by a failure rather than by cleanliness")
	}
}

// stubPlatformDupScanner is a platformBackdropDupScanner whose report the test
// dictates. The Router's publisher field is a concrete *publish.Publisher, so
// this narrow interface is what makes the partial-sweep guard reachable
// without standing up a live Emby.
type stubPlatformDupScanner struct {
	report publish.PlatformBackdropDupReport
	err    error
}

func (s stubPlatformDupScanner) ScanPlatformBackdropDuplicates(context.Context) (publish.PlatformBackdropDupReport, error) {
	return s.report, s.err
}

// ScanPlatformBackdropDuplicates swallows every failed per-artist query into
// ScanErrors and still returns err == nil. When a platform is unreachable that
// means PerArtist is EMPTY and ScanErrors is huge: a result indistinguishable,
// to naive code, from "every connected platform is clean". The cache would then
// use it to clear the rows.
//
// The sweep must therefore report itself as a failure, so the cache carries
// the last known counts forward instead.
func TestPlatformDupCounts_PartialSweepIsReportedAsAnError(t *testing.T) {
	r := dupNavRouter(t)

	scanner := stubPlatformDupScanner{
		report: publish.PlatformBackdropDupReport{ScanErrors: 3800}, // PerArtist empty: total outage
	}

	got, err := r.platformDupCountsFrom(context.Background(), scanner)
	if !errors.Is(err, dupimages.ErrPartialScan) {
		t.Fatalf("err = %v, want it to wrap dupimages.ErrPartialScan; a fully-failed sweep must not read as a clean one", err)
	}
	if got != nil {
		t.Errorf("counts = %+v, want nil; a partial sweep must publish no counts", got)
	}
}

// The control that keeps the guard honest: a COMPLETE sweep that legitimately
// found nothing must still succeed, because "empty means clean" is what clears
// stale rows after a real cleanup.
func TestPlatformDupCounts_CompleteEmptySweepSucceeds(t *testing.T) {
	r := dupNavRouter(t)

	scanner := stubPlatformDupScanner{report: publish.PlatformBackdropDupReport{ScanErrors: 0}}

	got, err := r.platformDupCountsFrom(context.Background(), scanner)
	if err != nil {
		t.Fatalf("a complete sweep with no offenders must succeed so it can clear stale rows; got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("counts = %+v, want empty", got)
	}
}

// The library half. ScanFanartDuplicates skips every artist it cannot re-hash
// (a dropped NFS/SMB mount) into ScanErrors and returns err == nil, so
// ExactRedundantSlots covers only the reachable part of the library. Caching
// that publishes an undercount as fact.
func TestLibraryDupCount_PartialScanIsReportedAsAnError(t *testing.T) {
	r := dupNavRouter(t)
	r.pipeline = &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(context.Context) (rule.FanartDupReport, error) {
			// Only the reachable half was scanned.
			return rule.FanartDupReport{ExactRedundantSlots: 3, ScanErrors: 19}, nil
		},
	}

	got, err := r.libraryDupCount(context.Background())
	if !errors.Is(err, dupimages.ErrPartialScan) {
		t.Fatalf("err = %v, want it to wrap dupimages.ErrPartialScan; an undercount must not be cached as authoritative", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0 alongside the error", got)
	}
}

// Control for the library guard: a complete scan is authoritative, including
// an authoritative zero.
func TestLibraryDupCount_CompleteScanReturnsTheCount(t *testing.T) {
	r := dupNavRouter(t)
	r.pipeline = &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(context.Context) (rule.FanartDupReport, error) {
			return rule.FanartDupReport{ExactRedundantSlots: 3, ScanErrors: 0}, nil
		},
	}

	got, err := r.libraryDupCount(context.Background())
	if err != nil {
		t.Fatalf("a complete scan must succeed; got %v", err)
	}
	if got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
}

// The scan sources must be installed when the Router is CONSTRUCTED, not
// lazily on the first sidebar request.
//
// The periodic maintenance refresh holds only the cache; the scan functions
// live on the Router. With a lazy install, a server nobody had opened the admin
// sidebar on would run the periodic task against a source-less cache and
// refresh nothing. A cache that has sources can be refreshed with no HTTP
// request having ever happened, which is exactly what this asserts.
func TestNewRouter_InstallsDupImageScanSourcesEagerly(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	// Construct a Router the way production does. No request is made.
	_ = NewRouter(RouterDeps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		StaticFS: os.DirFS("../../web/static"),
	})

	// Refresh must not bail out with "no scan sources installed". The scans
	// themselves fail on this bare Router (no pipeline/publisher), which is a
	// DIFFERENT error -- the point is that the cache had something to call.
	err := dupimages.Shared().Refresh(context.Background())
	if err != nil && strings.Contains(err.Error(), "no scan sources installed") {
		t.Fatal("Router construction did not install the scan sources; the periodic background refresh would do nothing until someone loaded the sidebar")
	}
}

// bucketTestLogger keeps the skip-warning out of test output.
func bucketTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// typeResolver builds a resolver over a fixed connection-id -> type map.
func typeResolver(types map[string]string) func(string) (string, error) {
	return func(connID string) (string, error) {
		if t, ok := types[connID]; ok {
			return t, nil
		}
		return "", errors.New("connection not found")
	}
}

// Two connections of the SAME platform type collapse into ONE row with the
// combined count -- the row says "Emby Duplicates", not one row per connection,
// and a user-chosen connection name never reaches the label.
func TestBucketByPlatformType_CollapsesConnectionsOfSameType(t *testing.T) {
	report := publish.PlatformBackdropDupReport{PerArtist: []publish.ArtistPlatformBackdropDup{
		{ConnectionID: "c1", Connection: "Living Room Emby", Redundant: 3},
		{ConnectionID: "c2", Connection: "Basement Emby", Redundant: 4},
		{ConnectionID: "c3", Connection: "Jellyfin", Redundant: 2},
		{ConnectionID: "c1", Connection: "Living Room Emby", Redundant: 1},
		{ConnectionID: "c3", Connection: "Jellyfin", Redundant: 0}, // not an offender
	}}

	got := bucketByPlatformType(report,
		typeResolver(map[string]string{"c1": "emby", "c2": "emby", "c3": "jellyfin"}),
		bucketTestLogger())

	// Assert the WHOLE slice, not got[0]/got[1] individually: with two map
	// entries, indexing is a coin flip on Go's randomized iteration order and
	// passes half the time even with the sort deleted. Row ORDER is pinned
	// separately and deterministically by
	// TestBucketByPlatformType_OrdersRowsByTypeAcrossRepeatedRuns; what this
	// test owns is the COLLAPSE -- 3+4+1 into one Emby row, labeled with the
	// brand name rather than any user-chosen connection name.
	want := []dupimages.PlatformCount{
		{Type: "emby", Label: "Emby", Count: 8},
		{Type: "jellyfin", Label: "Jellyfin", Count: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %+v, want %+v (two Emby connections collapse into one row of 3+4+1)", got, want)
	}
}

// ROW ORDER IS THE CONTRACT the sort exists for: the sidebar re-fetches this
// section every 60 seconds, and rows that reshuffle between polls would make
// the nav visibly jitter.
//
// Two properties make this test worth its repetition. Go's map iteration order
// is randomized, so a SINGLE assertion against an unsorted implementation
// passes by chance a large fraction of the time -- that is both a mutation that
// survives and a latent CI flake. Repeating the call drives the chance of an
// accidental pass to nil: four types over 30 rounds. A correct implementation
// passes all 30 deterministically.
func TestBucketByPlatformType_OrdersRowsByTypeAcrossRepeatedRuns(t *testing.T) {
	// Insertion order deliberately NOT the expected order.
	report := publish.PlatformBackdropDupReport{PerArtist: []publish.ArtistPlatformBackdropDup{
		{ConnectionID: "c-plex", Connection: "Plex Box", Redundant: 4},
		{ConnectionID: "c-emby", Connection: "Living Room Emby", Redundant: 1},
		{ConnectionID: "c-lidarr", Connection: "Lidarr", Redundant: 3},
		{ConnectionID: "c-jellyfin", Connection: "Jellyfin", Redundant: 2},
	}}
	resolve := typeResolver(map[string]string{
		"c-plex": "plex", "c-emby": "emby", "c-lidarr": "lidarr", "c-jellyfin": "jellyfin",
	})
	want := []dupimages.PlatformCount{
		{Type: "emby", Label: "Emby", Count: 1},
		{Type: "jellyfin", Label: "Jellyfin", Count: 2},
		{Type: "lidarr", Label: "Lidarr", Count: 3},
		{Type: "plex", Label: "Plex", Count: 4},
	}

	const rounds = 30
	for i := range rounds {
		got := bucketByPlatformType(report, resolve, bucketTestLogger())
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d/%d: rows = %+v, want %+v; sidebar row order must not depend on map iteration order",
				i+1, rounds, got, want)
		}
	}
}

// A connection whose type cannot be resolved is SKIPPED, not bucketed under a
// guess: a row is a claim about WHICH platform is dirty.
func TestBucketByPlatformType_SkipsUnresolvableConnections(t *testing.T) {
	report := publish.PlatformBackdropDupReport{PerArtist: []publish.ArtistPlatformBackdropDup{
		{ConnectionID: "ghost", Connection: "Deleted", Redundant: 5},
		{ConnectionID: "c1", Connection: "Emby", Redundant: 2},
	}}

	got := bucketByPlatformType(report, typeResolver(map[string]string{"c1": "emby"}), bucketTestLogger())

	if len(got) != 1 {
		t.Fatalf("got %+v, want only the resolvable emby row", got)
	}
	if got[0].Type != "emby" || got[0].Count != 2 {
		t.Errorf("row = %+v, want emby/2 with the unresolvable connection dropped", got[0])
	}
}

// A clean scan yields no rows at all, so nothing claims a clean platform is
// dirty.
func TestBucketByPlatformType_CleanScanYieldsNoRows(t *testing.T) {
	report := publish.PlatformBackdropDupReport{PerArtist: []publish.ArtistPlatformBackdropDup{
		{ConnectionID: "c1", Connection: "Emby", Redundant: 0},
	}}

	if got := bucketByPlatformType(report, typeResolver(map[string]string{"c1": "emby"}), bucketTestLogger()); len(got) != 0 {
		t.Fatalf("clean scan produced rows: %+v", got)
	}
}

// The resolver is consulted once per connection, not once per artist row.
func TestBucketByPlatformType_MemoizesTypeLookups(t *testing.T) {
	rows := make([]publish.ArtistPlatformBackdropDup, 0, 50)
	for range 50 {
		rows = append(rows, publish.ArtistPlatformBackdropDup{ConnectionID: "c1", Connection: "Emby", Redundant: 1})
	}

	var lookups int
	resolve := func(string) (string, error) { lookups++; return "emby", nil }

	got := bucketByPlatformType(publish.PlatformBackdropDupReport{PerArtist: rows}, resolve, bucketTestLogger())

	if lookups != 1 {
		t.Errorf("resolver called %d times for 50 rows on one connection; want 1", lookups)
	}
	if len(got) != 1 || got[0].Count != 50 {
		t.Errorf("got %+v, want a single emby row with count 50", got)
	}
}

func TestPlatformDisplayName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"emby", "Emby"},
		{"jellyfin", "Jellyfin"},
		{"lidarr", "Lidarr"},
		// An unmapped type is title-cased, never dropped: the label is composed
		// generically, so a platform added later renders correctly with no edit
		// here. Dropping it would hide a genuinely dirty platform.
		{"plex", "Plex"},
		{"navidrome", "Navidrome"},
		{"", ""},
	} {
		if got := platformDisplayName(tc.in); got != tc.want {
			t.Errorf("platformDisplayName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A platform type this code has never heard of must still produce a correct,
// clickable row -- the label template is generic ("<Name> Duplicates"), so
// adding a platform elsewhere in the codebase needs no change here.
func TestDupImagesNav_UnknownPlatformTypeStillRenders(t *testing.T) {
	r := dupNavRouter(t)
	seedCounts(0, dupimages.PlatformCount{Type: "plex", Label: "Plex", Count: 6})

	w := httptest.NewRecorder()
	r.handleDuplicateImagesNav(w, dupNavReq(t, "administrator"))
	body := w.Body.String()

	if !strings.Contains(body, `id="sidebar-image-duplicates-plex"`) {
		t.Errorf("unknown platform type produced no row:\n%s", body)
	}
	if !strings.Contains(body, ">Plex Duplicates<") {
		t.Errorf("label not composed generically for an unknown platform:\n%s", body)
	}
	if !strings.Contains(body, `aria-label="6 duplicate images on Plex"`) {
		t.Errorf("aria-label not composed generically for an unknown platform:\n%s", body)
	}
}

// End-to-end through the bucketing: an unmapped connection type survives the
// scan -> bucket -> label pipeline rather than vanishing.
func TestBucketByPlatformType_UnknownTypeSurvivesWithTitleCasedLabel(t *testing.T) {
	report := publish.PlatformBackdropDupReport{PerArtist: []publish.ArtistPlatformBackdropDup{
		{ConnectionID: "c9", Connection: "Media Box", Redundant: 4},
	}}

	got := bucketByPlatformType(report, typeResolver(map[string]string{"c9": "plex"}), bucketTestLogger())

	if len(got) != 1 {
		t.Fatalf("unknown platform type dropped: %+v", got)
	}
	if got[0].Type != "plex" || got[0].Label != "Plex" || got[0].Count != 4 {
		t.Errorf("row = %+v, want type=plex label=Plex count=4", got[0])
	}
}
