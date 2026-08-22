// Regression guard for #3118: the periodic duplicate-image count refresh
// (maintenance.Service.StartDuplicateImageCountRefresh) was fully built --
// cadence, deadline-bounded runs, its own test suite in
// internal/maintenance/dupimage_counts_test.go -- but startListeners never
// called it. dupimages.Cache.TriggerRefresh is deliberately best-effort: a
// refresh that establishes neither half writes nothing, per #2608, so as not
// to latch a never-successfully-scanned cache as authoritative-clean. That
// design relies on SOMETHING eventually retrying a failed refresh; with no
// production caller for the periodic path, a platform outage (or any other
// refresh failure) left the sidebar pill and the platform-backdrop-duplicates
// report stale until the process restarted.
//
// This mirrors mbid_sweep_wiring_test.go's approach and reuses its helpers
// (mainGoFile, parseMainGo, findFunc, callsSelector), for the same reason
// that test gives: proving startListeners actually calls in requires either
// booting the full HTTP listener or waiting out the real interval, neither of
// which a unit test should do. A static source-position check is the correct
// instrument here, not a behavioral gap.
package main

import "testing"

// TestStartListenersCallsStartDuplicateImageCountRefresh is the #3118
// regression test: it fails if the wiring this PR adds is ever deleted from
// startListeners, which is the exact defect found in the #3116 fix round --
// the periodic refresh existed and was fully tested in isolation, with
// nothing in cmd/stillwater ever launching it.
//
// Mutation-proof: deleting the
// `go a.maintenanceService.StartDuplicateImageCountRefresh(...)` line from
// startListeners in cmd/stillwater/main.go makes this test FAIL (verified:
// see the PR report for the observed failure and restore).
func TestStartListenersCallsStartDuplicateImageCountRefresh(t *testing.T) {
	file, _ := parseMainGo(t)
	fn := findFunc(file, "startListeners")
	if fn == nil {
		t.Fatal("could not find func startListeners in main.go -- has it been renamed?")
	}
	if !callsSelector(fn.Body, "StartDuplicateImageCountRefresh") {
		t.Fatal("startListeners no longer calls StartDuplicateImageCountRefresh -- a refresh that " +
			"fails once (platform unreachable, disk unavailable) would leave the sidebar duplicate-image " +
			"pill and the platform-backdrop-duplicates report stale forever, with nothing left to retry it (#3118)")
	}
}
