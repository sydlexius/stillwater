package image

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The three loops that must consult ReadFailureDistrustsLoop after a bounded
// read fails (#2933). Each reads a SET of files where a failure can be either a
// fact about one file or a fact about the mount, and each previously checked
// only ctx.Err() -- letting a stalled-read cap refusal take the skip branch.
//
// The consequence differs sharply per site, which is why all three are listed
// explicitly rather than left to a general rule:
//
//   - rule/phash_repair.go: a redundant restored slot. Mildest.
//   - publish/publisher.go: UNRECOVERABLE. The snapshot is the only copy that
//     can undo a peer's delete, and the restore loop skips nil-data entries.
//   - api/handlers_push.go: a push reported as succeeded that could not read
//     the files it claimed to push.
var distrustCallSites = []string{
	"internal/rule/phash_repair.go",
	"internal/publish/publisher.go",
	"internal/api/handlers_push.go",
}

// TestReadFailureDistrustsLoop_EveryCallSiteStillRoutesThroughIt is a
// STRUCTURAL guard, and it exists because the behavioral route is closed.
//
// Driving these loops to the cap branch cannot be done from a test: each begins
// by reading its directory through the SAME capped primitive, so a saturated
// cap fails there and returns before the per-candidate loop runs. The branch is
// reachable only through a concurrency window (the opening read succeeds below
// the cap, another operation pushes it over before the per-file reads), and
// reproducing that would race a process-wide counter on every run.
//
// So the property this can actually hold is that each site still ASKS the
// shared predicate rather than reverting to a bare ctx.Err() check -- which is
// exactly the regression shape, since the reverted form is a smaller, more
// obvious-looking edit that leaves every other test green.
//
// Deliberately a source assertion rather than a lint rule: it names the three
// sites and their differing consequences in one place a future reader will
// find, and a NEW loop of this shape is expected to be added here consciously.
func TestReadFailureDistrustsLoop_EveryCallSiteStillRoutesThroughIt(t *testing.T) {
	t.Parallel()
	root := repoRootForDistrustTest(t)

	for _, rel := range distrustCallSites {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}
			// Match the QUALIFIED call at the start of a statement, not the
			// name and not a bare call-shaped string.
			//
			// This pattern has now been tightened TWICE, and both loosenings
			// were caught by someone else:
			//   1. strings.Contains("ReadFailureDistrustsLoop") -- a mutation
			//      survived it, because phash_repair.go names the predicate in
			//      a doc comment as well as calling it.
			//   2. `ReadFailureDistrustsLoop\(` -- still matches prose, since a
			//      comment may legitimately write the call with its parens
			//      (#2976 review). Verified: that pattern matches
			//      "// call ReadFailureDistrustsLoop(ctx, err) here".
			//
			// Every call site is in a package that imports this one as `img`,
			// so the qualified form is what real code looks like, and requiring
			// a preceding `:=`/`=`/`(`/whitespace-at-line-start keeps a comment
			// from satisfying it. A comment is not a code path.
			if !regexp.MustCompile(`(?m)^[^/\n]*\bimg\.ReadFailureDistrustsLoop\(`).Match(src) {
				t.Errorf("%s no longer calls ReadFailureDistrustsLoop.\n"+
					"If this loop was intentionally removed, drop it from distrustCallSites.\n"+
					"If the check was reverted to a bare ctx.Err(), that is the #2933 regression: "+
					"a stalled-read cap refusal would take the per-file skip branch, even though it "+
					"means no later file in the set can be read either.", rel)
			}
		})
	}
}

// TestReadFailureDistrustsLoop_NoBoundedReadLoopChecksOnlyCtxErr sweeps for a
// FOURTH site of the same shape, which is the failure mode the #2933 review
// actually hit: the issue named one loop, and two more with identical code sat
// in other packages.
//
// It flags a bounded read whose error branch consults ctx.Err() without also
// consulting the shared predicate. A top-of-loop "should I keep going" check is
// a different shape and is not matched -- those appear before any read.
func TestReadFailureDistrustsLoop_NoBoundedReadLoopChecksOnlyCtxErr(t *testing.T) {
	t.Parallel()
	root := repoRootForDistrustTest(t)

	// A bounded read, then within the next few lines a ctx.Err() consultation.
	// Bounded to a small window so an unrelated later cancellation check in the
	// same function does not read as this shape.
	suspect := regexp.MustCompile(`(?s)ReadImageFileBounded\([^)]*\)[^\n]*\n(?:[^\n]*\n){0,8}?[^\n]*Err\(\) != nil`)

	for _, pkg := range []string{"internal/rule", "internal/publish", "internal/api", "internal/maintenance"} {
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
			src, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil {
				t.Fatalf("reading %s: %v", rel, readErr)
			}
			for _, m := range suspect.FindAllString(string(src), -1) {
				if strings.Contains(m, "ReadFailureDistrustsLoop") {
					continue
				}
				t.Errorf("%s: a bounded read's failure branch consults ctx.Err() without "+
					"ReadFailureDistrustsLoop.\n"+
					"A stalled-read cap refusal would take the skip branch there, though it means "+
					"no later read in the set can succeed either (#2933). Either route it through "+
					"the predicate, or -- if this read is genuinely not in a multi-candidate loop -- "+
					"leave it and note why here.\nMatched:\n%s", rel, m)
			}
		}
	}
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
