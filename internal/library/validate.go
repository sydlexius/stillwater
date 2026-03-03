package library

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidatePath sanitizes and validates a library filesystem path.
// Empty paths are allowed (degraded/API-only libraries).
// Non-empty paths must be absolute; the returned path is cleaned via
// filepath.Clean. Call CheckPathExists on the result to verify the
// target is an existing directory.
func ValidatePath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("library path must be absolute: %q", raw)
	}

	return filepath.Clean(raw), nil
}

// CheckPathExists verifies that path exists and is a directory.
func CheckPathExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("library path does not exist: %q", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("library path is not a directory: %q", path)
	}
	return nil
}
