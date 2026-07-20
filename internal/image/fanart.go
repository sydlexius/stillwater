package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// FanartFilename returns the correct filename for a fanart image at the given
// 0-based index. Index 0 returns the primary name unchanged. Index 1+ returns
// numbered variants following platform conventions.
//
// kodiNumbering controls the numbering offset for additional fanart:
//   - false (Emby/Jellyfin/Plex): index 1 -> base2.ext, index 2 -> base3.ext
//   - true  (Kodi):               index 1 -> base1.ext, index 2 -> base2.ext
func FanartFilename(primaryName string, index int, kodiNumbering bool) string {
	if index == 0 {
		return primaryName
	}
	ext := filepath.Ext(primaryName)
	base := strings.TrimSuffix(primaryName, ext)
	n := index + 1
	if kodiNumbering {
		n = index
	}
	return fmt.Sprintf("%s%d%s", base, n, ext)
}

// indexedFile pairs a discovery index with an absolute file path.
type indexedFile struct {
	index int
	path  string
}

// DiscoverFanart scans an artist directory and returns sorted absolute paths
// for all fanart files that match the primary name or its numbered variants.
// The primary name comes from the active platform profile (e.g., "backdrop.jpg"
// for Emby, "fanart.jpg" for Kodi). Files are returned in index order: primary
// first, then numbered variants sorted ascending.
func DiscoverFanart(dir string, primaryName string) ([]string, error) {
	if primaryName == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	return fanartPaths(fanartMatches(dir, entries, primaryName)), nil
}

// fanartMatches returns the fanart files among pre-read directory entries that
// match primaryName or its numbered variants, sorted by index and deduplicated
// so each index appears once.
//
// It takes entries rather than reading the directory itself so that a caller
// resolving across several naming conventions (ResolveFanart) pays for one
// os.ReadDir instead of one per convention.
func fanartMatches(dir string, entries []os.DirEntry, primaryName string) []indexedFile {
	if primaryName == "" {
		return nil
	}

	base := strings.TrimSuffix(primaryName, filepath.Ext(primaryName))
	baseLower := strings.ToLower(base)

	var files []indexedFile

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			continue
		}

		nameBase := strings.TrimSuffix(name, filepath.Ext(name))
		nameBaseLower := strings.ToLower(nameBase)

		// Primary (index 0): exact base match
		if nameBaseLower == baseLower {
			files = append(files, indexedFile{0, filepath.Join(dir, name)})
			continue
		}

		// Numbered variant: {base}{N} where N is a positive integer
		if strings.HasPrefix(nameBaseLower, baseLower) {
			suffix := nameBaseLower[len(baseLower):]
			if n, parseErr := strconv.Atoi(suffix); parseErr == nil && n > 0 {
				files = append(files, indexedFile{n, filepath.Join(dir, name)})
			}
		}
	}

	// Sort by index, then prefer the extension matching primaryName so that
	// when both backdrop.jpg and backdrop.png exist at index 0, only one is
	// returned. The preferred extension sorts first within each index group.
	primaryExt := strings.ToLower(filepath.Ext(primaryName))
	sort.Slice(files, func(i, j int) bool {
		if files[i].index != files[j].index {
			return files[i].index < files[j].index
		}
		ei := strings.ToLower(filepath.Ext(files[i].path))
		ej := strings.ToLower(filepath.Ext(files[j].path))
		if (ei == primaryExt) != (ej == primaryExt) {
			return ei == primaryExt
		}
		return files[i].path < files[j].path
	})

	// Deduplicate: keep only the first entry per index.
	out := make([]indexedFile, 0, len(files))
	lastIdx := -1
	for _, f := range files {
		if f.index == lastIdx {
			continue
		}
		lastIdx = f.index
		out = append(out, f)
	}
	return out
}

// fanartPaths projects a resolved match set down to its absolute paths.
func fanartPaths(files []indexedFile) []string {
	if len(files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.path)
	}
	return paths
}

