package image

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultFileNames returns the default naming patterns for each image type.
// These are the known filename patterns used by media servers.
var DefaultFileNames = map[string][]string{
	"thumb":  {"folder.jpg", "folder.png", "artist.jpg", "artist.png", "poster.jpg", "poster.png"},
	"fanart": {"fanart.jpg", "fanart.png", "backdrop.jpg", "backdrop.png"},
	"logo":   {"logo.png", "logo-white.png"},
	"banner": {"banner.jpg", "banner.png"},
}

// FileNamesForType returns the configured filenames for an image type.
// Falls back to an empty slice if the type is unknown.
func FileNamesForType(naming map[string][]string, imageType string) []string {
	if names, ok := naming[imageType]; ok {
		return names
	}
	return nil
}

// PrimaryFileName returns the first filename for the given image type,
// or empty string if none are configured.
func PrimaryFileName(naming map[string][]string, imageType string) string {
	names := FileNamesForType(naming, imageType)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// FindExistingImage searches for the first matching image file in a directory.
// For each configured pattern it first checks the exact filename, then probes
// alternate supported extensions (.jpg, .png) to handle cases where the saved
// format differs from the configured name (e.g. a PNG crop saved over folder.jpg).
func FindExistingImage(dir string, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if _, err := os.Stat(p); err == nil { //nolint:gosec // path from trusted naming patterns
			return p, true
		}
		// Check alternate extensions in case the format changed after save.
		base := strings.TrimSuffix(pattern, filepath.Ext(pattern))
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			if ext == filepath.Ext(pattern) {
				continue
			}
			alt := filepath.Join(dir, base+ext)
			if _, err := os.Stat(alt); err == nil { //nolint:gosec // path from trusted naming patterns
				return alt, true
			}
		}
	}
	return "", false
}
