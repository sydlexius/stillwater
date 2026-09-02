package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrInvalidSubdirName reports that a subdirectory name handed to
// ListArtworkSubdirFiles was not a single path element -- it contained a path
// separator, or was "." / "..". It is an error rather than an empty result
// for the same reason every other read failure here is: empty says "looked,
// nothing is there" and licenses the caller to proceed, and a name that could
// not be looked at safely licenses nothing.
var ErrInvalidSubdirName = errors.New("artwork subdirectory name must be a single path element")

// ListArtworkSubdirFiles enumerates the image files inside a named artwork
// subdirectory of an artist directory -- extrafanart/ today, and later
// extrathumbs/ when a caller needs it (#3177). subdirName is a PARAMETER
// rather than a hardcoded "extrafanart" so measuring a second such directory
// later costs a caller, not a redesign.
//
// This is NEW and ADDITIVE (#3177): it does not change DiscoverFanart,
// fanartMatches, ResolveFanart, or the scanner's discoverFanartFiles, all of
// which deliberately list only an artist directory's own entries and skip
// subdirectories -- extrafanart/ is invisible to every one of them, and
// nothing here alters that. The fanart set Stillwater pushes to a platform is
// unchanged; this function exists to make the population of that blind spot
// VISIBLE to a caller that wants to warn about it, not to fold it into
// discovery.
//
// ctx bounds the directory read (#2689), on the same terms as DiscoverFanart:
// a listing on an unresponsive network mount hangs exactly as completely as a
// file read.
//
// A read failure is returned as an error and NEVER as an empty result -- the
// two license OPPOSITE actions, matching ResolveFanart's doc comment (#2635):
// empty says "looked, nothing is there" and invites the caller to proceed; an
// error says "could not look" and must not.
//
// An ABSENT subdirectory is the common case, and it is NOT an error: it
// returns an empty result with a nil error, distinguishable from a read
// failure purely by the nil error (any other read failure -- permission
// denied, a stale handle, "not a directory" -- is reported as an error rather
// than folded into this case, since only true absence licenses treating it as
// "nothing is there").
//
// Only image files are returned -- the same extension allowlist fanartMatches
// applies (.jpg, .jpeg, .png, case-insensitive) -- and dotfiles and nested
// subdirectories are excluded, matching the filtering the artist merge path
// already applies to extrafanart/extrathumbs (see internal/artist's
// isAdditiveMergeDir and its callers).
//
// Results are returned sorted by filename, a deterministic order rather than
// filesystem enumeration order.
//
// subdirName must be a SINGLE path element -- no separator, no "." or "..".
// Anything else is rejected with ErrInvalidSubdirName rather than joined,
// because filepath.Join CLEANS its result: a subdirName of "../sibling" would
// resolve OUTSIDE artistDir and be enumerated as though it were inside it.
// Only a package constant is passed today, but this function is exported and
// advertised above as the extrathumbs/ extension point, so the containment is
// enforced here rather than left as an unwritten obligation on that caller.
//
// CASE SENSITIVITY IS THE FILESYSTEM'S, AND IT DIVERGES FROM internal/artist's
// isAdditiveMergeDir. subdirName reaches the OS verbatim, so querying
// "extrafanart" also finds "Extrafanart" on macOS/Windows and does not on
// Linux, while isAdditiveMergeDir folds case explicitly on every platform. The
// divergence is documented rather than removed: folding here would mean
// enumerating artistDir itself to find a match -- a second directory read, for
// a spelling neither Kodi nor Emby produces. isAdditiveMergeDir classifies a
// name already read off disk, so folding is free there; this function has to
// go LOOKING for one, so it is not. The observable consequence is that a
// mixed-case Extrafanart/ is warned about on macOS and silently not on Linux.
// TestListArtworkSubdirFiles_CaseFoldingIsTheFilesystems pins that so it stays
// a decision rather than an accident of the developer's filesystem.
func ListArtworkSubdirFiles(ctx context.Context, artistDir, subdirName string) ([]string, error) {
	if subdirName == "" {
		return nil, nil
	}
	// Containment: a single path element only. filepath.Base collapses
	// "a/b" -> "b", "../x" -> "x", "." -> "." and ".." -> "..", so requiring
	// Base(subdirName) == subdirName rejects every separator-bearing and
	// traversal form in one comparison. Checked BEFORE the Join, so an
	// escaping name is never turned into a path at all. A backslash is
	// rejected explicitly because it is an ordinary filename character on
	// Unix (filepath.Base would keep "..\\sibling" intact) while being a
	// separator on Windows -- accepting it would make the guard's strength
	// depend on GOOS.
	if filepath.Base(subdirName) != subdirName || subdirName == "." || subdirName == ".." ||
		strings.ContainsRune(subdirName, '\\') {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSubdirName, subdirName)
	}

	dir := filepath.Join(artistDir, subdirName)

	entries, err := readDirCtx(ctx, dir)
	if err != nil {
		if os.IsNotExist(err) {
			// The common case: no such subdirectory exists. Empty, not an
			// error -- see the doc comment above.
			return nil, nil
		}
		return nil, fmt.Errorf("reading artwork subdirectory %s: %w", dir, err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue // nested subdirectories are excluded
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue // dotfiles are excluded
		}
		// Same extension allowlist as fanartMatches in fanart.go: .jpg,
		// .jpeg, .png, case-insensitive. Duplicated rather than factored out
		// because fanartMatches is one of the functions #3177 forbids
		// changing, even to extract a shared helper from its body.
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			continue
		}
		names = append(names, name)
	}

	if len(names) == 0 {
		return nil, nil
	}

	sort.Strings(names)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths, nil
}
