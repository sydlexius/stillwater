package deleteintenttest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
)

// The probe is a test helper whose verdict gates seven production call sites'
// assertions, so its own selection rules need pinning: a helper that silently
// counted the wrong unlink would hand every caller a green light for free.
//
// BOTH the non-failing behavior AND the failure branches are exercised here.
// The non-failing paths run against a real *testing.T like any other test.
// The five failure branches (t.Fatalf/t.Errorf calls) are driven through
// fakeT below, a minimal recording double for the tHelper interface
// UnlinkProbe actually depends on -- see tHelper's doc comment for why that
// interface is safe to add.
//
// The fake-harness tests and the two-sided relocation proofs in internal/api
// and internal/rule answer DIFFERENT questions, and neither replaces the
// other. The fake tests pin the helper's own VERDICTS in isolation: does
// NewUnlinkProbe reject zero types, does it reject a pre-existing marker,
// does AssertMarkedBeforeUnlink reject "no unlink happened" and "marked too
// late", does AssertNeverMarked reject "marked at all" -- and do each of
// those name the RIGHT failure, not merely fail. None of that proves the
// helper is wired correctly into real production code; that is what the
// relocation proofs measure, by relocating a real marker call in a real
// handler/fixer and confirming the probe goes RED against it. A helper could
// pass every fake-harness test here and still be miswired into its callers
// (wrong directory, wrong type, wrong seam) -- only the relocation proofs
// catch that. Both are kept for this reason.

// fakeT is a minimal recording double for tHelper (probe.go), used to drive
// UnlinkProbe's five failure branches without aborting the real test's own
// goroutine.
//
// Fatalf PANICS with the fakeTFatal sentinel rather than recording and
// returning. Real testing.T.Fatalf calls runtime.Goexit and never returns to
// its caller; AssertMarkedBeforeUnlink's no-unlink-observed branch relies on
// exactly that -- it falls straight from its Fatalf into
// `for i, ty := range p.types { if !p.first.live[i]`, dereferencing a nil
// p.first on the very next line. A fake Fatalf that recorded the message and
// returned normally would crash there with an unrelated nil-pointer panic
// instead of reporting the intended failure, and a test asserting only "it
// panicked" would pass for that wrong reason too -- see runFatal below, which
// re-panics anything that is not the sentinel rather than swallowing it.
// Panicking in Fatalf reproduces "does not return" faithfully; each test
// recovers the sentinel and reads its message.
//
// Errorf does NOT panic, matching real testing.T.Errorf, which records a
// failure and continues -- AssertNeverMarked's loop depends on that to
// report every offending watched type, not just the first.
type fakeT struct {
	errors []string
}

// fakeTFatal is the sentinel fakeT.Fatalf panics with, so runFatal can tell
// "the code under test called Fatalf as expected" apart from any other panic.
type fakeTFatal struct{ msg string }

func (f *fakeT) Helper() {}

func (f *fakeT) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeT) Fatalf(format string, args ...any) {
	panic(fakeTFatal{msg: fmt.Sprintf(format, args...)})
}

// runFatal calls fn, which is expected to call Fatalf on the fakeT it closes
// over, and returns the recorded message.
//
// It fails the real t (not the fake) if fn returns without calling Fatalf --
// the branch under test was never reached, which is a test bug, not a
// helper bug. It re-panics anything recovered that is not the fakeTFatal
// sentinel, rather than swallowing it as if it were the expected abort: a
// genuine bug in the code under test (e.g. the nil-pointer panic fakeT's own
// doc comment describes) must surface as a failure, not be silently
// absorbed here.
func runFatal(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("fn returned normally; want it to call Fatalf")
			return
		}
		sentinel, ok := r.(fakeTFatal)
		if !ok {
			panic(r)
		}
		msg = sentinel.msg
	}()
	fn()
	return ""
}

