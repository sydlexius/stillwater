package image

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

// --- structured record capture (issue #2636) ---------------------------------
//
// Save's failure records are asserted on their ATTRIBUTES, not on the rendered
// text. A substring check against a TextHandler buffer cannot tell a `path`
// attribute from the same string appearing inside the wrapped error message, so
// `strings.Contains(got, "folder.jpg")` passed even with the attribute missing.
// These helpers read the record's attrs directly.

type saveLogEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type saveLogHandler struct {
	entries *[]saveLogEntry
	attrs   []slog.Attr
}

// Enabled returns true for every level on purpose: the handler must SEE the
// Debug "saved image" record, or a negative assertion about it is vacuous.
func (h *saveLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *saveLogHandler) Handle(_ context.Context, r slog.Record) error {
	e := saveLogEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for _, a := range h.attrs {
		e.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		e.attrs[a.Key] = a.Value.String()
		return true
	})
	*h.entries = append(*h.entries, e)
	return nil
}

func (h *saveLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &saveLogHandler{entries: h.entries, attrs: merged}
}

func (h *saveLogHandler) WithGroup(_ string) slog.Handler { return h }

// captureSaveLogs returns a logger that records every level and the slice its
// records land in.
func captureSaveLogs() (*slog.Logger, *[]saveLogEntry) {
	entries := &[]saveLogEntry{}
	return slog.New(&saveLogHandler{entries: entries}), entries
}

// requireOneSaveRecord asserts exactly one captured record has the given
// message, and returns it.
func requireOneSaveRecord(t *testing.T, entries *[]saveLogEntry, msg string) saveLogEntry {
	t.Helper()
	var got []saveLogEntry
	for _, e := range *entries {
		if e.msg == msg {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 %q record, got %d; all captured records: %+v", msg, len(got), *entries)
	}
	return got[0]
}

// countSaveRecords reports how many captured records carry the given message.
func countSaveRecords(entries *[]saveLogEntry, msg string) int {
	n := 0
	for _, e := range *entries {
		if e.msg == msg {
			n++
		}
	}
	return n
}

func TestSave_SingleFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, false, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "folder.jpg" {
		t.Errorf("saved = %v, want [folder.jpg]", saved)
	}

	// Verify file exists
	path := filepath.Join(dir, "folder.jpg")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("folder.jpg should exist")
	}
}

func TestSave_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "artist.jpg"}, false, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}

	for _, name := range saved {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("%s should exist", name)
		}
	}
}

func TestSave_LogoForcePNG(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Send a JPEG to be saved as a logo -- should convert to PNG
	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "logo", jpegData, []string{"logo.png"}, false, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "logo.png" {
		t.Errorf("saved = %v, want [logo.png]", saved)
	}

	// Verify the saved file is actually PNG
	data, err := os.ReadFile(filepath.Join(dir, "logo.png"))
	if err != nil {
		t.Fatal(err)
	}
	format, _, err := DetectFormat(bytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatPNG {
		t.Errorf("logo should be PNG, got %s", format)
	}
}

func TestSave_CleansUpConflicts(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create an existing folder.jpg
	oldPath := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Save a PNG thumb -- should delete the old JPG
	pngData := makePNG(t, 100, 100)
	_, err := Save(dir, "thumb", pngData, []string{"folder.jpg"}, false, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// folder.jpg should have been replaced (not the old content)
	newData, err := os.ReadFile(filepath.Join(dir, "folder.png"))
	if err != nil {
		t.Fatal(err)
	}
	format, _, err := DetectFormat(bytesReader(newData))
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatPNG {
		t.Errorf("expected PNG format, got %s", format)
	}

	// Old .jpg should be cleaned up
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old folder.jpg should have been deleted")
	}
}

func TestSave_NoFileNames_Error(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 10, 10)
	_, err := Save(dir, "thumb", jpegData, nil, false, nil, logger)
	if err == nil {
		t.Error("expected error for empty filenames")
	}
}

