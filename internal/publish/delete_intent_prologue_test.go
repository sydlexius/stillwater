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
	writeFile(t, filepath.Join(dir, "fanart.jpg"), []byte("OPERATOR-BACKDROP-0"))
	writeFile(t, victim, []byte("OPERATOR-BACKDROP-1"))
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
			"(stat err = %v); the fanart snapAt is stamped after the directory walk, so a delete landing "+
			"in the prologue is discarded as old and unrelated", err)
	}
}
