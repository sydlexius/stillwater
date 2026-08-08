package image

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A STRUCTURAL GUARD, REWRITTEN AS AN AST WALK AFTER THREE REGEX FAILURES.
//
// The property: any code that consults the CONTEXT after a capped filesystem
// read fails must also consult ReadFailureDistrustsLoop. A cap refusal
// (ErrTooManyStalledReads) means the read did not happen for a reason that
// applies to every later read too -- exactly what a cancellation means -- so
// treating it as a per-item skip produces a false "nothing found" (#2933).
//
// WHY NOT A REGEX. Three attempts, three wrong answers, each caught by someone
// else rather than by me:
//
//  1. `Err\(\) != nil` after `[^)]*` args -- MISSED a live defect twice. The
//     character class cannot cross the `)` inside
//     `ReadImageFileBounded(req.Context(), path)`, and a real site spelled the
//     check `if ctxErr := req.Context().Err(); ctxErr != nil` -- assigned,
//     compared to a named variable, on a receiver that is not `ctx`.
//  2. A lazy `.*?` over the arguments -- FLAGGED THREE INNOCENT FILES by
//     spanning past the failure block into unrelated later code.
//  3. Both versions had a comment-shaped hole: the skip clause tested whether
//     the matched WINDOW mentioned the predicate, and a window includes doc
//     comments. internal/rule/phash_repair.go names the predicate in prose, so
//     it was permanently exempt -- the same "a comment is not a code path"
//     defect the earlier fix claimed to have eliminated, moved from the pattern
//     into the skip clause.
//
// A hostile review then measured the regex against every known site
// individually and found it survived THREE of the six (#2976 review). Comments
// are not AST nodes, which kills failure 3 outright; matching call expressions
// by name kills 1 and 2.
//
// THE CAP IS PROCESS-WIDE, so this is not only about loops. A single read whose
// failure branch consults the context is equally affected, which is why the
// walk does not try to detect "is this in a loop".
//
// WHAT THIS GUARD CANNOT CATCH, stated so nobody reads it as total coverage.
// It detects a ctx check that FORGOT the cap. It cannot detect a DELETED cap
// arm: remove `if errors.Is(readErr, ErrTooManyStalledReads) { ... }` entirely
// and there is no ctx check left at that site to flag, so the walk sees
// nothing. Measured -- that mutation SURVIVES. Those arms need ordinary tests,
// and the api-package sibling (writeCanceledPush, which takes the cause as a
// parameter and is table-tested) is the shape that works. Verified against
// every known site individually: 6 of the 7 are caught, this is the seventh.

// cappedReadHelpers are the functions whose failure can mean "the mount is
// unresponsive" rather than "this one file is bad".
//
// Deliberately broader than ReadImageFileBounded: anchoring on that one name is
// what hid site #7 (a DiscoverFanart caller in internal/rule that builds the
// whitelist a deletion loop deletes against) and site #4 (a RepairEntryBytes
// caller). Every one of these bottoms out in runCancellable and can therefore
// return ErrTooManyStalledReads.
var cappedReadHelpers = map[string]bool{
	"ReadImageFileBounded":    true,
	"DiscoverFanart":          true,
	"ResolveFanart":           true,
	"HashFile":                true,
	"RepairEntryBytes":        true,
	"ReadRepairManifest":      true,
	"LstatBounded":            true,
	"FindExistingImage":       true,
	"FindExistingImageStrict": true,
	"readFileBounded":         true,
	"readDirCtx":              true,
	"statCtx":                 true,
}

// distrustSweepPackages are the consumer packages walked. internal/image itself
// is excluded: it DEFINES these helpers, so its own uses are not consumers.
var distrustSweepPackages = []string{
	"internal/rule",
	"internal/publish",
	"internal/api",
	"internal/maintenance",
}