func TestSave_PNGThumb_KeepsPNG(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	pngData := makePNG(t, 100, 100)
	saved, err := Save(dir, "thumb", pngData, []string{"folder.jpg"}, false, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// PNG data sent but config says "folder.jpg" -- the extension should change to .png
	if len(saved) != 1 || saved[0] != "folder.png" {
		t.Errorf("saved = %v, want [folder.png]", saved)
	}
}

func TestSave_Symlinks(t *testing.T) {
	if !filesystem.ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "artist.jpg"}, true, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}

	// First file should be a regular file
	primaryPath := filepath.Join(dir, saved[0])
	fi, err := os.Lstat(primaryPath)
	if err != nil {
		t.Fatalf("Lstat primary: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("primary file should not be a symlink")
	}

	// Second file should be a symlink
	secondPath := filepath.Join(dir, saved[1])
	fi, err = os.Lstat(secondPath)
	if err != nil {
		t.Fatalf("Lstat second: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("second file should be a symlink")
	}

	// Symlink target should be relative (just the filename)
	target, err := os.Readlink(secondPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != saved[0] {
		t.Errorf("symlink target = %q, want %q", target, saved[0])
	}

	// Content should be readable through the symlink
	primaryData, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("reading primary: %v", err)
	}
	symlinkData, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}
	if !bytes.Equal(primaryData, symlinkData) {
		t.Error("content mismatch between primary and symlink")
	}
}

func TestSave_Symlinks_FanartException(t *testing.T) {
	if !filesystem.ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "fanart", jpegData, []string{"fanart.jpg", "backdrop.jpg"}, true, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}

	// Both files should be regular files (fanart exception)
	for _, name := range saved {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Lstat %s: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s should be a regular file (fanart exception), but is a symlink", name)
		}
	}
}

func TestSave_Symlinks_ExtensionCoercionDuplicate(t *testing.T) {
	if !filesystem.ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Both "folder.jpg" and "folder.png" resolve to "folder.jpg" when saving
	// JPEG data. Without the guard, the second entry would delete the primary
	// and create a self-referential symlink.
	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "folder.png"}, true, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only one file should be reported (the duplicate is skipped).
	if len(saved) != 1 {
		t.Fatalf("saved %d files, want 1; got %v", len(saved), saved)
	}
	if saved[0] != "folder.jpg" {
		t.Errorf("saved[0] = %q, want %q", saved[0], "folder.jpg")
	}

	// The file should be a regular file, not a symlink.
	fi, err := os.Lstat(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("folder.jpg should be a regular file, not a symlink")
	}

	// Verify the file is readable and valid.
	data, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	format, _, err := DetectFormat(bytesReader(data))
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("format = %q, want %q", format, FormatJPEG)
	}
}

func TestSave_OverwritesCaseMismatchedFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a differently-cased file on disk (simulates case-sensitive filesystem)
	if err := os.WriteFile(filepath.Join(dir, "Folder.JPG"), []byte("old data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Save with the canonical (lowercase) name
	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, false, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "folder.jpg" {
		t.Errorf("saved = %v, want [folder.jpg]", saved)
	}

	// The canonical file should exist with valid content
	newData, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("folder.jpg should exist: %v", err)
	}
	format, _, err := DetectFormat(bytesReader(newData))
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("format = %q, want %q", format, FormatJPEG)
	}

	// Verify exactly one file in the directory, named "folder.jpg".
	// We use ReadDir instead of os.Stat because Stat follows the underlying
	// filesystem's lookup rules, which are typically case-insensitive on
	// Windows and default macOS volumes and would treat "folder.jpg" and
	// "Folder.JPG" as the same file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 1 file, got %d: %v", len(entries), names)
	} else if entries[0].Name() != "folder.jpg" {
		t.Errorf("expected file named folder.jpg, got %s", entries[0].Name())
	}
}

func TestSave_Symlinks_SingleFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, true, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "folder.jpg" {
		t.Errorf("saved = %v, want [folder.jpg]", saved)
	}

	// Single file should be a regular file, no symlinks
	fi, err := os.Lstat(filepath.Join(dir, saved[0]))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("single file should not be a symlink")
	}
}

func TestSave_WithExifMeta(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	meta := &ExifMeta{Source: "fanarttv", Rule: "thumb_exists", Mode: "auto"}

	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, false, meta, logger)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("saved %d files, want 1", len(saved))
	}

	got, err := ReadProvenance(filepath.Join(dir, saved[0]))
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got == nil {
		t.Fatal("ReadProvenance returned nil")
	}
	if got.Source != "fanarttv" {
		t.Errorf("Source = %q, want %q", got.Source, "fanarttv")
	}
	if got.Rule != "thumb_exists" {
		t.Errorf("Rule = %q, want %q", got.Rule, "thumb_exists")
	}
	if got.Mode != "auto" {
		t.Errorf("Mode = %q, want %q", got.Mode, "auto")
	}
	if got.DHash == "" {
		t.Error("DHash is empty; Save should compute perceptual hash when meta.DHash is unset")
	}
}

