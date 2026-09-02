package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// #3177 SPLIT C -- THE ORDERING GUARDS.
//
// The #3177 advisory (extrafanartExposureWarning) is raised from a DEFER
// registered ABOVE repairAfterPush's defer in syncAllFanartToPlatforms, with a
// budget detached via context.WithoutCancel and a body gated on uploadedTo.
// Every clause in that sentence is load-bearing, and TWO hostile-review rounds
// produced a CRITICAL from getting one of them wrong:
//
//   ROUND 1 -- the call was a PLAIN STATEMENT between the snapshot and the
//   upload loop. A ctx-bounded directory read of a wedged mount there burns the
//   push's shared 30s budget, so every upload below runs against a dead context
//   and fails, while the operator is told "the push proceeded".
//
//   ROUND 2 -- the call was moved to immediately before `return warnings`,
//   which put it between the last upload and the DEFERRED repairAfterPush.
//   Repair pass 1's overwrite branch is deliberately ungated on the documented
//   premise that it "runs the instant UploadImage returns"; a stalled advisory
//   there delays pass 1 by up to the advisory's whole budget, and pass 1 then
//   writes the pre-push bytes over a crop the operator saved in the gap --
//   logging a platform peer as the culprit.
//
// Round 3 then MEASURED that the exact edits reproducing both CRITICALs pass
// the merged suite without a murmur: defer -> plain statement is green, and
// swapping the two defer registrations is green. Defer registration order is
// invisible at the call site and reads as formatting, which is precisely why it
// needs a test rather than a comment.
//
// THIS FILE IS THAT TEST. Each test below names the property it guards and the
// mutation it is built to redden; the receipt for this branch records the
// measured RED/GREEN for every one of them, applied one at a time.
//
// It deliberately defines NO new harness that extrafanart_warning_test.go
// already provides: singlePeerHarness, budgetHarness, ctxHonoringUploader,
// stallEnumeration, seedPrimaryFanart, seedExtrafanart, primaryFanartBytes and
// findExtrafanartWarning all come from there (same package).

// advisoryStallTolerance is how far a wall-clock measurement in this file may
// sit below the interval it is asserting, to absorb scheduler jitter on a
// loaded machine. It is applied only to LOWER bounds, never to an ordering
// assertion: the ordering ones compare two timestamps taken inside the same
// process and need no slack.
const advisoryStallTolerance = 500 * time.Millisecond

// probeUploader is a connection.IndexedImageUploader that (a) honors the
// context exactly as a real HTTP client does, so it can observe deadline
// starvation, (b) records WHEN the last upload returned, so a test can measure
// what happens after the upload loop, and (c) runs an optional hook after each
// successful upload, so a test can make the world change mid-push (an operator
// save, a canceled request) at a deterministic point.
//
// ctxHonoringUploader in extrafanart_warning_test.go covers (a) alone and is
// still the right instrument where that is all a test needs; this one is not a
// fork of it but the same idea with the two observation points these ordering
// tests require. The hook runs OUTSIDE the mutex so a hook that blocks (the
// operator-save probe sleeps) cannot serialize a concurrent uploader.
type probeUploader struct {
	mu         sync.Mutex
	ok         int
	failed     int
	lastUpload time.Time

	// onUpload, if set, runs after a SUCCESSFUL upload has been recorded.
	onUpload func()
}

func (u *probeUploader) UploadImageAtIndex(ctx context.Context, _, _ string, _ int, _ []byte, _ string) error {
	u.mu.Lock()
	if err := ctx.Err(); err != nil {
		u.failed++
		u.mu.Unlock()
		return err
	}
	u.ok++
	u.lastUpload = time.Now()
	hook := u.onUpload
	u.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (u *probeUploader) counts() (ok, failed int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ok, u.failed
}

func (u *probeUploader) lastUploadAt() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastUpload
}

