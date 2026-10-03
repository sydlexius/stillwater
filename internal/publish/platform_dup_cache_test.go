package publish

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// observedCache installs a fresh cache as the publisher's backdrop-write
// observer, the way the background sweep does when it is constructed.
func observedCache(p *Publisher) *PlatformDupCache {
	c := newPlatformDupCache()
	observer := c.invalidateTarget
	p.backdropWriteObserver.Store(&observer)
	return c
}

// foundEntry is a two-connection finding computed at tolerance 0.90.
func foundEntry(at time.Time) PlatformDupEntry {
	return PlatformDupEntry{
		State: PlatformDupFound, ComputedAt: at, Tolerance: 0.90,
		Findings: []PlatformDupFinding{
			{ConnectionID: "c1", Connection: "emby", Backdrops: 4, Redundant: 2},
			{ConnectionID: "c2", Connection: "jellyfin", Backdrops: 3, Redundant: 1},
		},
	}
}

// The four reads a consumer must tell apart: never swept, current, computed
// at another tolerance, and dropped.
func TestPlatformDupCache_LookupStates(t *testing.T) {
	c := newPlatformDupCache()
	at := time.Now()
	if e := c.Lookup("a1", 0.90); e.State != PlatformDupUnknown || !e.ComputedAt.IsZero() || len(e.Reasons) != 0 {
		t.Fatalf("never-swept artist reads %+v, want the zero (Unknown) entry", e)
	}
	c.begin()
	if !c.store("a1", foundEntry(at), []string{backdropTargetKey("c1", "p1"), backdropTargetKey("c2", "q1")}) {
		t.Fatal("store refused with no write in flight")
	}
	c.begin()
	c.store("a2", PlatformDupEntry{State: PlatformDupClean, ComputedAt: at, Tolerance: 0.90}, []string{backdropTargetKey("c1", "p2")})

	got := c.Lookup("a1", 0.90)
	if got.State != PlatformDupFound || got.Redundant() != 3 || len(got.Findings) != 2 || !got.ComputedAt.Equal(at) {
		t.Errorf("a1 reads %+v, want Found with 3 redundant across 2 connections", got)
	}
	// Another tolerance: Unknown, never the old finding, but still stamped so
	// the reader can see it is stale and not missing.
	stale := c.Lookup("a1", 0.95)
	if stale.State != PlatformDupUnknown || len(stale.Findings) != 0 ||
		!slices.Contains(stale.Reasons, PlatformDupReasonStaleTolerance) || stale.Tolerance != 0.90 || !stale.ComputedAt.Equal(at) {
		t.Errorf("0.90 entry read at 0.95: %+v, want Unknown/stale_tolerance carrying its own stamp", stale)
	}
	c.Invalidate("a1")
	if e := c.Lookup("a1", 0.90); e.State != PlatformDupUnknown || c.Len() != 1 {
		t.Errorf("after Invalidate: entry %+v, %d entries, want Unknown and only a2 left", e, c.Len())
	}
	if e := c.Lookup("a2", 0.90); e.State != PlatformDupClean {
		t.Errorf("a2 was dropped with a1: %+v", e)
	}
	c.Clear()
	if e := c.Lookup("a2", 0.90); e.State != PlatformDupUnknown || c.Len() != 0 {
		t.Errorf("after Clear: entry %+v, %d entries, want nothing", e, c.Len())
	}
}

// A write to ANY of an artist's targets drops the whole entry, a write to
// another artist's target does not, and re-storing replaces the target index.
func TestPlatformDupCache_TargetWriteDropsTheOwningArtist(t *testing.T) {
	c := newPlatformDupCache()
	at := time.Now()
	c.begin()
	c.store("a1", foundEntry(at), []string{backdropTargetKey("c1", "p1"), backdropTargetKey("c2", "q1")})
	c.begin()
	c.store("a2", foundEntry(at), []string{backdropTargetKey("c1", "p2")})

	c.invalidateTarget("c9", "unknown-target") // nobody's target: nothing happens
	if c.Len() != 2 {
		t.Fatalf("a write to an untracked target dropped an entry: %d left", c.Len())
	}
	c.invalidateTarget("c2", "q1")
	if c.Lookup("a1", 0.90).State != PlatformDupUnknown || c.Lookup("a2", 0.90).State != PlatformDupFound {
		t.Errorf("after a write to a1's second target: a1 %+v a2 %+v, want only a1 dropped", c.Lookup("a1", 0.90), c.Lookup("a2", 0.90))
	}
	// a1's other target no longer points at it: re-storing under a new target
	// set must not leave the old key able to drop the new entry.
	c.begin()
	c.store("a1", foundEntry(at), []string{backdropTargetKey("c1", "p1-relinked")})
	c.invalidateTarget("c1", "p1")
	if c.Lookup("a1", 0.90).State != PlatformDupFound {
		t.Error("a stale target key dropped the re-stored entry")
	}
}

