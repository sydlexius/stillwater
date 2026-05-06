package image

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileNamesForType(t *testing.T) {
	naming := map[string][]string{
		"thumb":  {"folder.jpg", "artist.jpg"},
		"fanart": {"fanart.jpg"},
		"logo":   {"logo.png"},
	}

	tests := []struct {
		imageType string
		want      int
	}{
		{"thumb", 2},
		{"fanart", 1},
		{"logo", 1},
		{"banner", 0},
		{"unknown", 0},
	}

	for _, tt := range tests {
		t.Run(tt.imageType, func(t *testing.T) {
			got := FileNamesForType(naming, tt.imageType)
			if len(got) != tt.want {
				t.Errorf("FileNamesForType(%q) returned %d names, want %d", tt.imageType, len(got), tt.want)
			}
		})
	}
}

func TestPrimaryFileName(t *testing.T) {
	naming := map[string][]string{
		"thumb":  {"folder.jpg", "artist.jpg"},
		"fanart": {"fanart.jpg"},
	}

	if got := PrimaryFileName(naming, "thumb"); got != "folder.jpg" {
		t.Errorf("PrimaryFileName(thumb) = %q, want folder.jpg", got)
	}
	if got := PrimaryFileName(naming, "fanart"); got != "fanart.jpg" {
		t.Errorf("PrimaryFileName(fanart) = %q, want fanart.jpg", got)
	}
	if got := PrimaryFileName(naming, "banner"); got != "" {
		t.Errorf("PrimaryFileName(banner) = %q, want empty", got)
	}
}

func TestImageTermFor(t *testing.T) {
	tests := []struct {
		slot    string
		profile string
		want    string
	}{
		// Kodi uses filesystem-centric names
		{"thumb", "Kodi", "Folder"},
		{"fanart", "Kodi", "Fanart"},
		{"logo", "Kodi", "Logo"},
		{"banner", "Kodi", "Banner"},
		// Kodi case-insensitive
		{"thumb", "kodi", "Folder"},
		{"fanart", "KODI", "Fanart"},
		// Plex shares Kodi terminology
		{"thumb", "Plex", "Folder"},
		{"fanart", "Plex", "Fanart"},
		// Emby uses API-centric names
		{"thumb", "Emby", "Primary"},
		{"fanart", "Emby", "Backdrop"},
		{"logo", "Emby", "Logo"},
		{"banner", "Emby", "Banner"},
		// Jellyfin shares Emby terminology
		{"thumb", "Jellyfin", "Primary"},
		{"fanart", "Jellyfin", "Backdrop"},
		// Case-insensitive for Emby/Jellyfin
		{"thumb", "emby", "Primary"},
		{"fanart", "jellyfin", "Backdrop"},
		// Custom and unknown profiles use default terms
		{"thumb", "Custom", "Thumbnail"},
		{"fanart", "Custom", "Fanart"},
		{"thumb", "", "Thumbnail"},
		{"fanart", "SomeUnknown", "Fanart"},
		// Unknown slot returns empty string
		{"unknown", "Kodi", ""},
		{"unknown", "Emby", ""},
		{"unknown", "", ""},
	}

	for _, tt := range tests {
		name := tt.slot + "/" + tt.profile
		t.Run(name, func(t *testing.T) {
			got := ImageTermFor(tt.slot, tt.profile)
			if got != tt.want {
				t.Errorf("ImageTermFor(%q, %q) = %q, want %q", tt.slot, tt.profile, got, tt.want)
			}
		})
	}
}