// TestAround_IgnoresPathsOutsideTheWatchedDirectory pins the scoping rule. A
// handler may remove a temp file elsewhere in the same operation, and counting
// that as "the first unlink" would make the assertion depend on incidental
// ordering rather than on where the mark sits.
func TestAround_IgnoresPathsOutsideTheWatchedDirectory(t *testing.T) {
	watched := t.TempDir()
	other := t.TempDir()
	probe := NewUnlinkProbe(t, watched, "fanart")

	// A sibling directory, and a SUBdirectory of the watched one: the marker's
	// key is the image directory itself, so neither bears on the claim.
	sub := filepath.Join(watched, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("creating nested dir: %v", err)
	}
	for _, p := range []string{filepath.Join(other, "a.jpg"), filepath.Join(sub, "b.jpg")} {
		if err := probe.Around(p, func() error { return nil }); err != nil {
			t.Fatalf("Around(%s) returned %v; it must pass an unwatched removal through unchanged", p, err)
		}
	}
	if got := probe.UnlinkCount(); got != 0 {
		t.Errorf("UnlinkCount = %d after removals outside the watched directory, want 0; a probe that "+
			"counted them would report the wrong instant as the first unlink", got)
	}
}

// TestAround_DoesNotCountAFailedRemoval pins the success rule, which is what
// keeps a no-op attempt from presenting itself as the first unlink. The
// duplicate fixer clears a possibly-absent stale tomb before it stages
// anything, so this case is reached in production, not just in theory.
func TestAround_DoesNotCountAFailedRemoval(t *testing.T) {
	dir := t.TempDir()
	probe := NewUnlinkProbe(t, dir, "fanart")

	sentinel := errors.New("no such file")
	err := probe.Around(filepath.Join(dir, "gone.jpg"), func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Around returned %v, want the removal's own error unchanged: a probe that swallowed or "+
			"replaced it would change the behavior of the code under test", err)
	}
	if got := probe.UnlinkCount(); got != 0 {
		t.Errorf("UnlinkCount = %d after a FAILED removal, want 0; nothing vanished, so no window opened "+
			"and there is no instant to sample", got)
	}
}

// TestAround_CountsEverySuccessfulWatchedRemoval is the positive control for
// the two above: without it they would pass for a probe that counted nothing at
// all, which is the vacuity shape this whole mechanism exists to catch.
func TestAround_CountsEverySuccessfulWatchedRemoval(t *testing.T) {
	dir := t.TempDir()
	probe := NewUnlinkProbe(t, dir, "fanart")
	img.MarkDeleteIntent(dir, "fanart")

	for _, name := range []string{"fanart.jpg", "fanart1.jpg"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
			t.Fatalf("Around(%s): %v", name, err)
		}
	}
	if got := probe.UnlinkCount(); got != 2 {
		t.Errorf("UnlinkCount = %d, want 2", got)
	}
	probe.AssertMarkedBeforeUnlink("a marker written before both removals")
}

// TestAssertNeverMarked_PassesWhenNoTypeCarriesAMarker pins AssertNeverMarked's
// non-failing path -- the loop runs to completion and reports nothing when none
// of the watched types carry a marker of any age. It is the negative sibling of
// TestAround_CountsEverySuccessfulWatchedRemoval, watching for the opposite
// outcome instead of a removal.
//
// This test runs against a real *testing.T; AssertNeverMarked's FAILING branch
// is driven separately, through fakeT, by TestAssertNeverMarked_FailsWhenAMarkerExists
// below.
func TestAssertNeverMarked_PassesWhenNoTypeCarriesAMarker(t *testing.T) {
	dir := t.TempDir()

	// PRECONDITION, asserted directly against the marker store rather than
	// inferred from NewUnlinkProbe's own construction-time check: without this,
	// a probe silently reading the wrong directory or the wrong types would
	// still pass below for having asserted nothing.
	for _, ty := range []string{"fanart", "thumb"} {
		if img.DeleteIntentAfter(dir, ty, time.Time{}) {
			t.Fatalf("precondition failed: %s already carries a %s delete marker, so this test would not "+
				"be observing AssertNeverMarked's non-failing path", dir, ty)
		}
	}

	// Watching two types, not one, so the loop runs more than one iteration on
	// the non-failing path.
	//
	// THIS TEST DOES NOT PROVE THE LOOP VISITS BOTH TYPES, and an earlier
	// version of this comment claimed that it did. BOTH watched types are
	// unmarked here, so a probe that checked only the first returns the
	// identical verdict and passes this test unchanged. The multi-type property
	// is pinned by TestAssertNeverMarked_ChecksEveryWatchedType below, which
	// marks ONLY the second.
	probe := NewUnlinkProbe(t, dir, "fanart", "thumb")
	probe.AssertNeverMarked("no delete occurred for either watched type")
}

