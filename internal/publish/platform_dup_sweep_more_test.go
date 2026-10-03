package publish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// hookedFixture is a sweepFixture whose policy a test can steer: polErr is the
// policy's error, polCalls counts reads, and onPolicy (when set) runs first on
// each read with that count, so a test can change the answer at an exact point
// in a pass.
type hookedFixture struct {
	*sweepFixture
	polErr   error
	polCalls int
	onPolicy func(call int)
}

func newHookedFixture(t *testing.T, cfg PlatformDupSweepConfig, backdrops ...[][]byte) *hookedFixture {
	t.Helper()
	h := &hookedFixture{sweepFixture: newSweepFixture(t, cfg, backdrops...)}
	h.sweep.policy = func(context.Context) (float64, bool, error) {
		if h.polCalls++; h.onPolicy != nil {
			h.onPolicy(h.polCalls)
		}
		return h.tol, h.on, h.polErr
	}
	return h
}

// pagedLister pages like the real artist service (the shared fake returns
// everything on page 1).
type pagedLister struct{ all []artist.Artist }

func (l pagedLister) List(_ context.Context, p artist.ListParams) ([]artist.Artist, int, error) {
	lo := min((p.Page-1)*p.PageSize, len(l.all))
	return l.all[lo:min(lo+p.PageSize, len(l.all))], len(l.all), nil
}

// An artist with no prune target stays Unknown: nothing was read, so nothing
// can be called clean.
func TestPlatformDupSweep_NoTargetStaysUnknown(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{f.small, f.big})
	fx.p.connectionService.(*fakeConnectionGetter).conns["c-emby"].Enabled = false
	st := fx.run(t)
	if e := fx.lookup("a1"); e.State != PlatformDupUnknown || !slices.Contains(e.Reasons, PlatformDupReasonNoTarget) {
		t.Errorf("entry %+v, want Unknown with reason no_platform_target", e)
	}
	if fx.client.calls != 0 || st.NoTarget != 1 {
		t.Errorf("disabled connection: %d platform calls, stats %+v, want 0 and one no-target", fx.client.calls, st)
	}
}

// A Stillwater backdrop write invalidates that artist's entry (and only that
// artist's), through the per-target lock every writer takes.
func TestPlatformDupSweep_BackdropWriteInvalidatesTheArtist(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{f.small, f.big}, [][]byte{f.small, f.big})
	fx.run(t)
	if fx.lookup("a1").State != PlatformDupFound || fx.lookup("a2").State != PlatformDupFound {
		t.Fatalf("precondition: both artists must hold a finding, got %+v / %+v", fx.lookup("a1"), fx.lookup("a2"))
	}
	// The fixer's platform phase: the real entry point, not a dry run.
	res, err := fx.p.PrunePlatformBackdropsForArtist(context.Background(), &artist.Artist{ID: "a1"},
		ArtistBackdropPruneOptions{Perceptual: true, Tolerance: 0.90})
	if err != nil || res.BackdropsRemoved != 1 {
		t.Fatalf("prune: removed %d err %v, want 1", res.BackdropsRemoved, err)
	}
	if e := fx.lookup("a1"); e.State != PlatformDupUnknown {
		t.Errorf("a1 after a platform delete reads %+v, want Unknown", e)
	}
	if e := fx.lookup("a2"); e.State != PlatformDupFound {
		t.Errorf("a2 was invalidated by a1's write: %+v", e)
	}
	// Any other writer (sync, push, phash repair) takes the same lock.
	fx.p.LockBackdropTarget("c-emby", "p2")()
	if e := fx.lookup("a2"); e.State != PlatformDupUnknown {
		t.Errorf("a2 after a backdrop write lock reads %+v, want Unknown", e)
	}
	if st := fx.run(t); st.Swept != 2 || fx.lookup("a1").State != PlatformDupClean || fx.lookup("a2").State != PlatformDupFound {
		t.Errorf("re-sweep: stats %+v a1 %+v a2 %+v, want both refreshed (a1 now clean)", st, fx.lookup("a1"), fx.lookup("a2"))
	}
}

