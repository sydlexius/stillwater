package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/rule"
)

// This file covers #2931: a bulk-action pass that wedges must end itself.
//
// The defect was not "the per-artist work is slow" but "the goroutine never
// returns, so the progress record stays in the running state, so four
// endpoints -- bulk actions plus the three handlers sharing
// backdropRepairRunning -- answer 409 for the life of the process".
//
// The wedge is INJECTED at the pipeline seam rather than reproduced with a
// real stalled filesystem read, for the same reason the #2689 tests inject it
// (see handlers_remediate_stall_test.go): what this layer owns is the contract
// that a pass which blocks until its context ends still reaches its epilogue
// and still frees the gate. Injecting keeps the test hermetic and lets it
// assert exactly that.

// withBulkActionWorkDeadline shortens the bulk-action work deadline for the
// duration of one test and restores it after.
//
// This seam is what makes the tests below non-vacuous, which is worth stating
// because the obvious alternative silently is not. Driving the loop to a stop
// through the CANCEL endpoint would finish just as fast and assert nothing
// about this bound: that path cancels the same ctx and passes identically with
// bulkActionWorkDeadline deleted. Shortening the goroutine's OWN deadline is
// the only way to exercise the thing #2931 added.
func withBulkActionWorkDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	original := bulkActionWorkDeadline
	bulkActionWorkDeadline = d
	t.Cleanup(func() { bulkActionWorkDeadline = original })
}

// TestFinalBulkStatus is the terminal-state decision in isolation. The three
// outcomes are genuinely distinct to an operator -- "it finished", "you
// stopped it", "it ran out of time" -- and conflating the last two is the
// silent-failure the issue calls out.
func TestFinalBulkStatus(t *testing.T) {
	t.Parallel()
	// wrapped exercises the errors.Is path rather than an == comparison: the
	// run context's error can reach the epilogue wrapped by an intermediate
	// layer, and a == check would silently degrade such a run to "canceled".
	wrapped := errors.Join(errors.New("provider fetch aborted"), context.DeadlineExceeded)

	tests := []struct {
		name        string
		runErr      error
		interrupted bool
		want        bulkActionStatus
	}{
		{"clean run", nil, false, bulkActionCompleted},
		{"operator cancel mid-run", context.Canceled, true, bulkActionCanceled},
		{"deadline mid-run", context.DeadlineExceeded, true, bulkActionTimedOut},
		{"wrapped deadline mid-run", wrapped, true, bulkActionTimedOut},
		// The race the pre-existing code already handled and which must
		// survive: a cancel POST or the deadline landing after the last
		// artist finished cut no work short, so the run completed.
		{"cancel after last item", context.Canceled, false, bulkActionCompleted},
		{"deadline after last item", context.DeadlineExceeded, false, bulkActionCompleted},
		// Defensive: interrupted without a context error cannot happen (the
		// flag is only set under ctx.Err() != nil), but the helper must not
		// invent a terminal failure from the flag alone.
		{"interrupted with no error", nil, true, bulkActionCompleted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := finalBulkStatus(tc.runErr, tc.interrupted); got != tc.want {
				t.Errorf("finalBulkStatus(%v, %t) = %q, want %q",
					tc.runErr, tc.interrupted, got, tc.want)
			}
		})
	}
}

// TestBulkPillStatus pins the pill mapping, including the precedence question
// the run-level states raise: a timed-out pass whose processed artists also
// had failures must still report timed_out, because running out of time is
// why the run stopped and is the more actionable fact.
func TestBulkPillStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		final  bulkActionStatus
		failed int
		want   string
	}{
		{"clean completion", bulkActionCompleted, 0, "completed"},
		{"completion with failures", bulkActionCompleted, 3, "failed"},
		{"cancel", bulkActionCanceled, 0, "canceled"},
		{"timeout", bulkActionTimedOut, 0, "timed_out"},
		{"timeout outranks failures", bulkActionTimedOut, 3, "timed_out"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bulkPillStatus(tc.final, tc.failed); got != tc.want {
				t.Errorf("bulkPillStatus(%q, %d) = %q, want %q", tc.final, tc.failed, got, tc.want)
			}
		})
	}
}

