package publish

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// #3022. reassertSettleDelay's doc comment makes a load-bearing COST claim: the
// settle wait is paid ONCE PER PUSH, not once per fanart slot, "because
// repairAfterPush wraps the WHOLE restore loop". The shipped code is correct and
// nothing held it there -- moving repairAfterPush inside the snapshot loop, so
// each captured slot gets its own two passes and its own sleep, left the entire
// package green.
//
// WHY THAT MATTERS MORE THAN A MISSING TEST USUALLY DOES. The refactor that
// breaks it looks reasonable from the inside: hoist a nil check, wrap each slot
// rather than the loop. A 42-backdrop artist then goes from 250ms of added
// latency to 10.5 SECONDS, synchronously inside an HTTP handler, inside a 30s
// context. Every test passes. The only thing standing between that and
// production is a comment, and a comment is not a guard.

// settleCostUploader is a peer that does nothing but count. The repair has no
// damage to find, which is the point: this test measures the WAIT, and any
// restore work would add noise to the only quantity under test.
type settleCostUploader struct{ calls int }

func (u *settleCostUploader) UploadImage(_ context.Context, _, _ string, _ []byte, _ string) error {
	u.calls++
	return nil
}

func (u *settleCostUploader) UploadImageAtIndex(_ context.Context, _, _ string, _ int, _ []byte, _ string) error {
	u.calls++
	return nil
}

// TestSettleWait_IsPaidOncePerPush_NotPerFanartSlot pins the cost claim.
//
// It asserts an UPPER bound on total elapsed time across a multi-slot push. A
// lower bound is already covered elsewhere (TestRepairAfterPush_... and
// _LateDeleteIsCoveredOnlyByTheDelay both fail if the delay shrinks), so the
// only direction left unguarded was per-slot multiplication.
func TestSettleWait_IsPaidOncePerPush_NotPerFanartSlot(t *testing.T) {
	// No t.Parallel: this swaps the package-level uploader factories, and it
	// measures wall-clock, which a parallel neighbor would perturb.
	dir := t.TempDir()

	// Enough slots that per-slot multiplication is unmistakable, and chosen to
	// buy CEILING HEADROOM rather than to be minimal. At 10 slots the broken
	// shape costs 10 x 250ms = 2.5s, which lets the ceiling sit at 1s -- roughly
	// double the observed correct time -- instead of hugging it. More slots cost
	// this test nothing (they are tiny files and the wait does not scale with
	// them, which is the very property under test) and they widen the band the
	// assertion has to distinguish.
	const slots = 10
	seedFanartSlots(t, dir, slots)

	up := &settleCostUploader{}
	p, a := settleCostHarness(t, up, dir)

	start := time.Now()
	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)
	elapsed := time.Since(start)

	// PRECONDITIONS. Without these the timing assertion below passes vacuously:
	// a push that uploaded nothing skips the repair entirely (it is gated on
	// uploadedTo), so it would trivially be fast while proving nothing.
	if up.calls == 0 {
		t.Fatal("precondition failed: no backdrop reached the peer, so the repair was never registered " +
			"and no settle wait was ever owed -- this test would pass without measuring anything")
	}
	if up.calls < slots {
		t.Fatalf("precondition failed: only %d of %d backdrops reached the peer; the fixture is not "+
			"exercising a multi-slot push, which is the whole subject of this test", up.calls, slots)
	}
	if len(warnings) != 0 {
		t.Fatalf("precondition failed: the push warned %v; a degraded push may not have walked every "+
			"slot, so the elapsed time below would not measure what it claims", warnings)
	}

	// THE ASSERTION, and the ceiling is the load-bearing number.
	//
	// It must sit between ONE settle delay and TWO, because that is the only
	// band that separates "paid once" from "paid per slot". A first draft used
	// 2s and the per-slot mutation SURVIVED at 1.79s -- 6 x 250ms plus overhead
	// still fit underneath, so the test measured nothing. Anything at or above
	// 2 x reassertSettleDelay is not a guard, it is decoration.
	//
	// 1s against a measured ~0.52s correct run (one fanart settle plus the
	// single-image path's own) is roughly 2x headroom for a loaded runner, while
	// the broken shape costs 2.5s at this slot count and fails decisively. A
	// wall-clock assertion with a thin margin is a flake generator; that is the
	// same defect class this stack already fixed once, in the FIFO timeouts.
	const ceiling = time.Second
	if elapsed >= ceiling {
		t.Errorf("a %d-slot fanart push took %v, at or over the %v ceiling. The settle wait is meant to "+
			"be paid ONCE per push because repairAfterPush wraps the whole restore loop; this elapsed "+
			"time is consistent with paying it PER SLOT (%d x %v = %v), which is what a refactor that "+
			"moves repairAfterPush inside the loop produces. See reassertSettleDelay's doc comment.",
			slots, elapsed, ceiling, slots, reassertSettleDelay,
			time.Duration(slots)*reassertSettleDelay)
	}

	// And the wait genuinely happened: a push that skipped it entirely would
	// also be under the ceiling, so the ceiling alone cannot tell "once" from
	// "never". This is what keeps the test from passing on a build that deleted
	// the settle wait outright.
	if elapsed < reassertSettleDelay {
		t.Errorf("the push took %v, less than one settle delay (%v); the wait was not paid at all, so "+
			"the late-delete window this PR exists to cover is open again",
			elapsed, reassertSettleDelay)
	}
}

// seedFanartSlots writes n readable backdrops using the names the fanart sync
// actually reads: fanart.jpg, then fanart1.jpg upward.
func seedFanartSlots(t *testing.T, dir string, n int) {
	t.Helper()
	for i := range n {
		name := "fanart.jpg"
		if i > 0 {
			name = "fanart" + itoa(i) + ".jpg"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("BACKDROP-"+itoa(i)), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func settleCostHarness(t *testing.T, up *settleCostUploader, dir string) (*Publisher, *artist.Artist) {
	t.Helper()

	conn := &connection.Connection{
		ID: "c1", Name: "Peer", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL: "http://peer.invalid",
	}
	conn.FeatureManageServerFiles = true

	origSingle := newImageUploader
	origIndexed := newIndexedImageUploader
	newImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.ImageUploader { return up }
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() {
		newImageUploader = origSingle
		newIndexedImageUploader = origIndexed
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: conn.ID, PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{conn.ID: conn}},
		Logger:            silentLogger(),
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}
}