// TestNewUnlinkProbe_FailsWhenNoTypesGiven drives the zero-types precondition
// (probe.go, the len(types) == 0 branch) through fakeT, and asserts the
// recorded message names the actual failure, not merely that Fatalf fired --
// five tests that only checked "it complained" would pass for a helper that
// complained about the wrong thing.
func TestNewUnlinkProbe_FailsWhenNoTypesGiven(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()

	msg := runFatal(t, func() {
		NewUnlinkProbe(f, dir)
	})
	if !strings.Contains(msg, "no image types to watch") {
		t.Fatalf("Fatalf message = %q, want it to name the no-types failure", msg)
	}
}

// TestNewUnlinkProbe_FailsWhenMarkerAlreadyExists drives the pre-existing-
// marker precondition.
func TestNewUnlinkProbe_FailsWhenMarkerAlreadyExists(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	img.MarkDeleteIntent(dir, "fanart")

	msg := runFatal(t, func() {
		NewUnlinkProbe(f, dir, "fanart")
	})
	if !strings.Contains(msg, "already carries a fanart delete marker") {
		t.Fatalf("Fatalf message = %q, want it to name the pre-existing-marker failure", msg)
	}
}

// TestAssertMarkedBeforeUnlink_FailsWhenNoUnlinkObserved drives the
// no-unlink-observed precondition -- the branch that would nil-pointer-panic
// if Fatalf ever returned instead of aborting; see fakeT's doc comment for
// why that makes the panic-then-recover shape load-bearing here, not just a
// style choice.
func TestAssertMarkedBeforeUnlink_FailsWhenNoUnlinkObserved(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	probe := NewUnlinkProbe(f, dir, "fanart")

	msg := runFatal(t, func() {
		probe.AssertMarkedBeforeUnlink("no unlink happened")
	})
	if !strings.Contains(msg, "performed no unlink inside") {
		t.Fatalf("Fatalf message = %q, want it to name the no-unlink-observed failure", msg)
	}
}

// TestAssertMarkedBeforeUnlink_FailsWhenMarkerNotLiveAtUnlink drives the
// marker-not-live-at-unlink branch -- the same branch section 5's
// internal/api relocation proof fires against real production code (relocate
// a real img.MarkDeleteIntent call to after its unlink and watch these tests
// go red); this pins the helper's own message for that branch in isolation,
// which the relocation proof does not check.
func TestAssertMarkedBeforeUnlink_FailsWhenMarkerNotLiveAtUnlink(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	probe := NewUnlinkProbe(f, dir, "fanart")

	// Deliberately no img.MarkDeleteIntent call before the unlink: the marker
	// is not live at the sampled instant, which is the failure under test.
	p := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
		t.Fatalf("Around(%s): %v", p, err)
	}

	msg := runFatal(t, func() {
		probe.AssertMarkedBeforeUnlink("marked too late")
	})
	if !strings.Contains(msg, "was NOT live at the instant") {
		t.Fatalf("Fatalf message = %q, want it to name the not-live-at-unlink failure", msg)
	}
}

// TestAssertNeverMarked_FailsWhenAMarkerExists drives AssertNeverMarked's
// marker-found branch -- the same branch internal/rule's rollback-path
// relocation proof fires against real production code (Part 2 of #3015, not
// in this worktree); this pins the helper's own message for that branch in
// isolation.
func TestAssertNeverMarked_FailsWhenAMarkerExists(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	probe := NewUnlinkProbe(f, dir, "fanart")

	// Marked AFTER construction, not before: NewUnlinkProbe's own precondition
	// (pinned above) would reject a marker that already existed at
	// construction time, so this is the only way to reach the branch under
	// test through the public API.
	img.MarkDeleteIntent(dir, "fanart")

	probe.AssertNeverMarked("a rollback that should have recorded nothing")
	if len(f.errors) != 1 {
		t.Fatalf("Errorf calls = %d, want 1; got %v", len(f.errors), f.errors)
	}
	if !strings.Contains(f.errors[0], "recorded a fanart delete marker") {
		t.Fatalf("Errorf message = %q, want it to name the marker-found failure", f.errors[0])
	}
}

