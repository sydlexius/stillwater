package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// sweepClient counts every platform call and can interpose on a backdrop
// fetch. The platform STATE lives in the per-artist fakeBackdropClient behind
// it (a backdrop list that renumbers on delete).
type sweepClient struct {
	byItemClient
	calls   int
	fetched []string                            // platform artist id per backdrop fetch
	onFetch func(platformArtistID string) error // nil = pass through
}

func (c *sweepClient) GetArtistDetail(ctx context.Context, id string) (*connection.ArtistPlatformState, error) {
	c.calls++
	return c.byItemClient.GetArtistDetail(ctx, id)
}

func (c *sweepClient) GetArtistBackdrop(ctx context.Context, id string, i int) ([]byte, string, error) {
	c.calls++
	c.fetched = append(c.fetched, id)
	if c.onFetch != nil {
		if err := c.onFetch(id); err != nil {
			return nil, "", err
		}
	}
	return c.byItemClient.GetArtistBackdrop(ctx, id, i)
}

func (c *sweepClient) DeleteImageAtIndex(ctx context.Context, id, typ string, i int) error {
	c.calls++
	return c.byItemClient.DeleteImageAtIndex(ctx, id, typ, i)
}

// sweepFixture is a real Publisher over artists a1..aN (platform items
// p1..pN on one healthy Emby connection), each holding the given backdrops,
// plus a sweep whose policy the test controls.
type sweepFixture struct {
	p      *Publisher
	sweep  *PlatformDupSweep
	client *sweepClient
	fakes  []*fakeBackdropClient
	getter *fakeArtistGetter
	logs   *bytes.Buffer
	tol    float64
	on     bool
}

