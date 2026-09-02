package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestListArtworkSubdirFiles_AbsentSubdir_ReturnsEmptyNilError pins the
// common case (#3177 AC): an absent subdirectory is not an error.
func TestListArtworkSubdirFiles_AbsentSubdir_ReturnsEmptyNilError(t *testing.T) {
	dir := t.TempDir()

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart")
	if err != nil {
		t.Fatalf("absent subdirectory: got error %v, want nil", err)
	}
	if paths != nil {
		t.Fatalf("absent subdirectory: got paths %v, want nil", paths)
	}
}

// TestListArtworkSubdirFiles_EmptySubdir_ReturnsEmptyNilError covers the
// subdirectory existing but holding nothing countable.
func TestListArtworkSubdirFiles_EmptySubdir_ReturnsEmptyNilError(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrafanart")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart")
	if err != nil {
		t.Fatalf("empty subdirectory: got error %v, want nil", err)
	}
	if paths != nil {
		t.Fatalf("empty subdirectory: got paths %v, want nil", paths)
	}
}

// TestListArtworkSubdirFiles_ListsImagesOnly asserts the extension allowlist,
// dotfile exclusion, and nested-subdirectory exclusion together, and asserts
// the fixture's precondition (file count on disk) before exercising the
// function under test.
func TestListArtworkSubdirFiles_ListsImagesOnly(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrafanart")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	seed := map[string]string{
		"fanart1.jpg":     "a",
		"fanart2.JPEG":    "b", // extension case-insensitivity
		"fanart3.png":     "c",
		"not-an-image.gz": "d", // wrong extension, excluded
		".hidden.jpg":     "e", // dotfile, excluded
	}
	for name, contents := range seed {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(sub, "nested", "sneaky.jpg"), []byte("f"), 0o644); err != nil {
		t.Fatalf("seeding nested file: %v", err)
	}

	// Assert the fixture's precondition before exercising the function
	// (#3177 verification standard): 5 top-level files plus 1 nested dir
	// entry actually sit in the directory.
	preEntries, err := os.ReadDir(sub)
	if err != nil {
		t.Fatalf("precondition ReadDir: %v", err)
	}
	if len(preEntries) != 6 {
		t.Fatalf("fixture precondition: got %d entries in %s, want 6", len(preEntries), sub)
	}

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart")
	if err != nil {
		t.Fatalf("ListArtworkSubdirFiles: unexpected error %v", err)
	}

	wantBases := []string{"fanart1.jpg", "fanart2.JPEG", "fanart3.png"}
	if len(paths) != len(wantBases) {
		t.Fatalf("got %d paths %v, want %d matching %v", len(paths), paths, len(wantBases), wantBases)
	}
	for i, want := range wantBases {
		got := filepath.Base(paths[i])
		if got != want {
			t.Errorf("paths[%d] = %q, want %q (deterministic sorted order)", i, got, want)
		}
	}
}

// TestListArtworkSubdirFiles_DeterministicOrder asserts the order is sorted
// by filename rather than incidental filesystem enumeration order, per the
// #3177 AC that a test must assert order rather than rely on it.
func TestListArtworkSubdirFiles_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrafanart")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write in an order that is NOT alphabetical, so a pass here cannot be
	// explained by coincidental creation order.
	for _, name := range []string{"zzz.jpg", "aaa.jpg", "mmm.jpg"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"aaa.jpg", "mmm.jpg", "zzz.jpg"}
	if len(paths) != len(want) {
		t.Fatalf("got %d paths, want %d", len(paths), len(want))
	}
	for i, w := range want {
		if got := filepath.Base(paths[i]); got != w {
			t.Errorf("paths[%d] = %q, want %q", i, got, w)
		}
	}
}

// TestListArtworkSubdirFiles_EmptySubdirName_ReturnsError covers the
// degenerate parameter case: an empty name does not meet the exported
// single-path-element contract, so it is refused with ErrInvalidSubdirName
// rather than folded into the "absent subdirectory" empty-result case --
// an empty name is a caller configuration error, not a directory that
// happens not to exist (#3191 review). This is the SAME defect as the bare
// "/" case below: filepath.Join(artistDir, "") == artistDir, exactly like
// filepath.Join(artistDir, "/") == artistDir, so both must be rejected by
// the one containment guard rather than special-cased individually.
func TestListArtworkSubdirFiles_EmptySubdirName_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "")
	if !errors.Is(err, ErrInvalidSubdirName) {
		t.Fatalf("empty subdirName: got (%v, %v), want ErrInvalidSubdirName", paths, err)
	}
	if paths != nil {
		t.Fatalf("empty subdirName: got paths %v, want nil", paths)
	}
}

