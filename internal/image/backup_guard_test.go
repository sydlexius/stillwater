package image

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupTypeDir_Guard pins the path-sanitization guard at the single backup
// chokepoint (backup.go backupTypeDir). The imageType segment is the only part
// of a backup path that originates from caller input, so the guard must accept
// exactly the closed artwork-kind allowlist and reject anything that could
// traverse out of the .sw-backup subtree (empty, separators, ".."). This closes
// the go/path-injection CodeQL class (alerts 93-103, 73) as defense-in-depth.
func TestBackupTypeDir_Guard(t *testing.T) {
	t.Parallel()
	const dir = "/music/Some Artist"

	t.Run("accepts the four valid artwork kinds", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"thumb", "fanart", "logo", "banner"} {
			got, err := backupTypeDir(dir, typ)
			if err != nil {
				t.Errorf("backupTypeDir(%q) returned error: %v", typ, err)
				continue
			}
			want := filepath.Join(dir, BackupDirName, typ)
			if got != want {
				t.Errorf("backupTypeDir(%q) = %q, want %q", typ, got, want)
			}
			// The type must stay inside the .sw-backup subtree.
			if !strings.HasPrefix(got, filepath.Join(dir, BackupDirName)+string(os.PathSeparator)) {
				t.Errorf("backupTypeDir(%q) = %q escaped the backup subtree", typ, got)
			}
		}
	})

	t.Run("rejects traversal, separators, and unknown types", func(t *testing.T) {
		t.Parallel()
		bad := []string{
			"",                                      // empty
			"a/b",                                   // embedded separator
			"../../etc",                             // parent traversal
			"..",                                    // bare traversal
			"logo/../..",                            // mixed traversal
			"thumb/x",                               // valid prefix but separator
			"unknown",                               // not in the allowlist
			"Thumb",                                 // case must match the allowlist exactly
			"thumb\x00",                             // NUL byte
			"./thumb",                               // dot-segment
			"logo" + string(os.PathSeparator) + "x", // OS-separator
		}
		for _, typ := range bad {
			got, err := backupTypeDir(dir, typ)
			if err == nil {
				t.Errorf("backupTypeDir(%q) = %q, want error (must fail closed)", typ, got)
			}
			if got != "" {
				t.Errorf("backupTypeDir(%q) returned a non-empty path %q on rejection", typ, got)
			}
		}
	})
}

// TestBackupPublicSurface_FailsClosedOnBadType verifies the public backup
// entrypoints propagate the guard rejection (fail closed) rather than touching
// the filesystem with a tainted type.
func TestBackupPublicSurface_FailsClosedOnBadType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if HasBackup(dir, "../../etc") {
		t.Errorf("HasBackup with a traversal type should report false")
	}

	// RestoreSingleSlot routes through findBackupFile -> backupTypeDir, so a bad
	// type must surface a guard error, NOT the os.ErrNotExist "no backup" sentinel.
	err := RestoreSingleSlot(dir, "a/b", []string{"poster.jpg"}, false, nil, nil)
	if err == nil {
		t.Fatalf("RestoreSingleSlot with a separator type should error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("RestoreSingleSlot guard rejection must not masquerade as os.ErrNotExist: %v", err)
	}
}