// The sweep judges at the policy's tolerance, and an entry computed at another
// tolerance reads as stale. The pair differs from a duplicate along that axis
// only: one picture at 0.90, two at 0.95.
func TestPlatformDupSweep_ToleranceGovernsFindingsAndStaleness(t *testing.T) {
	a, b := fieldJPEG(t, 7, 1), blendJPEG(t, 7, 13, 0.20, 1)
	if s := similarity(t, a, b); s < 0.90 || s >= 0.95 {
		t.Fatalf("fixture precondition: similarity %.4f, want within [0.90, 0.95)", s)
	}
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{a, b})
	fx.run(t)
	if e := fx.lookup("a1"); e.State != PlatformDupFound || e.Tolerance != 0.90 {
		t.Fatalf("at 0.90: entry %+v, want Found", e)
	}
	fx.tol = 0.95 // the operator raised the rule's tolerance
	stale := fx.lookup("a1")
	if stale.State != PlatformDupUnknown || !slices.Contains(stale.Reasons, PlatformDupReasonStaleTolerance) ||
		stale.Tolerance != 0.90 || stale.ComputedAt.IsZero() {
		t.Errorf("0.90 entry read at 0.95: %+v, want Unknown/stale_tolerance carrying its own stamp", stale)
	}
	if st := fx.run(t); st.Swept != 1 {
		t.Fatalf("tolerance change must re-sweep; stats %+v", st)
	}
	if e := fx.lookup("a1"); e.State != PlatformDupClean || e.Tolerance != 0.95 {
		t.Errorf("at 0.95: entry %+v, want Clean", e)
	}
}

// Canceling mid-pass stops before the next artist, stores nothing for the
// artist in flight, and does not Warn once per remaining artist.
func TestPlatformDupSweep_CancelStopsPromptlyWithoutPartialEntries(t *testing.T) {
	f := newPerceptualFixture(t)
	pair := [][]byte{f.small, f.other}
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, pair, pair, pair, pair)
	ctx, cancel := context.WithCancel(context.Background())
	fx.client.onFetch = func(id string) error {
		if id == "p2" {
			cancel()
			return ctx.Err() // as a real client's aborted request
		}
		return nil
	}
	st, err := fx.sweep.Run(ctx)
	if err != nil || !st.Stopped || st.Swept != 1 {
		t.Fatalf("stats %+v err %v, want a stopped pass that completed one artist", st, err)
	}
	if e := fx.lookup("a1"); e.State != PlatformDupClean {
		t.Errorf("a1 (finished before the cancel) reads %+v, want Clean", e)
	}
	for _, id := range []string{"a2", "a3", "a4"} {
		if e := fx.lookup(id); e.State != PlatformDupUnknown || !e.ComputedAt.IsZero() {
			t.Errorf("%s reads %+v after a canceled pass, want Unknown with no entry", id, e)
		}
	}
	if slices.Contains(fx.client.fetched, "p3") || slices.Contains(fx.client.fetched, "p4") {
		t.Errorf("platform reads continued after the cancel: %v", fx.client.fetched)
	}
	if n := strings.Count(fx.logs.String(), "level=WARN"); n > 1 {
		t.Errorf("%d Warn lines for one canceled pass, want at most the in-flight artist's:\n%s", n, fx.logs)
	}
}

// A dead platform stops the pass instead of failing once per artist, and the
// per-pass cap bounds a healthy one.
func TestPlatformDupSweep_BoundsThePass(t *testing.T) {
	f := newPerceptualFixture(t)
	pair := [][]byte{f.small, f.other}
	lib := make([][][]byte, platformDupSweepMaxConsecutiveFailures+3)
	for i := range lib {
		lib[i] = pair
	}
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, lib...)
	fx.client.onFetch = func(string) error { return errors.New("connection refused") }
	if st := fx.run(t); !st.Stopped || st.Swept != platformDupSweepMaxConsecutiveFailures {
		t.Errorf("dead platform: stats %+v, want stopped after %d artists", st, platformDupSweepMaxConsecutiveFailures)
	}

	fx = newSweepFixture(t, PlatformDupSweepConfig{MaxPerPass: 2}, pair, pair, pair)
	if st := fx.run(t); st.Swept != 2 || !st.Stopped || fx.lookup("a3").State != PlatformDupUnknown {
		t.Errorf("capped pass: stats %+v a3 %+v, want 2 swept and a3 left Unknown", st, fx.lookup("a3"))
	}
	if st := fx.run(t); st.Swept != 1 || fx.lookup("a3").State != PlatformDupClean {
		t.Errorf("next pass: stats %+v a3 %+v, want it to resume with a3", st, fx.lookup("a3"))
	}
}

