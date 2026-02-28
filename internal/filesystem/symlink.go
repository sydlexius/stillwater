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
	// Use a unique temp file to avoid colliding with user files.
	f, err := os.CreateTemp(dir, ".symlink_probe_*")
	if err != nil {
		return false
	}
	tmpFile := f.Name()
	_ = f.Close()

	tmpLink := filepath.Join(dir, filepath.Base(tmpFile)+"_link") // #nosec G703 -- derived from our own temp file

	// Clean up only the files we created.
	defer func() {
		_ = os.Remove(tmpLink)
		_ = os.Remove(tmpFile)
	}()

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