// recordEnumeration replaces the listArtworkSubdirFiles seam with one that
// stamps the wall-clock instant the advisory enumeration is ENTERED and then
// delegates to the real implementation, so the check's observable behavior is
// unchanged and only its timing is instrumented. It returns an accessor for
// that instant and the call count.
//
// This is the instrument for the two ORDERING properties. Both prior CRITICALs
// were ordering defects whose damage is only visible under a stall; this seam
// makes the ordering itself directly observable on a healthy filesystem, so the
// guard does not depend on a stall to have teeth.
func recordEnumeration(t *testing.T) func() (time.Time, int) {
	t.Helper()
	var mu sync.Mutex
	var at time.Time
	calls := 0
	orig := listArtworkSubdirFiles
	listArtworkSubdirFiles = func(ctx context.Context, artistDir, subdirName string) ([]string, error) {
		mu.Lock()
		calls++
		if at.IsZero() {
			at = time.Now()
		}
		mu.Unlock()
		return orig(ctx, artistDir, subdirName)
	}
	t.Cleanup(func() { listArtworkSubdirFiles = orig })
	return func() (time.Time, int) {
		mu.Lock()
		defer mu.Unlock()
		return at, calls
	}
}

// readFanart returns the current bytes of the artist's top-level fanart.jpg.
func readFanart(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading fanart.jpg: %v", err)
	}
	return b
}

// operatorCropBytes is what the simulated operator save writes over the
// top-level fanart during the push. Distinct from primaryFanartBytes, so an
// assertion identifies WHICH write won rather than merely that the file exists.
var operatorCropBytes = []byte("OPERATOR-NEW-CROP-SAVED-DURING-THE-PUSH")

// PROPERTY 1 -- THE ADVISORY MUST RUN AFTER THE UPLOAD LOOP.
//
// TestSyncAllFanart_AdvisoryNeverBurnsTheCallersUploadBudget is round 1's
// CRITICAL, made into a test. Every production caller wraps this push in a
// shared budget and threads the same ctx to the peer HTTP client. If the
// advisory's directory read runs BEFORE the uploads -- as a plain statement at
// the position where the defer is registered -- then a wedged extrafanart/
// spends that whole budget before a single byte is pushed, and every upload
// below runs against a dead context.
//
// The caller's budget here (1s) is deliberately SHORTER than
// extrafanartCheckBudget (2s), which is what makes the two positions
// distinguishable: a stalled advisory ahead of the uploads outlives the
// caller's whole budget, while the same stall behind them costs the uploads
// nothing because they have already happened.
//
// MUTATION IT REDDENS: replacing the `defer func() { ... }()` registration with
// a plain `warnings = append(warnings, p.extrafanartExposureWarning(...)...)`
// statement at that position. Measured: ok=0 failed=1.
func TestSyncAllFanart_AdvisoryNeverBurnsTheCallersUploadBudget(t *testing.T) {
	up := &probeUploader{}
	p, a, dir := singlePeerHarness(t, up, silentLogger())
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 3)

	calls := stallEnumeration(t)

	// A budget shorter than the advisory's own, so a stall placed ahead of the
	// uploads certainly outlives it and a stall placed behind them certainly
	// does not touch them.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	warnings := p.SyncAllFanartToPlatforms(ctx, a)

	// PRECONDITION: the advisory really did run and really did stall. Without
	// this the test passes vacuously whenever the check is never reached at
	// all, which is a different (and separately guarded) defect.
	if got, _ := calls(), 0; got != 1 {
		t.Fatalf("precondition failed: the advisory enumeration was entered %d time(s), want exactly 1; "+
			"this test measures what a STALLED advisory costs the uploads and cannot do so if none ran", got)
	}
	if msg := findExtrafanartWarning(warnings); !strings.HasPrefix(msg, "the extrafanart/ check failed to read the folder:") {
		t.Fatalf("precondition failed: the stalled advisory did not report a read failure; got %v", warnings)
	}

	// THE PROPERTY: the uploads are untouched by the stall.
	ok, failed := up.counts()
	if ok != 1 || failed != 0 {
		t.Fatalf("the push uploaded ok=%d failed=%d, want ok=1 failed=0. A stalled extrafanart/ check "+
			"consumed the caller's upload budget, which means the advisory ran BEFORE the upload loop "+
			"instead of from its defer. #3177 round 1 measured exactly this and rated it CRITICAL: the "+
			"operator is told the push proceeded while every upload failed against a dead context. "+
			"warnings=%v", ok, failed, warnings)
	}
}