// Each way a pass must end early or leave an artist alone.
func TestPlatformDupSweep_StopsAndSkips(t *testing.T) {
	f := newPerceptualFixture(t)
	dup := [][]byte{f.small, f.big}
	midPass := func(change func(fx *hookedFixture)) func(*hookedFixture) {
		return func(fx *hookedFixture) {
			fx.onPolicy = func(call int) {
				if call == 3 { // pass start, before a1, before a2
					change(fx)
				}
			}
		}
	}
	for name, tc := range map[string]struct {
		setup   func(fx *hookedFixture)
		wantErr bool
		swept   int    // artists read from the platform
		wantLog string // a Warn the pass must leave
	}{
		"excluded artist is not swept":  {func(fx *hookedFixture) { fx.getter.artists["a2"].IsExcluded = true }, false, 1, ""},
		"policy error at pass start":    {func(fx *hookedFixture) { fx.polErr = errors.New("db down") }, true, 0, ""},
		"policy error mid-pass":         {midPass(func(fx *hookedFixture) { fx.polErr = errors.New("db down") }), false, 1, "policy unreadable mid-pass"},
		"option turned off mid-pass":    {midPass(func(fx *hookedFixture) { fx.on = false }), false, 1, ""},
		"tolerance changed mid-pass":    {midPass(func(fx *hookedFixture) { fx.tol = 0.95 }), false, 1, ""},
		"artist list error is returned": {func(fx *hookedFixture) { fx.p.artistLister = errArtistLister{} }, true, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newHookedFixture(t, PlatformDupSweepConfig{}, dup, dup)
			tc.setup(fx)
			st, err := fx.sweep.Run(context.Background())
			if (err != nil) != tc.wantErr || st.Swept != tc.swept || fx.fetches("p2") != 0 {
				t.Errorf("err %v stats %+v, %d fetches for a2; want error=%v, %d swept, a2 never read", err, st, fx.fetches("p2"), tc.wantErr, tc.swept)
			}
			if e := fx.sweep.Cache().Lookup("a2", 0.90); e.State != PlatformDupUnknown {
				t.Errorf("a2 reads %+v, want Unknown", e)
			}
			if (tc.swept == 1) != (fx.sweep.Cache().Lookup("a1", 0.90).State == PlatformDupFound) {
				t.Errorf("a1 reads %+v with %d swept", fx.sweep.Cache().Lookup("a1", 0.90), tc.swept)
			}
			if tc.wantLog != "" && !strings.Contains(fx.logs.String(), tc.wantLog) {
				t.Errorf("no %q in the log:\n%s", tc.wantLog, fx.logs)
			}
		})
	}

	// A failed policy read keeps what is cached, and excluding an artist drops
	// the entry it already had.
	fx := newHookedFixture(t, PlatformDupSweepConfig{}, dup, dup)
	fx.run(t)
	fx.polErr = errors.New("db down")
	if _, err := fx.sweep.Run(context.Background()); err == nil || fx.lookup("a1").State != PlatformDupFound {
		t.Errorf("after a failed policy read: err %v a1 %+v, want an error and the entry kept", err, fx.lookup("a1"))
	}
	fx.polErr, fx.getter.artists["a1"].IsExcluded = nil, true
	if fx.run(t); fx.lookup("a1").State != PlatformDupUnknown || fx.lookup("a2").State != PlatformDupFound {
		t.Errorf("after excluding a1: a1 %+v a2 %+v, want a1 dropped and a2 kept", fx.lookup("a1"), fx.lookup("a2"))
	}
}

// A decided entry is good for EntryTTL; an undecided one is re-read after the
// shorter RetryAfter. Driven through the sweep's clock.
func TestPlatformDupSweep_RetryWindowIsShorterThanTheTTL(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{f.small, f.big}, [][]byte{f.small, f.other})
	clock := time.Now()
	fx.sweep.now = func() time.Time { return clock }
	fx.fakes[1].failAt = 0 // a2: read failure, so Undetermined
	fx.run(t)
	if fx.lookup("a1").State != PlatformDupFound || fx.lookup("a2").State != PlatformDupUndetermined {
		t.Fatalf("precondition: a1 %+v a2 %+v, want Found and Undetermined", fx.lookup("a1"), fx.lookup("a2"))
	}
	for _, step := range []struct {
		advance  time.Duration
		a1, a2   bool // re-read on this pass
		whatItIs string
	}{
		{time.Hour, false, false, "inside both windows"},
		{fx.sweep.cfg.RetryAfter, false, true, "past the retry window only"},
		{fx.sweep.cfg.EntryTTL, true, true, "past the TTL"},
	} {
		clock = clock.Add(step.advance)
		p1, p2 := fx.fetches("p1"), fx.fetches("p2")
		fx.run(t)
		if (fx.fetches("p1") > p1) != step.a1 || (fx.fetches("p2") > p2) != step.a2 {
			t.Errorf("%s: a1 re-read=%v a2 re-read=%v, want %v/%v", step.whatItIs, fx.fetches("p1") > p1, fx.fetches("p2") > p2, step.a1, step.a2)
		}
	}
}

