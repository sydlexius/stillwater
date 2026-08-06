package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
// ctx bounds the directory read (#2689): a listing on an unresponsive network
// mount hangs exactly as completely as a file read, and this helper sits under
// the remediation handlers whose singleton a wedged request never releases.
// Only the ReadDir is I/O; fanartMatches below is pure matching over the
// already-read entries and needs no context.
func DiscoverFanart(ctx context.Context, dir string, primaryName string) ([]string, error) {
	if primaryName == "" {
		return nil, nil
	}

	entries, err := readDirCtx(ctx, dir)
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
// result, because the two outcomes license OPPOSITE actions: an empty result
// says "looked, nothing is there" and invites deleting stale rows, while an
// error says "could not look" and must leave everything untouched.
//
// ctx bounds the single directory read (#2689). One ReadDir serves every
// convention -- that is already why fanartMatches takes pre-read entries --
// so there is exactly one blocking call here to bound.
func ResolveFanart(ctx context.Context, dir string, names []string) (string, []string, error) {
	entries, err := readDirCtx(ctx, dir)
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
//
// ctx bounds the directory read (#2689), on the same terms as DiscoverFanart.
func MaxFanartIndex(ctx context.Context, dir string, primaryName string) (int, error) {
	if primaryName == "" {
		return -1, nil
	}

	base := strings.TrimSuffix(primaryName, filepath.Ext(primaryName))
	baseLower := strings.ToLower(base)

	entries, err := readDirCtx(ctx, dir)
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

// HashInvalidator drops the stored per-slot facts for an artist's images of a
// given type -- the perceptual and content hashes, and the geometry -- so that
// the next reader re-derives them from the files actually on disk.
//
// It is an interface here, rather than a concrete store, so that this package
// keeps depending on nothing but the filesystem.
//
// Geometry joined the hashes here (#2713) because it fails the same way for
// the same reason: artist_images.width/height are keyed per SLOT, and a
// renumber moves a different FILE into a slot without touching its row. The
// image rules read those columns in preference to the file, so a slot left
// holding a neighbor's dimensions makes the rules judge a picture that is no
// longer there -- a square backdrop reported as the wrong shape, or a
// full-resolution one reported as too small.
type HashInvalidator interface {
	InvalidateImageHashes(ctx context.Context, artistID, imageType string) error

	// InvalidateImageGeometry zeroes the stored width/height for the type.
	//
	// Zeroing rather than recomputing is deliberate, and is the same
	// argument RenumberFanart makes for the hashes below: zero has exactly
	// one meaning ("unknown"), and the rule resolver already handles it by
	// falling back to measuring the file. Recomputing would mean re-deriving
	// the slot-to-file mapping at the one moment that mapping is in flux,
	// which is precisely how the staleness arose.
	InvalidateImageGeometry(ctx context.Context, artistID, imageType string) error
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

	// Geometry is invalidated on the same terms and for the same reason
	// (#2713): it is a per-slot column about to describe a file that is no
	// longer in that slot. It runs BEFORE the rename for the identical
	// failure-isolation argument made above -- a DB-busy error here returns
	// with nothing on disk changed, so the caller's rollback stays safe.
	if invErr := inv.InvalidateImageGeometry(ctx, artistID, "fanart"); invErr != nil {
		return fmt.Errorf("invalidating fanart geometry for artist %s before renumber: %w", artistID, invErr)
	}

	if len(survivors) == 0 {
		return nil
	}

	return renumberFanartFiles(dir, primaryName, survivors, kodi)
}

// quarantineMarker is the infix stamped into a quarantined file's name. An
// INFIX rather than a trailing suffix, deliberately: see quarantineStrandedTemp.
const quarantineMarker = ".orphan-"

// quarantineNow is time.Now in production and a pinned clock in tests. It
// exists so the collision path in quarantineStrandedTemp can be reached at
// all: with a real clock, two quarantines get distinct names from the stamp
// and the refuse-to-clobber guard is never executed, so a test claiming to
// cover it would pass with the guard deleted.
var quarantineNow = time.Now

// quarantineRaceHook, when non-nil, runs after the staging path has been
// stat'd and before it is linked. Test-only: it is the only way to simulate a
// concurrent run clearing the source in that window, since a source that is
// absent up front never reaches the link at all. nil in production.
var quarantineRaceHook func(path string)

// quarantinePostLinkHook, when non-nil, runs after the quarantine link is
// committed and before the source is unlinked. Test-only, and it exists for a
// structural reason rather than convenience: the removal-failure branch needs a
// read-only parent directory, while the link that precedes it needs that same
// directory writable, so no single filesystem state reaches the branch. nil in
// production.
var quarantinePostLinkHook func(path string)

// quarantineStrandedTemp moves whatever sits at path aside instead of deleting
// it, and is a no-op when nothing is there (#2460).
//
// WHY NOT os.Remove, WHICH IS WHAT THIS REPLACED. A survivor's staging path
// holds one of two things, and nothing in the filename distinguishes them:
//
//  1. Inert junk left by a previously FAILED renumber. Deleting it is correct
//     and deleting it is what the old code did.
//  2. REAL, ONLY-COPY artwork stranded by a HARD crash -- power loss, SIGKILL,
//     OOM -- landing between the two rename phases below. The file has been
//     moved off its final name but not yet onto its new one, so this staging
//     path is the only place it exists on disk.
//
// The old sweep assumed (1) unconditionally, so every crash of shape (2) had
// its artwork silently unlinked by the next ordinary renumber, with no error
// and nothing in any log. Quarantining costs one rename in case (1) and saves
// the only copy in case (2).
//
// This is the CRASH-path twin of #2459, which fixed the ERROR path by hoisting
// this sweep ahead of any staging. That hoist made a transient I/O failure
// harmless; it could not help here, because a hard crash leaves no error to
// handle -- the process is simply gone.
//
// THE NAME KEEPS THE ORIGINAL EXTENSION TERMINAL, and that is load-bearing
// rather than cosmetic. The obvious shape, appending a suffix
// (fanart_renumber_0.jpg.orphan-<ts>), makes filepath.Ext return
// ".orphan-<ts>" -- so every consumer that classifies by extension stops
// recognizing the file as an image. internal/foreign's isForeignCandidate is
// exactly such a consumer: it looks the extension up in its imageExtensions map
// and returns false before it ever reaches its prefix matching. Since surfacing
// these orphans to an operator is the deferred follow-up to this fix (#2954),
// a name
// that cannot be classified would quietly foreclose it.
//
// A crash can recur, and a second quarantine that overwrote the first would
// destroy the earlier only-copy -- reintroducing this very bug one level up.
// UNIQUENESS COMES FROM THE COLLISION LOOP BELOW, not from the timestamp: the
// stamp's format carries nanosecond digits but the clock does not fill them
// (darwin is ~microsecond-granular, so consecutive calls repeat a stamp
// readily). Treating the stamp as the guarantee would be a comment that is
// format-true and behavior-false.
func quarantineStrandedTemp(path, ext string) error {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil // The ordinary case: nothing stranded, nothing to do.
		}
		return statErr
	}

	// ONLY A REGULAR FILE IS QUARANTINED. Anything else at a staging path --
	// a directory, a symlink, a socket -- is not crash-stranded artwork, so
	// preserving it serves no one, and MOVING it would be strictly worse than
	// the os.Remove this replaced: rename succeeds on a directory where remove
	// fails, so the sweep would quietly proceed into a two-phase rename over a
	// path something else is occupying.
	//
	// That is not hypothetical. #2459's error-path guard is built on this step
	// being FALLIBLE: internal/rule's stale-tmp-sweep tests squat the staging
	// path with a non-empty directory precisely so the sweep fails and the
	// whole operation aborts before any survivor is staged. A quarantine that
	// renamed the squatter away would have silently retired that guard --
	// making the crash path safe while making the error path unsafe. Failing
	// here keeps both.
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file (mode %s); refusing to touch it",
			filepath.Base(path), info.Mode().Type())
	}

	base := strings.TrimSuffix(filepath.Base(path), ".tmp")
	base = strings.TrimSuffix(base, ext)
	dir := filepath.Dir(path)
	// quarantineNow is a seam, not indirection for its own sake: with a real
	// clock the collision branch below is unreachable from a test, because two
	// quarantines land in different microseconds and get distinct names from
	// the STAMP rather than from the guard. Pinning the clock is what makes the
	// guard the only thing standing between two files and one.
	stamp := quarantineNow().UTC().Format("20060102T150405.000000000Z")

	// quarantineRaceHook is nil in production. It exists because the ENOENT
	// branch on the link below is otherwise UNREACHABLE from a test: a file
	// that is absent up front returns at the Lstat above, so the only way to
	// reach the link with a missing source is for it to vanish BETWEEN the two
	// -- which is exactly the concurrent-clear the branch handles. Without this
	// seam a test aimed at that branch passes via the Lstat early return
	// instead, proving nothing (measured: the mutation survived).
	if quarantineRaceHook != nil {
		quarantineRaceHook(path)
	}

	// os.Link + os.Remove, NOT os.Rename.
	//
	// An earlier version used Lstat-then-Rename and justified it with "a plain
	// rename is atomic within a directory". That premise is TRUE and the
	// conclusion did not follow: rename's atomicity means the SOURCE is never
	// nameless, and says nothing about the DESTINATION being unoccupied --
	// which is the only property this guard needs. os.Rename SILENTLY
	// OVERWRITES an existing regular file (verified), so the Lstat was a
	// check-then-act with a window between them: two quarantines computing the
	// same dest could both pass the Lstat, and the second would destroy the
	// first's only copy. Renumbering takes no lock (see backup.go's
	// "unlocked renumbering renames") and five call sites can reach it
	// concurrently, so that window is reachable rather than theoretical.
	//
	// os.Link fails with EEXIST when dest exists, making the refusal ATOMIC --
	// the kernel does the checking, so there is no window to lose. The cost is
	// a moment where the file exists under BOTH names, which is strictly safer
	// than a moment where the destination is silently overwritten.
	for attempt := range 100 {
		name := fmt.Sprintf("%s%s%s%s", base, quarantineMarker, stamp, ext)
		if attempt > 0 {
			name = fmt.Sprintf("%s%s%s-%d%s", base, quarantineMarker, stamp, attempt, ext)
		}
		dest := filepath.Join(dir, name)
		filesystem.TraceFSWrite("Link(quarantine)", dest, 0)
		if err := os.Link(path, dest); err != nil {
			// EEXIST: something already holds this name. Try the next counter
			// rather than clobbering it -- it may be somebody's only copy.
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			// ENOENT: the SOURCE is gone. Renumbering takes no lock, so a
			// concurrent run can clear the staging path between the Lstat above
			// and this link. That is not a failure -- this function's entire
			// purpose is "nothing stranded at this path", and nothing is. Note
			// the fail-safe direction is OPPOSITE to the EEXIST case one branch
			// up: a destination that already exists must never read as success
			// (it may be an earlier only-copy), while a source that is already
			// gone genuinely is success. Aborting here would fail a renumber
			// for having gotten what it wanted.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		// Test-only seam, nil in production. The removal-failure branch below is
		// otherwise unreachable: making the unlink fail needs a read-only
		// parent directory, but the link above needs that same directory
		// WRITABLE, so no single directory mode reaches it. The failure has to
		// be introduced BETWEEN the two operations, which is what this hook is.
		if quarantinePostLinkHook != nil {
			quarantinePostLinkHook(path)
		}

		// The link is committed, so the bytes are now safe under dest no matter
		// what happens next. Unlinking the source can fail (a read-only
		// directory, say) without risking the artwork; report it rather than
		// leaving the caller believing the staging path is clear, since the
		// two-phase rename below is about to want it.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// ENOENT is tolerated for the same reason as on the link above: a
			// concurrent run may have cleared the source, and the staging path
			// being clear is precisely the outcome wanted. Any OTHER failure
			// (a read-only directory, say) is reported -- the bytes are already
			// safe under dest, but the caller's two-phase rename is about to
			// want this path and must not be told it is free when it is not.
			return fmt.Errorf("quarantined %s to %s but could not clear the original: %w",
				filepath.Base(path), filepath.Base(dest), err)
		}
		// WARN, not Info. This is rare, it means an earlier operation died
		// mid-flight, and the file it names may be the only copy of that
		// artwork -- an operator who never sees this line has no way to learn
		// the file exists or where it went. Persistent operator-visible
		// surfacing is the deferred follow-up (#2954); this is the immediate signal.
		slog.Warn("quarantined a stranded temp file instead of deleting it; it may be the only copy of this artwork",
			slog.String("component", "fanart-renumber"),
			slog.String("original", path),
			slog.String("quarantined_to", dest))
		return nil
	}
	return fmt.Errorf("quarantining %s: exhausted name attempts", filepath.Base(path))
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
		if quarantineErr := quarantineStrandedTemp(tmpPath, ext); quarantineErr != nil {
			return fmt.Errorf("clearing stale temp file %s: %w", tmpName, quarantineErr)
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
