package publish

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// #2712, THE SETTLE WINDOW ITSELF. The late-delete fix waits
// reassertSettleDelay between two repair passes, and that wait is a window of
// live operator time that did not exist before. These tests cover the two facts
// the rest of the suite does not: that the window does not let the repair undo
// an OPERATOR's action, and that the wait is genuinely waited.
//
// EVERY TEST HERE IS SERIAL -- no t.Parallel, deliberately. They swap the
// package-level uploader factories, exactly like the other tests that do.

// settleActor is the peer for the operator-save case. It clobbers the file once
// during the upload, and the OPERATOR then saves a new crop from a goroutine
// that waits for the first repair pass to finish undoing that clobber.
//
// A RENDEZVOUS ON PASS 1'S OUTPUT, not a timer. An earlier version armed the
// save on a fixed delay, which could only prove the save landed before the push
// returned -- never that it landed after pass 1. A pass 1 delayed past that arm
// would restore the pre-push bytes AFTER the save, and the final assertion would
// fail blaming pass 2 for a fixture-timing problem. Waiting on bytes that only
// pass 1 can put on disk makes the ordering true by construction instead of
// inferred from wall-clock timing.
//
// It rendezvouses on the restore's OUTPUT rather than on anything earlier for
// the reason late_delete_test.go documents: WriteFileAtomic renames a temp file
// onto the target, so observing the bytes proves the rename completed and the
// pass is done with this file.
type settleActor struct {
	victim   string
	prePush  []byte // what pass 1 puts back; seeing it on disk means pass 1 finished
	newBytes []byte

	// restoredSeenAt is when the fixture observed pass 1's completed work. The
	// test asserts the operator's save came after it, which is the ordering the
	// whole scenario depends on.
	restoredSeenAt time.Time

	// wroteAt is when the operator's save completed. The tests use it to prove
	// the save landed while the push was still running, without which they
	// assert nothing about concurrency at all.
	wroteAt time.Time
	wrote   bool
	// writeErr is why the save never landed. These fixtures turn on wall-clock
	// timing, so when one breaks on CI the error text is the only evidence
	// available: "did not land on disk" alone cannot distinguish a permissions
	// problem from a torn-down temp dir from a bug in the test.
	writeErr error

	calls int
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
}

func newSettleActor(victim string, prePush, newBytes []byte) *settleActor {
	return &settleActor{victim: victim, prePush: prePush, newBytes: newBytes, done: make(chan struct{})}
}

func (u *settleActor) arm() error {
	u.calls++
	// The peer CLOBBERS the file during the upload. That is what gives the first
	// repair pass real work to do, and therefore an observable output the
	// operator's save can be ordered against. Without it pass 1 is a no-op and
	// nothing can prove the save followed it.
	if err := os.WriteFile(u.victim, []byte("PEER-CLOBBER-BYTES"), 0o600); err != nil {
		// Surface it as an upload failure rather than swallowing it. A clobber
		// that did not land leaves pass 1 with nothing to repair, so the
		// rendezvous below would wait out its full timeout and the test would
		// fail somewhere far from the cause.
		u.writeErr = err
		return err
	}
	u.once.Do(func() {
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			defer close(u.done)
			// RENDEZVOUS ON PASS 1'S COMPLETED OUTPUT, not on a timer. Pass 1
			// repairs the clobber by writing prePush back, so seeing those bytes
			// on disk proves the pass finished with this file -- WriteFileAtomic
			// renames a temp file onto the target, so the bytes appearing means
			// the rename is done. Latching on anything earlier would let the save
			// land mid-pass and prove nothing about ordering.
			if !u.waitForRestore() {
				return
			}
			u.restoredSeenAt = time.Now()
			if err := os.WriteFile(u.victim, u.newBytes, 0o600); err != nil {
				u.writeErr = err
				return
			}
			u.wroteAt = time.Now()
			u.wrote = true
		}()
	})
	return nil
}

// waitForRestore blocks until pass 1 has put prePush back, reporting false if it
// never does. The timeout is generous because exceeding it means the fixture is
// broken rather than slow.
func (u *settleActor) waitForRestore() bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(u.victim); err == nil && bytes.Equal(got, u.prePush) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func (u *settleActor) UploadImage(_ context.Context, _, _ string, _ []byte, _ string) error {
	return u.arm()
}