// acknowledgedCtxOnlySites are failure branches that consult the context
// without the predicate AND are safe for a stated reason. Each entry must carry
// that reason: the point of an allow-list is that adding to it is a decision
// somebody wrote down, not a way to silence the check.
//
// Keyed by "<pkg>/<file>:<funcName>".
var acknowledgedCtxOnlySites = map[string]string{
	"internal/maintenance/image_registry_repair.go:discover": "" +
		"Safe by ORDERING, not construction (#2976 hostile review): ResolveFanartFiles runs " +
		"first and fails under a cap, so discover returns before the loop and the false " +
		"'complete with N skipped' report never materializes. Reproduced: candidates=0, " +
		"err=too many stalled reads, skipped=0. Fragile -- one reordering makes it live (#2977).",

	"internal/maintenance/image_registry_repair.go:appendVerified": "" +
		"Same entry-point ordering as discover above; this is the inner loop it guards.",

	"internal/maintenance/image_registry_repair.go:verifyImageFile": "" +
		"Reached only via appendVerified, which is unreachable under a cap for the reason " +
		"above. Its own failure is per-candidate and correctly reported as such.",

	"internal/maintenance/maintenance.go:BackfillFanartHashes": "" +
		"NON-DESTRUCTIVE: a cap refusal misattributes to 'corrupt artwork' in the log and " +
		"increments skipped, but the registry row is LEFT ALONE. A message-quality defect of " +
		"the same family this PR fixes, not a data-integrity one. Follow-up, not a blocker.",

	"internal/maintenance/maintenance.go:backfillFanartPaths": "" +
		"The nil-cache sentinel is correctly normalized, so a cap refusal degrades to " +
		"'no paths backfilled' rather than to a wrong path. Declines to act -- safe direction.",

	"internal/rule/checkers.go:makeBackdropSequencingChecker": "" +
		"Returns nil (no violation) on a discovery failure, so a cap refusal under-reports " +
		"rather than acting. Conservative direction for a CHECKER, which only ever proposes.",

	"internal/rule/checkers.go:countBackdrops": "" +
		"Feeds a violation COUNT, never a delete. A cap refusal under-counts; nothing is " +
		"unlinked on the strength of it. Worth a follow-up for reporting accuracy (#2977).",

	"internal/rule/fixers.go:Fix": "" +
		"The renumbering fixer: every candidate naming convention fails identically under a " +
		"cap, so it falls through to 'no fanart needing renumbering' and renames nothing.",

	"internal/publish/publisher.go:syncImageToPlatforms": "" +
		"The ctx-only `if` here is the CANCELLATION arm that sits directly BELOW an " +
		"explicit `errors.Is(readErr, ErrTooManyStalledReads)` cap arm added by this PR. " +
		"Both causes are handled; they are simply two sibling if-statements, and the walk " +
		"deliberately refuses to let one vouch for the other (that leniency is what let two " +
		"mutations survive at function granularity). Correct as written.",

	"internal/rule/fixers.go:resyncFanartFields": "" +
		"Deliberately swallows the read error: zero geometry means 'unknown, go measure', " +
		"which the rule resolver already handles by re-measuring. Documented at the site.",
}

// NOTE ON PROVENANCE. These nine were enumerated and adjudicated by the #2976
// hostile review, which walked every caller of every capped helper and reported
// the full enumeration. They are recorded here with that reasoning rather than
// re-derived, and each states the SPECIFIC mechanism that makes a cap refusal
// non-destructive at that site -- not "looks fine". Two are flagged there as
// worth a follow-up for reporting accuracy; neither loses data.

// TestNoCappedReadTreatsAStalledMountAsAPerItemFailure walks every consumer
// package and flags a failure branch that consults the context after a capped
// read without also consulting the shared predicate.
func TestNoCappedReadTreatsAStalledMountAsAPerItemFailure(t *testing.T) {
	t.Parallel()
	root := repoRootForDistrustTest(t)

	var findings []string
	for _, pkg := range distrustSweepPackages {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", pkg, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := pkg + "/" + name
			findings = append(findings, sweepFileForCtxOnlyBranches(t, filepath.Join(dir, name), rel)...)
		}
	}

	sort.Strings(findings)
	for _, f := range findings {
		t.Errorf("%s\n"+
			"A capped read's failure branch consults the context but not "+
			"image.ReadFailureDistrustsLoop.\n"+
			"A stalled-read cap refusal arrives with the context still LIVE, so this "+
			"branch treats 'the mount stopped answering' as 'this one item is bad' -- "+
			"a false negative fed to whatever the caller does next (#2933).\n"+
			"Either route it through the predicate, or add it to "+
			"acknowledgedCtxOnlySites WITH the reason it is safe.", f)
	}
}