// PROPERTY 2 -- THE ADVISORY MUST RUN AFTER repairAfterPush's DEFER (LIFO).
//
// TestSyncAllFanart_AdvisoryRunsAfterTheDeferredRepair observes the ordering
// DIRECTLY, on a healthy filesystem, rather than inferring it from damage.
// repairAfterPush runs pass 1, sleeps reassertSettleDelay, then runs pass 2 --
// entirely inside its own defer. Because defers run LIFO and the advisory's
// defer is registered FIRST, the advisory cannot be entered until that whole
// sequence has finished, so the interval between the last upload returning and
// the advisory enumeration starting is at least reassertSettleDelay.
//
// This is the cheap, fast, unambiguous form of the guard; the destroyed-crop
// consequence is the sibling test below. It reddens on BOTH ordering
// mutations:
//   - defers swapped (advisory runs FIRST): interval ~0, far below the settle.
//   - defer -> plain statement: the enumeration is entered BEFORE any upload,
//     so the interval is negative.
//
// It asserts nothing about an upper bound: how long the repair takes is not
// this test's business, only that the advisory is behind it.
func TestSyncAllFanart_AdvisoryRunsAfterTheDeferredRepair(t *testing.T) {
	up := &probeUploader{}
	p, a, dir := singlePeerHarness(t, up, silentLogger())
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 2)

	enum := recordEnumeration(t)

	p.SyncAllFanartToPlatforms(context.Background(), a)

	enumAt, calls := enum()
	if calls != 1 {
		t.Fatalf("precondition failed: the advisory enumeration was entered %d time(s), want exactly 1", calls)
	}
	uploadAt := up.lastUploadAt()
	if uploadAt.IsZero() {
		t.Fatal("precondition failed: no upload was recorded, so there is no upload for the advisory to run after")
	}

	gap := enumAt.Sub(uploadAt)
	if gap < reassertSettleDelay {
		t.Fatalf("the advisory enumeration started %v after the last upload returned, want at least %v "+
			"(repairAfterPush's own settle window). The advisory must run AFTER the deferred "+
			"repairAfterPush, which it does only because its defer is registered ABOVE that one and "+
			"defers run LIFO. A gap below the settle window means either the two defer registrations "+
			"were swapped (#3177 round 2's CRITICAL: repair pass 1 is delayed and reverts an "+
			"operator's saved crop) or the advisory is no longer deferred at all (round 1's CRITICAL). "+
			"Defer registration ORDER is the mechanism here; it looks like formatting and is not.",
			gap, reassertSettleDelay)
	}
}