func (u *settleActor) UploadImageAtIndex(_ context.Context, _, _ string, _ int, _ []byte, _ string) error {
	return u.arm()
}

// settleHarness wires a publisher whose single peer is the given actor.
// Serial, like every test that swaps these package-level factories.
// The armed timer is joined in cleanup via wg, exactly as lateDeleteHarness
// joins its peer goroutine. Both tests also wait on wg in the body, but only
// AFTER a select on done -- and if that select times out into t.Fatal, the timer
// is still armed and can write into or remove from the t.TempDir() the framework
// is concurrently tearing down. The join has to be in cleanup to cover that path.
func settleHarness(t *testing.T, single connection.ImageUploader, indexed connection.IndexedImageUploader,
	wg *sync.WaitGroup, dir string,
) (*Publisher, *artist.Artist) {
	t.Helper()

	conn := &connection.Connection{
		ID: "c1", Name: "Peer", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL: "http://peer.invalid",
	}
	conn.FeatureManageServerFiles = true

	origSingle := newImageUploader
	origIndexed := newIndexedImageUploader
	newImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.ImageUploader { return single }
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return indexed
	}
	t.Cleanup(func() {
		newImageUploader = origSingle
		newIndexedImageUploader = origIndexed
		wg.Wait()
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

// TestSettleWindow_OperatorSaveIsNotReverted is the regression test for the
// defect the settle pass introduced.
//
// THE SHAPE. The peer clobbers the file once during the upload, and the first
// repair pass correctly undoes that -- which is the behavior #2701 exists for
// and is NOT what is under test here. It is the fixture's clock: the operator's
// save fires the moment that restore is visible on disk, so it lands after pass
// 1 by construction rather than by beating a timer. That save happens during
// reassertSettleDelay, inside the settle window. The second pass then reads
// bytes that differ from the PRE-PUSH snapshot -- and if it were allowed the
// overwrite branch, it would write the pre-push bytes back over the crop the
// operator saved a moment ago, logging it as a peer clobber. The operator's
// work is gone and the log blames a peer that did nothing.
//
// It is RED on the commit that added the settle pass without narrowing it, and
// green on the base commit that had one pass. Both sync entry points are
// covered because they register the repair in different places (an inline call
// after the upload loop, versus a defer registered before it) and each passes
// the pass scope through its own closure, so narrowing one leaves the other
// broken.
func TestSettleWindow_OperatorSaveIsNotReverted(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	cases := []struct {
		name     string
		filename string
		push     func(p *Publisher, a *artist.Artist)
	}{
		{
			name:     "single image",
			filename: "banner.jpg",
			push: func(p *Publisher, a *artist.Artist) {
				p.SyncImageToPlatforms(context.Background(), a, "banner")
			},
		},
		{
			name:     "fanart",
			filename: "fanart.jpg",
			push: func(p *Publisher, a *artist.Artist) {
				p.SyncAllFanartToPlatforms(context.Background(), a)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			victim := filepath.Join(dir, tc.filename)
			prePush := []byte("BYTES-THE-PUSH-CAPTURED")
			operatorCrop := []byte("THE-CROP-THE-OPERATOR-JUST-SAVED")
			writeFile(t, victim, prePush)

			// No arm delay: the save fires as soon as the fixture SEES pass 1's
			// restore on disk, so it lands inside the settle window by
			// construction rather than by clearing a timer.
			up := newSettleActor(victim, prePush, operatorCrop)
			p, a := settleHarness(t, up, up, &up.wg, dir)

			tc.push(p, a)
			pushReturnedAt := time.Now()

			// PRECONDITIONS. Without all three, the assertion below passes
			// vacuously -- a save that never happened, or one that landed after
			// the push had already finished, proves nothing about the window.
			select {
			case <-up.done:
			case <-time.After(10 * time.Second):
				t.Fatal("precondition failed: the operator's save timer never fired")
			}
			up.wg.Wait()
			if up.calls == 0 {
				t.Fatal("precondition failed: the peer was never handed the image, so no repair was owed " +
					"and no settle window was ever opened")
			}
			if !up.wrote {
				t.Fatalf("precondition failed: the operator's save did not land on disk: %v", up.writeErr)
			}
			if !up.wroteAt.Before(pushReturnedAt) {
				t.Fatalf("precondition failed: the operator's save completed at %v but the push had already "+
					"returned at %v; the save was not concurrent with the push, so nothing here exercises "+
					"the settle window", up.wroteAt, pushReturnedAt)
			}
			// THE ORDERING FACT, asserted rather than assumed. This is what makes
			// the scenario a settle-window case at all: the save must land AFTER
			// pass 1 finished, or pass 1 could legitimately restore the pre-push
			// bytes over it and the assertion below would fail blaming pass 2 for a
			// fixture-timing problem. The fixture waits on bytes only pass 1 can put
			// on disk, so this holds by construction; the check keeps it that way.
			if up.restoredSeenAt.IsZero() {
				t.Fatal("precondition failed: the fixture never observed the first repair pass restore the " +
					"file, so the operator's save was never ordered against it")
			}
			if !up.restoredSeenAt.Before(up.wroteAt) {
				t.Fatalf("precondition failed: the operator's save at %v did not follow the first repair "+
					"pass, observed complete at %v; pass 1 could have restored the pre-push bytes over the "+
					"save, so a failure below would not implicate pass 2", up.wroteAt, up.restoredSeenAt)
			}

			got, err := os.ReadFile(victim)
			if err != nil {
				t.Fatalf("reading the file after the push: %v", err)
			}
			if bytes.Equal(got, prePush) {
				t.Fatalf("the post-settle repair pass reverted the operator's save: the file holds the "+
					"pre-push bytes %q, want the crop the operator saved during the settle window (%q). "+
					"The second pass must repair the MISSING case only -- after the settle window, bytes "+
					"that merely differ are as likely the operator's new save as a peer's clobber",
					got, operatorCrop)
			}
			if !bytes.Equal(got, operatorCrop) {
				t.Errorf("the file holds %q, want the operator's %q", got, operatorCrop)
			}
		})
	}
}

// settleDeleter is a peer that deletes the operator's file from a TIMER armed
// during the upload, with no rendezvous of any kind.
//
// This is the ~15ms-late delete measured on the fanart path, reproduced so that
// ONLY the production settle delay can cover it. There is no polling and no
// waiting on the first pass's output, so a fixture-supplied delay cannot stand
// in for the real one: delete the sleep, or set the delay to zero, and the
// second pass runs microseconds after the first, long before this timer fires.
type settleDeleter struct {
	victim   string
	armAfter time.Duration

	deletedAt time.Time
	deleted   bool

	calls int
	// removeErr is why the delete never landed; see settleActor.writeErr for
	// why a timing fixture must carry the cause rather than only the outcome.
	removeErr error

	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func newSettleDeleter(victim string, armAfter time.Duration) *settleDeleter {
	return &settleDeleter{victim: victim, armAfter: armAfter, done: make(chan struct{})}
}

func (u *settleDeleter) arm() error {
	u.calls++
	u.once.Do(func() {
		u.wg.Add(1)
		time.AfterFunc(u.armAfter, func() {
			defer u.wg.Done()
			defer close(u.done)
			if err := os.Remove(u.victim); err != nil {
				u.removeErr = err
				return
			}
			u.deletedAt = time.Now()
			u.deleted = true
		})
	})
	return nil
}

func (u *settleDeleter) UploadImage(_ context.Context, _, _ string, _ []byte, _ string) error {
	return u.arm()
}

func (u *settleDeleter) UploadImageAtIndex(_ context.Context, _, _ string, _ int, _ []byte, _ string) error {
	return u.arm()
}

// TestSettleWindow_LateDeleteIsCoveredOnlyByTheDelay pins the DELAY rather than
// the second call.
//
// The existing late-delete tests prove a second pass exists, but their fixture
// waits until it can see the first pass's restore before deleting, and that
// wait is itself 8 to 15ms of delay -- enough for the delete to land even if
// the production wait were deleted. So they stay green under a mutation that
// removes the sleep, which makes them silent about the one number that decides
// whether a late delete is caught at all.
//
// Here the delete fires from a fixed 80ms timer with no rendezvous. With the
// real 250ms settle the second pass sees the missing file and restores it; with
// the sleep removed, or reassertSettleDelay set to zero, both passes are done
// within microseconds and the operator's backdrop stays gone.
func TestSettleWindow_LateDeleteIsCoveredOnlyByTheDelay(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()
	victim := filepath.Join(dir, "fanart.jpg")
	want := []byte("OPERATOR-BACKDROP-BYTES")
	writeFile(t, victim, want)

	up := newSettleDeleter(victim, 80*time.Millisecond)
	p, a := settleHarness(t, up, up, &up.wg, dir)

	start := time.Now()
	p.SyncAllFanartToPlatforms(context.Background(), a)
	pushReturnedAt := time.Now()

	select {
	case <-up.done:
	case <-time.After(10 * time.Second):
		t.Fatal("precondition failed: the peer's delete timer never fired")
	}
	up.wg.Wait()
	if up.calls == 0 {
		t.Fatal("precondition failed: the peer was never handed the backdrop, so no repair was owed")
	}
	if !up.deleted {
		t.Fatalf("precondition failed: the peer's delete did not remove the file, so the second pass had "+
			"nothing to find: %v", up.removeErr)
	}
	// THE LATENESS FACT. The first pass runs the moment the upload loop ends,
	// microseconds into the push; a delete this far in cannot have been seen by
	// it. And the delete must land before the push returns, or the second pass
	// was never given the chance and the restore below would prove nothing.
	if up.deletedAt.Sub(start) < 40*time.Millisecond {
		t.Fatalf("precondition failed: the delete landed %v into the push, too early to be provably after "+
			"the first repair pass", up.deletedAt.Sub(start))
	}
	if !up.deletedAt.Before(pushReturnedAt) {
		t.Fatalf("precondition failed: the delete landed at %v, after the push returned at %v; nothing "+
			"here exercises the settle window", up.deletedAt, pushReturnedAt)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("the backdrop is still gone after the push (%v). The peer deleted it %v after the push "+
			"began, with no rendezvous -- only a genuine settle DELAY between the two repair passes can "+
			"cover a delete of that shape, and nothing else in this codebase restores a local artwork "+
			"file from a peer", err, up.deletedAt.Sub(start))
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the backdrop reads %q, want the operator's %q", got, want)
	}
}

// TestRepairAfterPush_WaitsBetweenPassesAndNarrowsTheSecond asserts the two
// properties of repairAfterPush directly, without a filesystem in the way: it
// runs the pass exactly twice with a real wait in between, and the second run
// is the narrowed one.
//
// The wait is compared against a LITERAL, not against reassertSettleDelay. That
// is deliberate: the constant is one of the things a mutation moves, and an
// assertion phrased in terms of the value under test is satisfied by any value,
// including zero.
func TestRepairAfterPush_WaitsBetweenPassesAndNarrowsTheSecond(t *testing.T) {
	// No t.Parallel; this measures wall-clock time.
	p := New(Deps{Logger: silentLogger()})

	var at []time.Time
	var scopes []repairScope
	// repairAfterPush is SYNCHRONOUS: it calls the pass, sleeps, and calls it
	// again, all on this goroutine. That is why these appends need no
	// synchronization. If the passes ever move to a background goroutine, this
	// fixture needs a mutex and the race detector will say so.
	p.repairAfterPush(func(s repairScope) {
		at = append(at, time.Now())
		scopes = append(scopes, s)
	})

	if len(at) != 2 {
		t.Fatalf("the pass ran %d times, want exactly 2: one immediately and one after the settle window", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < 200*time.Millisecond {
		t.Errorf("the second pass ran %v after the first; it must wait out the settle window (250ms), or "+
			"it is the same point-in-time check twice and covers no late peer action at all", gap)
	}
	if scopes[0] != repairAllDamage {
		t.Errorf("the first pass ran with scope %v, want repairAllDamage: it runs before any settle window "+
			"has elapsed, so it still repairs an overwrite (the #2533 crop-clobber repair)", scopes[0])
	}
	if scopes[1] != repairMissingOnly {
		t.Errorf("the second pass ran with scope %v, want repairMissingOnly: after the settle window, "+
			"differing bytes may be the operator's own save and must not be overwritten", scopes[1])
	}
}
