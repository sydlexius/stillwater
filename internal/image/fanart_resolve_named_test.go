package image

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveFanart covers the name-returning, convention-agnostic resolver
// (#2635). Unlike ResolveFanartFiles it hands back WHICH convention matched, so
// a caller can renumber or write under it, and it reuses one os.ReadDir across
// every candidate name. Each case asserts the OUTCOME: the matched name and the
// resolved path list.
func TestResolveFanart(t *testing.T) {
	// Two conventions, primary first, plus an empty entry the resolver must skip
	// rather than treat as a match.
	names := []string{"fanart.jpg", "", "backdrop.jpg"}

	t.Run("pass 1 primary decides the convention", func(t *testing.T) {
		dir := resolveTestDir(t, "backdrop.jpg", "backdrop2.jpg")
		name, paths, err := ResolveFanart(dir, names)
		if err != nil {
			t.Fatalf("ResolveFanart: %v", err)
		}
		if name != "backdrop.jpg" {
			t.Errorf("matched name = %q, want backdrop.jpg", name)
		}
		if got := base(paths); len(got) != 2 || got[0] != "backdrop.jpg" || got[1] != "backdrop2.jpg" {
			t.Errorf("paths = %v, want [backdrop.jpg backdrop2.jpg]", got)
		}
	})

	t.Run("pass 2 adopts an orphan numbered variant with no primary", func(t *testing.T) {
		// The exact shape a slot delete that failed partway leaves behind: a
		// numbered variant with no primary. A pass-1-only resolver would call
		// this "no fanart" and strand it.
		dir := resolveTestDir(t, "backdrop2.jpg")
		name, paths, err := ResolveFanart(dir, names)
		if err != nil {
			t.Fatalf("ResolveFanart: %v", err)
		}
		if name != "backdrop.jpg" {
			t.Errorf("matched name = %q, want backdrop.jpg", name)
		}
		if got := base(paths); len(got) != 1 || got[0] != "backdrop2.jpg" {
			t.Errorf("paths = %v, want [backdrop2.jpg]", got)
		}
	})

	t.Run("no match returns the preferred name and an honest empty list", func(t *testing.T) {
		// A directory that genuinely holds no fanart returns (preferred, nil,
		// nil): "successfully looked, found none". The preferred name is the
		// first non-empty candidate, ready for a fresh write.
		dir := resolveTestDir(t, "folder.jpg", "logo.png")
		name, paths, err := ResolveFanart(dir, names)
		if err != nil {
			t.Fatalf("ResolveFanart: %v", err)
		}
		if name != "fanart.jpg" {
			t.Errorf("matched name = %q, want the preferred fanart.jpg", name)
		}
		if paths != nil {
			t.Errorf("paths = %v, want nil for a directory with no fanart", base(paths))
		}
	})
}

// TestResolveFanartUnreadableDirErrors is the invariant: an unreadable
// directory is an ERROR, never an empty result. Returning (_, nil, nil) here
// would tell the caller "no fanart" from a read that never completed -- the
// exact false-negative that stranded registry rows (#2635).
func TestResolveFanartUnreadableDirErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := resolveTestDir(t, "fanart.jpg")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	name, paths, err := ResolveFanart(dir, []string{"fanart.jpg"})
	if err == nil {
		t.Fatalf("unreadable directory returned no error (name %q, paths %v); "+
			"'cannot tell' must not read as 'no files'", name, filepath.Base(name))
	}
	if paths != nil {
		t.Errorf("unreadable directory returned paths: %v", base(paths))
	}
}
