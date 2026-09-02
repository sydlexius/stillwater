package image

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFanartFilename(t *testing.T) {
	tests := []struct {
		name          string
		primaryName   string
		index         int
		kodiNumbering bool
		want          string
	}{
		{"emby primary", "backdrop.jpg", 0, false, "backdrop.jpg"},
		{"emby second", "backdrop.jpg", 1, false, "backdrop2.jpg"},
		{"emby third", "backdrop.jpg", 2, false, "backdrop3.jpg"},
		{"kodi primary", "fanart.jpg", 0, true, "fanart.jpg"},
		{"kodi second", "fanart.jpg", 1, true, "fanart1.jpg"},
		{"kodi third", "fanart.jpg", 2, true, "fanart2.jpg"},
		{"plex primary", "fanart.jpg", 0, false, "fanart.jpg"},
		{"plex second", "fanart.jpg", 1, false, "fanart2.jpg"},
		{"png primary", "backdrop.png", 0, false, "backdrop.png"},
		{"png second", "backdrop.png", 1, false, "backdrop2.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FanartFilename(tt.primaryName, tt.index, tt.kodiNumbering)
			if got != tt.want {
				t.Errorf("FanartFilename(%q, %d, %v) = %q, want %q",
					tt.primaryName, tt.index, tt.kodiNumbering, got, tt.want)
			}
		})
	}
}

func TestDiscoverFanart(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg", "unrelated.jpg", "logo.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected 3 fanart files, got %d: %v", len(paths), paths)
	}

	wantBases := []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"}
	for i, want := range wantBases {
		got := filepath.Base(paths[i])
		if got != want {
			t.Errorf("paths[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestDiscoverFanart_KodiNaming(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected 3 fanart files, got %d: %v", len(paths), paths)
	}

	wantBases := []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"}
	for i, want := range wantBases {
		got := filepath.Base(paths[i])
		if got != want {
			t.Errorf("paths[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestDiscoverFanart_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-subdir")
	_, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
	}
}

func TestDiscoverFanart_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("expected 0 fanart files, got %d", len(paths))
	}
}

func TestDiscoverFanart_MixedCase(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"backdrop.jpg", "Backdrop2.jpg", "BACKDROP3.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected 3 fanart files (mixed case), got %d: %v", len(paths), paths)
	}
}

