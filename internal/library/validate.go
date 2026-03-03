package library

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidatePath sanitizes and validates a library filesystem path.
// Empty paths are allowed (degraded/API-only libraries).
// Non-empty paths must be absolute and point to an existing directory.
// The returned path is cleaned via filepath.Clean.
func ValidatePath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("library path must be absolute: %q", raw)
	}

	cleaned := filepath.Clean(raw)

	info, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("library path does not exist: %q", cleaned)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("library path is not a directory: %q", cleaned)
	}

	return cleaned, nil
}