// PROPERTY 3 -- THE OPERATOR-CROP REPAIR WINDOW.
//
// TestSyncAllFanart_OperatorCropSurvives_AdvisoryDoesNotDelayRepairPass1 is
// round 2's CRITICAL as a behavioral test: it watches the top-level fanart file
// across the whole call and asserts the operator's bytes survive.
//
// THE SCENARIO, which is the one #2712 and repairScope are built around. The
// peer behaves perfectly. The operator saves a new crop of the same slot 100ms
// after the upload returns -- inside the window repairScope's doc comment says
// pass 1 owns and pass 2 must not touch. Pass 1's overwrite branch is
// DELIBERATELY UNGATED, justified solely by pass 1 running "the instant
// UploadImage returns". If anything is inserted between the last upload and
// that repair, pass 1 no longer runs in that instant: it sees the operator's
// crop, decides a peer rewrote the file, and writes the PRE-PUSH bytes back
// over it, logging a peer as the culprit.
//
// TWO ARMS, differing only in whether the advisory enumeration stalls, because
// the property must hold on a healthy filesystem AND on a wedged one:
//   - healthy: the advisory costs microseconds; the crop survives.
//   - stalled: the advisory costs its whole budget; the crop must STILL
//     survive, which it does only because the repair already ran.
//
// MUTATION IT REDDENS: swapping the two defer registrations. Measured: the
// stalled arm finds primaryFanartBytes on disk -- the operator's save silently
// destroyed. The healthy arm stays green under that mutation and is the
// CONTROL: it is what proves the stalled arm's failure is the ordering and not
// the probe.
func TestSyncAllFanart_OperatorCropSurvives_AdvisoryDoesNotDelayRepairPass1(t *testing.T) {
	// The operator's save lands comfortably inside repairAfterPush's settle
	// window (reassertSettleDelay, 250ms) and comfortably after pass 1 would
	// have run in the correct ordering, so neither arm is a photo finish.
	const operatorSaveDelay = 100 * time.Millisecond

	for _, tc := range []struct {
		name    string
		stalled bool
	}{
		{name: "healthy_filesystem", stalled: false},
		{name: "stalled_advisory_check", stalled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wg sync.WaitGroup
			up := &probeUploader{}
			var p *Publisher
			var dir string

			// The operator save runs on its own goroutine so it lands DURING the
			// push rather than after it -- a sequential write would prove
			// nothing, since the repair would already have finished.
			up.onUpload = func() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					time.Sleep(operatorSaveDelay)
					if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), operatorCropBytes, 0o600); err != nil {
						t.Errorf("simulating the operator's crop save: %v", err)
					}
				}()
			}

			pp, art, d := singlePeerHarness(t, up, silentLogger())
			p, dir = pp, d
			seedPrimaryFanart(t, dir)
			seedExtrafanart(t, dir, 3)

			// PRECONDITION: the file the probe is about to watch really holds
			// the pre-push bytes, so "the crop survived" cannot be satisfied by
			// a fixture that never held anything else.
			if got := readFanart(t, dir); string(got) != string(primaryFanartBytes) {
				t.Fatalf("precondition failed: fanart.jpg holds %q before the push, want the seeded primary %q",
					got, primaryFanartBytes)
			}

			var calls func() int
			if tc.stalled {
				calls = stallEnumeration(t)
			} else {
				enum := recordEnumeration(t)
				calls = func() int { _, n := enum(); return n }
			}

			p.SyncAllFanartToPlatforms(context.Background(), art)
			wg.Wait() // the operator's save is part of the scenario, not of the teardown

			if ok, failed := up.counts(); ok != 1 || failed != 0 {
				t.Fatalf("precondition failed: the push uploaded ok=%d failed=%d, want ok=1 failed=0; "+
					"with no successful upload nothing registers the repair and this proves nothing", ok, failed)
			}
			if got := calls(); got != 1 {
				t.Fatalf("precondition failed: the advisory enumeration was entered %d time(s), want exactly 1", got)
			}

			// THE PROPERTY: the operator's bytes are what is on disk.
			got := readFanart(t, dir)
			if string(got) == string(primaryFanartBytes) {
				t.Fatalf("fanart.jpg holds the PRE-PUSH bytes after the push: the operator's crop, saved %v "+
					"after the upload returned, was reverted. repairAfterPush's pass 1 repairs an overwrite "+
					"UNCONDITIONALLY, on the stated premise that it runs the instant UploadImage returns; "+
					"anything that delays it past an operator's save turns that repair into data loss and "+
					"blames a platform peer in the log. The #3177 advisory must therefore run AFTER the "+
					"repair's defer -- which it does only while its own defer is registered ABOVE it "+
					"(defers run LIFO). #3177 round 2 rated this CRITICAL.", operatorSaveDelay)
			}
			if string(got) != string(operatorCropBytes) {
				t.Fatalf("fanart.jpg holds %q after the push, want the operator's crop %q", got, operatorCropBytes)
			}
		})
	}
}