func newSweepFixture(t *testing.T, cfg PlatformDupSweepConfig, backdrops ...[][]byte) *sweepFixture {
	t.Helper()
	fx := &sweepFixture{client: &sweepClient{byItemClient: byItemClient{}}, logs: &bytes.Buffer{}, tol: 0.90, on: true}
	fx.p = newTestPublisherWithNArtistsOnePlatform(t, len(backdrops), fx.client)
	fx.p.logger = slog.New(slog.NewTextHandler(fx.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fx.p.artistImages = &fakeArtistImages{}
	dir := t.TempDir() // local fanart no platform backdrop is a twin of
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("unrelated local fanart"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := fx.p.artistLister.(*fakePlatformLister)
	fx.p.artistService = perArtistLister{l}
	fx.getter = &fakeArtistGetter{artists: map[string]*artist.Artist{}}
	fx.p.artistGetter = fx.getter
	for i, b := range backdrops {
		fake := &fakeBackdropClient{backdrops: slices.Clone(b), failAt: -1, failDeleteAt: -1}
		fx.fakes = append(fx.fakes, fake)
		fx.client.byItemClient[fmt.Sprintf("p%d", i+1)] = fake
		l.artists[i].Path = dir
		fx.getter.artists[l.artists[i].ID] = &l.artists[i]
	}
	fx.sweep = NewPlatformDupSweep(fx.p, func(context.Context) (float64, bool, error) { return fx.tol, fx.on, nil }, cfg, fx.p.logger)
	return fx
}

// fetches counts backdrop fetches for one platform item.
func (fx *sweepFixture) fetches(platformArtistID string) int {
	n := 0
	for _, id := range fx.client.fetched {
		if id == platformArtistID {
			n++
		}
	}
	return n
}

func (fx *sweepFixture) run(t *testing.T) PlatformDupSweepStats {
	t.Helper()
	st, err := fx.sweep.Run(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return st
}

func (fx *sweepFixture) lookup(id string) PlatformDupEntry {
	return fx.sweep.Cache().Lookup(id, fx.tol)
}

// Option off (the default) means NO platform I/O and no entries, and turning
// it off later drops what an earlier pass cached.
func TestPlatformDupSweep_OptionOffDoesNoPlatformIO(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{f.small, f.big})
	fx.on = false
	if st := fx.run(t); st.Enabled || st.Swept != 0 {
		t.Fatalf("option off: stats %+v, want a disabled pass that swept nothing", st)
	}
	if fx.client.calls != 0 || fx.sweep.Cache().Len() != 0 {
		t.Fatalf("option off: %d platform calls, %d entries, want 0/0", fx.client.calls, fx.sweep.Cache().Len())
	}
	// Precondition for the assertion above: the SAME fixture does platform I/O
	// and caches a finding once the option is on.
	fx.on = true
	fx.run(t)
	if fx.client.calls == 0 || fx.lookup("a1").State != PlatformDupFound {
		t.Fatalf("option on: calls %d entry %+v, want I/O and a finding", fx.client.calls, fx.lookup("a1"))
	}
	fx.on = false
	before := fx.client.calls
	fx.run(t)
	if fx.client.calls != before || fx.lookup("a1").State != PlatformDupUnknown {
		t.Errorf("option turned off: %d new calls, entry %+v, want none and Unknown", fx.client.calls-before, fx.lookup("a1"))
	}
}

// Findings, clean and never-swept are three different answers, and the sweep
// leaves the platform exactly as it found it.
func TestPlatformDupSweep_CachesFindingsAndNeverDeletes(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{},
		[][]byte{f.small, f.other, f.big, f.other}, // one perceptual pair + one exact twin
		[][]byte{f.small, f.other})
	st := fx.run(t)

	got := fx.lookup("a1")
	want := []PlatformDupFinding{{ConnectionID: "c-emby", Connection: "emby", Backdrops: 4, Redundant: 2}}
	if got.State != PlatformDupFound || !slices.Equal(got.Findings, want) || got.Redundant() != 2 {
		t.Errorf("a1 entry %+v, want Found with %+v", got, want)
	}
	if got.ComputedAt.IsZero() || got.Tolerance != 0.90 {
		t.Errorf("a1 entry computed at %v tolerance %v, want stamped at 0.90", got.ComputedAt, got.Tolerance)
	}
	if e := fx.lookup("a2"); e.State != PlatformDupClean || len(e.Findings) != 0 || len(e.Reasons) != 0 {
		t.Errorf("a2 entry %+v, want Clean", e)
	}
	if e := fx.lookup("never-swept"); e.State != PlatformDupUnknown || !e.ComputedAt.IsZero() {
		t.Errorf("never-swept artist reads %+v, want the zero (Unknown) entry", e)
	}
	if st.Swept != 2 || st.Found != 1 || st.Clean != 1 || st.Redundant != 2 || st.Stopped {
		t.Errorf("stats %+v, want 2 swept: 1 found (2 redundant), 1 clean", st)
	}
	// The peer's state, not the calls made: nothing was deleted.
	assertPlatform(t, fx.fakes[0], f.small, f.other, f.big, f.other)
	assertPlatform(t, fx.fakes[1], f.small, f.other)

	// Incremental: a pass over current entries does no platform I/O.
	before := fx.client.calls
	if st = fx.run(t); fx.client.calls != before || st.Swept != 0 {
		t.Errorf("second pass made %d platform calls and swept %d, want 0/0", fx.client.calls-before, st.Swept)
	}
}

// Every way the sweep can fail to decide yields Undetermined with its reason.
// The platform holds a real near-duplicate pair in every case, so "clean"
// would be wrong and "found" would rest on a judgement that was refused.
func TestPlatformDupSweep_CouldNotDetermineIsNeitherCleanNorFinding(t *testing.T) {
	f := newPerceptualFixture(t)
	cases := map[string]func(t *testing.T, fx *sweepFixture){
		"":                          func(*testing.T, *sweepFixture) {}, // control: the fixture IS a finding
		PlatformDupReasonReadFailed: func(_ *testing.T, fx *sweepFixture) { fx.fakes[0].failAt = 1 },
		PlatformDupReasonUnhealthy: func(_ *testing.T, fx *sweepFixture) {
			fx.p.connectionService.(*fakeConnectionGetter).conns["c-emby"].Status = "error"
		},
		PlatformDupReasonError: func(_ *testing.T, fx *sweepFixture) { fx.getter.err = errors.New("db down") },
		PruneSkipLockedArtist:  func(_ *testing.T, fx *sweepFixture) { fx.getter.artists["a1"].Locked = true },
		PruneSkipProtectedFanart: func(_ *testing.T, fx *sweepFixture) {
			fx.p.artistImages = &fakeArtistImages{rows: []artist.ArtistImage{{ImageType: "fanart", Locked: true}}}
		},
		PruneSkipUnreadableLocal: func(t *testing.T, fx *sweepFixture) {
			dir := t.TempDir()
			if err := os.Symlink(filepath.Join(dir, "missing.jpg"), filepath.Join(dir, "fanart.jpg")); err != nil {
				t.Fatal(err)
			}
			fx.getter.artists["a1"].Path = dir
		},
	}
	for reason, setup := range cases {
		t.Run("reason="+reason, func(t *testing.T) {
			fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{f.small, f.big})
			setup(t, fx)
			st := fx.run(t)
			e := fx.lookup("a1")
			if reason == "" {
				if e.State != PlatformDupFound {
					t.Fatalf("control: entry %+v, want Found", e)
				}
				return
			}
			if e.State != PlatformDupUndetermined || len(e.Findings) != 0 || !slices.Contains(e.Reasons, reason) {
				t.Errorf("entry %+v, want Undetermined with reason %q and no findings", e, reason)
			}
			if st.Undetermined != 1 || st.Clean != 0 || st.Found != 0 {
				t.Errorf("stats %+v, want exactly one undetermined", st)
			}
			// The two ordinary policy skips are quiet. (An unreadable local file
			// is a real fault and keeps the reconciler's own Warn.)
			if quiet := reason == PruneSkipLockedArtist || reason == PruneSkipProtectedFanart; quiet &&
				strings.Contains(fx.logs.String(), "level=WARN") {
				t.Errorf("a policy skip logged at Warn:\n%s", fx.logs)
			}
		})
	}
}

// A write that lands while the sweep is reading the same target must not be
// overwritten by the sweep's (possibly pre-write) result.
func TestPlatformDupSweep_ResultRacedByAWriteIsDiscarded(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, [][]byte{f.small, f.big})
	fx.client.onFetch = func(string) error {
		fx.sweep.Cache().invalidateTarget("c-emby", "p1") // what the write observer delivers
		return nil
	}
	st := fx.run(t)
	if e := fx.lookup("a1"); e.State != PlatformDupUnknown || st.Discarded != 1 || st.Found != 0 {
		t.Errorf("entry %+v stats %+v, want the raced result discarded, uncounted, and the artist Unknown", e, st)
	}
}

