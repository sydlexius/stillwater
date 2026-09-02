// Package deleteintenttest holds the ONE assertion every delete-intent call
// site's test uses to prove the property #2712/#3015 actually turn on: that the
// delete marker is LIVE AT THE INSTANT OF THE UNLINK, not merely written
// somewhere in the same function.
//
// WHY THIS PACKAGE EXISTS RATHER THAN A HELPER PER TEST FILE. The marker's
// writers live in two packages (internal/api's five handlers, internal/rule's
// two fixers) and every one of them makes the same claim, so the assertion is a
// property of the MECHANISM and not of either caller. Written once per package
// it would drift; written once per test it did drift -- the first round of
// #3015 tests, and the four #3014 tests before them, all asserted only that a
// marker EXISTED once the handler returned.
//
// WHY THAT WEAKER ASSERTION IS NOT ENOUGH, stated plainly because it is the
// whole reason this file is here. "A marker exists afterwards" is satisfied
// equally by code that marks BEFORE its unlink and by code that marks AFTER it.
// Those two are not equivalent: img.MarkDeleteIntent's doc comment says a
// marker written after the unlink "leaves a window in which the file is gone
// and the intent is not yet visible -- which is the original bug, merely
// narrowed." A push verifying inside that window reads ENOENT, finds no
// marker, and resurrects the artwork. Relocating all three marks to after their
// unlink was measured to leave the entire internal/rule + internal/api +
// internal/publish + internal/image suite green. PLACEMENT is a strictly finer
// property than EXISTENCE, and only a probe that samples the marker store from
// INSIDE the removal can measure it.
//
// HOW IT WORKS. A caller installs the probe into whatever unlink seam its
// package already has -- internal/api's FileRemover interface, internal/rule's
// removeFile package variable -- by wrapping the real removal in Around. The
// probe samples img.DeleteIntentAfter immediately before the file is touched
// and reports on the FIRST SUCCESSFUL unlink, because that is the first instant
// at which a concurrent push could read ENOENT for a file that is genuinely
// gone. A failed removal is deliberately not an observation: nothing vanished,
// so no window opened.
//
// WHY THIS IS A NORMAL .go FILE AND NOT A _test.go ONE. Go does not export
// identifiers from a package's test files, so a helper shared by tests in
// internal/api AND internal/rule cannot live in either package's _test.go. It
// therefore has to be its own package, on the same footing as net/http/httptest
// -- which is likewise a non-test package importing testing. Nothing in
// production imports this package; the only importers are _test.go files.
package deleteintenttest

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
)

// UnlinkProbe samples the delete-intent marker store at each unlink performed
// under a directory, so a test can assert the marker was already live then.
//
// It is safe for concurrent use: handlers under test may unlink from more than
// one goroutine, and a probe that raced would report a nondeterministic answer
// to a deterministic question.
type UnlinkProbe struct {
	t     *testing.T
	dir   string
	types []string
	// since is stamped at construction, so "live" below means "recorded at or
	// after this test began" -- the same comparison reassertLocalImage makes
	// with its own push-entry snapshot. A zero since would also accept a marker
	// left behind by an earlier test.
	since time.Time

	mu sync.Mutex
	// first records the outcome at the FIRST observed unlink and is never
	// overwritten. Later unlinks in the same operation are uninteresting: by
	// then the window this mechanism protects is already open or already
	// closed.
	first    *observation
	unlinks  int
	observed []string
}

// observation is the marker state sampled at one unlink.
type observation struct {
	path string
	// live[i] reports whether types[i] had a marker recorded at or after since
	// at the instant path was about to be removed.
	live []bool
}

// NewUnlinkProbe returns a probe for markers of every imageType in types under
// dir, and asserts as a PRECONDITION that no such marker exists yet.
//
// The precondition is not ceremony. The marker store is a package-level
// sync.Map keyed by cleaned directory, so a marker leaked from another test for
// the same path would make every assertion below pass regardless of what the
// code under test did. Callers therefore pass a t.TempDir.
func NewUnlinkProbe(t *testing.T, dir string, types ...string) *UnlinkProbe {
	t.Helper()
	if len(types) == 0 {
		t.Fatalf("NewUnlinkProbe(%s) was given no image types to watch, so it would assert nothing", dir)
	}
	for _, ty := range types {
		if img.DeleteIntentAfter(dir, ty, time.Time{}) {
			t.Fatalf("precondition failed: %s already carries a %s delete marker before the run, so "+
				"every assertion this probe makes would be reading a marker this test did not write", dir, ty)
		}
	}
	return &UnlinkProbe{t: t, dir: filepath.Clean(dir), types: types, since: time.Now()}
}