// ResolveFanart discovers an artist directory's fanart across EVERY supplied
// naming convention, and reports which convention actually matched.
//
// It exists because resolving against a single presumed primary name is unsafe
// wherever the result bounds a DELETE. The active platform profile states which
// convention Stillwater WRITES; it is not evidence of what the library already
// HOLDS. An install whose profile says "backdrop.jpg" over a directory of
// fanart.jpg files gets a clean, error-free count of zero from DiscoverFanart --
// and a count of zero is a positive claim that every stored fanart row is stale,
// so the registry rows are deleted while every file is still on disk (#2635).
//
// Resolution mirrors the scanner's (scanner.discoverFanartFiles) in two passes,
// and the second pass is the point:
//
//   - Pass 1: the first convention whose PRIMARY file is on disk wins. A primary
//     present is the strongest available signal of which convention the library
//     uses, and checking it first keeps a directory holding both fanart.jpg and
//     backdrop2.jpg from resolving to the orphan.
//   - Pass 2 runs only when no convention has a primary, and accepts orphan
//     numbered variants -- fanart1.jpg with no fanart.jpg. That state is not
//     exotic: a slot delete that fails partway skips renumbering and leaves
//     exactly this shape.
//
// The returned name is the convention that matched, suitable for handing to
// FanartFilename or RenumberFanart. When nothing matches it is the first
// non-empty entry of names (the caller's preferred convention for new writes)
// and the path list is nil -- an honest "successfully looked, found none".
//
// A directory read failure is returned as an error and NEVER as an empty
// result, because the two license opposite actions.
func ResolveFanart(dir string, names []string) (string, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	preferred := ""
	matched := make([][]indexedFile, len(names))
	for i, name := range names {
		if name == "" {
			continue
		}
		if preferred == "" {
			preferred = name
		}
		matched[i] = fanartMatches(dir, entries, name)
		// Pass 1: index 0 present means this convention's primary is on disk.
		if len(matched[i]) > 0 && matched[i][0].index == 0 {
			return name, fanartPaths(matched[i]), nil
		}
	}

	// Pass 2: no primary under any convention, so orphan numbered variants may
	// still be present. Reusing the pass-1 match sets keeps this free.
	for i, name := range names {
		if len(matched[i]) > 0 {
			return name, fanartPaths(matched[i]), nil
		}
	}

	return preferred, nil, nil
}

// MaxFanartIndex scans an artist directory and returns the highest numeric
// suffix found among fanart files matching primaryName. Returns -1 if no
// fanart files exist. The primary file (exact base match) counts as index 0.
// This avoids overwriting existing files when gaps exist in the numbering
// sequence (e.g., fanart1.jpg deleted but fanart2.jpg still present).
func MaxFanartIndex(dir string, primaryName string) (int, error) {
	if primaryName == "" {
		return -1, nil
	}

	base := strings.TrimSuffix(primaryName, filepath.Ext(primaryName))
	baseLower := strings.ToLower(base)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	maxIdx := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			continue
		}

		nameBase := strings.TrimSuffix(name, filepath.Ext(name))
		nameBaseLower := strings.ToLower(nameBase)

		if nameBaseLower == baseLower {
			if maxIdx < 0 {
				maxIdx = 0
			}
			continue
		}

		if strings.HasPrefix(nameBaseLower, baseLower) {
			suffix := nameBaseLower[len(baseLower):]
			if n, parseErr := strconv.Atoi(suffix); parseErr == nil && n > 0 {
				if n > maxIdx {
					maxIdx = n
				}
			}
		}
	}

	return maxIdx, nil
}

// NextFanartIndex returns the correct 0-based index to pass to FanartFilename
// for the next fanart file, given the highest suffix currently on disk and
// whether Kodi numbering is active.
//
// For Kodi: suffix maps 1:1 to index, so next index = maxSuffix + 1.
// For non-Kodi (Emby/Jellyfin/Plex): suffix N corresponds to index N-1
// (e.g., backdrop2.jpg = index 1), so next index = maxSuffix (not maxSuffix+1).
// When no files exist (maxSuffix < 0), the next index is 0 so that callers
// can save the primary image first using FanartFilename(primaryName, 0, ...).
func NextFanartIndex(maxSuffix int, kodi bool) int {
	if maxSuffix < 0 {
		// No fanart files exist at all -- the caller should save the primary
		// (index 0) first. Return 0 so FanartFilename returns the primary name.
		return 0
	}
	if maxSuffix == 0 {
		// Only the primary exists. Next is index 1 for both conventions.
		return 1
	}
	if kodi {
		return maxSuffix + 1
	}
	// Non-Kodi: suffix N = index N-1, so next index = maxSuffix.
	return maxSuffix
}

// HashInvalidator drops the stored perceptual and content hashes for an
// artist's images of a given type, so that the next duplicate evaluation
// re-derives them from the files actually on disk.
//
// It is an interface here, rather than a concrete store, so that this package
// keeps depending on nothing but the filesystem.
type HashInvalidator interface {
	InvalidateImageHashes(ctx context.Context, artistID, imageType string) error
}

