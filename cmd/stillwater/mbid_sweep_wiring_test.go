// Regression guard for the #2810 MBID re-validation sweep's wiring into
// application startup.
//
// Four prior PRs (#2997, #2998, #3000, #3001) built the ~2900-line
// internal/mbidcheck sweep with nothing calling it. This PR wires it in.
// startMBIDRevalidateSweep itself is now covered behaviorally (see
// TestStartMBIDRevalidateSweep_* below), but two invariants in it are
// fundamentally about ORDERING and PRESENCE of source-level statements, not
// about return values a black-box behavioral test can observe on a useful
// timescale:
//
//  1. startListeners must actually CALL startMBIDRevalidateSweep. A behavioral
//     test cannot cover this without booting the full HTTP listener (which
//     startListeners does, and blocks on), so the property is checked
//     statically: parse main.go, find startListeners, and require a call to
//     a.startMBIDRevalidateSweep in its body. This is the single most
//     important check in this file -- it is the exact regression class this
//     whole feature has had four times running.
//
//  2. mbidSweep.SetFlagger must be called BEFORE `go mbidSweep.Start(ctx)`,
//     per SetFlagger's own doc comment (a data race if reordered, reachable
//     only once a failed verdict reaches flag()). Forcing that race
//     deterministically in a behavioral test would require driving the sweep
//     all the way to its first failed verdict, which -- through
//     startMBIDRevalidateSweep's production Config (StartupDelay always
//     defaults to mbidcheck.DefaultStartupDelay, 5 minutes, since this call
//     site never overrides it) -- takes five real minutes. So this invariant
//     is checked the same way: statically, by source position.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api"
)

// mainGoFile locates cmd/stillwater/main.go relative to this test file, so
// the test works regardless of the working directory `go test` is invoked
// from.
func mainGoFile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	return filepath.Join(filepath.Dir(thisFile), "main.go")
}

// parseMainGo parses main.go and returns its AST plus the fset needed to
// resolve positions.
func parseMainGo(t *testing.T) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainGoFile(t), nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	return file, fset
}

// findFunc locates a top-level function declaration by name (method or
// plain function; receiver is ignored).
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// callsSelector reports whether node's subtree contains a call expression
// whose selector (the part after the dot) matches selectorName -- e.g.
// "startMBIDRevalidateSweep" matches both a.startMBIDRevalidateSweep(...)
// and any other receiver calling a method of that name. Deliberately
// name-based rather than fully type-resolved: this file has no need for
// golang.org/x/tools/go/packages type information, and a name match is
// exactly as strong a guard against "the call was deleted" as a
// type-resolved one would be.
func callsSelector(node ast.Node, selectorName string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == selectorName {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestStartListenersCallsStartMBIDRevalidateSweep is the single most
// important test in this PR: it fails if the wiring this PR adds is ever
// deleted from startListeners, which is exactly what happened silently
// across #2997/#2998/#3000/#3001 (each landed with nothing calling in).
//
// Mutation-proof: deleting `a.startMBIDRevalidateSweep(ctx, db, logger)`
// from startListeners in cmd/stillwater/main.go makes this test FAIL.
func TestStartListenersCallsStartMBIDRevalidateSweep(t *testing.T) {
	file, _ := parseMainGo(t)
	fn := findFunc(file, "startListeners")
	if fn == nil {
		t.Fatal("could not find func startListeners in main.go -- has it been renamed?")
	}
	if !callsSelector(fn.Body, "startMBIDRevalidateSweep") {
		t.Fatal("startListeners no longer calls startMBIDRevalidateSweep -- the #2810 sweep " +
			"would be built but never launched, exactly the wiring gap this PR exists to close")
	}
}

// TestMBIDRevalidateSweepSetsFlaggerBeforeStart pins the ordering
// SetFlagger's own doc comment requires: SetFlagger must be called before
// `go mbidSweep.Start(ctx)` launches, because Start's goroutine reads the
// flagger on the first failed verdict and the doc comment is explicit that
// wiring it after Start has already begun is a genuine data race the mutex
// alone does not resolve (it only prevents memory corruption, not the
// logical race of a flag() call seeing a still-nil flagger).
//
// Mutation-proof: swapping the order of the SetFlagger and `go ...Start(ctx)`
// statements inside startMBIDRevalidateSweep in cmd/stillwater/main.go makes
// this test FAIL.
func TestMBIDRevalidateSweepSetsFlaggerBeforeStart(t *testing.T) {
	file, fset := parseMainGo(t)
	fn := findFunc(file, "startMBIDRevalidateSweep")
	if fn == nil {
		t.Fatal("could not find func startMBIDRevalidateSweep in main.go -- has it been renamed?")
	}

	var setFlaggerPos, goStartPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetFlagger" {
				setFlaggerPos = v.Pos()
			}
		case *ast.GoStmt:
			if sel, ok := v.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Start" {
				goStartPos = v.Pos()
			}
		}
		return true
	})

	if setFlaggerPos == token.NoPos {
		t.Fatal("startMBIDRevalidateSweep no longer calls SetFlagger -- the sweep would run " +
			"with no way to raise operator-review findings for a failed verdict")
	}
	if goStartPos == token.NoPos {
		t.Fatal("startMBIDRevalidateSweep no longer launches `go ...Start(ctx)` -- the sweep " +
			"would be built but never actually run")
	}
	if setFlaggerPos > goStartPos {
		setLine := fset.Position(setFlaggerPos).Line
		goLine := fset.Position(goStartPos).Line
		t.Fatalf("SetFlagger (line %d) is ordered AFTER `go ...Start(ctx)` (line %d) -- "+
			"Start's goroutine can begin reading the flagger before SetFlagger has wired it, "+
			"the exact race SetFlagger's own doc comment warns against", setLine, goLine)
	}
}