func TestMaxFanartIndex(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		primary string
		want    int
	}{
		{"empty dir", nil, "backdrop.jpg", -1},
		{"primary only", []string{"backdrop.jpg"}, "backdrop.jpg", 0},
		{"primary plus numbered", []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"}, "backdrop.jpg", 3},
		{"gap in numbering", []string{"backdrop.jpg", "backdrop5.jpg"}, "backdrop.jpg", 5},
		{"only high numbered", []string{"fanart3.jpg"}, "fanart.jpg", 3},
		{"unrelated files only", []string{"logo.png", "folder.jpg"}, "backdrop.jpg", -1},
		{"mixed case", []string{"Backdrop.jpg", "BACKDROP2.png"}, "backdrop.jpg", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := MaxFanartIndex(context.Background(), dir, tt.primary)
			if err != nil {
				t.Fatalf("MaxFanartIndex(context.Background(), ) error: %v", err)
			}
			if got != tt.want {
				t.Errorf("MaxFanartIndex(context.Background(), ) = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMaxFanartIndex_ReadDirError(t *testing.T) {
	_, err := MaxFanartIndex(context.Background(), "/nonexistent/path/abc123", "backdrop.jpg")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestMaxFanartIndex_EmptyPrimary(t *testing.T) {
	got, err := MaxFanartIndex(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != -1 {
		t.Errorf("MaxFanartIndex with empty primary = %d, want -1", got)
	}
}

func TestNextFanartIndex(t *testing.T) {
	tests := []struct {
		name      string
		maxSuffix int
		kodi      bool
		want      int
	}{
		{"no files, kodi", -1, true, 0},
		{"no files, emby", -1, false, 0},
		{"primary only, kodi", 0, true, 1},
		{"primary only, emby", 0, false, 1},
		{"kodi with fanart2", 2, true, 3},
		{"emby with backdrop2", 2, false, 2},
		{"emby with backdrop3", 3, false, 3},
		{"kodi with fanart5 (gap)", 5, true, 6},
		{"emby with backdrop5 (gap)", 5, false, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NextFanartIndex(tt.maxSuffix, tt.kodi)
			if got != tt.want {
				t.Errorf("NextFanartIndex(%d, %v) = %d, want %d",
					tt.maxSuffix, tt.kodi, got, tt.want)
			}
		})
	}
}

func TestNextFanartIndex_EmbySequence(t *testing.T) {
	// Emby scenario: backdrop.jpg + backdrop2.jpg exist.
	// MaxFanartIndex returns 2, NextFanartIndex should return 2,
	// FanartFilename(primary, 2, false) should produce backdrop3.jpg.
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	maxSuffix, err := MaxFanartIndex(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("MaxFanartIndex error: %v", err)
	}
	if maxSuffix != 2 {
		t.Fatalf("MaxFanartIndex = %d, want 2", maxSuffix)
	}
	nextIdx := NextFanartIndex(maxSuffix, false)
	nextName := FanartFilename("backdrop.jpg", nextIdx, false)
	if nextName != "backdrop3.jpg" {
		t.Errorf("next filename = %q, want backdrop3.jpg", nextName)
	}
}

func TestNextFanartIndex_KodiSequence(t *testing.T) {
	// Kodi scenario: fanart.jpg + fanart1.jpg + fanart2.jpg exist.
	// MaxFanartIndex returns 2, NextFanartIndex should return 3,
	// FanartFilename(primary, 3, true) should produce fanart3.jpg.
	dir := t.TempDir()
	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	maxSuffix, err := MaxFanartIndex(context.Background(), dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("MaxFanartIndex error: %v", err)
	}
	if maxSuffix != 2 {
		t.Fatalf("MaxFanartIndex = %d, want 2", maxSuffix)
	}
	nextIdx := NextFanartIndex(maxSuffix, true)
	nextName := FanartFilename("fanart.jpg", nextIdx, true)
	if nextName != "fanart3.jpg" {
		t.Errorf("next filename = %q, want fanart3.jpg", nextName)
	}
}

func TestDiscoverFanart_DuplicateExtension(t *testing.T) {
	dir := t.TempDir()

	// Both backdrop.jpg and backdrop.png exist; should only return one (prefer .jpg match)
	for _, name := range []string{"backdrop.jpg", "backdrop.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (dedup), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "backdrop.jpg" {
		t.Errorf("expected backdrop.jpg (preferred ext), got %q", filepath.Base(paths[0]))
	}
}

func TestDiscoverFanart_DuplicateNumbered(t *testing.T) {
	dir := t.TempDir()

	// backdrop2.jpg and backdrop2.png both exist at numeric index 2
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg", "backdrop2.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 fanart files (primary + one numbered), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[1]) != "backdrop2.jpg" {
		t.Errorf("expected backdrop2.jpg (preferred ext), got %q", filepath.Base(paths[1]))
	}
}

// TestDiscoverFanart_ExtensionPreferenceOverridesLexicalOrder pins the sort's
// extension-preference tiebreak specifically, as distinct from
// TestDiscoverFanart_DuplicateExtension above. That test uses primaryName
// "backdrop.jpg", and ".jpg" sorts lexically before ".png" anyway, so a sort
// that dropped the extension-preference clause entirely and fell back to
// pure lexical path comparison would still pass it for the wrong reason.
// Here primaryName is "backdrop.png", the extension that sorts lexically
// AFTER ".jpg", so only the extension-preference clause can produce the
// right winner.
func TestDiscoverFanart_ExtensionPreferenceOverridesLexicalOrder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.png")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (dedup), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "backdrop.png" {
		t.Errorf("expected backdrop.png (preferred ext, lexically later), got %q", filepath.Base(paths[0]))
	}
}

func TestDiscoverFanart_AlternateExtension(t *testing.T) {
	dir := t.TempDir()

	// Primary is backdrop.jpg but actual file is backdrop.png
	if err := os.WriteFile(filepath.Join(dir, "backdrop.png"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (alternate ext), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "backdrop.png" {
		t.Errorf("expected backdrop.png, got %q", filepath.Base(paths[0]))
	}
}

// TestDiscoverFanart_JpegExtensionAccepted pins the .jpeg allowlist entry.
// .jpeg is absent from internal/scanner's fanartPatterns list (which drives
// the scanner's PRIMARY NAME candidates, not this package's extension
// allowlist), so a directory holding only fanart.jpeg has no pattern-list
// primary on disk and depends on this package matching by base name
// regardless of which allowlisted extension the file actually has. Dropping
// .jpeg from the allowlist here silently drops every fanart.jpeg file from
// discovery.
func TestDiscoverFanart_JpegExtensionAccepted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpeg"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := DiscoverFanart(context.Background(), dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (.jpeg allowlisted), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "fanart.jpeg" {
		t.Errorf("expected fanart.jpeg, got %q", filepath.Base(paths[0]))
	}
}

// TestDiscoverFanart_NumberedVariantRequiresPositiveInteger pins the
// strconv.Atoi-succeeds-AND-n>0 rule for numbered variants. fanart-2.jpg
// parses to a negative index if the n>0 check is dropped entirely
// (deliberately not "-1": the dedupe below starts its "last seen index"
// sentinel at -1, so a suffix of exactly -1 would be silently swallowed by
// that sentinel and the test would pass for the wrong reason).
//
// The fanart0.jpg-alone case below pins the other half of the same rule: a
// realistic off-by-one (n >= 0 instead of n > 0) admits "0" as a suffix, and
// because "fanart.jpg" < "fanart0.jpg" in the path tiebreak, mixing
// fanart0.jpg into a directory that also holds fanart.jpg does not surface
// the bug -- fanart.jpg still wins ordinal 0 by the extension/path tiebreak
// and the count stays 1 either way. Only a directory holding fanart0.jpg
// ALONE, with no fanart.jpg to out-tiebreak it, exposes the off-by-one.
func TestDiscoverFanart_NumberedVariantRequiresPositiveInteger(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fanart.jpg", "fanart0.jpg", "fanart-2.jpg", "fanartx.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := DiscoverFanart(context.Background(), dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (only the primary is a valid match), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "fanart.jpg" {
		t.Errorf("expected fanart.jpg, got %q", filepath.Base(paths[0]))
	}
}

// TestDiscoverFanart_NumberedVariantZeroSuffixAlone pins n > 0 in isolation,
// with no fanart.jpg present to mask an off-by-one via the path tiebreak (see
// the comment above). Index 0 is reserved for the exact base match, not a "0"
// suffix, so a directory holding only fanart0.jpg has zero fanart files.
func TestDiscoverFanart_NumberedVariantZeroSuffixAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart0.jpg"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := DiscoverFanart(context.Background(), dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart(context.Background(), ) error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("fanart0.jpg alone should not match, got %v", paths)
	}
}

// fakeDirEntry is a minimal os.DirEntry stand-in for testing
// DiscoverFanartFrom's matching directly, without touching the filesystem.
// The matcher only calls Name() and IsDir(), so this is a faithful substitute
// for what os.ReadDir hands it -- and it is what makes the case-collision
// test below reach on every platform: a case-INSENSITIVE filesystem (APFS)
// cannot hold "Fanart1.jpg" and "fanart1.jpg" as two entries, so exercising
// that collision at all requires bypassing the filesystem.
type fakeDirEntry struct{ name string }

func (f fakeDirEntry) Name() string               { return f.name }
func (f fakeDirEntry) IsDir() bool                { return false }
func (f fakeDirEntry) Type() fs.FileMode          { return 0 }
func (f fakeDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

func fakeDirEntries(names ...string) []os.DirEntry {
	out := make([]os.DirEntry, 0, len(names))
	for _, n := range names {
		out = append(out, fakeDirEntry{n})
	}
	return out
}

// TestDiscoverFanartFrom_CaseCollisionResolvesDeterministically pins the case
// this refactor was undertaken for: two names differing only in case
// (Fanart1.jpg / fanart1.jpg) collide at the same ordinal, and which one WINS
// is load-bearing -- slot_index is a DiscoverFanart ordinal, and the path at
// an ordinal is what gets probed for dimensions and what a placeholder is
// generated from. The retired scanner/image parity test carried this exact
// fixture; nothing in the surviving suite does (the agreement test's
// "Fanart.JPG"/"FANART1.jpg" case is uppercase names that do not collide with
// anything, so it exercises case-insensitive matching, not collision
// resolution). Synthetic entries let this run on every platform, including a
// case-insensitive one that cannot hold the fixture on disk.
func TestDiscoverFanartFrom_CaseCollisionResolvesDeterministically(t *testing.T) {
	names := []string{"fanart.jpg", "Fanart1.jpg", "fanart1.jpg"}

	// Assert the fixture's own precondition: the two numbered names must
	// collide under case-folding while differing as raw bytes, or this test
	// exercises nothing.
	if names[1] == names[2] {
		t.Fatalf("fixture does not differ as raw bytes: %q == %q", names[1], names[2])
	}
	if !strings.EqualFold(names[1], names[2]) {
		t.Fatalf("fixture does not collide under case-folding: %q vs %q", names[1], names[2])
	}

	got := DiscoverFanartFrom("/d", fakeDirEntries(names...), "fanart.jpg")
	if len(got) != 2 {
		t.Fatalf("expected ordinals 0 and 1, got %d: %v", len(got), got)
	}
	if filepath.Base(got[1]) != "Fanart1.jpg" {
		t.Errorf("ordinal 1 = %q, want Fanart1.jpg (path tiebreak: 'F' sorts before 'f')",
			filepath.Base(got[1]))
	}

	// The winner must not depend on the order entries happen to arrive in.
	reversed := []string{names[2], names[1], names[0]}
	rev := DiscoverFanartFrom("/d", fakeDirEntries(reversed...), "fanart.jpg")
	if len(rev) != len(got) {
		t.Fatalf("reversed entry order changed the count: %d vs %d", len(rev), len(got))
	}
	for i := range got {
		if got[i] != rev[i] {
			t.Errorf("entry order changed ordinal %d: %s vs %s", i, got[i], rev[i])
		}
	}
}

// TestDiscoverFanartFrom_MatchesDiscoverFanart is a direct self-consistency
// check between the wrapper and the entries-accepting core it delegates to:
// given the SAME already-read listing, they must agree exactly. This is
// deliberately weaker than the retired scanner/image parity test -- it does
// not compare two independent implementations, only that the wrapper adds no
// behavior beyond the os.ReadDir it performs.
func TestDiscoverFanartFrom_MatchesDiscoverFanart(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg", "backdrop2.png", "fanart.jpeg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	want, err := DiscoverFanart(context.Background(), dir, "backdrop.jpg")
	if err != nil {
		t.Fatal(err)
	}
	got := DiscoverFanartFrom(dir, entries, "backdrop.jpg")
	if len(got) != len(want) {
		t.Fatalf("DiscoverFanartFrom = %v (%d), DiscoverFanart = %v (%d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: DiscoverFanartFrom = %s, DiscoverFanart = %s", i, got[i], want[i])
		}
	}
}

func TestRenumberFanart_Basic(t *testing.T) {
	dir := t.TempDir()

	// Create 3 files at indices 0, 1, 2.
	names := []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Delete index 1 (backdrop2.jpg). Survivors are index 0 and index 2.
	survivors := []string{
		filepath.Join(dir, "backdrop.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	if err := renumberFanartFiles(dir, "backdrop.jpg", survivors, false); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	// After renumber: index 0 = backdrop.jpg, index 1 = backdrop2.jpg
	wantFiles := []string{"backdrop.jpg", "backdrop2.jpg"}
	for _, want := range wantFiles {
		path := filepath.Join(dir, want)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist: %v", want, err)
		}
	}
	// The old backdrop3.jpg should no longer exist.
	if _, err := os.Stat(filepath.Join(dir, "backdrop3.jpg")); !os.IsNotExist(err) {
		t.Errorf("expected backdrop3.jpg to be gone, but it still exists or stat returned unexpected error: %v", err)
	}
}

func TestRenumberFanart_MixedExtensions(t *testing.T) {
	dir := t.TempDir()

	// Create files with mixed extensions.
	files := map[string]string{
		"backdrop.jpg":  "jpg-primary",
		"backdrop2.png": "png-second",
		"backdrop3.jpg": "jpg-third",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Remove index 0 (backdrop.jpg). Survivors are backdrop2.png and backdrop3.jpg.
	survivors := []string{
		filepath.Join(dir, "backdrop2.png"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	if err := renumberFanartFiles(dir, "backdrop.jpg", survivors, false); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	// Index 0 should keep .png extension, index 1 should keep .jpg extension.
	// FanartFilename("backdrop.jpg", 0, false) = "backdrop.jpg" but ext is .png => "backdrop.png"
	// FanartFilename("backdrop.jpg", 1, false) = "backdrop2.jpg" and ext is .jpg => "backdrop2.jpg"
	wantExists := []string{"backdrop.png", "backdrop2.jpg"}
	for _, want := range wantExists {
		path := filepath.Join(dir, want)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist: %v", want, err)
		}
	}
}

func TestRenumberFanart_KodiNumbering(t *testing.T) {
	dir := t.TempDir()

	// Kodi naming: fanart.jpg, fanart1.jpg, fanart2.jpg
	names := []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Delete index 1 (fanart1.jpg). Survivors are index 0 and index 2.
	survivors := []string{
		filepath.Join(dir, "fanart.jpg"),
		filepath.Join(dir, "fanart2.jpg"),
	}

	if err := renumberFanartFiles(dir, "fanart.jpg", survivors, true); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	// After renumber with kodi=true:
	// Index 0 = fanart.jpg (FanartFilename("fanart.jpg", 0, true) = "fanart.jpg")
	// Index 1 = fanart1.jpg (FanartFilename("fanart.jpg", 1, true) = "fanart1.jpg")
	wantFiles := []string{"fanart.jpg", "fanart1.jpg"}
	for _, want := range wantFiles {
		path := filepath.Join(dir, want)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist: %v", want, err)
		}
	}
	// The old fanart2.jpg should no longer exist.
	if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); !os.IsNotExist(err) {
		t.Errorf("expected fanart2.jpg to be gone, but it still exists or stat returned unexpected error: %v", err)
	}
}

func TestRenumberFanart_EmptySurvivors(t *testing.T) {
	dir := t.TempDir()

	// Empty survivors should return nil without crashing.
	if err := renumberFanartFiles(dir, "backdrop.jpg", nil, false); err != nil {
		t.Errorf("RenumberFanart(nil survivors) = %v, want nil", err)
	}
	if err := renumberFanartFiles(dir, "backdrop.jpg", []string{}, false); err != nil {
		t.Errorf("RenumberFanart(empty survivors) = %v, want nil", err)
	}
}

func TestRenumberFanart_SingleSurvivor(t *testing.T) {
	dir := t.TempDir()

	// Single file at index 2 should become the primary (index 0).
	if err := os.WriteFile(filepath.Join(dir, "backdrop3.jpg"), []byte("only-one"), 0o644); err != nil {
		t.Fatal(err)
	}

	survivors := []string{filepath.Join(dir, "backdrop3.jpg")}

	if err := renumberFanartFiles(dir, "backdrop.jpg", survivors, false); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	// Should be renamed to the primary name (index 0).
	path := filepath.Join(dir, "backdrop.jpg")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected backdrop.jpg to exist: %v", err)
	}
	// The original file should be gone.
	if _, err := os.Stat(filepath.Join(dir, "backdrop3.jpg")); !os.IsNotExist(err) {
		t.Errorf("expected backdrop3.jpg to be gone after renumber")
	}
}

func TestRenumberFanart_Phase1Rollback(t *testing.T) {
	dir := t.TempDir()

	// Create 2 real files and reference a third that does not exist.
	if err := os.WriteFile(filepath.Join(dir, "backdrop2.jpg"), []byte("content-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop3.jpg"), []byte("content-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	survivors := []string{
		filepath.Join(dir, "backdrop2.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
		filepath.Join(dir, "nonexistent.jpg"), // triggers Phase 1 failure
	}

	err := renumberFanartFiles(dir, "backdrop.jpg", survivors, false)
	if err == nil {
		t.Fatal("expected error from non-existent survivor, got nil")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("expected error to mention 'staging', got: %v", err)
	}

	// Rollback should restore the first two files to their original paths.
	for i, path := range survivors[:2] {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("survivor[%d] (%s) not restored: %v", i, filepath.Base(path), readErr)
			continue
		}
		want := []string{"content-a", "content-b"}[i]
		if string(data) != want {
			t.Errorf("survivor[%d] content = %q, want %q", i, string(data), want)
		}
	}

	// Temp files should not be left behind.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("reading dir after rollback: %v", readErr)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "fanart_renumber_") {
			t.Errorf("leftover temp file after rollback: %s", e.Name())
		}
	}
}