func TestImageTermWithAttribution(t *testing.T) {
	tests := []struct {
		slot    string
		profile string
		want    string
	}{
		{"fanart", "Emby", "Backdrop (Emby)"},
		{"fanart", "Kodi", "Fanart (Kodi)"},
		{"thumb", "Jellyfin", "Primary (Jellyfin)"},
		{"thumb", "Plex", "Folder (Plex)"},
		{"logo", "Emby", "Logo (Emby)"},
		// Empty/whitespace profile returns default term without parentheses
		{"fanart", "", "Fanart"},
		{"thumb", "  ", "Thumbnail"},
		// Unknown slot returns empty
		{"unknown", "Emby", ""},
	}

	for _, tt := range tests {
		name := tt.slot + "/" + tt.profile
		t.Run(name, func(t *testing.T) {
			got := ImageTermWithAttribution(tt.slot, tt.profile)
			if got != tt.want {
				t.Errorf("ImageTermWithAttribution(%q, %q) = %q, want %q", tt.slot, tt.profile, got, tt.want)
			}
		})
	}
}

func TestAllSlots(t *testing.T) {
	if len(AllSlots) != 4 {
		t.Fatalf("AllSlots has %d entries, want 4", len(AllSlots))
	}
	// Verify order: thumb, fanart, logo, banner
	expected := []string{"thumb", "fanart", "logo", "banner"}
	for i, s := range expected {
		if AllSlots[i] != s {
			t.Errorf("AllSlots[%d] = %q, want %q", i, AllSlots[i], s)
		}
	}
}