func TestExpectedPaths(t *testing.T) {
	tests := []struct {
		name      string
		dir       string
		fileNames []string
		want      []string
	}{
		{
			name:      "single filename",
			dir:       "/music/Artist",
			fileNames: []string{"folder.jpg"},
			want: []string{
				"/music/Artist/folder.jpeg",
				"/music/Artist/folder.jpg",
				"/music/Artist/folder.png",
				"/music/Artist/folder.webp",
			},
		},
		{
			name:      "multiple filenames",
			dir:       "/music/Artist",
			fileNames: []string{"folder.jpg", "artist.jpg"},
			want: []string{
				"/music/Artist/folder.jpeg",
				"/music/Artist/folder.jpg",
				"/music/Artist/folder.png",
				"/music/Artist/folder.webp",
				"/music/Artist/artist.jpeg",
				"/music/Artist/artist.jpg",
				"/music/Artist/artist.png",
				"/music/Artist/artist.webp",
			},
		},
		{
			name:      "png input extension",
			dir:       "/music/Artist",
			fileNames: []string{"logo.png"},
			want: []string{
				"/music/Artist/logo.jpeg",
				"/music/Artist/logo.jpg",
				"/music/Artist/logo.png",
				"/music/Artist/logo.webp",
			},
		},
		{
			name:      "no extension",
			dir:       "/music/Artist",
			fileNames: []string{"fanart1"},
			want: []string{
				"/music/Artist/fanart1.jpeg",
				"/music/Artist/fanart1.jpg",
				"/music/Artist/fanart1.png",
				"/music/Artist/fanart1.webp",
			},
		},
		{
			name:      "empty filenames",
			dir:       "/music/Artist",
			fileNames: []string{},
			want:      []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpectedPaths(tt.dir, tt.fileNames)
			if len(got) != len(tt.want) {
				t.Fatalf("ExpectedPaths returned %d paths, want %d: %v", len(got), len(tt.want), got)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("ExpectedPaths[%d] = %q, want %q", i, g, tt.want[i])
				}
			}
		})
	}
}

// TestSave_FailedWriteEmitsAFailureRecord pins the failure record added for
// issue #2636. Save logged "saved image" for every file that worked and nothing
// at all for the one that did not, so an operator whose artwork never appeared
// had no greppable term for the failure anywhere in the production log. The
// error does reach the caller, but each caller wraps it into its own wording.
//
// The failure is provoked through the real writer: the target's parent is a
// FILE, so WriteFileAtomic's MkdirAll cannot create the directory and the write
// genuinely fails. Nothing about the record is stubbed.
func TestSave_FailedWriteEmitsAFailureRecord(t *testing.T) {
	root := t.TempDir()
	// A regular file where Save expects a directory. MkdirAll on a path whose
	// parent is a file returns ENOTDIR.
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seeding blocker file: %v", err)
	}
	dir := filepath.Join(blocker, "artist")

	// The handler must be level-permissive. "saved image" is emitted at Debug,
	// so a handler configured at Info filtered it out before the negative
	// assertion below ever saw it -- that assertion could not have detected a
	// Save that emitted BOTH records, which is exactly the regression it exists
	// to catch. captureSaveLogs is Enabled for every level.
	logger, entries := captureSaveLogs()
	targetPath := filepath.Join(dir, "folder.jpg")

	saved, err := Save(dir, "thumb", makeJPEG(t, 100, 100), []string{"folder.jpg"}, false, nil, logger)
	if err == nil {
		t.Fatal("Save: want an error when the underlying write fails, got nil")
	}
	// Precondition on the failure shape: nothing was written, so there is no
	// partial success that could account for a missing record.
	if len(saved) != 0 {
		t.Fatalf("saved = %v, want empty on a failed first write", saved)
	}
	// Precondition on the FIXTURE: the intended failure is ENOTDIR, because the
	// target's parent is a regular file. A bare `statErr == nil` check would
	// accept any unrelated setup error (a permissions problem, a path typo) and
	// so would not prove the fixture this test describes actually ran.
	if _, statErr := os.Stat(targetPath); !errors.Is(statErr, syscall.ENOTDIR) {
		t.Fatalf("precondition: stat of %s should fail with ENOTDIR (its parent is a regular file), got %v", targetPath, statErr)
	}

	e := requireOneSaveRecord(t, entries, "failed to save image")
	if e.level != slog.LevelError {
		t.Errorf("level = %v, want %v (a Warn or Info record is silenced by a routine level bump)", e.level, slog.LevelError)
	}
	if e.attrs["type"] != "thumb" {
		t.Errorf("type = %q, want %q", e.attrs["type"], "thumb")
	}
	// Asserted as a structured ATTRIBUTE, not as a substring of the rendered
	// line: the wrapped error text also contains "folder.jpg", so a substring
	// check passed even when the path attribute was absent.
	if e.attrs["path"] != targetPath {
		t.Errorf("path = %q, want %q (the operator greps this attribute to find the file that never appeared)", e.attrs["path"], targetPath)
	}
	if e.attrs["error"] == "" {
		t.Error("error attribute is empty; the record must name the failure")
	}
	// And the success record must NOT be there. This is only meaningful because
	// the handler above is Enabled at Debug: at Info it was filtered out and the
	// assertion could never fail.
	if n := countSaveRecords(entries, "saved image"); n != 0 {
		t.Errorf("a failed save must not also emit the success record; got %d; all records: %+v", n, *entries)
	}
}

