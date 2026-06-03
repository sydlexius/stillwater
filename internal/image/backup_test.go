package image

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// makeImageBytes returns encoded image bytes in the requested format ("jpeg" or
// "png") so backup tests exercise the real Save path (which detects format).
func makeImageBytes(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 16), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	default:
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		t.Fatalf("encoding %s: %v", format, err)
	}
	return buf.Bytes()
}

func TestBackupSingleSlot_CreatesPeerInertCopy(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "folder.jpg")
	orig := []byte("original-bytes")
	if err := os.WriteFile(canonical, orig, 0o644); err != nil {
		t.Fatalf("seeding canonical: %v", err)
	}

	if err := BackupSingleSlot(dir, "thumb", []string{"folder.jpg", "folder.png"}); err != nil {
		t.Fatalf("BackupSingleSlot: %v", err)
	}

	// Backup lives inside the hidden per-type subdir, keyed by image TYPE,
	// preserving the original basename (and format).
	want := filepath.Join(dir, BackupDirName, "thumb", "folder.jpg")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("reading backup at %s: %v", want, err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("backup bytes = %q, want %q", got, orig)
	}
	// Canonical file is untouched by the backup op.
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical must still exist after backup: %v", err)
	}
}

func TestBackupSingleSlot_OneDeepOverwrite(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "logo.png")
	if err := os.WriteFile(canonical, []byte("v1"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := BackupSingleSlot(dir, "logo", []string{"logo.png", "logo-white.png"}); err != nil {
		t.Fatalf("backup v1: %v", err)
	}
	// Second edit overwrites the canonical, then re-backs-up.
	if err := os.WriteFile(canonical, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if err := BackupSingleSlot(dir, "logo", []string{"logo.png", "logo-white.png"}); err != nil {
		t.Fatalf("backup v2: %v", err)
	}
	// Exactly one backup file should remain (one-deep) and it should hold v2.
	typeDir := filepath.Join(dir, BackupDirName, "logo")
	entries, err := os.ReadDir(typeDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one backup, got %d", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(typeDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("one-deep backup = %q, want most-recent pre-edit %q", got, "v2")
	}
}

func TestRestoreSingleSlot_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	naming := []string{"folder.jpg", "folder.png"}
	canonical := filepath.Join(dir, "folder.jpg")
	orig := makeImageBytes(t, "jpeg")
	if err := os.WriteFile(canonical, orig, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := BackupSingleSlot(dir, "thumb", naming); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// Simulate a destructive edit.
	if err := os.WriteFile(canonical, makeImageBytes(t, "png"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := RestoreSingleSlot(dir, "thumb", naming, false, nil, testLogger(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	// Save re-encodes via WriteFileAtomic of the original bytes (no transform for
	// jpeg input), so the canonical must decode to the same dimensions as orig.
	if !sameDecodedSize(t, got, orig) {
		t.Errorf("restored image does not match original dimensions")
	}
	// Restore consumes the backup so a second revert is a no-backup error.
	if HasBackup(dir, "thumb") {
		t.Error("backup should be consumed after restore")
	}
}

// TestRestoreSingleSlot_FormatChangeRevertible proves the F4 fix: a png original
// cropped to jpg is still revertible (backup is keyed by type, not basename) and
// restore drops the post-edit jpg, leaving the original png.
func TestRestoreSingleSlot_FormatChangeRevertible(t *testing.T) {
	dir := t.TempDir()
	naming := []string{"folder.jpg", "folder.png"}
	origPNG := makeImageBytes(t, "png")
	if err := os.WriteFile(filepath.Join(dir, "folder.png"), origPNG, 0o644); err != nil {
		t.Fatalf("seed png: %v", err)
	}
	// Back up the png original.
	if err := BackupSingleSlot(dir, "thumb", naming); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// Simulate the crop: Save jpg data, which deletes folder.png and writes folder.jpg.
	if _, err := Save(dir, "thumb", makeImageBytes(t, "jpeg"), naming, false, nil, testLogger(t)); err != nil {
		t.Fatalf("save jpg crop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "folder.png")); !os.IsNotExist(err) {
		t.Fatalf("expected folder.png removed after jpg save, err=%v", err)
	}
	// Backup must still be findable even though the on-disk format changed.
	if !HasBackup(dir, "thumb") {
		t.Fatal("backup must survive a format-changing edit")
	}
	// Revert: should rebuild folder.png and drop folder.jpg.
	if err := RestoreSingleSlot(dir, "thumb", naming, false, nil, testLogger(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "folder.png")); err != nil {
		t.Errorf("folder.png should be restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "folder.jpg")); !os.IsNotExist(err) {
		t.Errorf("post-edit folder.jpg should be cleaned up, err=%v", err)
	}
	if HasBackup(dir, "thumb") {
		t.Error("backup should be consumed after restore")
	}
}

func TestRestoreSingleSlot_NoBackupReturnsNotExist(t *testing.T) {
	dir := t.TempDir()
	if err := RestoreSingleSlot(dir, "thumb", []string{"folder.jpg"}, false, nil, testLogger(t)); !os.IsNotExist(err) {
		t.Errorf("RestoreSingleSlot with no backup err = %v, want os.IsNotExist", err)
	}
}

func TestHasBackup(t *testing.T) {
	dir := t.TempDir()
	if HasBackup(dir, "banner") {
		t.Fatal("no backup expected initially")
	}
	if err := os.WriteFile(filepath.Join(dir, "banner.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := BackupSingleSlot(dir, "banner", []string{"banner.jpg", "banner.png"}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !HasBackup(dir, "banner") {
		t.Error("HasBackup should report true after backup")
	}
}

// TestBackupSingleSlot_NoOriginalNoOp confirms a first write (no prior original)
// leaves no backup.
func TestBackupSingleSlot_NoOriginalNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := BackupSingleSlot(dir, "thumb", []string{"folder.jpg", "folder.png"}); err != nil {
		t.Fatalf("BackupSingleSlot no-original: %v", err)
	}
	if HasBackup(dir, "thumb") {
		t.Error("no backup should exist when there was no original")
	}
}

// sameDecodedSize reports whether two encoded images decode to the same bounds.
func sameDecodedSize(t *testing.T, a, b []byte) bool {
	t.Helper()
	ca, _, err := image.DecodeConfig(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("decode a: %v", err)
	}
	cb, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode b: %v", err)
	}
	return ca.Width == cb.Width && ca.Height == cb.Height
}
