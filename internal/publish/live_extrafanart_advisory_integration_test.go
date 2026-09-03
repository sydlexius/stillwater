//go:build integration

package publish

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
)

// #3177 SPLIT C -- the live-hardware arm.
//
// Every other #3177 test is fake-backed: the uploader is a stub and no real
// peer ever sees the push. This one drives syncAllFanartToPlatforms against a
// REAL Emby through the real client, with a real extrafanart/ directory on
// disk, three consecutive times.
//
// ================== WHAT THIS TEST PROVES, AND WHAT IT DOES NOT ==================
//
// This section is deliberate and load-bearing. An earlier live test on this
// issue reported a "byte-identical inventory across 3 runs" result that it
// could not actually observe, because the artist directory it inventoried was
// a t.TempDir() on the test machine that the Emby server has no filesystem
// path to. This test uses the same t.TempDir() shape -- there is no way for a
// Go test to place a fixture inside the server's own library without knowing
// and writing to that library -- so it makes the identical observation and is
// explicit that the observation is narrower than the issue's headline.
//
// IT PROVES, against real hardware and real network latency:
//
//   1. The advisory fires EXACTLY ONCE for a real push to a real peer, and
//      names the real file count. Every fake-backed proof of this uses an
//      uploader that returns instantly; here the upload loop takes real
//      round-trip time and the advisory still fires once.
//
//   2. The ordering holds under real latency: the advisory enumeration is
//      entered only AFTER the last upload has returned AND after
//      repairAfterPush's settle window. The unit test asserts this against a
//      microsecond-fast stub; a real peer's variable latency is the condition
//      under which an ordering assumption is most likely to be wrong.
//
//   3. Stillwater itself does not touch the artist's extrafanart/ files across
//      three consecutive pushes. The inventory (name, size, content hash) is
//      taken before and after each run and compared. Since the repair path
//      (repairAfterPush -> reassertLocalImage) DOES write to this directory --
//      it is the one component that writes to an artist directory during a
//      push -- this is a real measurement of a real writer, not a tautology.
//
//   4. The top-level fanart that WAS pushed survives all three runs
//      byte-identically, which is the #3174 control: it is the file the
//      snapshot protects, and it must come back unchanged.
//
// IT DOES NOT PROVE, and no assertion here should be read as claiming:
//
//   - Anything about whether EMBY destroys extrafanart/ files. That is #3174's
//     measurement and it requires the artist directory to be inside the
//     server's own library, which this fixture is not. Emby never sees this
//     directory; it receives image BYTES over HTTP and has no path to write
//     back to. A green run here says nothing about the destruction #3177
//     exists to warn about.
//
//   - That extrafanart/ is safe. #3177's entire premise is that it is not.
//     This test measures STILLWATER's local behavior; the loss happens on the
//     server's side of the library and is invisible from here.
//
// Requires SW_LIVE_EMBY_URL / _API_KEY / _USER_ID / _ITEM_ID (loadLiveEmbyEnv,
// live_backdrop_replace_integration_test.go); skips cleanly when any is unset.

// liveExtrafanartRuns is how many consecutive pushes the inventory is compared
// across. THREE, not two, per the #3177 verification standard: two runs cannot
// distinguish "does it once" from "does it every time", and #3174's own
// measurement needed three to show the 2 -> 1 -> 0 progression.
const liveExtrafanartRuns = 3

// liveExtrafanartTimeout bounds the WHOLE test: three sequential pushes, each
// one upload round trip plus repairAfterPush's 250ms settle, plus the clear and
// the per-run inventories. Generous rather than tight -- a genuinely dead peer
// still fails on the first call, long before this.
const liveExtrafanartTimeout = 120 * time.Second

// dirInventory is one artist-directory listing: file name -> its bytes. Bytes
// rather than a size or an mtime, because "the file is still there and is the
// right length" is exactly the near-miss #3174 recorded -- a different file
// having been destroyed and replaced would satisfy a size check.
type dirInventory map[string][]byte