// PROPERTY 4 -- THE ADVISORY'S BUDGET MUST BE DETACHED FROM THE CALLER'S.
//
// TestSyncAllFanart_AdvisoryBudgetIsDetachedFromTheCaller pins the
// context.WithoutCancel in extrafanartExposureWarning. The advisory runs from a
// defer that fires after the uploads AND after the deferred repair, so by then
// the caller's context may well be spent -- a slow-but-successful push, or a
// canceled request whose repair still has work to do. Deriving the advisory's
// budget from ctx instead means runCancellable short-circuits on the dead
// context and the operator silently loses the notice, on a perfectly healthy
// local filesystem, precisely on the pushes most likely to be destructive.
//
// The caller's context is canceled DETERMINISTICALLY, from inside the uploader
// immediately after a successful upload, rather than by racing a timeout: the
// upload is proven to have happened while the context was alive (ok=1
// failed=0 on a ctx-honoring uploader) and the context is proven dead before
// the defers run.
//
// MUTATION IT REDDENS: context.WithoutCancel(ctx) -> ctx. Measured: the
// exposure notice is replaced by the enumeration-failure message.
func TestSyncAllFanart_AdvisoryBudgetIsDetachedFromTheCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := &probeUploader{onUpload: cancel}
	p, a, dir := singlePeerHarness(t, up, silentLogger())
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 3)

	warnings := p.SyncAllFanartToPlatforms(ctx, a)

	// PRECONDITIONS: the upload really succeeded (so the advisory's uploadedTo
	// gate is open), and the caller's context really is dead by now (so the
	// detachment is genuinely under test rather than trivially satisfied).
	if ok, failed := up.counts(); ok != 1 || failed != 0 {
		t.Fatalf("precondition failed: the push uploaded ok=%d failed=%d, want ok=1 failed=0", ok, failed)
	}
	if ctx.Err() == nil {
		t.Fatal("precondition failed: the caller's context is still alive, so this test is not exercising " +
			"the detachment it claims to")
	}

	msg := findExtrafanartWarning(warnings)
	if !strings.Contains(msg, "holds 3 file(s)") {
		t.Fatalf("the advisory did not report the 3-file exposure on a push whose caller context was "+
			"canceled after a SUCCESSFUL upload; got %q (all warnings: %v). The advisory's budget must be "+
			"detached with context.WithoutCancel: it runs from a defer that fires after the uploads and "+
			"after the deferred repair, so a spent caller context is the normal case there, and deriving "+
			"the budget from it loses the notice on a healthy filesystem -- exactly on the slow or "+
			"canceled pushes most likely to have been destructive.", msg, warnings)
	}
}