// The walk continues past the first page of artists, and one good read resets
// the consecutive-failure run (11 failures around 1 success never reach 10).
func TestPlatformDupSweep_PagesTheLibraryAndResetsTheFailureRun(t *testing.T) {
	f := newPerceptualFixture(t)
	lib := make([][][]byte, scanBackdropPageSize+1)
	for i := range lib {
		lib[i] = [][]byte{f.other} // one backdrop: read, nothing to compare
	}
	fx := newSweepFixture(t, PlatformDupSweepConfig{MaxPerPass: len(lib)}, lib...)
	fx.p.artistLister = pagedLister{fx.p.artistLister.(*fakePlatformLister).artists}
	last := fmt.Sprintf("a%d", len(lib))
	if st := fx.run(t); st.Artists != len(lib) || fx.lookup(last).State != PlatformDupClean {
		t.Errorf("stats %+v, %s reads %+v; want every artist listed and the last one swept", st, last, fx.lookup(last))
	}

	pair := [][]byte{f.small, f.other}
	fx = newSweepFixture(t, PlatformDupSweepConfig{}, pair, pair, pair, pair, pair, pair, pair, pair, pair, pair, pair, pair)
	fx.client.onFetch = func(id string) error {
		if id == "p6" {
			return nil
		}
		return errors.New("connection refused")
	}
	if st := fx.run(t); st.Stopped || st.Swept != 12 || st.Clean != 1 {
		t.Errorf("stats %+v, want all 12 swept (1 clean) with the failure run reset by the success", st)
	}
}

// Start keeps running passes on the interval, not just the first one.
func TestPlatformDupSweep_RunsAgainAfterTheInterval(t *testing.T) {
	fx := newHookedFixture(t, PlatformDupSweepConfig{StartupDelay: time.Millisecond, Interval: time.Millisecond})
	fx.on = false // a pass is one policy read and no platform I/O
	passes := make(chan struct{}, 8)
	fx.onPolicy = func(int) {
		select {
		case passes <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fx.sweep.Start(ctx) }()
	// Registered before any fatal assertion, so a failure below still stops and
	// joins the loop; bounded, so a shutdown regression fails instead of hanging.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Start did not return after ctx was canceled")
		}
	})
	for n := 1; n <= 3; n++ {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatalf("pass %d never ran", n)
		}
	}
}

// The constructor refuses a nil publisher and a nil policy with its OWN panic,
// not whatever nil dereference would follow later.
func TestNewPlatformDupSweep_RefusesMissingWiring(t *testing.T) {
	policy := func(context.Context) (float64, bool, error) { return 0, false, nil }
	for name, build := range map[string]func(){
		"nil policy":    func() { NewPlatformDupSweep(New(Deps{Logger: silentLogger()}), nil, PlatformDupSweepConfig{}, nil) },
		"nil publisher": func() { NewPlatformDupSweep(nil, policy, PlatformDupSweepConfig{}, nil) },
	} {
		func() {
			defer func() {
				if msg, _ := recover().(string); !strings.Contains(msg, "nil publisher or policy") {
					t.Errorf("%s: recovered %q, want the constructor's own refusal", name, msg)
				}
			}()
			build()
		}()
	}
}

// A read that panics is still an attempted read and a failure: both pass
// bounds apply, so a platform that panics on every artist cannot make one
// pass walk the whole library.
func TestPlatformDupSweep_PanickingReadsAreBounded(t *testing.T) {
	f := newPerceptualFixture(t)
	lib := make([][][]byte, platformDupSweepMaxConsecutiveFailures+3)
	for i := range lib {
		lib[i] = [][]byte{f.small, f.other}
	}
	for name, tc := range map[string]struct{ maxPerPass, wantSwept int }{
		"failure run stops the pass":  {0, platformDupSweepMaxConsecutiveFailures},
		"per-pass cap stops the pass": {2, 2},
	} {
		fx := newSweepFixture(t, PlatformDupSweepConfig{MaxPerPass: tc.maxPerPass}, lib...)
		fx.client.onFetch = func(string) error { panic("boom") }
		if st := fx.run(t); !st.Stopped || st.Swept != tc.wantSwept || len(fx.client.fetched) != tc.wantSwept {
			t.Errorf("%s: stats %+v, %d artists read; want the pass stopped after %d", name, st, len(fx.client.fetched), tc.wantSwept)
		}
	}
}

// The option is re-read before EVERY artist, including ones whose entry is
// still fresh: turned off mid-pass, the pass stops at the next artist.
func TestPlatformDupSweep_OptionOffStopsAPassOverFreshEntries(t *testing.T) {
	f := newPerceptualFixture(t)
	pair := [][]byte{f.small, f.other}
	fx := newHookedFixture(t, PlatformDupSweepConfig{}, pair, pair, pair)
	fx.run(t) // every artist now has a fresh entry
	fx.sweep.Cache().Invalidate("a1")
	fx.client.onFetch = func(string) error { fx.on = false; return nil } // off while a1 is being read
	fx.polCalls = 0
	if st := fx.run(t); !st.Stopped || st.Artists != 2 || fx.polCalls != 3 {
		t.Errorf("stats %+v after %d policy reads; want the pass stopped at a2 (start, a1, a2)", st, fx.polCalls)
	}
}
