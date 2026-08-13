package publish

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// #2712, the LATE-DELETE window. The post-push repair is a point-in-time check:
// it runs once after UploadImage returns and asks "is the file still what I
// uploaded?". That question only has an answer for damage that has ALREADY
// landed.
//
// In every case observed on the primary path the peer's delete completed before
// UploadImage returned, which is an observation about two unsynchronized
// processes rather than a happens-before relation -- and a delete roughly 15ms
// AFTER the return was measured on the fanart path. One pass reports the file
// healthy and the operator's artwork is gone, with nothing in this codebase that
// would put it back later.
//
// THE RENDEZVOUS, and why these tests are deterministic rather than a race
// against a sleep. A test that simply deleted the file "15ms later" would be
// asserting on timing: if the repair pass happened to run late the test would
// still pass, for the wrong reason, and would prove nothing about a second pass
// existing at all.
//
// So the delete is triggered by THE FIRST PASS'S OWN COMPLETED WORK. The fake
// peer OVERWRITES the file during the upload, which the first pass repairs by
// writing the operator's bytes back. The peer's goroutine -- started from inside
// the upload, so the push is unambiguously already in flight -- waits until it
// can SEE those bytes on disk, and only then deletes.
//
// Waiting on the restore's OUTPUT rather than on the log that precedes it
// matters. WriteFileAtomic renames a temp file onto the target, so observing the
// operator's bytes proves the rename has completed and the first pass is
// finished with this file. Latching on the "restoring it" log instead would let
// the delete land BETWEEN the log and the rename, and the restore would then put
// the file back after the delete -- a green test that proved nothing, which is
// what the first version of this fixture actually did.
//
// The probe technique is the one that found the snapAt-ordering defect in PR 1
// (see delete_intent_prologue_test.go): drive the event from inside a dependency
// the push itself calls, so no ordering argument can call it "earlier and
// unrelated".

// lateDeleteUploader is a peer that overwrites the operator's file during the
// upload and then, once the first repair pass has provably restored it, deletes
// it -- the late-delete shape measured on the fanart path.
//
// markIntent turns the same fixture into the OPERATOR's delete rather than the
// peer's, which is what proves the second pass inherits PR 1's delete gate
// instead of working around it.
type lateDeleteUploader struct {
	victim     string
	dir        string
	imageType  string
	restored   []byte // the operator's bytes; seeing these on disk means pass 1 finished
	markIntent bool

	// calls counts uploads. It is written only from the push's own goroutine
	// (UploadImage is synchronous) and read after the push returns.
	calls int

	// done closes when the late-delete attempt has finished, and deleted records
	// whether the file was genuinely absent immediately afterwards. Without that
	// second fact a Remove that silently failed would leave every assertion below
	// checking a delete that never happened. Both are written before done closes
	// and read only after it, which is what orders them for the race detector.
	done    chan struct{}
	deleted bool
	// bytesBeforeDelete is what the file actually held at the instant before the
	// delete, read by the peer's own goroutine. It is the EVIDENCE that the
	// delete followed the first repair pass rather than racing it: the operator's
	// bytes can only be there because that pass put them back. Empty means the
	// rendezvous timed out, which is a fixture failure and is reported as one.
	bytesBeforeDelete []byte

	once *sync.Once
	wg   *sync.WaitGroup
}

func newLateDeleteUploader(victim, dir, imageType string, restored []byte, markIntent bool) *lateDeleteUploader {
	return &lateDeleteUploader{
		victim: victim, dir: dir, imageType: imageType, restored: restored, markIntent: markIntent,
		done: make(chan struct{}), once: &sync.Once{}, wg: &sync.WaitGroup{},
	}
}

func (u *lateDeleteUploader) UploadImage(_ context.Context, _, _ string, _ []byte, _ string) error {
	return u.clobberThenLateDelete()
}

func (u *lateDeleteUploader) UploadImageAtIndex(_ context.Context, _, _ string, _ int, _ []byte, _ string) error {
	return u.clobberThenLateDelete()
}

