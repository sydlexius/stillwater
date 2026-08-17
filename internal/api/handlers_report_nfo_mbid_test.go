package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// handlers_report_nfo_mbid_test.go covers the reports-workspace PANE half of
// issue #2809 (repPaneNFOHasMBID / loadReportsNFOMBIDData in
// web/templates/reports_page.templ and internal/api/handlers_report.go). The
// query and JSON/CSV surfaces are covered separately by
// internal/artist/nfo_mbid_report_test.go and internal/api/handlers_nfo_mbid_test.go;
// this file asserts only what the SERVER-RENDERED PAGE does with that data.
//
// The maintainer reopened #2809 because an API + CSV alone did not meet the
// "surfaced somewhere an operator can act on it" bar. Every test below
// targets that bar specifically: does the rail list the report, does the pane
// render real rows, and -- the substance of the issue -- are the caveats
// (scope/floor/no-prior-value/not-confirmed) visible on the page an operator
// actually looks at, not just in a JSON field nobody reads.

// renderNFOMBIDPane seeds the fixture, serves GET /reports/nfo-has-mbid, and
// returns the rendered HTML. The fixture is seeded first so the pane's rows,
// not just its empty state, are what every assertion below checks: an empty
// pane would trivially "contain" no forbidden text and pass every assertion
// vacuously.
func renderNFOMBIDPane(t *testing.T) string {
	t.Helper()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)

	req := httptest.NewRequest(http.MethodGet, "/reports/nfo-has-mbid", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "nfo-has-mbid")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Precondition. Without this the "table renders rows" assertions below
	// would pass on a page that rendered nothing at all.
	if !strings.Contains(body, `id="nfo-mbid-tbl"`) {
		t.Fatalf("nfo-has-mbid table absent; the page did not render the pane at all")
	}
	return body
}

// TestReportsRail_ListsNFOHasMBID guards the rail-registry half of the fix:
// repBuiltinReports must carry an entry whose ID is exactly "nfo-has-mbid"
// (the {name} path segment /reports/{name} dispatches on), or the report is
// unreachable from the rail regardless of how good its pane is.
//
// Renders the neighboring, already-shipped blast-radius pane's page instead
// of the nfo pane itself: the rail is shared chrome around EVERY pane, so any
// page render proves the rail entry exists. Using blast-radius here keeps
// this test independent of whether the nfo pane itself renders correctly,
// which the other tests in this file already cover on their own.
func TestReportsRail_ListsNFOHasMBID(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	if !strings.Contains(body, `href="/reports/nfo-has-mbid"`) {
		t.Fatalf("reports rail carries no link to /reports/nfo-has-mbid; " +
			"the report is unreachable by clicking through the workspace")
	}
}

// TestNFOMBIDPane_RendersRows asserts the pane shows the artists the fixture
// seeded, keyed on a MARKER ATTRIBUTE (the row id, id="nfo-mbid-row-<change
// id>"), never on prose. A prose-only assertion (e.g. Contains(body,
// "Alpha Artist")) would still pass if the row markup were deleted and the
// name leaked in some unrelated place (a page title, a nav breadcrumb), so
// this pins to the row container specifically.
func TestNFOMBIDPane_RendersRows(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)

	for _, id := range []string{"m-old", "m-new"} {
		marker := `id="nfo-mbid-row-` + id + `"`
		if !strings.Contains(body, marker) {
			t.Errorf("pane missing row marker %q; seeded change %q did not reach the rendered table", marker, id)
		}
	}
	// The unrelated row (a different rule) must be excluded -- the single
	// most important correctness constraint on this report's query. Asserted
	// here too (in addition to the query-layer test) because a pane could in
	// principle re-fetch or re-filter incorrectly even with a correct query.
	if strings.Contains(body, "Bystander") {
		t.Fatalf("pane rendered the Bystander artist, whose change was written by rule:bio_exists, not nfo_has_mbid; " +
			"the pane is not filtering to this report's source")
	}
}

