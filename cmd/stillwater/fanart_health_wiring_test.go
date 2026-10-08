package main

import (
	"go/ast"
	"go/types"
	"testing"
)

// TestMainWiresTheRuleServiceAsTheFanartHealthReporter is the #3200 wiring
// guard: the publisher raises the "backdrop file cannot be read" finding only
// through the reporter wired here, and an unwired publisher says nothing (the
// reporter is optional by design). Deleting the SetFanartHealthReporter line
// keeps every other test green, so the call is pinned statically, the same way
// as the other startup wiring guards in this package. The argument must be the
// rule service, not some other reporter.
//
// Mutation-proof: deleting `a.publisher.SetFanartHealthReporter(a.ruleService)`
// from main.go makes this test FAIL.
func TestMainWiresTheRuleServiceAsTheFanartHealthReporter(t *testing.T) {
	file, _ := parseMainGo(t)
	var calls, withRuleService int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetFanartHealthReporter" {
			return true
		}
		calls++
		if len(call.Args) == 1 && types.ExprString(call.Args[0]) == "a.ruleService" {
			withRuleService++
		}
		return true
	})
	if calls != 1 || withRuleService != 1 {
		t.Fatalf("main.go has %d SetFanartHealthReporter call(s), %d of them passing a.ruleService; want exactly 1 and 1, "+
			"or unreadable backdrops would never be raised as findings", calls, withRuleService)
	}
}