func TestRenumberFanart_Phase2Rollback(t *testing.T) {
	dir := t.TempDir()

	// Create 2 survivor files at non-contiguous indices.
	if err := os.WriteFile(filepath.Join(dir, "backdrop3.jpg"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop5.jpg"), []byte("bravo"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a directory at the target path for index 0. On Linux os.Rename
	// returns EISDIR when trying to rename a file over a directory, which
	// triggers the Phase 2 rollback path.
	target := filepath.Join(dir, "backdrop.jpg")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	survivors := []string{
		filepath.Join(dir, "backdrop3.jpg"),
		filepath.Join(dir, "backdrop5.jpg"),
	}

	err := renumberFanartFiles(dir, "backdrop.jpg", survivors, false)
	if err == nil {
		t.Fatal("expected error from Phase 2 collision with directory, got nil")
	}

	// Rollback should restore both files to their original paths.
	for i, path := range survivors {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("survivor[%d] (%s) not restored: %v", i, filepath.Base(path), readErr)
			continue
		}
		want := []string{"alpha", "bravo"}[i]
		if string(data) != want {
			t.Errorf("survivor[%d] content = %q, want %q", i, string(data), want)
		}
	}

	// Temp files should not be left behind.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("reading dir after rollback: %v", readErr)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "fanart_renumber_") {
			t.Errorf("leftover temp file after rollback: %s", e.Name())
		}
	}
}