// TestSave_SymlinkFallbackWriteFailureEmitsAFailureRecord covers the OTHER
// branch that returns a failed save: the symlink for a secondary filename
// cannot be created, and the WriteFileAtomic fallback then fails too. It is a
// distinct code path from the primary write, and before #2636 it was equally
// silent -- an operator would see the primary file appear and the alias simply
// not exist, with nothing in the log naming it.
//
// Both failures are provoked structurally rather than stubbed: the secondary
// target path is an existing non-empty DIRECTORY, so CreateRelativeSymlink
// fails (the directory cannot be unlinked to make room) and WriteFileAtomic's
// promoting rename onto that same directory fails too.
func TestSave_SymlinkFallbackWriteFailureEmitsAFailureRecord(t *testing.T) {
	dir := t.TempDir()
	// The alias name Save will try to create as a symlink, pre-occupied by a
	// non-empty directory so neither the symlink nor the fallback write can
	// take the name.
	blocked := filepath.Join(dir, "artist.jpg")
	if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o755); err != nil {
		t.Fatalf("seeding blocking directory: %v", err)
	}

	logger, entries := captureSaveLogs()

	saved, err := Save(dir, "thumb", makeJPEG(t, 100, 100), []string{"folder.jpg", "artist.jpg"}, true, nil, logger)
	if err == nil {
		t.Fatal("Save: want an error when both the symlink and its write fallback fail, got nil")
	}

	// Precondition on the failure SHAPE: the primary write succeeded, so this
	// really is the symlink-fallback branch and not the primary-write branch
	// wearing the same record.
	if len(saved) != 1 || saved[0] != "folder.jpg" {
		t.Fatalf("saved = %v, want [folder.jpg] (the primary must have been written before the alias failed)", saved)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "folder.jpg")); statErr != nil {
		t.Fatalf("precondition: the primary file should exist, stat err = %v", statErr)
	}
	// Precondition that the symlink leg was actually attempted and failed:
	// the blocker is still a directory, so nothing replaced it.
	info, statErr := os.Lstat(blocked)
	if statErr != nil {
		t.Fatalf("precondition: stat of the blocking path: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("precondition: %s should still be the blocking directory, got mode %v", blocked, info.Mode())
	}

	// The Warn that announces the fallback was taken.
	w := requireOneSaveRecord(t, entries, "symlink creation failed, falling back to copy")
	if w.level != slog.LevelWarn {
		t.Errorf("fallback-notice level = %v, want %v", w.level, slog.LevelWarn)
	}
	if w.attrs["target"] != blocked {
		t.Errorf("fallback-notice target = %q, want %q", w.attrs["target"], blocked)
	}

	// And the Error that says the save produced no file.
	e := requireOneSaveRecord(t, entries, "failed to save image")
	if e.level != slog.LevelError {
		t.Errorf("level = %v, want %v (a Warn or Info record is silenced by a routine level bump)", e.level, slog.LevelError)
	}
	if e.attrs["path"] != blocked {
		t.Errorf("path = %q, want %q", e.attrs["path"], blocked)
	}
	if e.attrs["type"] != "thumb" {
		t.Errorf("type = %q, want %q", e.attrs["type"], "thumb")
	}
	// saved_before_failure is what tells the operator how far the save got.
	if e.attrs["saved_before_failure"] != "1" {
		t.Errorf("saved_before_failure = %q, want %q", e.attrs["saved_before_failure"], "1")
	}
	if e.attrs["error"] == "" {
		t.Error("error attribute is empty; the record must name the failure")
	}

	// The returned error must name the path too, so a caller wrapping it keeps
	// the file identifiable.
	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("returned error = %v, want it to name %s", err, blocked)
	}
}