// TestNFOMBIDPane_CurrentIDNotTheNote is the field-confusion regression this
// report was specifically designed to avoid: "Use the current MusicBrainz ID
// column, not the note, when acting on a row" (NFOMBIDCaveatMessageWording).
// The seeded m-old row has a recorded note ID
// (5b11f4ce-a62d-471e-81fc-a69a8278c7da) that DIFFERS from its live provider
// row (ffffffff-0000-0000-0000-000000000000, seeded by seedAPIMBIDProviderID)
// -- exactly so a pane that accidentally rendered the note's ID in the
// "Current MusicBrainz ID" column, instead of the live one, is caught here
// rather than only in a JSON-level test.
func TestNFOMBIDPane_CurrentIDNotTheNote(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)

	if !strings.Contains(body, "ffffffff-0000-0000-0000-000000000000") {
		t.Fatalf("pane does not show the artist's CURRENT MusicBrainz ID " +
			"(ffffffff-0000-0000-0000-000000000000); it must read from " +
			"artist_provider_ids, not from the recorded note")
	}
	// The note's own ID is legitimately present too (inside the Recorded Note
	// cell), so this does not assert its absence -- only that the current-ID
	// value is present and distinct, which the first assertion already
	// establishes.
}

// TestNFOMBIDPane_MissingCurrentIDReadsAsNoneRecorded covers the second seeded
// row (m-new / Zulu Artist), which the fixture deliberately gives no
// artist_provider_ids row at all -- its ID has since been cleared. Guards
// against a blank table cell, which reads as "clean" on this surface, and
// against a false zero-value fallback (an empty string is not the same claim
// as "no ID recorded").
func TestNFOMBIDPane_MissingCurrentIDReadsAsNoneRecorded(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)

	if !strings.Contains(body, "none recorded") {
		t.Fatalf("pane does not render \"none recorded\" for the artist with no live " +
			"MusicBrainz ID; a blank cell here would read as a clean value")
	}
}

// TestNFOMBIDPane_CaveatsAreVisibleOnThePage is THE test for the reopened
// issue's exact complaint: caveats existing in a JSON field nobody reads does
// not meet the "surfaced somewhere an operator can act on it" bar. Every
// caveat constant the JSON/CSV surfaces already ship
// (internal/artist/nfo_mbid_report.go) must appear verbatim in the rendered
// HTML, and NOT inside a <details> element (which most browsers and all
// screen readers treat as collapsed/hidden by default unless it carries an
// `open` attribute) -- a caveat an operator has to know to expand is not
// surfaced, it is buried.
func TestNFOMBIDPane_CaveatsAreVisibleOnThePage(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)
	// templ HTML-escapes text content, so a caveat carrying a literal quote
	// or apostrophe (several do: "not on this list", "artist's history")
	// renders as &#34;/&#39; entities rather than the raw characters. Unescape
	// before comparing so this test verifies the caveat TEXT reached the
	// page, not the coincidence of an escaping scheme matching the source.
	unescaped := html.UnescapeString(body)

	caveats := []struct {
		name string
		text string
	}{
		{"scope", artist.NFOMBIDCaveatScope},
		{"floor", artist.NFOMBIDCaveatFloor},
		{"retention", artist.NFOMBIDCaveatRetention},
		{"no_prior_value", artist.NFOMBIDCaveatNoPriorValue},
		{"not_confirmed", artist.NFOMBIDCaveatNotConfirmed},
		{"message_wording", artist.NFOMBIDCaveatMessageWording},
	}
	for _, c := range caveats {
		if !strings.Contains(unescaped, c.text) {
			t.Errorf("pane does not render the %s caveat verbatim; an operator viewing "+
				"the page cannot see this limit of the report", c.name)
		}
	}

	// Not buried behind a closed disclosure. A naive fix could satisfy every
	// assertion above by putting the caveat text inside a collapsed
	// <details> -- present in the DOM, invisible without an extra click.
	if strings.Contains(body, "<details") {
		t.Fatalf("pane wraps content in a <details> element; the caveat band on this " +
			"pane must be always-visible, never a collapsed disclosure an operator has to open")
	}

	// The caveat band renders inside role="note", matching the pattern the
	// already-shipped blast-radius pane uses for the same purpose (always
	// visible, announced to assistive tech as informational content).
	// role="note" alone is not unique on this page -- the shared
	// .sw-list-tips page footer also carries it for an unrelated
	// keyboard-shortcuts tip, rendered on every reports-workspace page
	// regardless of which pane is active -- so pin the assertion to the
	// caveat band's own id rather than the shared role, or this test passes
	// even if role="note" is dropped from the caveat band specifically.
	// Bind role="note" to the caveat band's OWN opening tag, not just to the
	// page somewhere. The shared .sw-list-tips footer above also carries
	// role="note", so a bare Contains(body, `role="note"`) would still pass
	// if role="note" were deleted from the caveat band specifically -- the
	// footer's copy alone would satisfy it. Matching the exact opening tag
	// (id and role as sibling attributes on the same element, in the order
	// the templ source emits them) ties the assertion to the caveat band
	// itself.
	if !strings.Contains(body, `<div id="nfo-mbid-caveats" class="sw-rep-blast-caveat" role="note">`) {
		t.Fatalf("pane's caveat band element (id=\"nfo-mbid-caveats\") does not carry " +
			"role=\"note\" on its own opening tag")
	}
}