// TestListArtworkSubdirFiles_ReadFailure_IsAnErrorNeverEmpty pins the load-
// bearing AC: a genuine read failure (not "absent") must be reported as an
// error, never silently folded into the empty-result case. Reproduced by
// replacing the subdirectory with a file, so the ReadDir on it fails with
// ENOTDIR rather than ENOENT.
func TestListArtworkSubdirFiles_ReadFailure_IsAnErrorNeverEmpty(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrafanart")
	// A regular file sitting where a directory is expected: os.ReadDir fails
	// with something other than IsNotExist.
	if err := os.WriteFile(sub, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seeding non-directory: %v", err)
	}

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart")
	if err == nil {
		t.Fatal("read failure (ENOTDIR): got nil error, want a reported error")
	}
	if os.IsNotExist(err) {
		t.Fatalf("read failure (ENOTDIR): got an IsNotExist error %v, want a genuine failure distinguishable from absence", err)
	}
	if paths != nil {
		t.Fatalf("read failure: got paths %v, want nil", paths)
	}
}

// TestListArtworkSubdirFiles_CanceledContext_DoesNotScan pins the same
// contract DiscoverFanart carries (#2689/#2635): a canceled context must
// never be answered with an error-free empty listing, because empty licenses
// a caller to proceed as though nothing is there.
func TestListArtworkSubdirFiles_CanceledContext_DoesNotScan(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrafanart")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "fanart1.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	paths, err := ListArtworkSubdirFiles(ctx, dir, "extrafanart")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: got err %v, want context.Canceled", err)
	}
	if paths != nil {
		t.Fatalf("canceled context: got paths %v, want nil", paths)
	}
}

// TestListArtworkSubdirFiles_ParameterizedSubdirName pins the #3177 design
// requirement that the subdirectory name is a caller-supplied parameter, not
// a hardcoded "extrafanart" -- a differently-named subdirectory (standing in
// for a future extrathumbs/ caller) is enumerated identically.
func TestListArtworkSubdirFiles_ParameterizedSubdirName(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrathumbs")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "thumb1.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrathumbs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "thumb1.jpg" {
		t.Fatalf("got %v, want exactly [thumb1.jpg] under extrathumbs/", paths)
	}

	// The sibling extrafanart/ was never created, so it must independently
	// report empty -- the two subdirectory names do not leak into each other.
	fanartPaths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart")
	if err != nil || fanartPaths != nil {
		t.Fatalf("extrafanart (never created): got (%v, %v), want (nil, nil)", fanartPaths, err)
	}
}

