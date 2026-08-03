package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// This file covers the actual defect in #2689: not "the read is slow" but "the
// handler never returns, so its deferred release never runs, so the endpoint
// 409s for the life of the process".
//
// The stall is INJECTED at the pipeline seam rather than reproduced with a real
// FIFO. That is deliberate and is the stronger test here: the primitive's own
// FIFO repro lives in internal/image (readio_stall_test.go) where the blocking
// read actually is, while what this layer owns is the handler contract -- a
// pass that blocks until its context ends must release the singleton on the way
// out. Injecting the block keeps this test hermetic (no mkfifo, no unix build
// tag, no dependence on how the host schedules a wedged read) and lets it
// assert the ONE thing the handler is responsible for.

// withRemediationWorkTimeout shortens the handlers' shared work deadline for
// the duration of one test and restores it after.
//
// This seam is what makes these tests non-vacuous, and that is worth stating
// plainly because the obvious alternative silently is not. Canceling the
// REQUEST context instead would finish just as fast and assert nothing: the
// handler passes req.Context() down regardless, so such a test passes
// identically with the work deadline DELETED -- it would measure the request
// context, never the bound under test. Shortening the handler's OWN timeout is
// the only way to exercise the thing #2689 added.
func withRemediationWorkTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := remediationWorkTimeout
	remediationWorkTimeout = d
	t.Cleanup(func() { remediationWorkTimeout = original })
}

// blockUntilCtxDone stands in for a remediation pass wedged inside a stalled
// filesystem read: it returns only when its context ends. Before #2689 the
// context never ended, because nothing bounded the work -- the write deadline
// bounds the RESPONSE, and the hang starts before any write is attempted.
func blockUntilCtxDone(ctx context.Context) (rule.FanartRepairResult, error) {
	<-ctx.Done()
	return rule.FanartRepairResult{}, ctx.Err()
}

// TestBackdropDuplicatesRemediate_StalledPassReleasesSingleton is the
// regression for the permanent-409. A pass that blocks until its context ends
// must still leave the slot free, and the endpoint must still be usable.
//
// The second POST is the assertion that matters. Reading the flag alone would
// pass against a fix that cleared it but left the endpoint broken some other
// way; issuing a real follow-up request tests the property the operator
// actually has -- "can I run this again" -- rather than its proxy.
func TestBackdropDuplicatesRemediate_StalledPassReleasesSingleton(t *testing.T) {
	// Deliberately NOT t.Parallel(): withRemediationWorkTimeout writes a
	// package-level var, so running these alongside another test in this
	// package would be a data race AND could hand an unrelated handler a
	// 150ms deadline.

	stalled := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn:  blockUntilCtxDone,
	}
	r := testRouterWithFanartPipeline(t, stalled)

	// Shorten the HANDLER's own deadline. The request context is left
	// uncancelled on purpose: it is what a real wedged operator request looks
	// like (the client is still waiting), and it forces the handler's own
	// bound to be the thing that ends the run.
	withRemediationWorkTimeout(t, 150*time.Millisecond)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		r.handleBackdropDuplicatesRemediate(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Not a flake: this is the bug. The handler is wedged and its deferred
		// release cannot run.
		t.Fatal("the remediation handler never returned once its work deadline elapsed; " +
			"the singleton is pinned and every later POST will 409")
	}

	r.bulkActionMu.Lock()
	running := r.backdropRepairRunning
	r.bulkActionMu.Unlock()
	if running {
		t.Fatal("backdropRepairRunning is still claimed after a stalled pass; " +
			"the endpoint is now permanently 409")
	}

	// The operator-visible property: the endpoint still works. A second POST
	// must NOT get the 409 that says "a repair is already in progress" -- there
	// is no such repair.
	second := httptest.NewRecorder()
	secondReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/backdrop-duplicates/remediate", nil)
	// Swap in a pass that returns immediately so this request measures slot
	// availability rather than re-running the stall.
	stalled.remediateFn = func(context.Context) (rule.FanartRepairResult, error) {
		return rule.FanartRepairResult{}, nil
	}
	r.handleBackdropDuplicatesRemediate(second, secondReq)

	if second.Code == http.StatusConflict {
		t.Fatal("a later remediation POST returned 409 after the stalled run ended; " +
			"this is the permanent-409 the issue reports")
	}
	if second.Code != http.StatusOK {
		t.Fatalf("a later remediation POST returned %d, want 200", second.Code)
	}
}

// TestPHashRemediate_StalledPassReleasesSharedSingleton covers the SAME slot
// from its other claimant. backdropRepairRunning is claimed by three handlers
// (fanart-duplicate remediation, phash back-out, phash restore) and gated on by
// bulk actions, so a wedge in ANY of them 409s ALL of them. A test covering
// only the first claimant would leave the other two able to pin the shared slot
// with nothing noticing.
func TestPHashRemediate_StalledPassReleasesSharedSingleton(t *testing.T) {
	// Not parallel, for the reason given on the test above.

	r := newPHashRepairRouter(t, &pHashCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(ctx context.Context, _ rule.PHashMismatchScope, _ rule.PHashRemediateOpts) (rule.PHashRemediateResult, error) {
			<-ctx.Done()
			return rule.PHashRemediateResult{}, ctx.Err()
		},
	})

	withRemediationWorkTimeout(t, 150*time.Millisecond)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"all_artists":true}`))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		r.handlePHashMismatchRemediate(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the phash back-out handler never returned once its work deadline elapsed; " +
			"the SHARED destructive-fanart slot is pinned, which 409s the fanart-duplicate " +
			"remediation and every bulk action too")
	}

	r.bulkActionMu.Lock()
	running := r.backdropRepairRunning
	r.bulkActionMu.Unlock()
	if running {
		t.Fatal("backdropRepairRunning is still claimed after a stalled phash back-out")
	}
}