// TestFindExistingImageStrict_FilePresent verifies that a present file is
// returned with found=true and no error (the success path).
func TestFindExistingImageStrict_FilePresent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(target, []byte("img"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, found, err := FindExistingImageStrict(dir, []string{"folder.jpg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

// TestFindExistingImageStrict_FileAbsent verifies that ENOENT is treated as a
// clean miss: found=false, err=nil. This is the only "not present" signal
// callers should trust for destructive actions.
func TestFindExistingImageStrict_FileAbsent(t *testing.T) {
	dir := t.TempDir()
	got, found, err := FindExistingImageStrict(dir, []string{"folder.jpg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Errorf("expected found=false, got true (path=%q)", got)
	}
}

// TestFindExistingImageStrict_DirAbsent verifies that probing a missing
// directory surfaces as ENOENT (clean miss, no error).
func TestFindExistingImageStrict_DirAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	_, found, err := FindExistingImageStrict(dir, []string{"folder.jpg"})
	if err != nil {
		t.Fatalf("ENOENT must surface as nil error, got %v", err)
	}
	if found {
		t.Error("expected found=false on missing dir")
	}
}

// TestFindExistingImageStrict_AlternateExtension verifies the alt-extension
// probe path: configured pattern is folder.jpg but actual file is folder.png.
func TestFindExistingImageStrict_AlternateExtension(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "folder.png")
	if err := os.WriteFile(target, []byte("img"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, found, err := FindExistingImageStrict(dir, []string{"folder.jpg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true via alternate extension")
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

// TestFindExistingImageStrict_PermissionDenied verifies that a stat error
// other than fs.ErrNotExist (here EACCES from an unreadable parent dir) is
// surfaced to the caller and probing stops. Without this, transient FS errors
// would silently masquerade as "file absent" and drive destructive writes.
func TestFindExistingImageStrict_PermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	parent := t.TempDir()
	child := filepath.Join(parent, "artist")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	_, found, err := FindExistingImageStrict(child, []string{"folder.jpg"})
	if err == nil {
		t.Fatal("expected non-nil error from permission-denied stat")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error must NOT be fs.ErrNotExist (got %v)", err)
	}
	if found {
		t.Error("expected found=false when error surfaces")
	}
}

// TestFindExistingImage_LooseWrapper verifies the loose wrapper preserves
// the legacy 2-return shape and treats every error as "not found". Callers
// that depend on this behavior (read-only consumers) must continue to work.
func TestFindExistingImage_LooseWrapper(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(target, []byte("img"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, found := FindExistingImage(dir, []string{"folder.jpg"})
	if !found || got != target {
		t.Errorf("FindExistingImage(present)=(%q,%v), want (%q,true)", got, found, target)
	}
	got, found = FindExistingImage(filepath.Join(t.TempDir(), "missing"), []string{"folder.jpg"})
	if found || got != "" {
		t.Errorf("FindExistingImage(absent)=(%q,%v), want (\"\",false)", got, found)
	}
}

// TestFindExistingImageStrict_PermissionDeniedAltExt covers the alt-extension
// branch's EACCES short-circuit. The primary pattern's stat must surface as
// ENOENT (so the loop continues into the alt-extension probes) while the
// alt-extension stat then returns a non-ENOENT error. Without this branch,
// callers driving destructive state on `found == false` could silently treat
// EACCES on an alt-named file as "absent".
func TestFindExistingImageStrict_PermissionDeniedAltExt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	// Layout:
	//   parent/                (0o755 throughout)
	//     visible/             (0o755) -- the dir we probe with patterns
	//   We make `visible` itself unreadable AFTER creating folder.png inside,
	//   so the alt-extension probe (folder.png) gets EACCES, but the primary
	//   probe (folder.jpg) also gets EACCES -- which is fine, the function
	//   returns the first non-ENOENT error encountered.
	//
	// To exercise specifically the alt-extension branch, we instead use a
	// pattern whose primary file is genuinely absent in a readable dir, then
	// place an unreadable subdir for the alt extension. But Stat does not
	// recurse, so a single dir with a stat-blocked file is what we need.
	// We achieve that by making the parent dir traversable but the file's
	// containing dir unreadable for `folder.png` only via a separate sub-path.
	//
	// Simpler approach: use a probe directory whose parent has exec bit
	// cleared. Stat on `dir/folder.jpg` returns EACCES, but errors.Is(err,
	// fs.ErrNotExist) is false on most systems for a directory traversal
	// failure, so the function returns immediately on the primary pattern.
	// That covers the primary-pattern EACCES branch already tested.
	//
	// To hit the *alt-extension* branch specifically, we need the primary
	// stat to return ENOENT and the alt stat to return EACCES. This is
	// achievable on Linux/macOS by making the dir traversable + readable
	// (so ENOENT is returned for missing names) but creating the alt file
	// with mode that makes Stat fail. Stat itself only needs parent
	// traversal, not file read perms, so we cannot block Stat with file
	// mode bits alone.
	//
	// Workaround: create a *symlink* whose target is inside an unreadable
	// directory. Stat follows symlinks; if the target's parent is
	// unreadable, Stat returns EACCES rather than ENOENT.
	parent := t.TempDir()
	probeDir := filepath.Join(parent, "probe")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll probe: %v", err)
	}
	hidden := filepath.Join(parent, "hidden")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatalf("MkdirAll hidden: %v", err)
	}
	target := filepath.Join(hidden, "folder.png")
	if err := os.WriteFile(target, []byte("img"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	// Symlink probe/folder.png -> hidden/folder.png. Then chmod hidden to
	// 0o000 so Stat (which follows symlinks) fails with EACCES on the alt
	// extension probe. The primary pattern (folder.jpg) does not exist in
	// probeDir and returns ENOENT, so the loop falls through to the alt
	// extension probe.
	link := filepath.Join(probeDir, "folder.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatalf("Chmod hidden: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })

	_, found, err := FindExistingImageStrict(probeDir, []string{"folder.jpg"})
	if err == nil {
		t.Fatal("expected non-nil error from alt-extension EACCES probe")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error must NOT be fs.ErrNotExist (got %v)", err)
	}
	if found {
		t.Error("expected found=false when error surfaces")
	}
}

func TestDefaultFileNames(t *testing.T) {
	if len(DefaultFileNames["thumb"]) == 0 {
		t.Error("DefaultFileNames should have thumb entries")
	}
	if len(DefaultFileNames["logo"]) == 0 {
		t.Error("DefaultFileNames should have logo entries")
	}
}