// Two passes never overlap, the cache is readable and invalidatable while a
// pass is mid-read, and Start returns once ctx is canceled. CONCURRENT: run
// under -race.
func TestPlatformDupSweep_NoOverlapConcurrentReadsAndShutdown(t *testing.T) {
	f := newPerceptualFixture(t)
	fx := newSweepFixture(t, PlatformDupSweepConfig{StartupDelay: time.Millisecond, Interval: time.Hour},
		[][]byte{f.small, f.big}, [][]byte{f.small, f.other})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fx.client.onFetch = func(string) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fx.sweep.Start(ctx) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep never started its first pass")
	}
	// The pass is parked inside a platform read, so this is a genuine overlap.
	// Run on its own goroutine: an overlapping pass would park in the same read,
	// and must fail this test rather than hang it.
	second := make(chan error, 1)
	go func() { _, err := fx.sweep.Run(context.Background()); second <- err }()
	select {
	case err := <-second:
		if !errors.Is(err, ErrPlatformDupSweepRunning) {
			t.Errorf("second Run during a pass: err %v, want ErrPlatformDupSweepRunning", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a second Run started while a pass was in flight")
	}
	// No cache lock is held across that read: readers and writers do not block.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = fx.sweep.Cache().Lookup("a1", 0.90)
			fx.p.LockBackdropTarget("c-emby", "p2")()
			fx.sweep.Cache().Invalidate("a2")
		}()
	}
	wg.Wait()
	close(release)
	deadline := time.After(5 * time.Second)
	for fx.sweep.Cache().Lookup("a2", 0.90).State == PlatformDupUnknown {
		select {
		case <-deadline:
			t.Fatal("the pass never finished")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx was canceled")
	}
}

