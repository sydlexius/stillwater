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

import (
	"go/ast"
	"go/types"
	"testing"
)

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

// TestDupImageCacheWiringNamesOneInstance pins (#2936) that the router, the
// periodic refresh and the shutdown drain all name dupimages.Shared(). The
// router now owns its cache and builds a private one when RouterDeps.DupImageCache
// is nil, so deleting that one line leaves every api test green while the
// scheduler refreshes a sourceless Shared() and shutdown drains the wrong
// instance. Source-position check for the same reason as the test above.
func TestDupImageCacheWiringNamesOneInstance(t *testing.T) {
	file, _ := parseMainGo(t)
	const want = "dupimages.Shared()"

	var routerCache, schedulerCache, drainRecv string
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			if types.ExprString(v.Type) != "api.RouterDeps" {
				return true
			}
			for _, el := range v.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if ok && types.ExprString(kv.Key) == "DupImageCache" {
					routerCache = types.ExprString(kv.Value)
				}
			}
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "StartDuplicateImageCountRefresh":
				if len(v.Args) >= 2 {
					schedulerCache = types.ExprString(v.Args[1])
				}
			case "Drain":
				if recv := types.ExprString(sel.X); recv == want {
					drainRecv = recv
				}
			}
		}
		return true
	})

	if routerCache != want {
		t.Errorf("api.RouterDeps.DupImageCache = %q, want %q: the router would own a private cache the scheduler never refreshes and shutdown never drains", routerCache, want)
	}
	if schedulerCache != want {
		t.Errorf("StartDuplicateImageCountRefresh cache argument = %q, want %q", schedulerCache, want)
	}
	if drainRecv != want {
		t.Errorf("no %s.Drain(...) call found in main.go; shutdown would not drain the router's cache", want)
	}
}

// TestPlatformDupCacheWiring pins (#3138 S3b) the lines that connect the
// sweep's cache to the rule; each package's own tests wire their own.
func TestPlatformDupCacheWiring(t *testing.T) {
	file, _ := parseMainGo(t)
	calls := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			calls[types.ExprString(call.Fun)] = true
		}
		return true
	})
	for _, want := range []string{"a.ruleEngine.SetPlatformDupCache", "a.imageDupFixer.SetPlatformDupCache", "dupCache.SetFindingObserver", "a.artistService.MarkDirty"} {
		if !calls[want] {
			t.Errorf("main.go no longer calls %s", want)
		}
	}
	if !calls["time.Now().UTC().Add"] {
		t.Error("the finding observer no longer stamps the dirty mark one second ahead")
	}
}