// inventoryDir reads every regular file directly inside dir (not recursing)
// and returns its contents keyed by name.
func inventoryDir(t *testing.T, dir string) dirInventory {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("inventorying %s: %v", dir, err)
	}
	inv := dirInventory{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("inventorying %s/%s: %v", dir, e.Name(), err)
		}
		inv[e.Name()] = b
	}
	return inv
}

// diffInventory returns a human-readable description of every difference
// between two inventories, NAMING what went missing, what appeared, and what
// changed -- per the #3177 standard that a test in this area must name what
// went missing rather than assert only that the pushed image survived.
func diffInventory(before, after dirInventory) []string {
	var problems []string
	var names []string
	for n := range before {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		got, ok := after[n]
		switch {
		case !ok:
			problems = append(problems, "DESTROYED: "+n+" was present before the push and is gone after it")
		case string(got) != string(before[n]):
			problems = append(problems, "MODIFIED: "+n+" changed content across the push")
		}
	}
	names = names[:0]
	for n := range after {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := before[n]; !ok {
			problems = append(problems, "APPEARED: "+n+" was created during the push")
		}
	}
	return problems
}

// TestLiveEmby_ExtrafanartAdvisory_RealPushThreeRuns is the live arm described
// at the top of this file. Read that comment before reading an assertion here;
// it states precisely what a green run means and what it does not.
func TestLiveEmby_ExtrafanartAdvisory_RealPushThreeRuns(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveExtrafanartTimeout)
	defer cancel()

	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)

	// Leave the item as we found it (empty), pass or fail, exactly as the
	// sibling live tests in this package do.
	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
	})

	dir := t.TempDir()

	// A genuine decodable raster for the pushed primary, so the real server
	// accepts and processes it as it would an operator's artwork.
	primary := bandJPEG(t, 0x5177)
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), primary, 0o600); err != nil {
		t.Fatalf("seeding the top-level primary fanart: %v", err)
	}

	// The extrafanart/ fixture: three distinct real JPEGs, so a per-file
	// assertion identifies WHICH file rather than merely "something in there".
	sub := filepath.Join(dir, "extrafanart")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("creating extrafanart/: %v", err)
	}
	for i, name := range []string{"extra1.jpg", "extra2.jpg", "extra3.jpg"} {
		if err := os.WriteFile(filepath.Join(sub, name), bandJPEG(t, 0xE000+i), 0o600); err != nil {
			t.Fatalf("seeding extrafanart/%s: %v", name, err)
		}
	}

	// PRECONDITION, asserted before anything is pushed: the fixture really
	// holds the three files whose fate is being measured.
	subBefore := inventoryDir(t, sub)
	if len(subBefore) != 3 {
		t.Fatalf("precondition failed: extrafanart/ holds %d file(s), want 3", len(subBefore))
	}
	topBefore := inventoryDir(t, dir)
	if _, ok := topBefore["fanart.jpg"]; !ok {
		t.Fatal("precondition failed: the top-level fanart.jpg fixture is missing")
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-3177", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {
				ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby,
				URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true},
			},
		}},
	})
	art := &artist.Artist{ID: "live-3177", Name: "Live UAT Artist", Path: dir}

	for run := 1; run <= liveExtrafanartRuns; run++ {
		// Fresh timing instrument per run, so run N's ordering is measured on
		// run N's own latency rather than on the first run's.
		enum := recordEnumeration(t)

		pushStart := time.Now()
		warnings := p.SyncAllFanartToPlatforms(ctx, art)
		pushEnd := time.Now()

		enumAt, calls := enum()

		// (1) EXACTLY ONE advisory, naming the real count, on a real push.
		if got := countExtrafanartWarnings(warnings); got != 1 {
			t.Fatalf("run %d: got %d extrafanart warning(s) from a real Emby push, want exactly 1: %v",
				run, got, warnings)
		}
		msg := findExtrafanartWarning(warnings)
		if !strings.Contains(msg, "holds 3 file(s)") {
			t.Errorf("run %d: the advisory %q does not name the 3-file count the fixture holds", run, msg)
		}
		if calls != 1 {
			t.Fatalf("run %d: the advisory enumeration was entered %d time(s), want exactly 1", run, calls)
		}

		// (2) THE ORDERING, under real latency. The advisory enumeration's
		// defer is registered BEFORE repairAfterPush's defer, so LIFO runs it
		// LAST -- only after repairAfterPush has run its two passes with the
		// settle window between them -- see the "DEFERS RUN LIFO" comment at
		// the enumeration's registration site. What that
		// buys the enumeration is a bound on how close it lands to the
		// function's RETURN, not to the push's START: real upload round-trip
		// latency inflates the time since start by an amount unrelated to
		// whether the repair ran, so a bound anchored there would still be
		// satisfied by a defer-swap mutation whenever an upload alone took
		// longer than reassertSettleDelay -- exactly the condition a real
		// peer over a network produces. Anchored to the return instead, the
		// enumeration must land within reassertSettleDelay of pushEnd
		// regardless of how long the uploads took, which is what actually
		// catches the mutation. elapsed (logged, not asserted) is the whole
		// run's wall time, for diagnosing an implausible read.
		elapsed := pushEnd.Sub(pushStart)
		remaining := pushEnd.Sub(enumAt)
		if remaining < 0 {
			t.Fatalf("run %d: implausible timing -- the advisory enumeration was entered at %v, "+
				"AFTER the push had already returned at %v (push took %v)", run, enumAt, pushEnd, elapsed)
		}
		if remaining >= reassertSettleDelay {
			t.Errorf("run %d: the advisory enumeration was entered %v before the push returned "+
				"(push took %v total), at or beyond repairAfterPush's own %v settle window -- so it "+
				"may have run BEFORE the deferred repair completed rather than after it. Against a "+
				"real peer, with real round-trip upload latency, that ordering is what keeps repair "+
				"pass 1's ungated overwrite branch from reverting an operator's crop (#3177 round 2 "+
				"CRITICAL).", run, remaining, elapsed, reassertSettleDelay)
		}

		// (3) STILLWATER DID NOT TOUCH extrafanart/. See the header: this
		// measures Stillwater's own writers (repairAfterPush is the one that
		// writes into an artist directory during a push), NOT Emby's, which
		// has no path to this fixture.
		subAfter := inventoryDir(t, sub)
		if problems := diffInventory(subBefore, subAfter); len(problems) > 0 {
			t.Errorf("run %d: the artist's extrafanart/ changed across a real push:\n  %s",
				run, strings.Join(problems, "\n  "))
		}

		// (4) THE #3174 CONTROL: the top-level file that WAS pushed comes back
		// byte-identical, which is what the snapshot and repair exist to
		// guarantee. The whole top-level directory is inventoried, not just
		// fanart.jpg, so a file destroyed alongside it is named rather than
		// missed.
		topAfter := inventoryDir(t, dir)
		if problems := diffInventory(topBefore, topAfter); len(problems) > 0 {
			t.Errorf("run %d: the artist directory changed across a real push:\n  %s",
				run, strings.Join(problems, "\n  "))
		}

		// The peer really did receive the push: without this the three runs
		// above could all be measuring a no-op.
		state, err := client.GetArtistDetail(ctx, env.itemID)
		if err != nil {
			t.Fatalf("run %d: reading platform state after the push: %v", run, err)
		}
		if state.BackdropCount != 1 {
			t.Errorf("run %d: BackdropCount = %d after pushing one top-level fanart, want 1 -- "+
				"the push must replace in place across repeated runs, not append", run, state.BackdropCount)
		}
	}
}