// PROPERTIES 5 AND 6 -- THE BUDGET MUST EXIST, AND ITS VALUE IS WHAT BOUNDS THE
// STALL.
//
// TestSyncAllFanart_AdvisoryStallIsBoundedByItsOwnBudget stalls the enumeration
// against a caller with NO deadline at all, so the only thing that can end the
// read is the advisory's own WithTimeout. It asserts the stall costs
// approximately extrafanartCheckBudget and not more:
//
//   - LOWER bound (property 6): the elapsed time is measured against a
//     HARD-CODED 1.5s floor rather than against extrafanartCheckBudget itself.
//     Asserting against the constant would move with any mutation of it and
//     guard nothing; the literal is what states that shrinking the budget is a
//     behavior change someone must come here and defend.
//   - UPPER bound (property 5): a HARD-CODED 8s ceiling, far above the 2s
//     budget and far below stallEnumeration's own 15s backstop, so removing the
//     WithTimeout entirely (`checkCtx := ctx`) reddens instead of merely making
//     the suite slow.
//
// The push itself must be unaffected either way: a stalled ADVISORY is not a
// failed push.
func TestSyncAllFanart_AdvisoryStallIsBoundedByItsOwnBudget(t *testing.T) {
	const (
		stallFloor   = 1500 * time.Millisecond
		stallCeiling = 8 * time.Second
	)

	p, a, dir, up := budgetHarness(t)
	seedPrimaryFanart(t, dir)
	seedExtrafanart(t, dir, 2)

	calls := stallEnumeration(t)

	// No deadline: the advisory's own budget is the ONLY thing that can end
	// the stalled read.
	start := time.Now()
	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)
	elapsed := time.Since(start)

	if got := calls(); got != 1 {
		t.Fatalf("precondition failed: the advisory enumeration was entered %d time(s), want exactly 1", got)
	}
	if ok, failed := up.counts(); ok != 1 || failed != 0 {
		t.Fatalf("a stalled advisory check must not affect the push: ok=%d failed=%d, want ok=1 failed=0: %v",
			ok, failed, warnings)
	}
	if msg := findExtrafanartWarning(warnings); !strings.HasPrefix(msg, "the extrafanart/ check failed to read the folder:") {
		t.Fatalf("a stalled advisory must report the read failure to the operator; got %v", warnings)
	}

	if elapsed > stallCeiling {
		t.Fatalf("a stalled advisory enumeration took %v, over the %v ceiling. The advisory must derive its "+
			"own short deadline (context.WithTimeout, extrafanartCheckBudget); without one an advisory "+
			"read of a wedged mount stalls without bound and stretches the push's response by however "+
			"long the kernel takes to answer.", elapsed, stallCeiling)
	}
	if elapsed+advisoryStallTolerance < stallFloor {
		t.Fatalf("a stalled advisory enumeration took only %v, under the %v floor. The %v budget is a "+
			"CEILING on how long a wedged extrafanart/ may stretch the response, not a latency target -- "+
			"the check is one local directory listing and costs microseconds on a healthy filesystem, so "+
			"nothing is bought by shrinking it, while shrinking it makes a merely slow mount report a "+
			"read failure the operator cannot act on. Changing extrafanartCheckBudget is a deliberate "+
			"act; update this floor with it and say why.", elapsed, stallFloor, extrafanartCheckBudget)
	}
}

// TestExtrafanartCheckBudget_IsPinned states, as an executable sentence, what
// the advisory's budget is and what it is for. The behavioral test above bounds
// the stall with hard-coded literals; this one names the constant, so a change
// to it reddens with the rationale attached rather than only as an elapsed-time
// mismatch in a slower test.
func TestExtrafanartCheckBudget_IsPinned(t *testing.T) {
	if extrafanartCheckBudget != 2*time.Second {
		t.Fatalf("extrafanartCheckBudget = %v, want 2s. This is a CEILING on how long a wedged "+
			"extrafanart/ may stretch a push's response, not a performance target: the check is a single "+
			"local directory listing that completes in microseconds on a healthy filesystem, and it runs "+
			"from a defer AFTER the uploads and AFTER the deferred repair, so the only thing this number "+
			"bounds is how long an operator waits for a response that has already done all its work. "+
			"Raising it lengthens that tail; lowering it makes a slow-but-working mount report a read "+
			"failure instead of the exposure notice. Either is a decision, not a tweak.", extrafanartCheckBudget)
	}
}

// TestMaxAdvisoryStalledReads_IsBelowTheProcessCap keeps the advisory's
// courtesy ceiling meaningfully below internal/image's process-wide cap. It is
// not one of the ordering properties, but it is the same shape of unpinned
// constant: the merged suite drives the gate with the constant itself, so the
// relationship the constant exists to express -- "far below the real cap" -- is
// asserted nowhere.
func TestMaxAdvisoryStalledReads_IsBelowTheProcessCap(t *testing.T) {
	// internal/image's maxStalledReads is 16 and is not exported; the value is
	// restated here rather than imported, and this test is the place that
	// notices if the two drift apart, since the whole point of the advisory
	// ceiling is to sit well under it.
	const imageMaxStalledReads = 16
	if maxAdvisoryStalledReads >= imageMaxStalledReads/2 {
		t.Fatalf("maxAdvisoryStalledReads = %d, want well below internal/image's process-wide cap of %d "+
			"(at most half). An ADVISORY notice must never be able to consume the abandoned-read budget "+
			"that the paths actually moving bytes depend on: past the process cap EVERY read anywhere in "+
			"the process is refused, healthy files on other filesystems included.",
			maxAdvisoryStalledReads, imageMaxStalledReads)
	}
}