// TestListArtworkSubdirFiles_RejectsEscapingSubdirName is the containment
// guard (#3177 review, MINOR-2). filepath.Join CLEANS its result, so a
// subdirName of "../sibling" resolves OUTSIDE artistDir and would be
// enumerated as though it were inside it. The function is exported and its
// doc comment advertises subdirName as the extension point for a future
// extrathumbs/ caller, so the containment is enforced here rather than left
// as an unwritten obligation on that caller.
func TestListArtworkSubdirFiles_RejectsEscapingSubdirName(t *testing.T) {
	root := t.TempDir()
	artistDir := filepath.Join(root, "artist")
	if err := os.Mkdir(artistDir, 0o755); err != nil {
		t.Fatalf("mkdir artist: %v", err)
	}
	// A populated SIBLING of the artist directory. Without the guard,
	// "../sibling" enumerates this and the escape is silent.
	sibling := filepath.Join(root, "sibling")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "outside.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding sibling: %v", err)
	}
	// PRECONDITION: the sibling really is enumerable, so a rejection below is
	// the GUARD refusing and not merely an empty directory.
	if entries, err := os.ReadDir(sibling); err != nil || len(entries) != 1 {
		t.Fatalf("precondition: sibling holds %d entries (err=%v), want 1", len(entries), err)
	}

	// Every shape that escapes or is not a single path element. On Unix a
	// backslash is an ordinary filename character, so it is rejected by an
	// explicit check rather than by filepath.Base -- the guard must not be
	// weaker on one GOOS than another.
	for _, name := range []string{
		"../sibling",
		"..",
		".",
		"sub/nested",
		"/absolute",
		"..\\sibling",
		"",
		"/",
	} {
		t.Run(name, func(t *testing.T) {
			paths, err := ListArtworkSubdirFiles(context.Background(), artistDir, name)
			if !errors.Is(err, ErrInvalidSubdirName) {
				t.Fatalf("subdirName %q: got (%v, %v), want ErrInvalidSubdirName -- a name that is not a "+
					"single path element must be refused, never joined", name, paths, err)
			}
			if paths != nil {
				t.Fatalf("subdirName %q returned paths %v; a refused name must return nothing, since an "+
					"empty-but-nil-error result licenses the caller to proceed", name, paths)
			}
		})
	}

	// And the ordinary case still works from the same artistDir, so the guard
	// rejects escapes rather than everything.
	inside := filepath.Join(artistDir, "extrafanart")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("mkdir extrafanart: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inside, "a.jpg"), []byte("y"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	paths, err := ListArtworkSubdirFiles(context.Background(), artistDir, "extrafanart")
	if err != nil || len(paths) != 1 {
		t.Fatalf("the ordinary in-directory case broke: got (%v, %v), want one path", paths, err)
	}
}

// TestListArtworkSubdirFiles_CaseFoldingIsTheFilesystems pins the divergence
// documented on ListArtworkSubdirFiles (#3177 review, MINOR-3): this function
// passes subdirName to the OS verbatim and therefore folds case only where
// the FILESYSTEM does, while internal/artist's isAdditiveMergeDir folds case
// explicitly on every platform.
//
// The test does not assert one answer, because the correct answer differs per
// filesystem and asserting either one unconditionally would fail on the other.
// It DETECTS which behavior the filesystem under test has -- by creating a
// lowercase directory and asking whether an uppercase name reaches it -- and
// then asserts the enumeration agrees with that detection. So it pins the
// behavior as "whatever the filesystem does, consistently", which is exactly
// the documented contract, and it FAILS if this function ever starts folding
// case itself on a case-sensitive filesystem (or stops on an insensitive one)
// without the doc comment being updated with it.
func TestListArtworkSubdirFiles_CaseFoldingIsTheFilesystems(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "extrafanart")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Detect the filesystem's own behavior WITHOUT going through the function
	// under test -- verifying from outside the mechanism, so a bug in the
	// function cannot also supply the expectation it is measured against.
	fsFolds := false
	if _, err := os.Stat(filepath.Join(dir, "Extrafanart")); err == nil {
		fsFolds = true
	}

	// Sanity: the lowercase spelling must work either way, or the fixture is
	// broken and everything below is vacuous.
	if paths, err := ListArtworkSubdirFiles(context.Background(), dir, "extrafanart"); err != nil || len(paths) != 1 {
		t.Fatalf("precondition: the lowercase name returned (%v, %v), want one path", paths, err)
	}

	paths, err := ListArtworkSubdirFiles(context.Background(), dir, "Extrafanart")
	if err != nil {
		t.Fatalf("querying the mixed-case name returned an error: %v (want either one path or a nil/nil "+
			"empty result, never an error)", err)
	}
	if fsFolds && len(paths) != 1 {
		t.Fatalf("this filesystem folds case (Stat found Extrafanart/), so the enumeration should too, "+
			"but it returned %v. If this function has started folding case itself, update its doc comment "+
			"and reconcile the divergence with internal/artist's isAdditiveMergeDir.", paths)
	}
	if !fsFolds && len(paths) != 0 {
		t.Fatalf("this filesystem is case-SENSITIVE (Stat did not find Extrafanart/), so a mixed-case query "+
			"must find nothing, but the enumeration returned %v -- meaning this function has started folding "+
			"case itself. That may be the right call, but it is a documented divergence: update the doc "+
			"comment and this test together.", paths)
	}
	t.Logf("filesystem case-folding detected: %v (paths for the mixed-case query: %d)", fsFolds, len(paths))
}
