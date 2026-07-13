package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unprotectedFanartSinks are the calls that DESTROY a fanart image when handed the
// "fanart" image type outside a sanctioned chokepoint.
//
// img.Save deletes the slot's other-format file (CleanupConflictingFormats) before it
// writes, with no backup and no rollback -- that is #2413 itself.
//
// The two SingleSlot primitives are just as destructive for fanart, for a different
// reason: their prune is one-deep PER TYPE, so backing up the primary wipes every
// numbered slot's backup. Fanart is multi-slot and must use the slot-scoped path.
//
// saveSingleSlotWithRollback is the single-slot chokepoint. Reached with "fanart" it
// routes into exactly those SingleSlot primitives, so it belongs here too.
var unprotectedFanartSinks = map[string]bool{
	"Save":                       true, // img.Save
	"BackupSingleSlot":           true, // img.BackupSingleSlot
	"RestoreSingleSlot":          true, // img.RestoreSingleSlot
	"saveSingleSlotWithRollback": true, // the SINGLE-slot chokepoint, wrong for fanart
}

// fanartChokepoints are the functions in package api allowed to name "fanart" at a sink.
// The AST walk does not descend into them.
//
//	saveFanartSlotProtected -- the Router wrapper (watcher registration + delegate)
//	processAndAppendFanart  -- APPEND: writes a NEW numbered slot, destroys nothing
//	saveSingleSlotWithRollback -- the single-slot chokepoint; its own img.Save takes the
//	                              image type as a PARAMETER, so it is generic, not a
//	                              fanart write. Its callers are what this test polices.
var fanartChokepoints = map[string]bool{
	"saveFanartSlotProtected":    true,
	"processAndAppendFanart":     true,
	"saveSingleSlotWithRollback": true,
}

// ruleImageSinks are the image-write primitives internal/rule must never reach directly.
// Matched as img.<name> (internal/rule imports internal/image under the alias img).
var ruleImageSinks = map[string]bool{
	"Save":              true,
	"BackupSingleSlot":  true,
	"RestoreSingleSlot": true,
}

// ruleImageChokepoints are the functions in internal/rule allowed to call an image-write
// primitive. There is exactly ONE, deliberately:
//
//	saveImageToDisk -- routes fanart to img.SaveSlotProtected (backup + rollback) and
//	                   everything else to img.Save.
//
// Keeping it to a single function is what lets this guard be blunt: ANY img.Save
// anywhere else in the package is an offense, with no need to reason about what the
// image type happens to be at that call site.
var ruleImageChokepoints = map[string]bool{
	"saveImageToDisk": true,
}

// TestFanartSaveHasASingleChokepoint stops #2413 from being re-introduced, in EITHER of
// the two packages that write artwork.
//
// This bug class keeps coming back because the safety behavior of an image write is
// chosen IMPLICITLY, by branching on a string image type, so a destructive write is
// indistinguishable from a safe one at the call site. #2413 was a carve-out in
// processAndSaveImage; the first fix closed that and MISSED the per-slot Crop,
// Fetch/Replace, assign and import handlers, which called img.Save directly and
// destroyed the primary backdrop anyway (FanartFilename(primary, 0, ...) IS the primary
// name). The SECOND fix closed those and MISSED internal/rule entirely -- where the rule
// engine was overwriting the primary backdrop library-wide, unattended, in bulk auto-fix
// mode (#2433). A unit test of one helper cannot catch a caller that bypasses it, and a
// guard that scans one package cannot catch the package next door.
//
// So both packages are scanned, under the policy each one needs:
//
//	internal/api  -- a destructive sink NAMED "fanart" outside a sanctioned chokepoint.
//	                 The api package writes every image type through shared helpers, so
//	                 the image type is what distinguishes a destructive write here.
//	internal/rule -- ANY direct img.Save/BackupSingleSlot/RestoreSingleSlot outside
//	                 saveImageToDisk. The rule engine's image type is a VARIABLE
//	                 (ruleToImageType(v.RuleID)), never a literal, so a fanart-literal
//	                 match would have been blind to the exact bug that was there. Matching
//	                 the CALLEE instead of the argument is what closes that.
//
// WHAT EVADES A GUARD LIKE THIS. It is a tripwire on the shape of the bug we have already
// had, not a proof. Known holes, stated plainly rather than implied away:
//
//   - A THIRD PACKAGE. This walks internal/api and internal/rule. Any other package that
//     imports internal/image and writes artwork is invisible. That is precisely how
//     internal/rule stayed invisible through two rounds of this fix.
//   - THE SINK ITSELF. saveImageToDisk is allowlisted, so deleting its
//     `if imageType == "fanart"` branch would route fanart back to a bare img.Save and
//     this test would still pass. Nothing structural can catch that; the BEHAVIORAL
//     guards do -- TestImageFixer_AutoFix_BacksUpTheUsersFanartBeforeReplacingIt and
//     TestSaveImageFromData_RollsBackAFailedFanartOverwrite (internal/rule) both go RED.
//     Structure and behavior cover each other here; neither alone is sufficient.
//   - AN IMPORT ALIAS. The rule scan matches the selector `img.Save`. Importing
//     internal/image under a different name would slip past it.
//   - INDIRECTION. A sink reached through a function value, an interface method, or a
//     type assembled at runtime (imageType := kind + "art") is not matched anywhere.
//
// If it fails, you have added a new way to destroy a user's artwork. Route it through the
// chokepoint instead of adding a name to an allowlist.
func TestFanartSaveHasASingleChokepoint(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	var offenders []string
	var scanned int

	// Package api: a destructive sink handed the "fanart" image type.
	scanned += walkGoFiles(t, fset, ".", func(fn *ast.FuncDecl, f *ast.File) {
		if fanartChokepoints[fn.Name.Name] {
			return
		}
		// Identifiers bound to the literal "fanart" at FILE scope are in scope in every
		// function in the file; plus any bound inside this function body.
		idents := fanartIdents(fn.Body)
		for k := range fanartIdents(f) {
			idents[k] = true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || !isUnprotectedFanartSink(call) {
				return true
			}
			for _, arg := range call.Args {
				if namesFanart(arg, idents) {
					offenders = append(offenders,
						fset.Position(call.Pos()).String()+" in api."+fn.Name.Name+
							" (destructive sink named \"fanart\" outside saveFanartSlotProtected)")
					break
				}
			}
			return true
		})
	})

	// Package rule: ANY direct image-write primitive, whatever the image type.
	scanned += walkGoFiles(t, fset, filepath.Join("..", "rule"), func(fn *ast.FuncDecl, _ *ast.File) {
		if ruleImageChokepoints[fn.Name.Name] {
			return
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || !isRuleImageSink(call) {
				return true
			}
			offenders = append(offenders,
				fset.Position(call.Pos()).String()+" in rule."+fn.Name.Name+
					" (image-write primitive outside saveImageToDisk)")
			return true
		})
	})

	// The walk silently passes if it reads nothing -- a wrong working directory, a
	// renamed package, a mistyped relative path. Assert it actually scanned both.
	if scanned == 0 {
		t.Fatal("the guard parsed ZERO source files; it would pass no matter what the code did")
	}

	if len(offenders) > 0 {
		t.Errorf("unprotected image write(s) outside the sanctioned chokepoints:\n  %s\n\n"+
			"A destructive fanart write MUST go through the protected chokepoint (img.SaveSlotProtected, "+
			"reached via saveFanartSlotProtected in api and saveImageToDisk in rule), which backs the "+
			"existing image up first and restores it if the save fails. Fanart slot 0 IS the primary "+
			"backdrop, so an unprotected slot write destroys the user's artwork with no way back "+
			"(#2413, #2433).",
			strings.Join(offenders, "\n  "))
	}
}

