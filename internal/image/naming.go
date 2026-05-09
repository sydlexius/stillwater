package image

import (
	"errors"
	"io/fs"
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

// ImageTermFor returns the platform-specific display label for an image slot.
// The profileName should be the platform profile name (e.g., "Kodi", "Emby",
// "Jellyfin", "Plex", "Custom"). Profile matching is case-insensitive.
//
// Terminology mapping:
//
//	Slot      | Kodi/Plex       | Emby/Jellyfin    | Default
//	----------+-----------------+------------------+---------
//	thumb     | Folder          | Primary          | Thumbnail
//	fanart    | Fanart          | Backdrop         | Fanart
//	logo      | Logo            | Logo             | Logo
//	banner    | Banner          | Banner           | Banner
func ImageTermFor(slot, profileName string) string {
	key := strings.ToLower(strings.TrimSpace(profileName))
	switch key {
	case "kodi", "plex":
		return kodiTerms[slot]
	case "emby", "jellyfin":
		return embyTerms[slot]
	}
	return defaultTerms[slot]
}

// ImageTermWithAttribution returns a platform-specific label with the platform
// name appended in parentheses, e.g. "Backdrop (Emby)" or "Fanart (Kodi)".
// Useful for multi-platform displays where the source must be clear.
func ImageTermWithAttribution(slot, profileName string) string {
	term := ImageTermFor(slot, profileName)
	if term == "" {
		return ""
	}
	if strings.TrimSpace(profileName) == "" {
		return term
	}
	return term + " (" + profileName + ")"
}

// kodiTerms maps internal slot names to Kodi/Plex display labels.
var kodiTerms = map[string]string{
	"thumb":  "Folder",
	"fanart": "Fanart",
	"logo":   "Logo",
	"banner": "Banner",
}

// embyTerms maps internal slot names to Emby/Jellyfin display labels.
var embyTerms = map[string]string{
	"thumb":  "Primary",
	"fanart": "Backdrop",
	"logo":   "Logo",
	"banner": "Banner",
}

// defaultTerms maps internal slot names to generic display labels.
// Used when no specific platform profile is active or for the "Custom" profile.
var defaultTerms = map[string]string{
	"thumb":  "Thumbnail",
	"fanart": "Fanart",
	"logo":   "Logo",
	"banner": "Banner",
}

// AllSlots returns the list of known image slot names in display order.
var AllSlots = []string{"thumb", "fanart", "logo", "banner"}

// FindExistingImage searches for the first matching image file in a directory.
// For each configured pattern it first checks the exact filename, then probes
// alternate supported extensions (.jpg, .png) to handle cases where the saved
// format differs from the configured name (e.g. a PNG crop saved over folder.jpg).
//
// This is a thin wrapper over FindExistingImageStrict that discards the error.
// Callers that branch on `found == false` to drive destructive state (clearing
// flags, deleting rows, overwriting NFOs) MUST call FindExistingImageStrict
// instead so transient stat errors (EACCES, EIO, ESTALE, ELOOP) are not
// silently treated as "file absent". See issue #1161.
func FindExistingImage(dir string, patterns []string) (string, bool) {
	path, found, _ := FindExistingImageStrict(dir, patterns)
	return path, found
}

// FindExistingImageStrict is the same as FindExistingImage but surfaces the
// first non-fs.ErrNotExist stat error encountered. Callers that act
// destructively on `found == false` should use this variant: an error means
// "we don't know whether the file is absent" and the caller should refuse to
// proceed (skip the destructive write, log, retry next cycle).
//
// On a clean miss (every probe returned fs.ErrNotExist), the result is
// ("", false, nil). On a hit, the result is (path, true, nil). On the first
// non-ENOENT error, the result is ("", false, err) and probing stops.
func FindExistingImageStrict(dir string, patterns []string) (string, bool, error) {
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if _, err := os.Stat(p); err == nil {
			return p, true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		// Check alternate extensions in case the format changed after save.
		base := strings.TrimSuffix(pattern, filepath.Ext(pattern))
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			if ext == filepath.Ext(pattern) {
				continue
			}
			alt := filepath.Join(dir, base+ext)
			if _, err := os.Stat(alt); err == nil {
				return alt, true, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", false, err
			}
		}
	}
	return "", false, nil
}