// TestNFOMBIDPane_SummaryStatesTheFloorNotACount asserts the pane's headline
// summary line renders the seeded counts (2 writes, 2 artists) AND explicitly
// says "at least" -- reading a bare number here would repeat the exact
// "unknown rendered as clean" defect the whole report exists to prevent (a
// count presented as complete when it is a floor).
func TestNFOMBIDPane_SummaryStatesTheFloorNotACount(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)

	if !strings.Contains(body, "2 ID assignment(s) across 2 artist(s), at least.") {
		t.Fatalf("pane summary does not render the seeded counts (2 writes across " +
			"2 artists) with the \"at least\" floor language")
	}
}

// TestNFOMBIDPane_OversizedPageClampsInsteadOfClaimingEmpty guards the fix for
// an out-of-range ?page: requesting a page past the last one used to run
// ListNFOMBIDWrites at the raw (unclamped) offset, which returns zero rows
// while CountNFOMBIDWrites still reports the fixture's real total -- the pane
// then rendered the "No rule-written MusicBrainz IDs recorded." empty state
// right next to a nonzero count, a false claim. The fixture seeds 2 reported
// rows at the default page size (50), so page=999 is unreachable by design.
func TestNFOMBIDPane_OversizedPageClampsInsteadOfClaimingEmpty(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)

	req := httptest.NewRequest(http.MethodGet, "/reports/nfo-has-mbid?page=999", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "nfo-has-mbid")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, "No rule-written MusicBrainz IDs recorded.") {
		t.Fatalf("pane rendered the empty state for ?page=999 even though the fixture " +
			"has 2 reported rows; an out-of-range page must clamp to the last real page, " +
			"not fall through to a false \"nothing recorded\" claim")
	}
	if !strings.Contains(body, `id="nfo-mbid-row-m-old"`) {
		t.Fatalf("pane did not render seeded row m-old after clamping ?page=999 to the " +
			"last valid page; the clamp must actually re-list rows, not just avoid the " +
			"empty-state string")
	}
}

// TestNFOMBIDPane_RowLinksToArtistDetail asserts every row carries a working
// review link to the artist's own detail page -- the pane's only action,
// since validating or reverting an ID is out of scope (issue #2810). A row
// with no route to the artist would leave the operator with data but no
// action, which is the exact gap #2809 was reopened over.
func TestNFOMBIDPane_RowLinksToArtistDetail(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)

	if !strings.Contains(body, `href="/artists/mb-alpha"`) {
		t.Fatalf("pane's row for artist mb-alpha does not link to its detail page " +
			"(/artists/mb-alpha); an operator reviewing a rule-picked ID has no route to act on it")
	}
}

// TestNFOMBIDPane_RowLinkKeepsBasePath asserts the row's review link is built
// from the router's configured base path, not a bare "/artists/<id>". With
// SW_BASE_PATH set (Stillwater served under a sub-path), a bare-rooted link
// would leave the deployment path entirely -- the operator's browser
// requests the wrong origin path and 404s. Asserts the full prefixed URL, not
// a bare substring like "/artists/", which a regressed bare-rooted href would
// also satisfy.
func TestNFOMBIDPane_RowLinkKeepsBasePath(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	r.basePath = "/musicbrainz"
	seedAPIMBIDFixture(t, r)

	req := httptest.NewRequest(http.MethodGet, "/reports/nfo-has-mbid", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "nfo-has-mbid")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `href="/musicbrainz/artists/mb-alpha"`) {
		t.Fatalf("pane's review link for mb-alpha does not carry the configured base "+
			"path (/musicbrainz); got body without href=\"/musicbrainz/artists/mb-alpha\": %s", body)
	}
}