// sweepFileForCtxOnlyBranches parses one file and returns a finding per
// offending function.
//
// The unit is the FUNCTION rather than the if-statement: a failure branch can
// be several statements away from the read, and pairing them precisely needs
// dataflow this does not have. Function granularity over-approximates, which
// the allow-list absorbs, and it never under-approximates -- the direction that
// matters, since under-approximating is exactly how three regexes passed over
// live defects.
func sweepFileForCtxOnlyBranches(t *testing.T, path, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", rel, err)
	}

	var findings []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if !bodyCallsCappedRead(fn.Body) {
			continue
		}
		// The unit is the IF-STATEMENT, not the function. Function granularity
		// was tried and SURVIVED two mutations: a file whose function holds a
		// second, unrelated predicate call satisfies a whole-function check
		// even after the branch under test is reverted. The decision being
		// guarded lives in one `if`, so that is what gets inspected.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ifStmt, isIf := n.(*ast.IfStmt)
			if !isIf {
				return true
			}
			// Only the CONDITION and its init, never the body: a nested
			// `errors.Is(...)` deeper inside is a different decision and must
			// not vouch for this one.
			var consultsCtx, consultsPredicate bool
			for _, node := range []ast.Node{ifStmt.Init, ifStmt.Cond} {
				if node == nil {
					continue
				}
				ast.Inspect(node, func(m ast.Node) bool {
					call, isCall := m.(*ast.CallExpr)
					if !isCall {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.Ident:
						if fun.Name == "ReadFailureDistrustsLoop" {
							consultsPredicate = true
						}
						if fun.Name == "Is" && callHasStalledCapArg(call) {
							consultsPredicate = true
						}
					case *ast.SelectorExpr:
						switch fun.Sel.Name {
						case "Err":
							consultsCtx = true
						case "ReadFailureDistrustsLoop":
							consultsPredicate = true
						case "Is":
							if callHasStalledCapArg(call) {
								consultsPredicate = true
							}
						}
					}
					return true
				})
			}
			if !consultsCtx || consultsPredicate {
				return true
			}
			key := rel + ":" + fn.Name.Name
			if _, acknowledged := acknowledgedCtxOnlySites[key]; acknowledged {
				return true
			}
			pos := fset.Position(ifStmt.Pos())
			findings = append(findings,
				rel+":"+itoa(pos.Line)+" in func "+fn.Name.Name)
			return true
		})
	}
	return findings
}

// bodyCallsCappedRead reports whether fn calls any helper that can return the
// stalled-read cap sentinel. Used to scope the if-statement walk: a context
// check in a function that never does a capped read is not this defect.
func bodyCallsCappedRead(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if cappedReadHelpers[fun.Name] {
				found = true
			}
		case *ast.SelectorExpr:
			if cappedReadHelpers[fun.Sel.Name] {
				found = true
			}
		}
		return true
	})
	return found
}

// callHasStalledCapArg reports whether an errors.Is(...) call names the
// stalled-read sentinel, in either the bare or package-qualified form.
func callHasStalledCapArg(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		switch a := arg.(type) {
		case *ast.Ident:
			if a.Name == "ErrTooManyStalledReads" {
				return true
			}
		case *ast.SelectorExpr:
			if a.Sel.Name == "ErrTooManyStalledReads" {
				return true
			}
		}
	}
	return false
}

// TestAcknowledgedCtxOnlySitesAreStillReal keeps the allow-list honest: an
// entry that no longer matches anything is stale and must be removed, or it
// silently pre-exempts a future function that happens to take the same name.
func TestAcknowledgedCtxOnlySitesAreStillReal(t *testing.T) {
	t.Parallel()
	root := repoRootForDistrustTest(t)
	for key, reason := range acknowledgedCtxOnlySites {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is allow-listed with no reason; an exemption without a stated "+
				"why is indistinguishable from an oversight", key)
		}
		file := key[:strings.LastIndex(key, ":")]
		fnName := key[strings.LastIndex(key, ":")+1:]
		path := filepath.Join(root, filepath.FromSlash(file))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("allow-listed file %s no longer exists: %v", file, err)
			continue
		}
		// Check the FUNCTION, not just the file (#2976 review). Stat-ing the
		// file half-honors the rule above: rename or delete the function while
		// the file survives -- the ordinary shape of a refactor -- and the
		// entry sails through, silently pre-exempting any future function that
		// takes the freed name. That is precisely the stale-exemption failure
		// this test exists to catch, so checking only the file left the guard
		// asserting the easier half of its own contract.
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			t.Errorf("parsing allow-listed file %s: %v", file, parseErr)
			continue
		}
		var found bool
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == fnName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allow-listed func %s no longer exists in %s; the entry is stale and "+
				"pre-exempts any future function of that name", fnName, file)
		}
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// repoRootForDistrustTest walks up from the package dir to the module root.
func repoRootForDistrustTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate the module root above %s", dir)
	return ""
}