// TestAround_SamplesEveryWatchedType is the positive control for the two
// multi-type failure tests below, and it pins a DIFFERENT loop from theirs:
// Around's own sampling loop, which fills one live[i] per watched type before
// the removal runs. A probe that sampled only the first watched type would
// report the rest as not-live and redden this test, even though both markers
// were written before the unlink.
func TestAround_SamplesEveryWatchedType(t *testing.T) {
	dir := t.TempDir()
	probe := NewUnlinkProbe(t, dir, "fanart", "thumb")
	img.MarkDeleteIntent(dir, "fanart")
	img.MarkDeleteIntent(dir, "thumb")

	p := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
		t.Fatalf("Around(%s): %v", p, err)
	}
	probe.AssertMarkedBeforeUnlink("both watched types marked before the unlink")
}

// TestAssertMarkedBeforeUnlink_ChecksEveryWatchedType pins that the verdict
// covers ALL watched types, not just the first one.
//
// WHY THIS IS NOT REDUNDANT WITH THE TESTS ABOVE. Every other
// AssertMarkedBeforeUnlink test watches exactly one type, so a probe whose
// verdict loop stopped after types[0] would be indistinguishable from the real
// one. The concrete caller this protects marks the whole canonical set
// (fanart, thumb, logo, banner) in one operation: mark fanart before the unlink
// and the other three after, and a first-type-only probe reports PASS while
// three types get exactly the ENOENT window this package exists to close.
//
// The fixture therefore differs along the only axis that matters -- the FIRST
// watched type is marked in time and the SECOND is not marked at all -- and the
// assertion is on the TYPE NAMED in the message, not merely on the fact that
// the probe failed.
func TestAssertMarkedBeforeUnlink_ChecksEveryWatchedType(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	probe := NewUnlinkProbe(f, dir, "fanart", "thumb")

	// Only the FIRST watched type is marked before the unlink. thumb is never
	// marked, so a probe that checks every type must fail naming thumb.
	img.MarkDeleteIntent(dir, "fanart")

	p := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
		t.Fatalf("Around(%s): %v", p, err)
	}

	msg := runFatal(t, func() {
		probe.AssertMarkedBeforeUnlink("marked only the first watched type")
	})
	if !strings.Contains(msg, "the thumb delete marker was NOT live") {
		t.Fatalf("Fatalf message = %q, want it to name THUMB -- the watched type that had no live marker. "+
			"A message naming fanart (which WAS marked in time) would mean the probe reported the wrong "+
			"type; no message at all means it stopped after the first watched type", msg)
	}
}

// TestAssertNeverMarked_ChecksEveryWatchedType is the negative-half sibling of
// the test above: AssertNeverMarked loops over the same p.types, and every
// other test of it watches one type, so the loop's coverage of the REST is
// unpinned by them.
//
// Only the SECOND watched type is marked here. A probe that checked only the
// first would find nothing and record no failure at all.
func TestAssertNeverMarked_ChecksEveryWatchedType(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	probe := NewUnlinkProbe(f, dir, "fanart", "thumb")

	img.MarkDeleteIntent(dir, "thumb")

	// PRECONDITION: the FIRST watched type must stay unmarked, or the single
	// recorded failure below could be about fanart and the test would pass
	// without ever exercising the second iteration.
	if img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Fatalf("precondition failed: %s carries a fanart delete marker, so a failure recorded below would "+
			"not prove the probe looked past the first watched type", dir)
	}

	probe.AssertNeverMarked("a rollback that marked only the second watched type")
	if len(f.errors) != 1 {
		t.Fatalf("Errorf calls = %d, want 1; got %v. Zero calls means AssertNeverMarked stopped after the "+
			"first watched type and never looked at thumb", len(f.errors), f.errors)
	}
	if !strings.Contains(f.errors[0], "recorded a thumb delete marker") {
		t.Fatalf("Errorf message = %q, want it to name THUMB, the type that actually carries the marker", f.errors[0])
	}
}

