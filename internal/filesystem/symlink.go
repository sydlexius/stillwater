package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
)

// ProbeSymlinkSupport tests whether the given directory supports symlinks by
// creating a temporary file and symlink, verifying readlink, and cleaning up.
// Returns false on any failure (permission denied, unsupported filesystem, etc.).
func ProbeSymlinkSupport(dir string) bool {
	tmpFile := filepath.Join(dir, ".symlink_probe_target")
	tmpLink := filepath.Join(dir, ".symlink_probe_link")

	// Clean up on exit regardless of outcome.
	defer func() {
		_ = os.Remove(tmpLink)
		_ = os.Remove(tmpFile)
	}()

	if err := os.WriteFile(tmpFile, []byte("probe"), 0o600); err != nil {
		return false
	}

	// Use relative target so the symlink is portable within the directory.
	if err := os.Symlink(filepath.Base(tmpFile), tmpLink); err != nil {
		return false
	}

	target, err := os.Readlink(tmpLink)
	if err != nil {
		return false
	}

	return target == filepath.Base(tmpFile)
}

// CreateRelativeSymlink removes any existing file or symlink at linkPath and
// creates a new symlink pointing to filepath.Base(target) (a relative path
// within the same directory).
func CreateRelativeSymlink(target, linkPath string) error {
	relTarget := filepath.Base(target)

	// Remove existing file/symlink at linkPath if present.
	if _, err := os.Lstat(linkPath); err == nil {
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("removing existing file at link path: %w", err)
		}
	}

	if err := os.Symlink(relTarget, linkPath); err != nil {
		return fmt.Errorf("creating symlink %s -> %s: %w", linkPath, relTarget, err)
	}
	return nil
}
