package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// #2712 follow-up: the delete gate must cover THE WHOLE PUSH, prologue included.
//
// snapAt is the instant a push compares an operator delete marker against, and
// the gate only stands down for a marker at or after it. So every step that runs
// BEFORE snapAt is stamped is a window in which the operator's delete is read as
// "old and unrelated" and the artwork is put straight back -- #2712 unfixed for
// that part of the push.
//
// The original fix stamped snapAt after the prologue: GetPlatformIDs, ImageDir,
// the naming-config database read, the FindExistingImage stat loop, and on the
// single-image path the entire artwork byte read. On a network mount that
// prologue is the slow part of the push, so the untested window was most of it.
// Six existing tests missed this because every one of them passes snapAt in
// directly or deletes after the push has already started uploading.
//
// THE FIXTURE. A platform lister whose GetPlatformIDs marks delete intent puts
// the operator's action inside the push's own FIRST step, so the push is
// unambiguously already in flight and no ordering argument can call the delete
// "earlier and unrelated". The file itself is removed later, by the clobbering
// uploader, because it has to survive FindExistingImage and the byte read for
// the push to reach its verify at all -- which is exactly the shape a real
// operator delete presents to the repair: intent recorded during the push, and
// ENOENT by the time the repair looks.
//
// These tests fail against the pre-fix ordering (snapAt stamped after the read):
// the marker then predates snapAt, the gate declines to fire, and the repair
// resurrects artwork the operator deleted.

// markingPlatformLister records an operator delete from inside the push's first
// step. Everything except GetPlatformIDs delegates to the embedded fake.
type markingPlatformLister struct {
	*fakePlatformLister
	dir       string
	imageType string
	// markedAt is the instant the marker was written, and called proves the hook
	// ran at all. A test that did not assert the hook fired would pass vacuously
	// if the prologue ever stopped calling GetPlatformIDs.
	markedAt time.Time
	called   bool
}

func (f *markingPlatformLister) GetPlatformIDs(ctx context.Context, artistID string) ([]artist.PlatformID, error) {
	f.called = true
	f.markedAt = time.Now()
	img.MarkDeleteIntent(f.dir, f.imageType)
	return f.fakePlatformLister.GetPlatformIDs(ctx, artistID)
}

// installPrologueMarker swaps the publisher's lister for one that marks delete
// intent during the prologue, and asserts the directory carries no marker of any
// age beforehand -- without that precondition a leaked marker from another test
// would make the stand-down correct for the wrong reason.
func installPrologueMarker(t *testing.T, p *Publisher, dir, imageType string) *markingPlatformLister {
	t.Helper()
	if img.DeleteIntentAfter(dir, imageType, time.Time{}) {
		t.Fatalf("precondition failed: %s already carries a %s delete marker, so a stand-down would be "+
			"correct regardless of when this test writes one", dir, imageType)
	}
	inner, ok := p.artistService.(*fakePlatformLister)
	if !ok {
		t.Fatalf("precondition failed: the harness lister is %T, not *fakePlatformLister, so the "+
			"prologue hook is not wired and this test proves nothing", p.artistService)
	}
	marker := &markingPlatformLister{fakePlatformLister: inner, dir: dir, imageType: imageType}
	p.artistService = marker
	return marker
}

// TestSyncImage_OperatorDeletesDuringPrologue_NotRestored is the single-image
// half. The operator's delete lands during GetPlatformIDs -- the push's first
// step -- and the artwork must stay deleted.
func TestSyncImage_OperatorDeletesDuringPrologue_NotRestored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "banner.jpg", "delete", &calls)

	victim := filepath.Join(dir, "banner.jpg")
	writeFile(t, victim, []byte("OPERATOR-BANNER-BYTES"))
	marker := installPrologueMarker(t, p, dir, "banner")

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	if !marker.called {
		t.Fatal("precondition failed: the prologue never asked for platform IDs, so no delete was ever " +
			"recorded during the push and this test asserts nothing")
	}
	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so the file was never destroyed and the " +
			"repair was never owed")
	}
	if !img.DeleteIntentAfter(dir, "banner", time.Time{}) {
		t.Fatal("precondition failed: no delete marker is on record after the push")
	}
	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repair resurrected a banner the operator deleted during the push prologue "+
			"(stat err = %v); snapAt is stamped too late, so a delete landing before the byte read is "+
			"discarded as old and unrelated", err)
	}
}