// A policy skip is known from local state, so it costs NO platform request:
// not on the first pass and not when the retry window reopens it.
func TestPlatformDupSweep_PolicySkipDoesNoPlatformIO(t *testing.T) {
	f := newPerceptualFixture(t)
	pair := [][]byte{f.small, f.big} // a real near-duplicate pair behind each skip
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, pair, pair, pair)
	clock := time.Now()
	fx.sweep.now = func() time.Time { return clock }
	fx.getter.artists["a1"].Locked = true
	fx.getter.artists["a2"].Path = ""
	for pass := 1; pass <= 2; pass++ {
		st := fx.run(t)
		if fx.fetches("p1")+fx.fetches("p2") != 0 || st.Undetermined != 2 {
			t.Fatalf("pass %d: %d platform fetches for the skipped artists, stats %+v, want 0 and 2 undetermined", pass, fx.fetches("p1")+fx.fetches("p2"), st)
		}
		for id, reason := range map[string]string{"a1": PruneSkipLockedArtist, "a2": PruneSkipNoLocalFolder} {
			if e := fx.lookup(id); e.State != PlatformDupUndetermined || !slices.Contains(e.Reasons, reason) {
				t.Errorf("pass %d: %s reads %+v, want Undetermined/%s", pass, id, e, reason)
			}
		}
		clock = clock.Add(fx.sweep.cfg.RetryAfter + time.Minute)
	}
	if fx.lookup("a3").State != PlatformDupFound { // precondition: the same pair IS read when nothing skips it
		t.Errorf("control a3 reads %+v, want Found", fx.lookup("a3"))
	}
}

// One artist that panics is recorded and stepped over: the artists after it
// are swept in the same pass, and it is not retried on the next one.
func TestPlatformDupSweep_PanicOnOneArtistDoesNotStopThePass(t *testing.T) {
	f := newPerceptualFixture(t)
	pair := [][]byte{f.small, f.other}
	fx := newSweepFixture(t, PlatformDupSweepConfig{}, pair, pair, pair)
	fx.client.onFetch = func(id string) error {
		if id == "p2" {
			panic("boom")
		}
		return nil
	}
	st := fx.run(t)
	if fx.lookup("a1").State != PlatformDupClean || fx.lookup("a3").State != PlatformDupClean || st.Stopped || st.Swept != 3 {
		t.Errorf("a1 %+v a3 %+v stats %+v, want both swept around the panic and the panic counted as a read", fx.lookup("a1"), fx.lookup("a3"), st)
	}
	if e := fx.lookup("a2"); e.State != PlatformDupUndetermined || !slices.Contains(e.Reasons, PlatformDupReasonError) {
		t.Errorf("the panicking artist reads %+v, want Undetermined/sweep_error", e)
	}
	if n := strings.Count(fx.logs.String(), "panic on one artist"); n != 1 || !strings.Contains(fx.logs.String(), "artist_id=a2") {
		t.Errorf("%d panic log lines, want exactly one naming a2", n)
	}
	before := fx.fetches("p2")
	if fx.run(t); fx.fetches("p2") != before {
		t.Error("the panicking artist was retried on the very next pass")
	}
}