// A result whose target was written between begin and store describes the
// platform as it was BEFORE the write, so it is refused; an unrelated write
// in the same window is not a reason to refuse.
func TestPlatformDupCache_StoreRefusesAResultRacedByAWrite(t *testing.T) {
	c := newPlatformDupCache()
	mine := []string{backdropTargetKey("c1", "p1")}

	c.begin()
	c.invalidateTarget("c1", "p1")
	if c.store("a1", foundEntry(time.Now()), mine) || c.Lookup("a1", 0.90).State != PlatformDupUnknown {
		t.Fatalf("a raced result was stored: %+v", c.Lookup("a1", 0.90))
	}
	// The window closed with that store: the same write does not poison the next read.
	c.begin()
	c.invalidateTarget("c1", "someone-else")
	if !c.store("a1", foundEntry(time.Now()), mine) || c.Lookup("a1", 0.90).State != PlatformDupFound {
		t.Errorf("an unrelated write refused the result: %+v", c.Lookup("a1", 0.90))
	}
}

// fresh is the sweep's "skip this artist" test: decided entries last ttl,
// undecided ones retry sooner, and another tolerance is never fresh.
func TestPlatformDupCache_Fresh(t *testing.T) {
	c := newPlatformDupCache()
	at := time.Now()
	ttl, retry := 7*24*time.Hour, 6*time.Hour
	c.begin()
	c.store("found", foundEntry(at), nil)
	c.begin()
	c.store("undecided", PlatformDupEntry{State: PlatformDupUndetermined, ComputedAt: at, Tolerance: 0.90, Reasons: []string{PlatformDupReasonReadFailed}}, nil)

	for _, tc := range []struct {
		id    string
		tol   float64
		after time.Duration
		want  bool
	}{
		{"missing", 0.90, 0, false},
		{"found", 0.90, time.Hour, true},
		{"found", 0.90, 12 * time.Hour, true}, // past retry, inside ttl
		{"found", 0.90, ttl + time.Minute, false},
		{"found", 0.95, time.Hour, false},
		{"undecided", 0.90, time.Hour, true},
		{"undecided", 0.90, 12 * time.Hour, false}, // past retry
	} {
		if got := c.fresh(tc.id, tc.tol, at.Add(tc.after), ttl, retry); got != tc.want {
			t.Errorf("fresh(%s, %.2f, +%s) = %v, want %v", tc.id, tc.tol, tc.after, got, tc.want)
		}
	}
}

// The seam: a write-intent lock announces its target to the observer, the
// quiet (read) lock does not, and with no observer installed the lock is
// exactly what it was before.
func TestLockPhashTarget_AnnouncesWritesOnly(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	p.lockPhashTarget("c1", "p1")() // no observer installed: must not panic

	var mu sync.Mutex
	var seen []string
	observer := func(conn, item string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, conn+"/"+item)
	}
	p.backdropWriteObserver.Store(&observer)

	p.lockPhashTargetQuiet("c1", "p1")()
	if len(seen) != 0 {
		t.Fatalf("the quiet lock announced a write: %v", seen)
	}
	p.lockPhashTarget("c1", "p1")()
	p.LockBackdropTarget("c2", "p2")()
	if !slices.Equal(seen, []string{"c1/p1", "c2/p2"}) {
		t.Errorf("announced %v, want one announcement per write-intent lock, in order", seen)
	}
	// Both variants are the SAME mutex: a quiet holder blocks a writer.
	unlock := p.lockPhashTargetQuiet("c3", "p3")
	acquired := make(chan struct{})
	go func() { p.lockPhashTarget("c3", "p3")(); close(acquired) }()
	select {
	case <-acquired:
		t.Fatal("a writer took the target lock while the quiet lock was held")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never acquired the lock after the quiet holder released it")
	}
}