// TestNFOMBIDPane_HasNoPriorValueColumn guards against the "Replaced Value"
// column the brief explicitly forbids: this rule only ever fills a BLANK
// MusicBrainz ID (fixMBID declines to run when one is already present), and
// the rule-fix history path always records an empty old_value, so a
// "Replaced Value" column would be permanently empty by construction --
// present in the markup but never populated, which invites the reader to
// conclude an ID was overwritten when one never existed. The seeded old-form
// message intentionally does NOT contain the word "Replaced" anywhere, so a
// hit here can only come from an actual added column, not the message text
// leaking through.
func TestNFOMBIDPane_HasNoPriorValueColumn(t *testing.T) {
	t.Parallel()
	body := renderNFOMBIDPane(t)

	if strings.Contains(body, "Replaced Value") {
		t.Fatalf("pane renders a \"Replaced Value\" column header; this rule only ever " +
			"fills a blank MusicBrainz ID and the history path never records a prior " +
			"value, so such a column would always read empty and imply an overwrite " +
			"that never happened")
	}
}

// TestLoadReportsNFOMBIDData_ServiceUnavailable mirrors
// TestLoadReportsBlastRadiusData_ServiceUnavailable: when the history service
// is not wired, loadReportsNFOMBIDData must refuse with 503/ok=false rather
// than panic on a nil dereference or silently render an empty pane that
// reads as "the rule has written nothing" instead of "we could not check".
//
// Mutation proving teeth: deleting the `if r.historyService == nil` guard in
// loadReportsNFOMBIDData makes this test fail with a nil-pointer panic
// instead of the expected 503/ok=false -- recorded below.
func TestLoadReportsNFOMBIDData_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.historyService = nil

	req := httptest.NewRequest(http.MethodGet, "/reports/nfo-has-mbid", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()

	_, ok := r.loadReportsNFOMBIDData(w, req)
	if ok {
		t.Fatalf("loadReportsNFOMBIDData returned ok=true with a nil history service")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestLoadReportsNFOMBIDData_ListErrorSurfacesAs500 exercises the
// ListNFOMBIDWrites error path: when the list query fails, the loader must
// refuse (500/ok=false) rather than fall through with a zero-value pane that
// would render as "no rule-written IDs" -- the reassuring answer produced by
// an error on a data-loss recovery surface.
//
// Kept separate from the count-error test (below) on purpose: the two error
// branches fail differently in the source (first vs second query), and a
// combined test that injected both failures at once would let a broken
// Count-error branch regress silently, since the List error is checked
// first and returns before Count is ever reached.
func TestLoadReportsNFOMBIDData_ListErrorSurfacesAs500(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	stub := &listErrCallTrackingHistoryRepo{delegate: r.historyService.Repo()}
	r.historyService = artist.NewHistoryServiceWithRepo(stub)

	req := httptest.NewRequest(http.MethodGet, "/reports/nfo-has-mbid", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()

	_, ok := r.loadReportsNFOMBIDData(w, req)

	// Precondition: the stub's ListNFOMBIDWrites was actually invoked, so a
	// short-circuit earlier in the loader (e.g. an early return before the
	// query runs) cannot make the assertions below pass for the wrong
	// reason.
	if !stub.listCalled {
		t.Fatalf("precondition: ListNFOMBIDWrites was never called; the loader " +
			"short-circuited before reaching the query this test targets")
	}
	if ok {
		t.Fatalf("loadReportsNFOMBIDData returned ok=true after ListNFOMBIDWrites failed")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// TestLoadReportsNFOMBIDData_CountErrorSurfacesAs500 exercises the
// CountNFOMBIDWrites error path in isolation: ListNFOMBIDWrites succeeds
// (delegated to a real repo) and only the count query fails, so a broken
// count-error branch cannot hide behind the list-error test above.
func TestLoadReportsNFOMBIDData_CountErrorSurfacesAs500(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	stub := &countErrCallTrackingHistoryRepo{delegate: r.historyService.Repo()}
	r.historyService = artist.NewHistoryServiceWithRepo(stub)

	req := httptest.NewRequest(http.MethodGet, "/reports/nfo-has-mbid", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()

	_, ok := r.loadReportsNFOMBIDData(w, req)

	// Precondition: CountNFOMBIDWrites was actually invoked -- proves the
	// loader got past the (successful) list query and reached the count
	// query, so this test cannot pass because the list call already failed.
	if !stub.countCalled {
		t.Fatalf("precondition: CountNFOMBIDWrites was never called; the loader " +
			"never reached the query this test targets")
	}
	if ok {
		t.Fatalf("loadReportsNFOMBIDData returned ok=true after CountNFOMBIDWrites failed")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// listErrCallTrackingHistoryRepo delegates everything to a real repo except
// ListNFOMBIDWrites, which always fails. Tracks whether it was called so the
// test above can assert its own precondition rather than trust an exit code.
type listErrCallTrackingHistoryRepo struct {
	delegate   artist.HistoryRepository
	listCalled bool
}

func (l *listErrCallTrackingHistoryRepo) Record(ctx context.Context, c *artist.MetadataChange) error {
	return l.delegate.Record(ctx, c)
}

func (l *listErrCallTrackingHistoryRepo) GetByID(ctx context.Context, id string) (*artist.MetadataChange, error) {
	return l.delegate.GetByID(ctx, id)
}

func (l *listErrCallTrackingHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) ([]artist.MetadataChange, int, error) {
	return l.delegate.List(ctx, artistID, limit, offset)
}

func (l *listErrCallTrackingHistoryRepo) ListGlobal(ctx context.Context, filter artist.GlobalHistoryFilter) ([]artist.MetadataChangeWithArtist, int, error) {
	return l.delegate.ListGlobal(ctx, filter)
}

func (l *listErrCallTrackingHistoryRepo) ListBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error) {
	return l.delegate.ListBlastRadius(ctx, f)
}

func (l *listErrCallTrackingHistoryRepo) CountBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) (artist.BlastRadiusCounts, error) {
	return l.delegate.CountBlastRadius(ctx, f)
}

func (l *listErrCallTrackingHistoryRepo) ListNFOMBIDWrites(_ context.Context, _ artist.NFOMBIDFilter) ([]artist.NFOMBIDWriteRow, error) {
	l.listCalled = true
	return nil, errors.New("simulated list failure")
}

func (l *listErrCallTrackingHistoryRepo) CountNFOMBIDWrites(ctx context.Context, f artist.NFOMBIDFilter) (artist.NFOMBIDCounts, error) {
	return l.delegate.CountNFOMBIDWrites(ctx, f)
}

func (l *listErrCallTrackingHistoryRepo) LockDamageCandidates(ctx context.Context) ([]artist.LockDamageCandidate, error) {
	return l.delegate.LockDamageCandidates(ctx)
}

func (l *listErrCallTrackingHistoryRepo) LockDamageUnattributed(ctx context.Context) ([]artist.LockDamageUnattributedRow, error) {
	return l.delegate.LockDamageUnattributed(ctx)
}

// countErrCallTrackingHistoryRepo delegates everything to a real repo except
// CountNFOMBIDWrites, which always fails; ListNFOMBIDWrites is left to
// succeed so the count-error branch is isolated from the list-error one.
type countErrCallTrackingHistoryRepo struct {
	delegate    artist.HistoryRepository
	countCalled bool
}

func (c *countErrCallTrackingHistoryRepo) Record(ctx context.Context, ch *artist.MetadataChange) error {
	return c.delegate.Record(ctx, ch)
}

func (c *countErrCallTrackingHistoryRepo) GetByID(ctx context.Context, id string) (*artist.MetadataChange, error) {
	return c.delegate.GetByID(ctx, id)
}

func (c *countErrCallTrackingHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) ([]artist.MetadataChange, int, error) {
	return c.delegate.List(ctx, artistID, limit, offset)
}

func (c *countErrCallTrackingHistoryRepo) ListGlobal(ctx context.Context, filter artist.GlobalHistoryFilter) ([]artist.MetadataChangeWithArtist, int, error) {
	return c.delegate.ListGlobal(ctx, filter)
}

func (c *countErrCallTrackingHistoryRepo) ListBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error) {
	return c.delegate.ListBlastRadius(ctx, f)
}

func (c *countErrCallTrackingHistoryRepo) CountBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) (artist.BlastRadiusCounts, error) {
	return c.delegate.CountBlastRadius(ctx, f)
}

func (c *countErrCallTrackingHistoryRepo) ListNFOMBIDWrites(ctx context.Context, f artist.NFOMBIDFilter) ([]artist.NFOMBIDWriteRow, error) {
	return c.delegate.ListNFOMBIDWrites(ctx, f)
}

func (c *countErrCallTrackingHistoryRepo) CountNFOMBIDWrites(_ context.Context, _ artist.NFOMBIDFilter) (artist.NFOMBIDCounts, error) {
	c.countCalled = true
	return artist.NFOMBIDCounts{}, errors.New("simulated count failure")
}

func (c *countErrCallTrackingHistoryRepo) LockDamageCandidates(ctx context.Context) ([]artist.LockDamageCandidate, error) {
	return c.delegate.LockDamageCandidates(ctx)
}

func (c *countErrCallTrackingHistoryRepo) LockDamageUnattributed(ctx context.Context) ([]artist.LockDamageUnattributedRow, error) {
	return c.delegate.LockDamageUnattributed(ctx)
}