// wedgedBulkRouter builds a router whose rule pipeline blocks inside
// RunForArtist until its context ends -- a bulk pass wedged on a stalled read.
// The pipeline is fanart-capable so the same router can also exercise the
// backdrop-remediation endpoint that shares the singleton.
func wedgedBulkRouter(t *testing.T) (*Router, *artist.Artist) {
	t.Helper()
	wedged := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{
			runForArtistFn: func(ctx context.Context, _ *artist.Artist) (*rule.RunResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	}
	r := testRouterWithFanartPipeline(t, wedged)
	a := addTestArtist(t, r.artistService, "Wedged Bulk Artist")
	return r, a
}

// startWedgedBulkAction posts a run_rules bulk action over one artist and
// asserts the handler accepted it, so the caller is measuring a pass that
// really started rather than one that never claimed the slot.
func startWedgedBulkAction(t *testing.T, r *Router, a *artist.Artist) {
	t.Helper()
	payload := `{"action":"run_rules","ids":["` + a.ID + `"]}`
	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("bulk action start = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	// Precondition. Without this the test could pass against a pass that
	// never entered the running state at all, which is not the scenario
	// under test: the defect is specifically about a run that IS running
	// and never stops.
	waitBulkActionStatus(t, r, bulkActionRunning)
}

// TestBulkAction_WedgedPassTimesOutAndUnblocksEndpoints is the regression for
// the permanent-409. A pass that blocks until its context ends must reach a
// terminal state on its own, and both the bulk endpoint and the remediation
// endpoints that share the singleton must be usable afterwards.
//
// Before the fix, ctx had no deadline, RunForArtist never returned, the
// epilogue never ran, Status stayed "running", and every assertion below
// failed by timing out.
func TestBulkAction_WedgedPassTimesOutAndUnblocksEndpoints(t *testing.T) {
	// Deliberately NOT t.Parallel(): withBulkActionWorkDeadline writes a
	// package-level var, so a parallel test would be a data race AND could
	// hand an unrelated bulk action a 200ms deadline.

	r, a := wedgedBulkRouter(t)
	// Shorten the goroutine's OWN deadline. No request context is canceled
	// here on purpose: that is what a real wedge looks like once the 202 has
	// already been returned and nobody is watching, and it forces this bound
	// to be the thing that ends the run.
	withBulkActionWorkDeadline(t, 200*time.Millisecond)

	startWedgedBulkAction(t, r, a)

	// The pass must end itself. waitBulkActionStatus fails the test if the
	// status never arrives, which is exactly the bug: an unbounded pass sits
	// in "running" forever.
	waitBulkActionStatus(t, r, bulkActionTimedOut)

	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if progress == nil {
		t.Fatal("progress snapshot is nil after the deadline")
	}
	snap := progress.snapshot()
	// The operator-facing half of the issue: the record must SAY it timed
	// out. A run that flipped to idle or reported "completed" would leave
	// someone returning to it believing the work was done.
	if snap["status"] != "timed_out" {
		t.Errorf("status = %v, want timed_out", snap["status"])
	}
	// The wedged artist's work was aborted by the deadline, so it lands in
	// the failed column rather than succeeding. Asserting it pins the thing
	// that makes the status non-trivial: processed reaches total here, so a
	// terminal-status rule based on remaining COUNTS would have reported this
	// run as a clean completion.
	if snap["succeeded"] != 0 {
		t.Errorf("succeeded = %v, want 0; the wedged artist never completed its work", snap["succeeded"])
	}
	if snap["failed"] != 1 {
		t.Errorf("failed = %v, want 1", snap["failed"])
	}

	// The property the operator actually has: can I run this again. A status
	// read alone would pass against a fix that recorded the state but left
	// the gate claimed.
	second := httptest.NewRecorder()
	secondPayload := `{"action":"lock","ids":["` + a.ID + `"]}`
	secondReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/artists/bulk-actions", strings.NewReader(secondPayload))
	secondReq.Header.Set("Content-Type", "application/json")
	r.handleBulkAction(second, secondReq)
	if second.Code == http.StatusConflict {
		t.Fatal("a later bulk action returned 409 after the wedged run timed out; " +
			"this is the permanent-409 the issue reports")
	}
	if second.Code != http.StatusAccepted {
		t.Fatalf("a later bulk action returned %d, want 202; body: %s", second.Code, second.Body.String())
	}
	waitBulkActionCompleted(t, r)
}

// TestBulkAction_WedgedPassFreesSharedRemediationSingleton covers the other
// three endpoints named in the issue. bulkActionProgress.Status == running is
// read by claimPHashRepairSlot and by the backdrop-remediation gate, so a
// wedged bulk pass 409s handlers it never touches. A test asserting only that
// the bulk endpoint recovered would leave that blast radius unguarded.
func TestBulkAction_WedgedPassFreesSharedRemediationSingleton(t *testing.T) {
	// Not parallel, for the reason given on the test above.

	r, a := wedgedBulkRouter(t)
	withBulkActionWorkDeadline(t, 200*time.Millisecond)

	startWedgedBulkAction(t, r, a)

	// Precondition, and the blast radius itself: while the bulk pass is
	// running the remediation endpoint is legitimately 409. If this did not
	// hold, the recovery assertion below would prove nothing -- the endpoint
	// would have been reachable all along.
	blocked := httptest.NewRecorder()
	blockedReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/backdrop-duplicates/remediate", nil)
	r.handleBackdropDuplicatesRemediate(blocked, blockedReq)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("remediation during a running bulk pass = %d, want 409; "+
			"the shared-singleton gate is not wired as this test assumes", blocked.Code)
	}

	waitBulkActionStatus(t, r, bulkActionTimedOut)

	after := httptest.NewRecorder()
	afterReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/backdrop-duplicates/remediate", nil)
	r.handleBackdropDuplicatesRemediate(after, afterReq)
	if after.Code == http.StatusConflict {
		t.Fatal("remediation still 409s after the wedged bulk pass timed out; " +
			"one wedged bulk action is still locking the endpoints it shares a singleton with")
	}
	if after.Code != http.StatusOK {
		t.Fatalf("remediation after the timeout = %d, want 200; body: %s", after.Code, after.Body.String())
	}
}

// TestBulkAction_CancelStillReportsCanceledNotTimedOut guards the boundary the
// new status introduces. An operator cancel and a deadline both end the same
// context; if the epilogue told them apart by "ctx.Err() != nil" alone, every
// cancel would be relabelled a timeout and the operator would be told the
// server gave up on work they themselves stopped.
func TestBulkAction_CancelStillReportsCanceledNotTimedOut(t *testing.T) {
	// Not parallel: writes the package-level deadline var.

	r, a := wedgedBulkRouter(t)
	// A long deadline so the deadline provably cannot be what ends this run.
	withBulkActionWorkDeadline(t, time.Hour)

	startWedgedBulkAction(t, r, a)

	cancelW := httptest.NewRecorder()
	cancelReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/artists/bulk-actions/cancel", nil)
	r.handleBulkActionCancel(cancelW, cancelReq)
	if cancelW.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200; body: %s", cancelW.Code, cancelW.Body.String())
	}

	waitBulkActionStatus(t, r, bulkActionCanceled)

	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if progress == nil {
		t.Fatal("progress snapshot is nil after the cancel")
	}
	if snap := progress.snapshot(); snap["status"] != "canceled" {
		t.Errorf("status = %v, want canceled; a user-initiated stop must not read as a timeout", snap["status"])
	}
}