// clobberThenLateDelete performs the peer's immediate overwrite and arms the
// late delete. It returns as soon as the overwrite is done, exactly as a real
// peer's HTTP call returns before whatever it does next.
func (u *lateDeleteUploader) clobberThenLateDelete() error {
	u.calls++
	// The overwrite happens BEFORE the goroutine starts, so the rendezvous below
	// can never observe the operator's bytes from before the push.
	if err := os.WriteFile(u.victim, []byte("PEER-OWN-BYTES"), 0o600); err != nil {
		return err
	}
	u.once.Do(func() {
		u.wg.Add(1)
		go u.lateDelete()
	})
	return nil
}

// lateDelete waits for the first repair pass to finish restoring the file, then
// removes it -- after UploadImage has already returned.
func (u *lateDeleteUploader) lateDelete() {
	defer u.wg.Done()
	defer close(u.done)

	if !u.waitForRestore() {
		return
	}
	if !u.waitForRestore() {
		return
	}

	// EVIDENCE, not a self-report. What the file held at the instant just before
	// the delete is the fact that makes this delete provably LATE, and the test
	// asserts it. A flag the fixture sets for itself proves nothing: deleting the
	// wait above and leaving `sawRestore = true` would keep every test green
	// while the delete raced the first pass instead of following it.
	if seen, err := os.ReadFile(u.victim); err == nil {
		u.bytesBeforeDelete = append([]byte(nil), seen...)
	}

	if u.markIntent {
		// The operator's intent, recorded by the actor whose intent it is,
		// immediately before touching the filesystem -- the same order the real
		// delete handlers use.
		img.MarkDeleteIntent(u.dir, u.imageType)
	}
	if err := os.Remove(u.victim); err != nil {
		return
	}
	if _, statErr := os.Stat(u.victim); errors.Is(statErr, os.ErrNotExist) {
		u.deleted = true
	}
}

// waitForRestore blocks until the operator's bytes are back on disk, reporting
// false if they never are.
//
// The poll interval is far shorter than reassertSettleDelay, so the delete lands
// well inside the settle window the second pass is waiting out. The timeout is
// generous because exceeding it means the fixture is broken, not slow.
func (u *lateDeleteUploader) waitForRestore() bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(u.victim); err == nil && bytes.Equal(got, u.restored) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// lateDeleteHarness wires a publisher whose single peer performs the late delete.
//
// Serial, like every test that swaps these package-level factories: do not add
// t.Parallel().
func lateDeleteHarness(t *testing.T, up *lateDeleteUploader, dir string) (*Publisher, *artist.Artist) {
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
		// The peer's goroutine can outlive the push on the fixture-timeout path.
		// Join it before the temp dir is torn down.
		up.wg.Wait()
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

// assertLateDeleteHappened is the shared precondition block. Every assertion in
// every test below is vacuous without all four of these facts.
func assertLateDeleteHappened(t *testing.T, up *lateDeleteUploader) {
	t.Helper()
	if up.calls == 0 {
		t.Fatal("precondition failed: the peer was never handed the image, so nothing was overwritten " +
			"and no repair was ever owed")
	}
	select {
	case <-up.done:
	case <-time.After(10 * time.Second):
		t.Fatal("precondition failed: the peer's late-delete goroutine never finished")
	}
	// THE LATENESS FACT, asserted from what the peer actually read off disk
	// rather than from a flag the fixture set for itself. Only the first repair
	// pass can have put the operator's bytes back over the peer's overwrite, so
	// seeing them there immediately before the delete is what makes this delete
	// provably LATE. Without this assertion, removing the rendezvous entirely
	// leaves every test in this file green while the delete merely races pass 1.
	if !bytes.Equal(up.bytesBeforeDelete, up.restored) {
		t.Fatalf("precondition failed: the file held %q immediately before the peer's delete, want the "+
			"operator's %q; the delete did not follow the first repair pass, so it is not provably late "+
			"and nothing below proves a second pass exists",
			up.bytesBeforeDelete, up.restored)
	}
	if !up.deleted {
		t.Fatal("precondition failed: the peer's late delete did not remove the file, so the second pass " +
			"had nothing to find")
	}
}

// TestSyncAllFanart_PeerDeletesAfterUploadReturns_Restored is the fanart case,
// which is where the ~15ms-late delete was actually measured.
//
// It fails with a single repair pass: that pass sees the overwrite, restores it,
// and returns before the peer's delete lands, so the operator's backdrop is gone
// and the push reports success.
func TestSyncAllFanart_PeerDeletesAfterUploadReturns_Restored(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "fanart.jpg")
	want := []byte("OPERATOR-BACKDROP-BYTES")
	writeFile(t, victim, want)

	up := newLateDeleteUploader(victim, dir, "fanart", want, false)
	p, a := lateDeleteHarness(t, up, dir)

	p.SyncAllFanartToPlatforms(context.Background(), a)

	assertLateDeleteHappened(t, up)

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("the backdrop is gone after the push (%v); the peer deleted it AFTER UploadImage "+
			"returned, so a single point-in-time repair pass never sees the damage -- and nothing else "+
			"in this codebase restores a local artwork file from a peer", err)
	}
	if string(got) != string(want) {
		t.Errorf("the backdrop reads %q, want the operator's %q; the file was restored from the wrong bytes",
			got, want)
	}
}