// walkGoFiles parses every non-test .go file in dir and hands each function declaration
// to visit. It returns the number of files scanned so the caller can prove the walk was
// not a no-op.
func walkGoFiles(t *testing.T, fset *token.FileSet, dir string, visit func(fn *ast.FuncDecl, f *ast.File)) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading package dir %s: %v", dir, err)
	}
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", path, parseErr)
		}
		scanned++
		for _, decl := range f.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Body == nil {
				continue
			}
			visit(fn, f)
		}
	}
	if scanned == 0 {
		t.Fatalf("the guard parsed ZERO source files in %s; it would pass no matter what the code did", dir)
	}
	return scanned
}

// isUnprotectedFanartSink reports whether the call is one of the destructive sinks,
// whether written as a package selector (img.Save), a method (r.saveSingleSlotWithRollback)
// or a bare call (saveSingleSlotWithRollback).
func isUnprotectedFanartSink(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return unprotectedFanartSinks[fun.Sel.Name]
	case *ast.Ident:
		return unprotectedFanartSinks[fun.Name]
	}
	return false
}

// isRuleImageSink reports whether the call is a direct image-write primitive, written as
// the img.<Name> selector internal/rule uses. It deliberately does NOT look at the
// arguments: the rule engine's image type is a variable, so an argument-based match would
// be blind to the very call that was destroying fanart (#2433).
func isRuleImageSink(call *ast.CallExpr) bool {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || !ruleImageSinks[sel.Sel.Name] {
		return false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	return isIdent && pkg.Name == "img"
}

// namesFanart reports whether an argument expression is the image type "fanart" --
// either the literal, or an identifier bound to it.
func namesFanart(arg ast.Expr, idents map[string]bool) bool {
	switch a := arg.(type) {
	case *ast.BasicLit:
		return a.Kind == token.STRING && a.Value == `"fanart"`
	case *ast.Ident:
		return idents[a.Name]
	}
	return false
}

// fanartIdents collects the names bound to the literal "fanart" anywhere in the node --
// `x := "fanart"`, `x = "fanart"`, `var x = "fanart"`, `const x = "fanart"`. This is
// deliberately flow-insensitive: a name that is EVER the fanart type is treated as the
// fanart type, so the guard errs toward flagging rather than toward missing a write.
func fanartIdents(node ast.Node) map[string]bool {
	found := map[string]bool{}

	bind := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, l := range lhs {
			if i >= len(rhs) {
				return
			}
			name, isIdent := l.(*ast.Ident)
			if !isIdent {
				continue
			}
			if lit, isLit := rhs[i].(*ast.BasicLit); isLit &&
				lit.Kind == token.STRING && lit.Value == `"fanart"` {
				found[name.Name] = true
			}
		}
	}

	ast.Inspect(node, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			bind(s.Lhs, s.Rhs)
		case *ast.ValueSpec: // var/const, at file scope or in a body
			lhs := make([]ast.Expr, len(s.Names))
			for i, nm := range s.Names {
				lhs[i] = nm
			}
			bind(lhs, s.Values)
		}
		return true
	})
	return found
}