// RenumberFanart renames the given survivor paths so they occupy contiguous
// 0-based indices, then invalidates the artist's stored fanart hashes.
//
// Each file keeps its original extension. primaryName is the base name for
// index 0 (e.g. "backdrop.jpg"). dir is the parent directory. kodi controls the
// numbering convention (see FanartFilename).
//
// The invalidator is a required argument rather than an optional one because
// renumbering is precisely the operation that breaks the assumption the hash
// columns encode: hashes are stored per SLOT, and a renumber moves a different
// FILE into a slot while leaving that slot's row untouched. A stale hash is not
// merely a cache miss -- the exact-duplicate fixer deletes files on the strength
// of it, so a slot holding a neighbour's hash makes distinct artwork look like a
// byte-identical copy and get removed.
//
// Threading the invalidator through the signature is what stops that from
// recurring: a caller cannot renumber without confronting the hashes, because
// the code does not compile otherwise. Every previous version of this function
// left invalidation to the caller's memory, and every caller forgot.
//
// Hashes are cleared rather than recomputed. Clearing has exactly one meaning
// ("unknown"), which the detector already handles -- an empty hash never matches
// anything, including another empty one -- and it costs one re-read on the next
// evaluation. Recomputing would mean re-deriving the slot-to-file mapping at the
// one moment that mapping is in flux, which is the same reasoning that produced
// the bug.
func RenumberFanart(ctx context.Context, inv HashInvalidator, artistID, dir, primaryName string, survivors []string, kodi bool) error {
	if inv == nil {
		return fmt.Errorf("renumbering fanart in %s: no hash invalidator supplied", dir)
	}

	// Invalidate BEFORE the destructive rename, and unconditionally -- even
	// when survivors is empty. An empty-survivors call means every fanart
	// file for this artist just vanished, so there is MORE to invalidate in
	// that case, not less; returning early here (as a prior version of this
	// function did) walked straight past the one call that keeps the
	// compile-time "cannot renumber without confronting the hashes"
	// guarantee honest, and left the stale hash from the deleted slot ready
	// to falsely match whatever distinct image gets uploaded into it next.
	//
	// Ordering also matters for failure isolation. If invalidation ran AFTER
	// the rename (the previous shape of this function), an invalidation-only
	// failure -- a transient DB-busy error, unrelated to the filesystem --
	// surfaced after the survivors were already sitting at their new,
	// correct paths. The caller cannot tell that failure apart from a failed
	// rename, so it rolls back by restoring the tombed duplicates to their
	// ORIGINAL paths -- paths the just-renumbered survivors may now occupy,
	// silently overwriting distinct artwork with content that was supposed
	// to be permanently deleted. Invalidating first removes THAT race
	// entirely: if invalidation fails, this function returns before any file
	// moves, so the caller's rollback is safe WITH RESPECT TO AN
	// INVALIDATION FAILURE SPECIFICALLY (nothing on disk has changed yet).
	//
	// This is NOT a general "the caller's rollback is always safe" claim --
	// it narrows to the one trigger this reorder closes. renumberFanartFiles
	// below still has its own internal rollback paths (staging failures,
	// finalize failures), and if ONE of those best-effort rollbacks itself
	// only partially succeeds, a survivor can still end up sitting on a path
	// the caller's rollback would then overwrite. See restoreStaged's own
	// occupancy check in fixers.go for the hardening that covers that
	// remaining trigger; this ordering fixes the invalidation-failure
	// trigger, not every trigger.
	//
	// Reordering is also strictly safer than the reverse order for the hash
	// cache itself -- an empty hash never matches anything, so clearing
	// early can only ever cost an extra re-read on the next evaluation,
	// never a wrong-hash-based delete.
	if invErr := inv.InvalidateImageHashes(ctx, artistID, "fanart"); invErr != nil {
		return fmt.Errorf("invalidating fanart hashes for artist %s before renumber: %w", artistID, invErr)
	}

	if len(survivors) == 0 {
		return nil
	}

	return renumberFanartFiles(dir, primaryName, survivors, kodi)
}