// TestSyncAllFanart_OperatorDeletesDuringPrologue_NotRestored is the fanart half.
// The fanart prologue is longer than the single-image one (a directory walk and
// an identity index on top of the platform lookup), so the untested window was
// wider here, not narrower.
func TestSyncAllFanart_OperatorDeletesDuringPrologue_NotRestored(t *testing.T) {
	calls := 0
	// The victim is the SECOND backdrop, destroyed while slot 0 uploads, so the
	// repair reaches its ENOENT branch for a file the loop never rewrote.
	p, a, dir := clobberHarness(t, "fanart1.jpg", "delete", &calls)

	victim := filepath.Join(dir, "fanart1.jpg")
	// The SURVIVOR. This push snapshotted it too and passes it through the same
	// reassertLocalImage call, so asserting only the victim is vacuous in two
	// directions: a gate that stood down for the WRONG file, and a repair that
	// rewrote a healthy backdrop with its own stale snapshot bytes, both leave
	// the victim deleted and pass.
	survivor := filepath.Join(dir, "fanart.jpg")
	survivorBytes := []byte("OPERATOR-BACKDROP-0")
	writeFile(t, survivor, survivorBytes)
	writeFile(t, victim, []byte("OPERATOR-BACKDROP-1"))

	// Identity, not just content. The survivor's snapshot bytes EQUAL what is on
	// disk, so a spurious repair would rewrite it with the very same bytes and a
	// content-only assertion could never see it. WriteFileAtomic writes a temp
	// file and renames it onto the target, so any repair at all replaces the
	// inode -- os.SameFile is what makes "the repair left this file alone"
	// checkable rather than merely plausible.
	survivorBefore, statErr := os.Stat(survivor)
	if statErr != nil {
		t.Fatalf("stat'ing the healthy backdrop before the push: %v", statErr)
	}

	marker := installPrologueMarker(t, p, dir, "fanart")

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if !marker.called {
		t.Fatal("precondition failed: the prologue never asked for platform IDs, so no delete was ever " +
			"recorded during the push and this test asserts nothing")
	}
	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so no backdrop was destroyed")
	}
	if !img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Fatal("precondition failed: no delete marker is on record after the push")
	}
	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repair resurrected a backdrop the operator deleted during the push prologue "+
			"(stat err = %v); the fanart push stamp is taken after the directory walk, so a delete "+
			"landing in the prologue is discarded as old and unrelated", err)
	}
	// The survivor is untouched, byte for byte. The uploader never destroyed it,
	// so the correct outcome for it is that reassertLocalImage's first branch
	// (bytes equal, return) fires and nothing is written at all.
	if got := mustRead(t, survivor); string(got) != string(survivorBytes) {
		t.Errorf("the healthy backdrop reads %q, want the operator's untouched %q; the push also passed "+
			"this file through the repair, so either the delete gate stood down for the wrong file or the "+
			"repair rewrote a file no peer had touched", got, survivorBytes)
	}
	survivorAfter, statErr := os.Stat(survivor)
	if statErr != nil {
		t.Fatalf("the healthy backdrop is not stat-able after the push: %v", statErr)
	}
	if !os.SameFile(survivorBefore, survivorAfter) {
		t.Error("the healthy backdrop was REPLACED during the push (different inode), even though its " +
			"bytes still match: the repair rewrote a file no peer had touched, and the matching content " +
			"only hides it because the snapshot bytes happened to equal what was on disk")
	}
}

// TestSyncImage_MarkerOlderThanThePush_StillRepairsAPeerClobber is the
// publisher-side half of #3015's non-self-suppression criterion.
//
// The two prologue tests above prove the gate FIRES for a delete concurrent
// with the push. This proves the complementary property, which is the one a
// rule fixer's own push depends on: a marker written STRICTLY BEFORE the push
// began does NOT suppress that push's repair. A fixer marks, unlinks, and only
// then pushes; if its own marker covered that push, the fixer would silently
// give up the #2698 peer-clobber repair for the artist it just fixed -- for the
// whole five-minute retention window, on every fix.
//
// THE FIXTURE, and why it is not simply "mark, then push". The victim file must
// exist when the push reads it and be destroyed by the PEER during the upload,
// so the repair reaches its ENOENT branch for a genuinely peer-caused absence.
// The marker is written before the push starts, exactly as a fixer's is.
// installPrologueMarker's precondition check is reused so a marker leaked from
// another test cannot make this pass for the wrong reason.
//
// The assertion is the OPPOSITE of the prologue tests': the file must be BACK.
// That direction matters -- a gate broken toward "always suppress" passes every
// prologue test in this file and fails only here.
func TestSyncImage_MarkerOlderThanThePush_StillRepairsAPeerClobber(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "banner.jpg", "delete", &calls)

	victim := filepath.Join(dir, "banner.jpg")
	want := []byte("OPERATOR-BANNER-BYTES")
	writeFile(t, victim, want)

	if img.DeleteIntentAfter(dir, "banner", time.Time{}) {
		t.Fatal("precondition failed: the directory already carries a banner delete marker, so this test " +
			"cannot control the mark-versus-push ordering it exists to measure")
	}

	// The fixer's mark: written before its push, never during it.
	img.MarkDeleteIntent(dir, "banner")
	if !img.DeleteIntentAfter(dir, "banner", time.Time{}) {
		t.Fatal("precondition failed: no marker is on record after MarkDeleteIntent, so a repair here " +
			"would prove nothing about a marker's effect on a later push")
	}
	// Finite clock resolution again: without this the marker and the push's
	// snapAt can land in the same tick, which DeleteIntentAfter deliberately
	// treats as concurrent -- and this test would then be measuring the
	// concurrent case the prologue tests already cover.
	time.Sleep(2 * time.Millisecond)

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so the peer never destroyed the file and " +
			"no repair was owed -- the assertion below would pass on an untouched fixture")
	}

	got, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("the peer deleted the operator's banner during the push and the repair did NOT put it "+
			"back (%v). The only marker on record was written BEFORE the push began, so it must not "+
			"suppress this repair: a rule fixer marks and then pushes, and a fixer that suppresses its "+
			"own repair loses the #2698 protection entirely (#3015)", readErr)
	}
	if string(got) != string(want) {
		t.Errorf("the banner was restored with %q, want the operator's %q", got, want)
	}
}
