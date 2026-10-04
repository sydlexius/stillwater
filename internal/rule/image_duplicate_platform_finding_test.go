package rule

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/publish"
)

// dupPeer is an Emby stand-in that models the PEER'S STATE: an ordered
// backdrop list per artist that re-indexes on delete. The real Emby client,
// publisher and sweep run against it, so production code fills the cache.
type dupPeer struct {
	mu        sync.Mutex
	backdrops map[string][][]byte // platform artist id -> ordered backdrops
	failReads map[string]bool     // platform artist id -> backdrop reads answer 500
	// failFrom: the Nth backdrop request of a method, and every later one,
	// answers an error (absent = never). n counts them per method.
	failFrom, n map[string]int
	requests    atomic.Int64
}

func (p *dupPeer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests.Add(1)
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "Users" && parts[2] == "Items": // artist detail
		tags := make([]string, len(p.backdrops[parts[3]]))
		for i := range tags {
			tags[i] = fmt.Sprintf(`"tag%d"`, i)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Name":"x","Id":%q,"ImageTags":{},"BackdropImageTags":[%s],"ProviderIds":{}}`,
			parts[3], strings.Join(tags, ","))
	case len(parts) == 5 && parts[0] == "Items" && parts[3] == "Backdrop":
		id := parts[1]
		list := p.backdrops[id]
		idx, err := strconv.Atoi(parts[4])
		if err != nil || idx < 0 || idx >= len(list) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p.n[req.Method]++
		switch from := p.failFrom[req.Method]; {
		case p.failReads[id] && req.Method == http.MethodGet, from > 0 && p.n[req.Method] >= from:
			w.WriteHeader(http.StatusForbidden)
		case req.Method == http.MethodGet:
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(list[idx])
		case req.Method == http.MethodDelete:
			p.backdrops[id] = append(list[:idx:idx], list[idx+1:]...)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// platHarness is main.go's wiring on a real database, with the real services.
type platHarness struct {
	db               *sql.DB
	ctx              context.Context
	peer             *dupPeer
	artists          *artist.Service
	rules            *Service
	conns            *connection.Service
	libs             *library.Service
	lib              *library.Library
	pub              *publish.Publisher
	sweep            *publish.PlatformDupSweep
	engine           *Engine
	control          *Engine // same rules and database, no platform cache wired
	pipeline         *Pipeline
	fixer            *ImageDuplicateFixer
	pic, same, same2 []byte // one picture, three encodings: near-duplicates, not byte-identical
	other            []byte // a different picture
	failReload       bool   // the publisher's artist reload fails
}

// reloadGetter is the publisher's artist reader, able to fail on demand.
type reloadGetter struct {
	*artist.Service
	h *platHarness
}

func (g reloadGetter) GetByID(ctx context.Context, id string, o ...artist.HydrateOpts) (*artist.Artist, error) {
	if g.h.failReload {
		return nil, errors.New("database is locked")
	}
	return g.Service.GetByID(ctx, id, o...)
}

const platConnID = "c-emby"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (h *platHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := h.db.ExecContext(h.ctx, query, args...)
	must(t, err)
}

func newPlatHarness(t *testing.T) *platHarness {
	t.Helper()
	ctx := context.Background()
	db := setupTestDB(t)
	h := &platHarness{db: db, ctx: ctx, peer: &dupPeer{backdrops: map[string][][]byte{}, failReads: map[string]bool{},
		failFrom: map[string]int{}, n: map[string]int{}}}
	srv := httptest.NewServer(h.peer)
	t.Cleanup(srv.Close)

	h.artists, h.rules, h.libs = artist.NewService(db), NewService(db), library.NewService(db)
	enc, _, err := encryption.NewEncryptor("")
	must(t, err)
	h.conns = connection.NewService(db, enc)
	must(t, h.conns.Create(ctx, &connection.Connection{
		ID: platConnID, Name: "My Emby", Type: connection.TypeEmby, URL: srv.URL, APIKey: "k",
		Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
	}))
	h.lib = &library.Library{Name: "Music", Type: library.TypeRegular, Path: t.TempDir()}
	must(t, h.libs.Create(ctx, h.lib))
	// Only the rule under test is enabled, so health speaks for it alone.
	must(t, h.rules.SeedDefaults(ctx))
	all, err := h.rules.List(ctx)
	must(t, err)
	for i := range all {
		all[i].Enabled = all[i].ID == RuleImageDuplicate
		must(t, h.rules.Update(ctx, &all[i]))
	}

	dir := t.TempDir()
	read := func(name string, variant, quality int) []byte {
		p := filepath.Join(dir, name)
		createGradientJPEGQuality(t, p, variant, quality)
		b, err := os.ReadFile(p)
		must(t, err)
		return b
	}
	h.pic, h.same, h.same2, h.other = read("a.jpg", 0, 90), read("b.jpg", 0, 60), read("d.jpg", 0, 75), read("c.jpg", 1, 90)

	logger := testLogger()
	h.pub = publish.New(publish.Deps{
		ArtistService: h.artists, ArtistLister: h.artists, ArtistGetter: reloadGetter{h.artists, h}, ArtistImages: h.artists,
		ConnectionService: h.conns, LibraryService: h.libs, PlatformService: platform.NewService(db),
		ImageCacheDir: t.TempDir(), Logger: logger,
	})
	h.sweep = publish.NewPlatformDupSweep(h.pub, h.rules.PlatformDupSweepPolicy, publish.PlatformDupSweepConfig{}, logger)
	h.sweep.Cache().SetFindingObserver(func(id string) { // as main.go wires it
		if err := h.artists.MarkDirty(ctx, id, time.Now().UTC().Add(time.Second)); err != nil {
			t.Errorf("MarkDirty: %v", err)
		}
	})
	h.engine = NewEngine(h.rules, db, nil, h.libs, logger)
	h.engine.SetImageHashRecorder(h.artists)
	h.engine.SetPlatformDupCache(h.sweep.Cache())
	h.control = NewEngine(h.rules, db, nil, h.libs, logger)
	h.control.SetImageHashRecorder(h.artists)
	h.fixer = NewImageDuplicateFixer(db, nil, NewSharedFSCheck(h.libs, logger), h.artists, logger)
	h.fixer.SetPlatformPruner(h.pub, func() {})
	h.fixer.SetPlatformDupCache(h.sweep.Cache())
	h.pipeline = NewPipeline(h.engine, h.artists, h.rules, []Fixer{h.fixer}, nil, logger)
	h.setRule(t, true, 0)
	return h
}

// setRule sets the rule's platform option and tolerance, as the admin PUT does.
func (h *platHarness) setRule(t *testing.T, prune bool, tolerance float64) {
	t.Helper()
	r, err := h.rules.GetByID(h.ctx, RuleImageDuplicate)
	must(t, err)
	r.Config.PrunePlatformCopies, r.Config.Tolerance = prune, tolerance
	must(t, h.rules.Update(h.ctx, r))
	h.engine.InvalidateRuleCache()
	h.control.InvalidateRuleCache()
	h.pipeline.ClearRuleCache()
}

// settle makes the artist not dirty: only what the test does next can dirty it.
func (h *platHarness) settle(t *testing.T, a *artist.Artist) {
	t.Helper()
	h.exec(t, `UPDATE rules SET updated_at = datetime('now', '-2 hours')`)
	h.exec(t, `UPDATE artists SET dirty_since = NULL, rules_evaluated_at = datetime('now', '-1 hour') WHERE id = ?`, a.ID)
	if h.dirty(t, a) {
		t.Fatal("precondition: the artist must not be dirty after settle")
	}
}

func (h *platHarness) dirty(t *testing.T, a *artist.Artist) bool {
	t.Helper()
	ids, err := h.artists.ListDirtyIDs(h.ctx)
	must(t, err)
	return slices.Contains(ids, a.ID)
}

func (h *platHarness) run(t *testing.T, scope RunScope) *RunResult {
	t.Helper()
	res, err := h.pipeline.RunAllScoped(h.ctx, scope)
	must(t, err)
	return res
}

// row is the artist's violation row (any status) and its stored health score.
func (h *platHarness) row(t *testing.T, a *artist.Artist) (RuleViolation, float64) {
	t.Helper()
	rows, err := h.rules.ListViolationsFiltered(h.ctx, ViolationListParams{ArtistID: a.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("want exactly one violation row for %s, got %d (err %v)", a.Name, len(rows), err)
	}
	got, err := h.artists.GetByID(h.ctx, a.ID)
	must(t, err)
	return rows[0], got.HealthScore
}

// addArtist: a clean local folder, mapped to a platform item with backdrops.
func (h *platHarness) addArtist(t *testing.T, name string, backdrops ...[]byte) *artist.Artist {
	t.Helper()
	dir := t.TempDir()
	for file, b := range map[string][]byte{"fanart.jpg": h.pic, "fanart2.jpg": h.other} {
		must(t, os.WriteFile(filepath.Join(dir, file), b, 0o600))
	}
	a := &artist.Artist{Name: name, SortName: name, Path: dir, LibraryID: h.lib.ID}
	must(t, h.artists.Create(h.ctx, a))
	if backdrops != nil {
		h.peer.mu.Lock()
		h.peer.backdrops["p-"+name] = backdrops
		h.peer.mu.Unlock()
		must(t, h.artists.SetPlatformID(h.ctx, a.ID, platConnID, "p-"+name))
	}
	return a
}

func (h *platHarness) onPlatform(a *artist.Artist) [][]byte {
	h.peer.mu.Lock()
	defer h.peer.mu.Unlock()
	return append([][]byte(nil), h.peer.backdrops["p-"+a.Name]...)
}

func (h *platHarness) runSweep(t *testing.T) {
	t.Helper()
	_, err := h.sweep.Run(h.ctx)
	must(t, err)
}

func (h *platHarness) state(a *artist.Artist) publish.PlatformDupState {
	return h.sweep.Cache().Lookup(a.ID, defaultImageDupTolerance).State
}

func (h *platHarness) finding(t *testing.T, e *Engine, a *artist.Artist) *Violation {
	t.Helper()
	res, err := e.Evaluate(h.ctx, a)
	must(t, err)
	if len(res.Violations) == 0 {
		return nil
	}
	return &res.Violations[0]
}

// Option on: a clean LOCAL folder still gets a fixable finding naming the
// connection and counts, with no platform request. Option off: results are
// identical to an engine with no cache, even while the cache holds the finding.
func TestPlatformDupFinding_SurfacesOnlyWithTheOptionOn(t *testing.T) {
	h := newPlatHarness(t)
	dup := h.addArtist(t, "Dup", h.pic, h.other, h.same)
	h.runSweep(t)
	if h.state(dup) != publish.PlatformDupFound {
		t.Fatalf("precondition: the sweep must cache a finding, got %v", h.state(dup))
	}
	if v := h.finding(t, h.control, dup); v != nil {
		t.Fatalf("precondition: Dup's local folder must be clean, got %q", v.Message)
	}

	before := h.peer.requests.Load()
	v := h.finding(t, h.engine, dup)
	const want = "artist Dup: near-duplicate backdrops on connected platforms: My Emby has 1 redundant of 3 backdrops"
	if v == nil || v.Message != want || !v.Fixable || v.RuleID != RuleImageDuplicate || v.Category != "image" {
		t.Errorf("platform-only finding = %+v, want fixable %q", v, want)
	}
	if got := h.peer.requests.Load(); got != before {
		t.Errorf("the checker made %d platform request(s); it must only read the cache", got-before)
	}

	h.setRule(t, false, 0) // off; no sweep pass has cleared the cache yet
	if h.sweep.Cache().Len() == 0 {
		t.Fatal("precondition: the cache must still hold entries with the option off")
	}
	got, err := h.engine.Evaluate(h.ctx, dup)
	must(t, err)
	base, err := h.control.Evaluate(h.ctx, dup)
	must(t, err)
	if !reflect.DeepEqual(got, base) {
		t.Errorf("option off: result %+v differs from the cache-less engine's %+v", got, base)
	}
}

// Everything that is not a current, fixable finding reads as "no finding".
// Each case starts from a platform that really holds a near-duplicate pair.
func TestPlatformDupFinding_NotAFinding(t *testing.T) {
	h := newPlatHarness(t)
	dup := h.addArtist(t, "Dup", h.pic, h.other, h.same)
	clean := h.addArtist(t, "Clean", h.pic, h.other)
	protected := h.addArtist(t, "Protected", h.pic, h.other, h.same)
	h.exec(t, `INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, locked) VALUES ('prot-0', ?, 'fanart', 0, 1, 1)`,
		protected.ID)
	unreadable := h.addArtist(t, "Unreadable", h.pic, h.other, h.same)
	h.peer.failReads["p-Unreadable"] = true
	h.runSweep(t)
	unswept := h.addArtist(t, "Unswept", h.pic, h.other, h.same) // created after the pass

	for a, want := range map[*artist.Artist]publish.PlatformDupState{
		dup: publish.PlatformDupFound, clean: publish.PlatformDupClean, unswept: publish.PlatformDupUnknown,
		protected: publish.PlatformDupUndetermined, unreadable: publish.PlatformDupUndetermined,
	} {
		if got := h.state(a); got != want {
			t.Fatalf("precondition: %s cached as %v, want %v", a.Name, got, want)
		}
	}
	if h.finding(t, h.engine, dup) == nil {
		t.Fatal("precondition: Dup must be a finding before any case removes it")
	}
	locked, excluded, noPath, noLib, goneLib := *dup, *dup, *dup, *dup, *dup
	locked.Locked, excluded.IsExcluded, noPath.Path, noLib.LibraryID, goneLib.LibraryID = true, true, "", "", "gone"
	for name, a := range map[string]*artist.Artist{
		"never swept":                     unswept,
		"platform clean":                  clean,
		"policy skip (locked fanart)":     protected,
		"sweep read failed":               unreadable,
		"artist locked since the sweep":   &locked,
		"artist excluded since the sweep": &excluded,
		"no local folder (fixer refuses)": &noPath,
		"no library (fixer fails closed)": &noLib,
		"unknown library (fixer refuses)": &goneLib,
	} {
		if v := h.finding(t, h.engine, a); v != nil {
			t.Errorf("%s: raised %q, want no finding", name, v.Message)
		}
	}
	// The engine cannot tell whether the library is shared: fail closed.
	noLibs := NewEngine(h.rules, h.db, nil, nil, testLogger())
	noLibs.SetPlatformDupCache(h.sweep.Cache())
	if v := h.finding(t, noLibs, dup); v != nil {
		t.Errorf("no library service: raised %q", v.Message)
	}

	// Another tolerance than the entry's: not a finding until re-swept.
	h.setRule(t, true, 0.95)
	if v := h.finding(t, h.engine, dup); v != nil {
		t.Errorf("entry from tolerance 0.90 raised at 0.95: %q", v.Message)
	}
	h.setRule(t, true, 0)
	must(t, h.libs.SetSharedFSStatus(h.ctx, h.lib.ID, library.SharedFSConfirmed, "", ""))
	if v := h.finding(t, h.engine, dup); v != nil {
		t.Errorf("shared-filesystem library (fixer refuses) raised %q", v.Message)
	}
}

// What a fix leaves behind, in auto mode (the run fixes) and manual mode (run,
// Fix click, run), over two passes with a sweep between. Only a fix that
// removed every redundant copy is Fixed. One that FAILED, COULD NOT RUN or
// only PARTLY ran leaves the finding, its row and the health score as they
// were. One that completed and found nothing redundant drops the stale entry.
func TestPlatformDupFinding_FixOutcomes(t *testing.T) {
	const open, resolved = ViolationStatusOpen, ViolationStatusResolved
	const found, clean, unknown = publish.PlatformDupFound, publish.PlatformDupClean, publish.PlatformDupUnknown
	sql := func(q string) func(*platHarness, *testing.T) {
		return func(h *platHarness, t *testing.T) { h.exec(t, q) }
	}
	peer := func(f func(p *dupPeer, h *platHarness)) func(*platHarness, *testing.T) {
		return func(h *platHarness, _ *testing.T) { f(h.peer, h) }
	}
	four := func(p *dupPeer, h *platHarness) { p.backdrops["p-Dup"] = [][]byte{h.pic, h.other, h.same, h.same2} }
	for name, tc := range map[string]struct {
		breakIt   func(h *platHarness, t *testing.T)
		swept     publish.PlatformDupState // the entry once the sweep has run again; before that, Found or dropped
		wantRow   string
		wantScore float64
		removed   int  // backdrops the FIRST pass removes; a later pass removes none
		fixed     bool // the first pass reports Fixed
	}{
		"nothing goes wrong":                       {peer(func(*dupPeer, *platHarness) {}), clean, resolved, 100, 1, true},
		"two redundant copies":                     {peer(four), clean, resolved, 100, 2, true},
		"delete refused by the platform":           {peer(func(p *dupPeer, _ *platHarness) { p.failFrom["DELETE"] = 1 }), found, open, 0, 0, false},
		"connection unhealthy at fix time":         {sql(`UPDATE connections SET status = 'error'`), found, open, 0, 0, false},
		"platform unreadable at fix time":          {peer(func(p *dupPeer, _ *platHarness) { p.failReads["p-Dup"] = true }), found, open, 0, 0, false},
		"artist reload fails":                      {func(h *platHarness, _ *testing.T) { h.failReload = true }, found, open, 0, 0, false},
		"duplicate already gone from the platform": {peer(func(p *dupPeer, h *platHarness) { p.backdrops["p-Dup"] = [][]byte{h.pic, h.other} }), clean, resolved, 100, 0, false},
		// The 4th read is the pre-delete re-verify: plan skipped, no failure recorded.
		"re-verify read fails":  {peer(func(p *dupPeer, _ *platHarness) { p.failFrom["GET"] = p.n["GET"] + 4 }), found, open, 0, 0, false},
		"second delete refused": {peer(func(p *dupPeer, h *platHarness) { four(p, h); p.failFrom["DELETE"] = p.n["DELETE"] + 2 }), found, open, 0, 1, false},
	} {
		for _, auto := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/auto=%v", name, auto), func(t *testing.T) {
				h := newPlatHarness(t)
				if auto { // the run attempts the fix itself
					h.exec(t, `UPDATE rules SET automation_mode = ? WHERE id = ?`, AutomationModeAuto, RuleImageDuplicate)
					h.setRule(t, true, 0)
				}
				dup := h.addArtist(t, "Dup", h.pic, h.other, h.same)
				h.runSweep(t)
				h.peer.mu.Lock()
				tc.breakIt(h, t)
				h.peer.mu.Unlock()
				if h.finding(t, h.engine, dup) == nil {
					t.Fatal("precondition: the cached finding must be raised before the fix")
				}
				for pass := 1; pass <= 2; pass++ {
					removed, wantFixed := 0, pass == 1 && tc.fixed
					if pass == 1 {
						removed = tc.removed
					}
					held, before := len(h.onPlatform(dup)), h.peer.requests.Load()
					res := h.run(t, RunScopeAll)
					tried, fixed, msg := res.FixesAttempted > 0, res.FixesSucceeded > 0, fmt.Sprint(res.Results)
					if row, _ := h.row(t, dup); !auto && row.Status == open { // the operator clicks Fix
						fr, err := h.pipeline.FixViolation(h.ctx, row.ID)
						must(t, err)
						tried, fixed, msg = true, fr.Fixed, fr.Message
						h.run(t, RunScopeAll)
					}
					t.Logf("pass %d: platform requests=%d msg=%q", pass, h.peer.requests.Load()-before, msg)
					left := h.onPlatform(dup)
					if fixed != wantFixed || len(left) != held-removed || !bytes.Equal(left[0], h.pic) || !bytes.Equal(left[1], h.other) ||
						(tried && !strings.Contains(msg, fmt.Sprintf("removed %d backdrop(s)", removed))) {
						t.Fatalf("pass %d: fixed=%v, %d of %d backdrops left, %q; want fixed=%v, %d removed and reported, the local twin and the other picture kept",
							pass, fixed, len(left), held, msg, wantFixed, removed)
					}
					wantState := unknown // dropped, until the sweep reads the artist again
					if tc.swept == found || (pass == 2 && tc.swept == clean) {
						wantState = tc.swept
					}
					row, score := h.row(t, dup)
					if got := h.state(dup); got != wantState || row.Status != tc.wantRow || score != tc.wantScore {
						t.Errorf("pass %d: cache %v, row %s, health %v; want %v, %s, %v", pass, got, row.Status, score, wantState, tc.wantRow, tc.wantScore)
					}
					// Dropped in the second the run stamped its evaluation: must still count.
					if auto && pass == 1 && tc.swept != found && !h.dirty(t, dup) {
						t.Error("the artist is not dirty after its finding went away during the run")
					}
					h.runSweep(t) // production interleaves sweep passes with runs
					if got := h.state(dup); got != tc.swept {
						t.Errorf("pass %d: re-swept entry is %v, want %v", pass, got, tc.swept)
					}
				}
			})
		}
	}
}

