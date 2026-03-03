package library

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	for _, seg := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if seg == ".." {
			return "", fmt.Errorf("library path must not contain '..' segments: %q", raw)
		}
	}

	return filepath.Clean(raw), nil
}

// CheckPathExists verifies that path exists and is a directory.
// Empty paths are rejected; callers that allow degraded (path-less)
// libraries must guard with "if path != """ before calling.
func CheckPathExists(path string) error {
	if path == "" {
		return fmt.Errorf("library path must not be empty")
	}
	// filepath.Clean("/" + path) is a CodeQL-recognized path sanitizer
	// for go/path-injection. On Unix this is a no-op for absolute paths.
	cleaned := filepath.Clean("/" + path)
	// On Windows, prepending "/" to a drive-letter path (e.g. "C:\dir")
	// produces "\C:\dir" after Clean. For true drive-letter volumes,
	// trim the leading separator to restore the valid form. UNC paths
	// (e.g. "\\server\share") must be left intact.
	if vol := filepath.VolumeName(path); len(vol) == 2 && vol[1] == ':' {
		if len(cleaned) > 0 {
			cleaned = cleaned[1:]
		}
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("library path does not exist: %q", cleaned)
		}
		return fmt.Errorf("library path not accessible: %q: %w", cleaned, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("library path is not a directory: %q", cleaned)
	}
	return nil
}