// Through the real prune entry point: a dry run leaves a cached entry alone
// (and deletes nothing), a live run drops it.
func TestPruneForArtist_OnlyALiveRunInvalidatesTheCache(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	c := observedCache(p)
	a := &fixtureArtist(t, p).artists[0]
	c.begin()
	c.store("a1", foundEntry(time.Now()), []string{backdropTargetKey("c-emby", "p1")})

	opts := ArtistBackdropPruneOptions{Perceptual: true, DryRun: true, Tolerance: img.DefaultDuplicateTolerance}
	res, err := pruneFor(p, a, opts)
	if err != nil || len(res.Plan) != 1 {
		t.Fatalf("dry run: plan %+v err %v, want one planned delete", res.Plan, err)
	}
	assertPlatform(t, fake, f.small, f.big)
	if c.Lookup("a1", 0.90).State != PlatformDupFound {
		t.Fatal("a dry run invalidated the cache entry")
	}
	opts.DryRun = false
	if res, err = pruneFor(p, a, opts); err != nil || res.BackdropsRemoved != 1 {
		t.Fatalf("live run: removed %d err %v, want 1", res.BackdropsRemoved, err)
	}
	if e := c.Lookup("a1", 0.90); e.State != PlatformDupUnknown {
		t.Errorf("after a live prune the entry reads %+v, want Unknown", e)
	}
}

// Examined and Unhealthy are what let a caller tell "nothing redundant" from
// "nothing was read": each case below has an EMPTY plan for a different reason.
func TestPruneForArtist_ReportsWhatWasRead(t *testing.T) {
	f := newPerceptualFixture(t)
	want := PlatformBackdropPruneTarget{ConnectionID: "c-emby", Connection: "emby", PlatformArtistID: "p1", Backdrops: 2}
	for name, tc := range map[string]struct {
		setup         func(p *Publisher, fake *fakeBackdropClient)
		examined      []PlatformBackdropPruneTarget
		unhealthy     []string
		wantFailures  int
		wantPlatformN int
	}{
		"read and clean": {func(*Publisher, *fakeBackdropClient) {}, []PlatformBackdropPruneTarget{want}, nil, 0, 2},
		"unhealthy connection": {func(p *Publisher, _ *fakeBackdropClient) {
			p.connectionService.(*fakeConnectionGetter).conns["c-emby"].Status = "error"
		}, nil, []string{"c-emby"}, 0, 2},
		"disabled connection": {func(p *Publisher, _ *fakeBackdropClient) {
			p.connectionService.(*fakeConnectionGetter).conns["c-emby"].Enabled = false
		}, nil, nil, 0, 2},
		"fetch failed": {func(_ *Publisher, fake *fakeBackdropClient) { fake.failAt = 1 }, nil, nil, 1, 2},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.other}, failAt: -1, failDeleteAt: -1}
			p := perceptualPublisher(t, fake)
			tc.setup(p, fake)
			res, err := pruneFor(p, &fixtureArtist(t, p).artists[0],
				ArtistBackdropPruneOptions{Perceptual: true, DryRun: true, Tolerance: img.DefaultDuplicateTolerance})
			if err != nil || len(res.Plan) != 0 {
				t.Fatalf("precondition: plan %+v err %v, want an empty plan", res.Plan, err)
			}
			if !slices.Equal(res.Examined, tc.examined) || !slices.Equal(res.Unhealthy, tc.unhealthy) || len(res.Failures) != tc.wantFailures {
				t.Errorf("examined %+v unhealthy %v failures %v, want %+v / %v / %d failure(s)",
					res.Examined, res.Unhealthy, res.Failures, tc.examined, tc.unhealthy, tc.wantFailures)
			}
			if len(fake.backdrops) != tc.wantPlatformN {
				t.Errorf("platform holds %d backdrops, want %d", len(fake.backdrops), tc.wantPlatformN)
			}
		})
	}
	// A target WITH redundant copies is examined too, with its full count.
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big, f.other}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	res, err := pruneFor(p, &artist.Artist{ID: "a1", Path: fixtureArtist(t, p).artists[0].Path},
		ArtistBackdropPruneOptions{Perceptual: true, DryRun: true, Tolerance: img.DefaultDuplicateTolerance})
	if err != nil || len(res.Plan) != 1 || len(res.Examined) != 1 || res.Examined[0].Backdrops != 3 {
		t.Errorf("with a duplicate: plan %+v examined %+v err %v, want 1 planned and 1 examined holding 3", res.Plan, res.Examined, err)
	}
}

// F1: a target has ONE owner. The mapping moves from a1 to a2; dropping a1
// afterwards must not take a2's index key with it, or a later write to the
// target would leave a2 reading Found.
func TestPlatformDupCache_MovedTargetStaysInvalidatable(t *testing.T) {
	c := newPlatformDupCache()
	k := []string{backdropTargetKey("c1", "p1")}
	c.begin()
	c.store("a1", foundEntry(time.Now()), k)
	c.begin()
	c.store("a2", foundEntry(time.Now()), k)
	if c.Lookup("a1", 0.90).State != PlatformDupUnknown || c.Lookup("a2", 0.90).State != PlatformDupFound {
		t.Errorf("after the target moved: a1 %+v a2 %+v, want a1 dropped and a2 Found", c.Lookup("a1", 0.90), c.Lookup("a2", 0.90))
	}
	c.Invalidate("a1") // a re-sweep or invalidation of the previous owner
	c.invalidateTarget("c1", "p1")
	if e := c.Lookup("a2", 0.90); e.State != PlatformDupUnknown {
		t.Errorf("a write to a2's target left it reading %+v, want Unknown", e)
	}
}