// Around samples the delete-intent marker store for path, performs the removal
// by calling remove, and records the sample as an observation only if that
// removal SUCCEEDED. It returns remove's error unchanged, so a caller can
// install it inside a seam without altering the behavior under test.
//
// Sampling happens BEFORE remove runs, which is the entire point: asking after
// the unlink cannot distinguish a marker written before it from one written
// after it, and those are the two cases this package exists to separate.
//
// Recording only on success keeps the first observation meaningful. Production
// paths attempt removals that legitimately find nothing -- the duplicate
// fixer clears a possibly-absent stale tomb before it stages anything -- and
// counting an ENOENT as "the first unlink" would make the assertion depend on
// which no-op happened to run first rather than on where the mark sits.
//
// Paths outside the probe's directory are passed through unobserved: a handler
// may remove a temp file elsewhere in the same operation, and the marker's key
// is the image directory itself, so nothing about such a path bears on the
// claim. The comparison is on filepath.Dir, so a file in a SUBdirectory does
// not count either.
func (p *UnlinkProbe) Around(path string, remove func() error) error {
	if filepath.Clean(filepath.Dir(path)) != p.dir {
		return remove()
	}
	live := make([]bool, len(p.types))
	for i, ty := range p.types {
		live[i] = img.DeleteIntentAfter(p.dir, ty, p.since)
	}

	err := remove()
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.unlinks++
	p.observed = append(p.observed, filepath.Base(path))
	if p.first == nil {
		p.first = &observation{path: path, live: live}
	}
	return nil
}

// AssertMarkedBeforeUnlink fails unless the code under test unlinked at least
// one file in the probe's directory AND every watched image type already had a
// live marker at that first unlink.
//
// callSite names the production path under test and appears in the failure, so
// a shared helper still says which of the seven writers broke.
func (p *UnlinkProbe) AssertMarkedBeforeUnlink(callSite string) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	// PRECONDITION, and the one that makes this assertion non-vacuous: an
	// operation that removed nothing has no instant to sample, so "the marker
	// was live at the unlink" would be trivially true of a no-op.
	if p.first == nil {
		p.t.Fatalf("precondition failed: %s performed no unlink inside %s, so there was no instant at which "+
			"to sample the delete marker and this assertion measures nothing", callSite, p.dir)
	}
	for i, ty := range p.types {
		if !p.first.live[i] {
			p.t.Fatalf("%s: the %s delete marker was NOT live at the instant %s was unlinked. The mark is "+
				"placed at or after the unlink instead of before it, which leaves a window in which the "+
				"file is gone and the operator's intent is invisible: a push verifying inside that window "+
				"reads ENOENT, finds no marker, and restores the artwork the operator deleted (#2712, "+
				"narrowed but not closed -- see img.MarkDeleteIntent's doc comment). Unlinks observed "+
				"in order: %v",
				callSite, ty, filepath.Base(p.first.path), p.observed)
		}
	}
}

// AssertNeverMarked fails if any watched type carries a marker of ANY age.
//
// This is the negative half of the same class, used by the paths that must
// record nothing: a rollback that put the files back, and a fixer that removed
// nothing. The `since` used here is the zero time deliberately -- the claim is
// "no marker at all", not "no marker newer than the run", because a marker of
// any age suppresses repairs for its full retention window.
func (p *UnlinkProbe) AssertNeverMarked(callSite string) {
	p.t.Helper()
	for _, ty := range p.types {
		if img.DeleteIntentAfter(p.dir, ty, time.Time{}) {
			p.t.Errorf("%s recorded a %s delete marker for %s. Every push for this artist in the next %s "+
				"will decline to repair a genuine peer clobber of that type, and the marker is write-once "+
				"by design so nothing withdraws it (#3015)", callSite, ty, p.dir, img.DeleteIntentRetention)
		}
	}
}

// UnlinkCount reports how many unlinks inside the probe's directory were
// observed, for tests that need to pin the fixture's shape.
func (p *UnlinkProbe) UnlinkCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unlinks
}