// TestMBIDRevalidateSettingKeysMatchValidators asserts that every
// mbid_revalidate.* settings key this package reads is validated by
// PUT /api/v1/settings.
//
// This is the #3004 defect class as a mechanical guard. The five
// mbid_revalidate.* keys shipped in #3003 were read here and registered in
// the validator registry under no name at all, so PUT stored any string with a
// 200 OK and getDBIntSetting -- whose fmt.Sscanf("%d") stops at the first
// non-digit and reports success -- read the garbage back as a plausible
// number. A stored "0.5" became a real, in-range 0, and a name-similarity
// threshold of 0 matches every name.
//
// The assertion runs against api.HasSettingValidator, the live map, rather
// than against a second hand-written list of the same five keys. An earlier
// version of this test pinned a local `want` set and claimed a matching test
// in internal/api covered the other direction; it did not -- that test never
// read this file, so a SIXTH key added here and added to `want` passed both.
// Reading the real map means a new key cannot be waved through by editing
// this test's own expectations.
//
// SCOPE, and what this does not catch: it parses the .go files in this
// package directory and matches STRING LITERALS carrying the
// "mbid_revalidate." prefix. A key assembled at runtime from fragments
// (prefix + "suffix") is invisible to it, as is one read from a package this
// test does not parse. Both are deliberate limits of a source-level check --
// noted here rather than left for the next reader to discover, because an
// overstated guard is worse than an honest one.
func TestMBIDRevalidateSettingKeysMatchValidators(t *testing.T) {
	t.Parallel()

	// Parse every .go file in this package directory, not just main.go: a key
	// moved to a neighboring file must not escape the check.
	dir := filepath.Dir(mainGoFile(t))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading package dir %s: %v", dir, err)
	}

	found := map[string]string{} // key -> the file it was found in
	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.HasPrefix(v, "mbid_revalidate.") {
				found[v] = name
			}
			return true
		})
	}

	// A parse that silently matched nothing would make this test vacuous --
	// it would pass just as happily if the whole feature were deleted or the
	// directory walk broke. Assert the preconditions before the real check.
	if parsed == 0 {
		t.Fatalf("parsed no non-test .go files in %s; the directory walk is broken", dir)
	}
	if len(found) == 0 {
		t.Fatalf("found no mbid_revalidate.* keys across %d files in %s; "+
			"either the sweep's settings reads were removed or this scan is broken", parsed, dir)
	}

	for key, file := range found {
		if !api.HasSettingValidator(key) {
			t.Errorf("%s reads settings key %q, which has no validator registry entry "+
				"in internal/api: PUT /api/v1/settings would store any string for it "+
				"with a 200 OK, and the boot reader would parse the garbage into a "+
				"plausible number (#3004)", file, key)
		}
	}
}