// A fixable LOCAL duplicate plus a platform finding whose delete is refused:
// the fix must NOT be Fixed, or the row resolves with the platform finding
// standing. The local removal is still reported and persisted, in both modes.
func TestPlatformDupFinding_LocalFixedPlatformIncompleteStaysOpen(t *testing.T) {
	for _, auto := range []bool{true, false} {
		h := newPlatHarness(t)
		if auto {
			h.exec(t, `UPDATE rules SET automation_mode = ? WHERE id = ?`, AutomationModeAuto, RuleImageDuplicate)
			h.setRule(t, true, 0)
		}
		dup := h.addArtist(t, "Dup", h.pic, h.other, h.same)
		must(t, os.WriteFile(filepath.Join(dup.Path, "fanart3.jpg"), h.same2, 0o600)) // a local near-duplicate
		dup.FanartExists, dup.FanartCount = true, 3
		must(t, h.artists.Update(h.ctx, dup))
		h.runSweep(t)
		h.peer.failFrom["DELETE"] = 1
		res := h.run(t, RunScopeAll)
		row, _ := h.row(t, dup)
		fixed, msg := res.FixesSucceeded > 0, fmt.Sprint(res.Results)
		if !auto {
			fr, err := h.pipeline.FixViolation(h.ctx, row.ID)
			must(t, err)
			fixed, msg = fr.Fixed, fr.Message
		}
		row, _ = h.row(t, dup)
		got, err := h.artists.GetByID(h.ctx, dup.ID)
		must(t, err)
		rows := 0
		must(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND exists_flag = 1`, dup.ID).Scan(&rows))
		if fixed || row.Status != ViolationStatusOpen || h.state(dup) != publish.PlatformDupFound || len(h.onPlatform(dup)) != 3 ||
			!strings.Contains(msg, "removed 1 duplicate fanart file(s)") || !strings.Contains(msg, "removed 0 backdrop(s)") {
			t.Errorf("auto=%v: fixed=%v row=%s cache=%v %q; want not fixed, open, finding kept, both phases reported", auto, fixed, row.Status, h.state(dup), msg)
		}
		if got.FanartCount != 2 || rows != 2 {
			t.Errorf("auto=%v: fanart count %d, %d registry rows; want the local removal persisted (2 and 2)", auto, got.FanartCount, rows)
		}
	}
}

// State changed AFTER the sweep cached a finding, which the cache never hears
// about. The checker must stop offering a fix the prune would refuse or skip;
// a fix attempted anyway keeps the entry when the prune refused, and drops it
// when no target is left.
func TestPlatformDupFinding_StateChangedSinceTheSweep(t *testing.T) {
	const found, unknown = publish.PlatformDupFound, publish.PlatformDupUnknown
	for name, tc := range map[string]struct {
		change  string
		hidden  bool // the checker no longer raises the finding
		after   publish.PlatformDupState
		removed int // the exact tier still removes the byte-identical copy
	}{
		"fanart locked":   {`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, locked) VALUES ('x', ?1, 'fanart', 7, 1, 1)`, true, found, 1},
		"fanart user-set": {`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, source) VALUES ('x', ?1, 'fanart', 7, 1, 'user')`, true, found, 1},
		// The evaluated artist struct predates the lock; the prune reloads it.
		"artist locked":          {`UPDATE artists SET locked = 1, locked_at = datetime('now') WHERE id = ?1`, false, found, 1},
		"mapping removed":        {`DELETE FROM artist_platform_ids WHERE artist_id = ?1`, true, unknown, 0},
		"connection disabled":    {`UPDATE connections SET enabled = 0 WHERE ?1 = ?1`, true, unknown, 0},
		"image write turned off": {`UPDATE connections SET feature_image_write = 0 WHERE ?1 = ?1`, true, unknown, 0},
	} {
		h := newPlatHarness(t)
		dup := h.addArtist(t, "Dup", h.pic, h.other, h.same, h.other) // a near-duplicate pair and a byte-identical twin
		h.runSweep(t)
		if h.finding(t, h.engine, dup) == nil {
			t.Fatalf("%s: precondition: the finding must be raised before the change", name)
		}
		h.exec(t, tc.change, dup.ID)
		if hidden := h.finding(t, h.engine, dup) == nil; hidden != tc.hidden {
			t.Errorf("%s: checker hides the finding = %v, want %v", name, hidden, tc.hidden)
		}
		fr, err := h.fixer.Fix(h.ctx, dup, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{PrunePlatformCopies: true}})
		must(t, err)
		if fr.Fixed || h.state(dup) != tc.after || len(h.onPlatform(dup)) != 4-tc.removed {
			t.Errorf("%s: fix fixed=%v cache=%v platform=%d %q; want not fixed, cache %v, %d removed",
				name, fr.Fixed, h.state(dup), len(h.onPlatform(dup)), fr.Message, tc.after, tc.removed)
		}
	}
}

// The fixer's cache wiring must not depend on the order of its two setters.
func TestPlatformDupFinding_FixerWiringOrder(t *testing.T) {
	h := newPlatHarness(t)
	f := NewImageDuplicateFixer(h.db, nil, nonSharedFSCheck(), h.artists, testLogger())
	f.SetPlatformDupCache(h.sweep.Cache())
	for _, order := range []string{"cache then pruner", "pruner re-wired after the cache"} {
		f.SetPlatformPruner(h.pub, func() {})
		if w := f.platformPrune.Load(); w.dups != h.sweep.Cache() || w.pruner == nil {
			t.Errorf("%s: wiring %+v lost the cache or the pruner", order, w)
		}
	}
}

// A platform finding merged onto a local cross-type pair the fixer cannot
// remove: the fix prunes the platform and must NOT resolve the row, since the
// rule still fails. Real artist_images rows survive the fix's artist write.
func TestPlatformDupFinding_MergedOntoUnfixableLocalStaysOpen(t *testing.T) {
	h := newPlatHarness(t)
	both := h.addArtist(t, "Both", h.pic, h.other, h.same)
	both.ThumbExists, both.FanartExists, both.FanartCount = true, true, 2
	must(t, h.artists.Update(h.ctx, both))
	h.finding(t, h.control, both) // backfills the fanart hashes from disk
	h.exec(t, `UPDATE artist_images SET phash = (SELECT phash FROM artist_images
		WHERE artist_id = ?1 AND image_type = 'fanart' AND slot_index = 1) WHERE artist_id = ?1 AND image_type = 'thumb'`, both.ID)
	local := h.finding(t, h.control, both)
	if local == nil || local.Fixable {
		t.Fatalf("precondition: want a non-fixable local cross-type finding, got %+v", local)
	}
	h.runSweep(t)
	h.run(t, RunScopeAll)
	row, _ := h.row(t, both)
	merged := local.Message + "; also near-duplicate backdrops on connected platforms: My Emby has 1 redundant of 3 backdrops"
	if row.Status != ViolationStatusOpen || !row.Fixable || row.Message != merged {
		t.Errorf("merged row %+v, want open, fixable, %q", row, merged)
	}

	fr, err := h.pipeline.FixViolation(h.ctx, row.ID)
	must(t, err)
	if fr.Fixed || !fr.Irreversible || !strings.Contains(fr.Message, "removed 1 backdrop(s)") || !strings.Contains(fr.Message, "not resolved") {
		t.Errorf("fix result %+v, want not-fixed, irreversible, with the deletion and the reason recorded", fr)
	}
	if row, _ = h.row(t, both); row.Status != ViolationStatusOpen || len(h.onPlatform(both)) != 2 {
		t.Errorf("after the fix: row %s, %d platform backdrops; want open and 2", row.Status, len(h.onPlatform(both)))
	}
	h.run(t, RunScopeAll)
	if row, _ = h.row(t, both); row.Status != ViolationStatusOpen || row.Fixable || row.Message != local.Message {
		t.Errorf("after the next run: %+v, want the open, non-fixable local finding alone", row)
	}
}

// The DEFAULT Run Rules evaluates only dirty artists. A finding that appears
// or goes away changes the rule result with no artist write, so the cache
// must mark the artist dirty itself.
func TestPlatformDupFinding_IncrementalRunSeesCacheChanges(t *testing.T) {
	h := newPlatHarness(t)
	dup := h.addArtist(t, "Dup", h.pic, h.other, h.same)
	h.run(t, RunScopeAll) // evaluated before any sweep: clean
	h.settle(t, dup)

	h.runSweep(t) // the finding appears
	if res := h.run(t, RunScopeIncremental); res.ArtistsProcessed != 1 || res.ViolationsFound != 1 {
		t.Fatalf("incremental run after the sweep found a duplicate: processed %d, violations %d; want 1 and 1", res.ArtistsProcessed, res.ViolationsFound)
	}
	if row, score := h.row(t, dup); row.Status != ViolationStatusOpen || score != 0 {
		t.Errorf("row %s health %v, want open and 0", row.Status, score)
	}

	h.settle(t, dup)
	h.pub.LockBackdropTarget(platConnID, "p-Dup")() // a backdrop write drops the entry
	if !h.dirty(t, dup) {
		t.Error("a dropped finding did not mark the artist dirty")
	}

	// Option off: the rule edit re-dirties every artist (ListDirtyIDs), so the
	// next incremental run retires the platform-only row...
	h.runSweep(t)
	h.run(t, RunScopeIncremental)
	h.settle(t, dup)
	h.setRule(t, false, 0)
	if res := h.run(t, RunScopeIncremental); res.ArtistsProcessed != 1 {
		t.Fatalf("incremental run after the option was turned off processed %d artists, want 1", res.ArtistsProcessed)
	}
	if row, score := h.row(t, dup); row.Status != ViolationStatusResolved || score != 100 {
		t.Errorf("option off: row %s health %v, want resolved and 100", row.Status, score)
	}
	// ...and the sweep pass that clears the cache marks its findings' artists too.
	h.settle(t, dup)
	if h.state(dup) != publish.PlatformDupFound {
		t.Fatal("precondition: the entry must outlive the option until a sweep pass")
	}
	h.runSweep(t)
	if h.sweep.Cache().Len() != 0 || !h.dirty(t, dup) {
		t.Errorf("after the clearing pass: %d entries, dirty=%v; want 0 and true", h.sweep.Cache().Len(), h.dirty(t, dup))
	}
}