// renumberFanartFiles performs the on-disk half of RenumberFanart. It is
// separate only so the two-phase rename can be tested without a hash store;
// production code must go through RenumberFanart, which cannot skip the
// invalidation.
//
//nolint:gocognit // Two-phase rename (stage to .tmp then commit to final name) with best-effort rollback in both phases; the rollback walks the already-mutated subset of files so the partial-failure recovery has to remain inline alongside the forward path.
func renumberFanartFiles(dir, primaryName string, survivors []string, kodi bool) error {
	if len(survivors) == 0 {
		return nil
	}

	// Phase 0: compute every survivor's staging path and clear any leftover
	// temp file from a previous crashed operation, for ALL survivors, BEFORE
	// any survivor is staged (renamed away from its current path).
	//
	// This is hoisted out of the staging loop on purpose -- same medicine as
	// the RenumberFanart invalidate-before-rename reorder: do the fallible,
	// non-destructive step FIRST, so a failure costs nothing. Sweeping stale
	// .tmp files inline within the staging loop (the previous shape) had
	// exactly one asymmetric exit: an os.Remove failure at survivor i left
	// survivors 0..i-1 already staged at their .tmp names with NO rollback
	// (the os.Rename failure branch four lines below it DOES roll back; this
	// one did not). Those stranded .tmp files are invisible to
	// DiscoverFanart, so the caller's restoreStaged() -- which only knows
	// about tombed duplicates, not stranded survivors -- would restore the
	// duplicate while the stranded originals stayed vanished. The NEXT
	// renumber's stale-tmp sweep would then find those same .tmp paths,
	// remove them cleanly, and permanently unlink the stranded originals.
	// Doing the whole sweep before any file moves makes that sequence
	// structurally unreachable rather than correctly recoverable.
	type staged struct {
		tmpPath string
		ext     string
	}
	stagedFiles := make([]staged, len(survivors))
	for i, oldPath := range survivors {
		ext := filepath.Ext(oldPath)
		tmpName := fmt.Sprintf("fanart_renumber_%d%s.tmp", i, ext)
		tmpPath := filepath.Join(dir, tmpName)
		if removeErr := os.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("clearing stale temp file %s: %w", tmpName, removeErr)
		}
		stagedFiles[i] = staged{tmpPath: tmpPath, ext: ext}
	}

	// Phase 1: stage all survivors to temporary names to avoid collisions
	// when renaming (e.g., fanart1->fanart0 while fanart0 still exists).
	for i, oldPath := range survivors {
		filesystem.TraceFSWrite("Rename(stage)", stagedFiles[i].tmpPath, 0)
		if err := os.Rename(oldPath, stagedFiles[i].tmpPath); err != nil {
			// Best-effort rollback of already-staged files.
			var rollbackErrs []string
			for rollback := range i {
				if rbErr := os.Rename(stagedFiles[rollback].tmpPath, survivors[rollback]); rbErr != nil { //nolint:gosec // rollback index bounded by loop range
					rollbackErrs = append(rollbackErrs, fmt.Sprintf("restore %s: %v", filepath.Base(stagedFiles[rollback].tmpPath), rbErr))
				}
			}
			if len(rollbackErrs) > 0 {
				return fmt.Errorf("staging %s for renumber: %w (rollback errors: %s)", filepath.Base(oldPath), err, strings.Join(rollbackErrs, "; "))
			}
			return fmt.Errorf("staging %s for renumber: %w", filepath.Base(oldPath), err)
		}
	}

	// Phase 2: rename staged files to their final contiguous names.
	// Track finalized files for rollback on failure.
	type finalized struct {
		finalPath string
		tmpPath   string
	}
	var done []finalized
	var phase2Err error
	for i, sf := range stagedFiles {
		newName := FanartFilename(primaryName, i, kodi)
		newBase := strings.TrimSuffix(newName, filepath.Ext(newName))
		finalName := newBase + sf.ext
		finalPath := filepath.Join(dir, finalName)
		filesystem.TraceFSWrite("Rename(finalize)", finalPath, 0)
		if err := os.Rename(sf.tmpPath, finalPath); err != nil {
			phase2Err = fmt.Errorf("renaming %s to %s: %w", filepath.Base(sf.tmpPath), finalName, err)
			break
		}
		done = append(done, finalized{finalPath: finalPath, tmpPath: sf.tmpPath})
	}
	if phase2Err != nil {
		// Best-effort rollback: revert finalized files to tmp, then restore originals.
		var rollbackErrs []string
		for _, f := range done {
			if rbErr := os.Rename(f.finalPath, f.tmpPath); rbErr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("revert %s: %v", filepath.Base(f.finalPath), rbErr))
			}
		}
		for i, sf := range stagedFiles {
			if rbErr := os.Rename(sf.tmpPath, survivors[i]); rbErr != nil { //nolint:gosec // stagedFiles and survivors have same length
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("restore %s: %v", filepath.Base(sf.tmpPath), rbErr))
			}
		}
		if len(rollbackErrs) > 0 {
			return fmt.Errorf("%w (rollback errors: %s)", phase2Err, strings.Join(rollbackErrs, "; "))
		}
		return phase2Err
	}
	return nil
}