// F5/F8: store needs an open window. It closes the window itself, and Clear
// closes it too (and forgets the target index), so a read that began before
// either cannot land afterwards.
func TestPlatformDupCache_StoreNeedsAnOpenWindow(t *testing.T) {
	c := newPlatformDupCache()
	k := []string{backdropTargetKey("c1", "p1")}
	if c.store("a1", foundEntry(time.Now()), k) || c.Len() != 0 {
		t.Fatal("store with no begin was accepted")
	}
	c.begin()
	if !c.store("a1", foundEntry(time.Now()), k) {
		t.Fatal("precondition: store inside a window was refused")
	}
	if c.store("a2", foundEntry(time.Now()), nil) {
		t.Error("the window stayed open after a store: a second store was accepted")
	}
	c.begin()
	c.Clear()
	if c.store("a1", foundEntry(time.Now()), k) || c.Len() != 0 || len(c.byTarget) != 0 {
		t.Errorf("after Clear: store accepted or state left behind (%d entries, %d index keys)", c.Len(), len(c.byTarget))
	}
}

// targetLockHeld reports whether the per-target lock is held right now.
func targetLockHeld(p *Publisher, connectionID, platformArtistID string) bool {
	m, _ := p.phashTargetLocks.LoadOrStore(backdropTargetKey(connectionID, platformArtistID), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

// F4: the write is announced only once the lock is HELD. That ordering is what
// makes the raced-store guard sound: a reader holding the lock cannot have a
// write announced (and then forgotten) underneath it.
func TestLockPhashTarget_AnnouncesAfterAcquiring(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	held := make(chan bool, 1)
	observer := func(conn, item string) { held <- targetLockHeld(p, conn, item) }
	p.backdropWriteObserver.Store(&observer)
	unlock := p.lockPhashTarget("c1", "p1")
	defer unlock()
	if !<-held {
		t.Error("the write was announced before the target lock was held")
	}
}

// lockProbeClient records whether the target lock was held at the first read.
type lockProbeClient struct {
	backdropPruneClient
	p    *Publisher
	held chan bool
}

func (c lockProbeClient) GetArtistDetail(ctx context.Context, id string) (*connection.ArtistPlatformState, error) {
	c.held <- targetLockHeld(c.p, "c-emby", id)
	return c.backdropPruneClient.GetArtistDetail(ctx, id)
}

// F3: a dry run reads under the same per-target lock a live run holds, so it
// serializes with writers even though it announces nothing.
func TestPruneForArtist_DryRunReadsUnderTheTargetLock(t *testing.T) {
	f := newPerceptualFixture(t)
	probe := lockProbeClient{held: make(chan bool, 1),
		backdropPruneClient: &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}}
	p := perceptualPublisher(t, probe)
	probe.p = p
	backdropPruneClientFactory = func(*connection.Connection, *slog.Logger) backdropPruneClient { return probe }
	res, err := pruneFor(p, &fixtureArtist(t, p).artists[0],
		ArtistBackdropPruneOptions{Perceptual: true, DryRun: true, Tolerance: img.DefaultDuplicateTolerance})
	if err != nil || len(res.Plan) != 1 {
		t.Fatalf("precondition: plan %+v err %v, want the dry run to read the platform and plan one delete", res.Plan, err)
	}
	if !<-probe.held {
		t.Error("the dry run read the platform without holding the per-target lock")
	}
}

// CONCURRENT (for -race): several writers and readers against the one sweeper.
func TestPlatformDupCache_ConcurrentUse(t *testing.T) {
	c := newPlatformDupCache()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c.invalidateTarget("c1", "p1")
				_ = c.Lookup("a1", 0.90)
				_ = c.fresh("a1", 0.90, time.Now(), time.Hour, time.Minute)
			}
		}()
	}
	for i := 0; i < 200; i++ { // the single sweeper
		c.begin()
		c.store("a1", foundEntry(time.Now()), []string{backdropTargetKey("c1", "p1")})
	}
	wg.Wait()
	c.invalidateTarget("c1", "p1")
	if e := c.Lookup("a1", 0.90); e.State != PlatformDupUnknown {
		t.Errorf("after a final write the entry reads %+v, want Unknown", e)
	}
}