// TestBulkAction_TimedOutCompletionEventTitlesTheToast pins the down-cast that
// runBulkAction applies to the completion event's status field.
//
// The SSE hub reads that field with strVal, whose `v.(string)` assertion FAILS
// for a NAMED string type -- and bulkActionStatus is exactly that. Publishing
// the enum value directly yields an empty toast title: not a degraded message,
// no message at all. On a timed-out run that is the fact an operator most
// needs, which is why the producer converts with string() before publishing.
//
// The assertion runs against the event runBulkAction really emits, not against
// a hand-built map. That distinction is the whole point: a test that called
// buildBulkCompletedMsg with its own literal would pass with the down-cast
// deleted, because the mutation lives in the PRODUCER. Feeding the recorded
// event through the same consumer the SSE hub uses is what closes that gap.
func TestBulkAction_TimedOutCompletionEventTitlesTheToast(t *testing.T) {
	// Not t.Parallel(): withBulkActionWorkDeadline writes a package-level var.
	r, a := wedgedBulkRouter(t)
	withBulkActionWorkDeadline(t, 200*time.Millisecond)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 1024)
	rec := &opEventRecorder{}
	bus.Subscribe(event.BulkCompleted, rec.handle)
	go bus.Start()
	r.eventBus = bus
	defer func() {
		bus.Stop()
		time.Sleep(20 * time.Millisecond)
	}()

	startWedgedBulkAction(t, r, a)
	waitBulkActionStatus(t, r, bulkActionTimedOut)

	evts := rec.waitUntil(t, func(e []event.Event) bool { return len(e) > 0 },
		"a BulkCompleted event")
	data := evts[len(evts)-1].Data

	// Precondition: this really is the timed-out completion. Without it the
	// assertion below could pass on some other terminal event.
	// %#v is deliberate over %v: the failure this guards against is a TYPE
	// difference, and bulkActionStatus("timed_out") prints identically to
	// "timed_out" under %v, so a plain verb would render the two sides of
	// this comparison indistinguishable in the failure output.
	if got := data["status"]; got != string(bulkActionTimedOut) {
		t.Fatalf("completion event status = %#v (%T), want %#v (string); a named "+
			"string type here is the defect -- strVal's v.(string) assertion "+
			"rejects it and the toast loses its title",
			got, got, string(bulkActionTimedOut))
	}

	// The payoff: the hub's own message builder must produce a real title.
	// With the down-cast removed this returns "", and the operator sees a
	// toast that says nothing about a run that ran out of time.
	if got, want := buildBulkCompletedMsg(data), "Bulk run_rules timed_out"; got != want {
		t.Errorf("buildBulkCompletedMsg = %q, want %q; a named-type status makes "+
			"strVal return \"\" and the toast lose its title entirely", got, want)
	}
}