// TestAssertMarkedBeforeUnlink_ReportsTheFirstUnlinkNotTheLast pins the
// "first unlink, never overwritten" rule (probe.go's `if p.first == nil`).
//
// WHY THIS IS THE MOST LOAD-BEARING TEST IN THE FILE. Without that guard the
// probe reports on the LAST unlink instead of the first, which is very nearly
// the EXISTENCE assertion this whole package was built to replace -- and it
// would keep presenting itself as a placement probe while doing so. Every one
// of the production call sites would stay green.
//
// The fixture is the real defect shape: the marker is written BETWEEN two
// unlinks, so it is too late for the first file and in time for the second.
// Asserting merely that the probe failed is NOT enough -- a last-unlink probe
// fails here too, just naming the wrong file -- so the discriminator is the
// FILENAME in the recorded message.
func TestAssertMarkedBeforeUnlink_ReportsTheFirstUnlinkNotTheLast(t *testing.T) {
	f := &fakeT{}
	dir := t.TempDir()
	probe := NewUnlinkProbe(f, dir, "fanart")

	unlink := func(name string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
			t.Fatalf("Around(%s): %v", name, err)
		}
	}

	unlink("fanart.jpg")                // too early for the mark: the window is open here
	img.MarkDeleteIntent(dir, "fanart") // written BETWEEN the two unlinks
	unlink("fanart1.jpg")               // in time for this one only

	if got := probe.UnlinkCount(); got != 2 {
		t.Fatalf("UnlinkCount = %d, want 2; the mark-between-unlinks fixture needs both removals to be "+
			"observed or there is no first-versus-last distinction to make", got)
	}

	msg := runFatal(t, func() {
		probe.AssertMarkedBeforeUnlink("marked between the two unlinks")
	})
	if !strings.Contains(msg, "instant fanart.jpg was unlinked") {
		t.Fatalf("Fatalf message = %q, want it to name fanart.jpg -- the FIRST unlink, the one the marker "+
			"was too late for", msg)
	}
	if strings.Contains(msg, "instant fanart1.jpg was unlinked") {
		t.Fatalf("Fatalf message = %q names fanart1.jpg, the LAST unlink. The probe is reporting on the "+
			"last observation instead of the first, which downgrades it to approximately an existence "+
			"assertion while still presenting as a placement probe", msg)
	}
}

// TestAround_TreatsAMarkerOlderThanTheProbeAsNotLive pins Around's `since`
// baseline -- that "live" means "recorded at or after this run began", not
// "recorded at any time".
//
// WHY THE PROBE IS BUILT BY HAND HERE, which is the only place in this file
// that does so. The state under test is a marker that predates the run, and
// NewUnlinkProbe REJECTS exactly that state as a precondition failure, so the
// public constructor cannot reach it. The two guards are redundant by design,
// and that redundancy is precisely why neither one's removal is observable
// through the other. The struct below is built with the same fields
// NewUnlinkProbe sets, with `since` stamped after the stale mark.
func TestAround_TreatsAMarkerOlderThanTheProbeAsNotLive(t *testing.T) {
	dir := t.TempDir()

	// A marker left behind by an earlier test for the same directory, still
	// well inside DeleteIntentRetention.
	img.MarkDeleteIntent(dir, "fanart")
	time.Sleep(time.Millisecond)
	since := time.Now()

	// PRECONDITIONS, both required or the test is vacuous. The stale marker
	// must be VISIBLE to a zero baseline (otherwise the two baselines agree and
	// nothing is being discriminated) and INVISIBLE to the probe's own baseline
	// (otherwise it is not stale at all).
	if !img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Fatalf("precondition failed: %s carries no fanart marker at all, so a zero baseline would see "+
			"nothing either and this test could not tell the two baselines apart", dir)
	}
	if img.DeleteIntentAfter(dir, "fanart", since) {
		t.Fatalf("precondition failed: the marker in %s is not older than the probe's baseline", dir)
	}

	probe := &UnlinkProbe{t: &fakeT{}, dir: filepath.Clean(dir), types: []string{"fanart"}, since: since}

	p := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
		t.Fatalf("Around(%s): %v", p, err)
	}

	msg := runFatal(t, func() {
		probe.AssertMarkedBeforeUnlink("only a marker older than the run")
	})
	if !strings.Contains(msg, "was NOT live at the instant") {
		t.Fatalf("Fatalf message = %q, want the not-live-at-unlink failure. A probe sampling with a ZERO "+
			"baseline would count the earlier test's leftover marker as live and pass here, which is the "+
			"cross-test contamination `since` exists to exclude", msg)
	}
}