func TestRenumberFanart_ContentPreservation(t *testing.T) {
	dir := t.TempDir()

	// Write distinct content to each file so we can verify it survives renumber.
	contents := map[string]string{
		"backdrop.jpg":  "content-for-index-zero",
		"backdrop2.jpg": "content-for-index-one",
		"backdrop3.jpg": "content-for-index-two",
	}
	for name, content := range contents {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Remove index 1 (backdrop2.jpg). Survivors: index 0 and index 2.
	survivors := []string{
		filepath.Join(dir, "backdrop.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	if err := renumberFanartFiles(dir, "backdrop.jpg", survivors, false); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	// backdrop.jpg (index 0) should still contain "content-for-index-zero"
	got, err := os.ReadFile(filepath.Join(dir, "backdrop.jpg"))
	if err != nil {
		t.Fatalf("reading backdrop.jpg: %v", err)
	}
	if string(got) != "content-for-index-zero" {
		t.Errorf("backdrop.jpg content = %q, want %q", string(got), "content-for-index-zero")
	}

	// backdrop2.jpg (index 1, was backdrop3.jpg) should contain "content-for-index-two"
	got, err = os.ReadFile(filepath.Join(dir, "backdrop2.jpg"))
	if err != nil {
		t.Fatalf("reading backdrop2.jpg: %v", err)
	}
	if string(got) != "content-for-index-two" {
		t.Errorf("backdrop2.jpg content = %q, want %q", string(got), "content-for-index-two")
	}
}

// fakeHashInvalidator records the InvalidateImageHashes calls made against it.
type fakeHashInvalidator struct {
	calls    []string // "artistID/imageType" per InvalidateImageHashes call
	geomCall []string // "artistID/imageType" per InvalidateImageGeometry call
	err      error
	geomErr  error
}

func (f *fakeHashInvalidator) InvalidateImageHashes(_ context.Context, artistID, imageType string) error {
	f.calls = append(f.calls, artistID+"/"+imageType)
	return f.err
}

func (f *fakeHashInvalidator) InvalidateImageGeometry(_ context.Context, artistID, imageType string) error {
	f.geomCall = append(f.geomCall, artistID+"/"+imageType)
	return f.geomErr
}

// TestRenumberFanart_InvalidatesHashes proves the invalidation is not optional.
//
// Renumbering moves a different file into a slot while the stored hashes stay
// keyed by slot, so a renumber that does not invalidate leaves rows describing
// files their slots no longer hold -- which the exact-duplicate fixer then
// deletes as byte-identical copies. This asserts the observable outcome (the
// invalidator was actually called for this artist's fanart), not that the
// function returned nil.
func TestRenumberFanart_InvalidatesHashes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	survivors := []string{
		filepath.Join(dir, "backdrop.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	inv := &fakeHashInvalidator{}
	if err := RenumberFanart(context.Background(), inv, "artist-1", dir, "backdrop.jpg", survivors, false); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	if want := []string{"artist-1/fanart"}; len(inv.calls) != 1 || inv.calls[0] != want[0] {
		t.Errorf("invalidator calls = %v, want %v", inv.calls, want)
	}
	// The rename must still have happened.
	if _, err := os.Stat(filepath.Join(dir, "backdrop2.jpg")); err != nil {
		t.Errorf("survivor was not renumbered into slot 1: %v", err)
	}
}

// TestRenumberFanart_EmptySurvivorsStillInvalidates is the F2 regression test
// (PR #2458 review, CodeRabbit HIGH). Deleting the last/only fanart image for
// an artist calls RenumberFanart with an empty survivors slice; a prior
// version of this function returned nil immediately in that case, before ever
// calling the invalidator, leaving the deleted slot's hash to falsely match
// whatever distinct image gets uploaded into it next. An empty-survivors call
// means every fanart file for this artist just vanished, so there is MORE to
// invalidate, not less.
func TestRenumberFanart_EmptySurvivorsStillInvalidates(t *testing.T) {
	dir := t.TempDir()
	inv := &fakeHashInvalidator{}

	if err := RenumberFanart(context.Background(), inv, "artist-1", dir, "backdrop.jpg", nil, false); err != nil {
		t.Fatalf("RenumberFanart(empty survivors) error: %v", err)
	}

	if want := []string{"artist-1/fanart"}; len(inv.calls) != 1 || inv.calls[0] != want[0] {
		t.Errorf("invalidator calls = %v, want %v -- the empty-survivors path must still invalidate", inv.calls, want)
	}
}

// TestRenumberFanart_NilInvalidatorRefusesToRenumber proves the guard fails
// LOUD and CLOSED. A caller with nowhere to record the invalidation must not get
// a silently-unsafe renumber: the files must be left exactly as they were.
func TestRenumberFanart_NilInvalidatorRefusesToRenumber(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	survivors := []string{
		filepath.Join(dir, "backdrop.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	err := RenumberFanart(context.Background(), nil, "artist-1", dir, "backdrop.jpg", survivors, false)
	if err == nil {
		t.Fatal("RenumberFanart(nil invalidator) = nil, want an error")
	}

	// No file may have moved: backdrop3.jpg must still be where it was, and
	// slot 1 must not have been created.
	if _, statErr := os.Stat(filepath.Join(dir, "backdrop3.jpg")); statErr != nil {
		t.Errorf("original file was renamed despite the refusal: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "backdrop2.jpg")); !os.IsNotExist(statErr) {
		t.Errorf("renumber proceeded without an invalidator (backdrop2.jpg exists)")
	}
}

// TestRenumberFanart_InvalidationFailureSurfaces proves an invalidation
// failure is reported rather than swallowed, AND that it aborts before the
// destructive rename ever runs. Invalidation happens first specifically so a
// failure here cannot leave the caller staring at survivors that moved while
// the reason it moved got lost -- see the ordering rationale on
// RenumberFanart. A swallowed or post-hoc error would leave stale hashes (or
// worse, a rolled-back caller overwriting freshly-renumbered files) with
// nobody the wiser.
func TestRenumberFanart_InvalidationFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "backdrop3.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	survivors := []string{filepath.Join(dir, "backdrop3.jpg")}

	sentinel := errors.New("db is down")
	inv := &fakeHashInvalidator{err: sentinel}

	err := RenumberFanart(context.Background(), inv, "artist-1", dir, "backdrop.jpg", survivors, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("RenumberFanart() error = %v, want it to wrap %v", err, sentinel)
	}

	// The rename must NOT have happened: backdrop3.jpg stays exactly where it
	// was, and slot 0 (backdrop.jpg) must not have been created.
	if _, statErr := os.Stat(filepath.Join(dir, "backdrop3.jpg")); statErr != nil {
		t.Errorf("original file was renamed despite the invalidation failure: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "backdrop.jpg")); !os.IsNotExist(statErr) {
		t.Errorf("renumber proceeded despite the invalidation failure (backdrop.jpg exists)")
	}
}

// TestRenumberFanart_InvalidatesGeometry is the #2713 counterpart to
// TestRenumberFanart_InvalidatesHashes, and exists for the same reason.
//
// artist_images.width/height are keyed per SLOT, and a renumber moves a
// different FILE into a slot without touching that slot's row. The image rules
// read those columns in preference to the file -- rule.getImageDimensionsResolved
// measures the file only when the stored values are zero -- so a slot left
// holding its neighbor's dimensions makes a rule judge a picture that is no
// longer there. Zeroing is what restores the fall-through to the filesystem.
//
// The assertion is on the observable call, not on a nil return, so a
// reordering of the invalidation that skipped it would still be caught.
func TestRenumberFanart_InvalidatesGeometry(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	survivors := []string{
		filepath.Join(dir, "backdrop.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	inv := &fakeHashInvalidator{}
	if err := RenumberFanart(context.Background(), inv, "artist-1", dir, "backdrop.jpg", survivors, false); err != nil {
		t.Fatalf("RenumberFanart() error: %v", err)
	}

	if want := "artist-1/fanart"; len(inv.geomCall) != 1 || inv.geomCall[0] != want {
		t.Errorf("geometry invalidator calls = %v, want exactly [%s]", inv.geomCall, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "backdrop2.jpg")); err != nil {
		t.Errorf("survivor was not renumbered into slot 1: %v", err)
	}
}

// TestRenumberFanart_EmptySurvivorsStillInvalidatesGeometry mirrors the
// empty-survivors hash case. Deleting the last fanart file for an artist means
// every stored dimension for that type now describes nothing, so there is MORE
// to invalidate, not less. A short-circuit that returned before the geometry
// call would leave the deleted slot's size ready to be read as truth.
func TestRenumberFanart_EmptySurvivorsStillInvalidatesGeometry(t *testing.T) {
	dir := t.TempDir()
	inv := &fakeHashInvalidator{}

	if err := RenumberFanart(context.Background(), inv, "artist-1", dir, "backdrop.jpg", nil, false); err != nil {
		t.Fatalf("RenumberFanart(empty survivors) error: %v", err)
	}

	if want := "artist-1/fanart"; len(inv.geomCall) != 1 || inv.geomCall[0] != want {
		t.Errorf("geometry invalidator calls = %v, want exactly [%s]", inv.geomCall, want)
	}
}

// TestRenumberFanart_GeometryInvalidationFailureStopsBeforeRenaming pins the
// ordering argument the function's own comment makes for hashes: invalidation
// runs BEFORE the destructive rename so an invalidation-only failure (a
// transient DB error, nothing to do with the filesystem) returns with nothing
// moved, leaving the caller's rollback safe.
//
// Without this, a geometry failure after the rename would be indistinguishable
// to the caller from a failed rename, and its rollback could restore tombed
// files onto paths the renumbered survivors now occupy.
func TestRenumberFanart_GeometryInvalidationFailureStopsBeforeRenaming(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"backdrop.jpg", "backdrop3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	survivors := []string{
		filepath.Join(dir, "backdrop.jpg"),
		filepath.Join(dir, "backdrop3.jpg"),
	}

	inv := &fakeHashInvalidator{geomErr: errors.New("db busy")}
	if err := RenumberFanart(context.Background(), inv, "artist-1", dir, "backdrop.jpg", survivors, false); err == nil {
		t.Fatal("RenumberFanart must fail when geometry invalidation fails")
	}

	// Nothing moved: the original numbering is intact and no new slot exists.
	if _, err := os.Stat(filepath.Join(dir, "backdrop3.jpg")); err != nil {
		t.Errorf("original file was renamed despite the invalidation failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backdrop2.jpg")); err == nil {
		t.Error("a rename happened despite the invalidation failure")
	}
}

// FuzzDiscoverFanart replaces the retired scanner/image parity test with a
// generative guard on DiscoverFanart's postconditions. It must never panic on
// any filename set, and its output must obey the invariants that make the
// result usable as deleteStaleSlots' delete bound: no ordinal returned twice,
// ordinals non-decreasing, every path allowlisted, and the wrapper
// (DiscoverFanart) must agree exactly with the entries-accepting core
// (DiscoverFanartFrom) it delegates to -- the two are never allowed to see
// different input and produce different output, since the scanner depends on
// exactly that agreement.
func FuzzDiscoverFanart(f *testing.F) {
	// primary-only
	f.Add("fanart.jpg", "fanart.jpg")
	// numbered variants
	f.Add("fanart.jpg\nfanart1.jpg\nfanart2.jpg", "fanart.jpg")
	// mixed extensions at one ordinal
	f.Add("backdrop.jpg\nbackdrop.png", "backdrop.jpg")
	// .jpeg-only primary (absent from scanner's pattern list)
	f.Add("fanart.jpeg", "fanart.jpg")
	// zero, negative, and non-numeric suffixes
	f.Add("fanart.jpg\nfanart0.jpg\nfanart-1.jpg\nfanartx.jpg", "fanart.jpg")
	// case-colliding names
	f.Add("fanart.jpg\nFanart1.jpg\nfanart1.jpg", "fanart.jpg")
	// no fanart at all
	f.Add("folder.jpg\nlogo.png\nartist.nfo", "fanart.jpg")
	// empty primary
	f.Add("fanart.jpg", "")

	f.Fuzz(func(t *testing.T, namesBlob string, primaryName string) {
		dir := t.TempDir()
		for _, name := range fuzzFanartFilenames(namesBlob) {
			// Some generated names are not writable on every filesystem (an
			// invalid-UTF-8 byte sequence fails with "illegal byte sequence"
			// on macOS, for instance). Errors here are a fixture-construction
			// detail, not a finding: the oracle below derives its answer from
			// os.ReadDir, i.e. from what actually landed on disk, so a name
			// that failed to write simply is not a candidate.
			_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading fixture dir: %v", err)
		}

		got := DiscoverFanartFrom(dir, entries, primaryName)

		wrapped, err := DiscoverFanart(context.Background(), dir, primaryName)
		if err != nil {
			t.Fatalf("DiscoverFanart: %v", err)
		}
		if len(got) != len(wrapped) {
			t.Fatalf("DiscoverFanartFrom and DiscoverFanart disagree on count: %d vs %d (%v vs %v)",
				len(got), len(wrapped), got, wrapped)
		}
		for i := range got {
			if got[i] != wrapped[i] {
				t.Fatalf("DiscoverFanartFrom and DiscoverFanart disagree at %d: %q vs %q", i, got[i], wrapped[i])
			}
		}

		candidateOrdinals := make(map[int]bool)
		for _, entry := range entries {
			if n, ok := fuzzFanartCandidateOrdinal(entry.Name(), primaryName); ok {
				candidateOrdinals[n] = true
			}
		}

		lastOrdinal := -1
		seen := make(map[int]bool, len(got))
		for _, path := range got {
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
				t.Fatalf("returned path %q has a non-allowlisted extension", path)
			}
			n, ok := fuzzFanartCandidateOrdinal(filepath.Base(path), primaryName)
			if !ok {
				t.Fatalf("returned path %q does not qualify as a candidate for primaryName %q", path, primaryName)
			}
			if seen[n] {
				t.Fatalf("ordinal %d returned more than once: %v", n, got)
			}
			seen[n] = true
			if n < lastOrdinal {
				t.Fatalf("ordinals are not non-decreasing: %v", got)
			}
			lastOrdinal = n
		}

		// The returned ordinal SET must equal the candidate ordinal set
		// exactly, not merely be no larger than it: len(got) > len is an
		// over-count check alone, and pairs with the loop above (which
		// already proves every returned ordinal IS a candidate, so len(seen)
		// <= len(candidateOrdinals) always) to together prove set equality.
		// This direction -- every CANDIDATE ordinal must actually be
		// RETURNED -- is what an under-count drops: a missing allowlist
		// entry (e.g. .jpeg) silently removes candidates from `got` without
		// creating a duplicate, breaking sort order, or using a
		// non-allowlisted extension, so none of the checks above would catch
		// it.
		if len(seen) != len(candidateOrdinals) {
			t.Fatalf("returned %d ordinals but %d distinct ordinals are present in the fixture "+
				"(candidates=%v, returned=%v): an under-count here silently drops rows deleteStaleSlots "+
				"would then delete", len(seen), len(candidateOrdinals), candidateOrdinals, got)
		}
	})
}

// fuzzFanartFilenames turns one fuzz-generated string into a bounded set of
// distinct, filesystem-safe single-path-component names. It rejects path
// separators (/ and \) and NUL (which cannot name a single file on any platform)
// rather than trying to make every generated byte sequence writable, and caps
// both the name length and the fixture size so a pathological seed cannot blow
// past filesystem limits or make one fuzz iteration slow.
func fuzzFanartFilenames(blob string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, raw := range strings.Split(blob, "\n") {
		if len(names) >= 12 {
			break
		}
		if len(raw) > 80 {
			raw = raw[:80]
		}
		if raw == "" || raw == "." || raw == ".." || strings.ContainsAny(raw, "/\\\x00") {
			continue
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		names = append(names, raw)
	}
	return names
}

// fuzzFanartCandidateOrdinal reports the ordinal a filename would claim under
// primaryName, using only the specification of what an ordinal IS: an
// allowlisted-extension file whose base either exactly matches primaryName's
// base (ordinal 0) or extends it with a positive integer suffix. It
// deliberately does not reimplement dedupe, sort, or extension-preference --
// those are exercised by the invariants above, not by this oracle -- so it
// bounds the fuzz test's assertions without becoming a second copy of the
// full algorithm.
func fuzzFanartCandidateOrdinal(name, primaryName string) (int, bool) {
	if primaryName == "" {
		return 0, false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return 0, false
	}
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	primaryBase := strings.ToLower(strings.TrimSuffix(primaryName, filepath.Ext(primaryName)))
	if base == primaryBase {
		return 0, true
	}
	if !strings.HasPrefix(base, primaryBase) {
		return 0, false
	}
	n, err := strconv.Atoi(base[len(primaryBase):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