// TestSyncImage_PeerDeletesAfterUploadReturns_Restored is the single-image half.
//
// It is not redundant with the fanart case: the two sync entry points register
// the repair in completely different places (an inline call after the upload
// loop, versus a defer registered before it), so dropping the settle pass from
// one leaves the other's test green.
func TestSyncImage_PeerDeletesAfterUploadReturns_Restored(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "banner.jpg")
	want := []byte("OPERATOR-BANNER-BYTES")
	writeFile(t, victim, want)

	up := newLateDeleteUploader(victim, dir, "banner", want, false)
	p, a := lateDeleteHarness(t, up, dir)

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	assertLateDeleteHappened(t, up)

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("the banner is gone after the push (%v); the peer deleted it AFTER UploadImage returned "+
			"and no second pass looked again", err)
	}
	if string(got) != string(want) {
		t.Errorf("the banner reads %q, want the operator's %q", got, want)
	}
}

// TestSyncAllFanart_OperatorDeletesAfterUploadReturns_NotRestored is the GUARD.
//
// The two tests above add a second repair pass, and a second pass is a second
// chance to resurrect a delete the operator meant. This one WIRES that machinery
// in full -- the same late-delete fixture, the same second pass, with the delete
// recorded as the OPERATOR's -- and asserts the effect is ABSENT.
//
// It is RED against a reverify that skips the gate (one calling WriteFileAtomic
// directly, say, rather than reusing reassertLocalImage), which is the obvious
// wrong way to build this and would reopen #2712 inside its own hardening.
func TestSyncAllFanart_OperatorDeletesAfterUploadReturns_NotRestored(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "fanart.jpg")
	operatorBytes := []byte("OPERATOR-BACKDROP-BYTES")
	writeFile(t, victim, operatorBytes)

	// PRECONDITION: no marker of ANY age on this directory. A marker leaked from
	// another test would make the stand-down correct for the wrong reason.
	if img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Fatalf("precondition failed: %s already carries a fanart delete marker", dir)
	}

	up := newLateDeleteUploader(victim, dir, "fanart", operatorBytes, true)
	p, a := lateDeleteHarness(t, up, dir)

	p.SyncAllFanartToPlatforms(context.Background(), a)

	assertLateDeleteHappened(t, up)
	if !img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Fatal("precondition failed: no delete marker is on record, so the gate had nothing to consult " +
			"and a surviving deletion would prove nothing")
	}

	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the settle-and-reverify pass resurrected a backdrop the operator deleted during the "+
			"push (stat err = %v); the reverify must go through reassertLocalImage so it inherits the "+
			"delete-intent gate, rather than repairing the file itself", err)
	}
}
