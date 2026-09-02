package rule

import "os"

// unlinkHook, when non-nil, wraps every PERMANENT unlink the two delete-intent
// fixers perform: it is handed the path and a closure that does the real
// removal, and its return value becomes the removal's result. nil in
// production, so the production path is exactly os.Remove.
//
// TEST-ONLY, AND IT EXISTS FOR A STRUCTURAL REASON RATHER THAN CONVENIENCE.
// This is the same shape and the same justification as quarantineRaceHook and
// quarantinePostLinkHook in internal/image/fanart.go: the property under test
// is only observable from INSIDE the operation, so no arrangement of fixture
// state before the call and assertions after it can measure it.
//
// The property is #3015's central one -- that img.MarkDeleteIntent runs BEFORE
// the file is unlinked, never after. Asserting from outside can only establish
// that a marker exists once Fix returns, which is equally true of code that
// marks after its unlink and leaves the exact window #2712 is about: the file
// gone, the operator's intent not yet visible, and an in-flight push free to
// restore it. Relocating the marks to after their unlinks was measured to leave
// the whole rule/api/publish/image suite green, which is what forced this seam.
// internal/api needed none because its removals already route through the
// FileRemover interface (internal/api/fs.go).
//
// A test installs it with installUnlinkHook, which restores the nil production
// value on cleanup. It is a plain package variable with no mutex, so a test
// that installs it must NOT call t.Parallel.
var unlinkHook func(path string, remove func() error) error

// fixerRemove unlinks path, routed through unlinkHook when one is installed.
//
// It is used ONLY at the unlinks the delete-intent marker vouches for: the
// extraneous fixer's file removal and the duplicate fixer's post-commit tomb
// unlink. Deliberately NOT used for the duplicate fixer's stale-tomb clear,
// which runs before staging and normally finds nothing there -- routing it here
// would let an ENOENT no-op present itself to a probe as "the first unlink of
// this operation" and make the assertion depend on incidental ordering rather
// than on where the mark sits.
func fixerRemove(path string) error {
	if unlinkHook != nil {
		return unlinkHook(path, func() error { return os.Remove(path) })
	}
	return os.Remove(path)
}
