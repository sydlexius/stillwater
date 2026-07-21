package image

import (
	"errors"
	"fmt"
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

// ResolveFanartNames returns EVERY fanart filename that could name an artist's
// fanart, in preference order: the caller's configured names (from the active
// platform profile, empty if none) UNIONED with the built-in fanart defaults,
// profile names FIRST, deduplicated case-insensitively.
//
// The union, not the configured list alone, is what makes fanart enumeration
// convention-agnostic. A profile states what Stillwater WRITES; it is not a
// statement about what the library already HOLDS. A profile configured with
// only "backdrop.jpg" over a directory of fanart.jpg files would otherwise
// enumerate zero, and an empty walk is a positive claim that nothing is there,
// which deletes registry rows for artwork still on disk (#2635). Matching the
// fixed default set the scanner resolves against keeps the two from disagreeing
// about the same directory. Profile names come FIRST so a directory matching
// two conventions resolves to the one the operator configured.
//
// An empty union (nothing configured and no defaults) is returned as an error
// rather than an empty slice, because callers enumerate against the result and
// an empty list would be indistinguishable from "found nothing on disk". The
// caller is responsible for propagating any active-profile lookup failure
// BEFORE calling this (i.e. never pass configured names guessed after a
// GetActive error).
func ResolveFanartNames(configured []string) ([]string, error) {
	defaults := FileNamesForType(DefaultFileNames, "fanart")
	out := make([]string, 0, len(configured)+len(defaults))
	seen := make(map[string]bool, len(configured)+len(defaults))
	for _, n := range append(append([]string{}, configured...), defaults...) {
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("no fanart naming patterns configured")
	}
	return out, nil
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

// FindExistingImageStrictVerifyDir is FindExistingImageStrict with one added
// guard: it stats dir itself before probing for files in it.
//
// FindExistingImageStrict only ever stats dir/<pattern>. If dir does not
// exist, every one of those child stats returns ENOENT, which
// FindExistingImageStrict's own errors.Is(err, fs.ErrNotExist) handling
// collapses into a clean ("", false, nil) miss -- the exact same shape as
// "directory present, file genuinely absent". A caller that cannot tell
// those two cases apart cannot tell "the library share is unmounted" from
// "this artist genuinely has no artwork".
//
// That collapse is harmless for ADDITIVE callers (deciding whether to save a
// new file over an existing one: "no directory yet" and "directory present,
// file absent" both correctly mean "nothing to overwrite"), which is why
// FindExistingImage and FindExistingImageStrict are left unchanged and most
// callers keep using them directly.
//
// It is not harmless for DESTRUCTIVE callers: anything that clears an
// exists_flag, deletes a file, or otherwise acts on "not found" as
// definitive absence. For those, a missing directory must be unverifiable
// -- surfaced as an error, exactly like the existing EACCES/EIO/ESTALE
// handling -- rather than read as "confirmed gone". A transient unmount of
// a shared cache or library volume must degrade to "skip and retry next
// cycle", never to "the artwork is gone, clear the flag" or "the artwork is
// gone, delete the row". See issue #2686.
func FindExistingImageStrictVerifyDir(dir string, patterns []string) (string, bool, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", false, fmt.Errorf("stat image dir %s: %w", dir, err)
	}
	return FindExistingImageStrict(dir, patterns)
}
