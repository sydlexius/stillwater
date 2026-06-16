package artist

// merge_artists.go -- consolidate two or more near-duplicate artists into one
// by physically moving each loser's album subdirectories under the survivor
// directory and then deleting the loser artist rows.
//
// A DB-only merge would be futile: deleting one row leaves a directory on
// disk that the next scan re-promotes into a fresh artist row. The merge has
// to touch the filesystem, and the filesystem operations have to be atomic
// at a per-child granularity so a SIGKILL mid-flight leaves a recoverable
// state.
//
// Crash-safety contract: each album subdirectory move is an atomic rename
// (filesystem.RenameDirAtomic). The loser artist row is the LAST thing
// deleted. A process that dies between the first child rename and the loser
// row deletion leaves the remaining children on disk under the loser path
// and the loser row intact; the next MergeArtists call sees the same group
// in DetectDuplicates and resumes where the previous one left off.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// Merge-related error sentinels. Handlers inspect these via errors.Is to map
// to the documented HTTP status codes (409 / 422 / 423 / 400). Each error
// describes one structural reason a merge cannot proceed; the orchestrator
// never returns a free-form "merge failed: ..." for any case the caller is
// expected to recover from.
var (
	// ErrMergeInProgress is returned when another merge is already running
	// for any artist group. Handlers map this to HTTP 409.
	ErrMergeInProgress = errors.New("merge already in progress")

	// ErrMergeCollisions is returned when the pre-flight collision walk
	// finds at least one album subdirectory whose name already exists in
	// the survivor. The MergeResult.Conflicts slice carries the full list
	// so the caller can act on it; nothing has been moved. Handlers map
	// this to HTTP 409 and include the conflicts in the response body.
	ErrMergeCollisions = errors.New("merge would create filesystem collisions")

	// ErrMergeStaleGroup is returned when the survivor + loser IDs do not
	// co-resolve to a single near-duplicate group in the current
	// DetectDuplicates output. The detection runs against live DB state,
	// so the most common cause is concurrent edits between the user
	// loading the duplicates view and submitting the merge. Handlers map
	// this to HTTP 422.
	ErrMergeStaleGroup = errors.New("artists no longer form a near-duplicate group")

	// ErrMergeLocked is returned when any group member has Locked = true.
	// A locked artist explicitly opts out of automated and destructive
	// operations, so the merge refuses rather than silently overriding
	// the lock. Handlers map this to HTTP 423.
	ErrMergeLocked = errors.New("at least one group member is locked")

	// ErrMergeInvalidRequest is returned for malformed input that the
	// handler did not already reject (e.g. survivor_id matches one of the
	// loser_ids, empty IDs). Handlers map this to HTTP 400.
	ErrMergeInvalidRequest = errors.New("invalid merge request")

	// ErrMergeSurvivorMissing is returned when the survivor ID is not in
	// the resolved group. Handlers map this to HTTP 422 alongside the
	// stale-group case (same recovery: reload the duplicates view).
	ErrMergeSurvivorMissing = errors.New("survivor id is not a member of the duplicate group")
)

// MergeRequest describes one user-initiated merge. The survivor keeps its
// artist row and gains every album subdirectory from the losers; each loser
// row is deleted (FKs cascade) and its on-disk directory removed.
type MergeRequest struct {
	// SurvivorID is the artist row that survives the merge. Must be a
	// member of the same near-duplicate group as every loser ID.
	SurvivorID string

	// LoserIDs is the list of artist rows to fold into the survivor. At
	// least one entry; deduplicated and validated against the live group.
	LoserIDs []string

	// DryRun, when true, runs the pre-flight collision walk and returns
	// the would-be result without touching the filesystem or the
	// database. Use this to populate a confirmation UI before committing.
	DryRun bool

	// ArticleMode controls how CanonicalDirName resolves the
	// MB-canonical basename used by ChooseSurvivor's first precedence
	// rule. Empty string defaults to "prefix" (the same default the rule
	// engine uses). Callers that want to honor the configured rule
	// setting should pass the rule's ArticleMode here; tests can leave
	// it empty.
	ArticleMode string
}

// MovedItem records one filesystem rename performed by the merge. Surfaced
// in the API response so the caller can show a per-child progress list and
// so a curl-based operator can audit what moved.
type MovedItem struct {
	// Name is the child basename (album subdir or loose file).
	Name string
	// From is the absolute source path.
	From string
	// To is the absolute destination path under the survivor.
	To string
}

// DeletedItem records one loose file deleted from the loser directory during
// the merge. When a loose file's name already exists under the survivor the
// survivor's copy is authoritative; the loser's redundant copy is removed so
// the loser directory can be emptied and unlinked. In dry-run mode the slice
// previews which files WOULD be deleted; in commit mode it lists the files
// that WERE actually removed.
type DeletedItem struct {
	// Name is the file basename.
	Name string
	// Path is the absolute path of the file that was (or would be) deleted
	// from the loser directory.
	Path string
}

// ConflictItem records one would-be filesystem collision found by the
// pre-flight walk. Populated when MergeResult.Conflicts is non-empty;
// nothing has been moved in that case.
type ConflictItem struct {
	// Name is the child basename that exists in both directories.
	Name string
	// SurvivorPath is the absolute path to the existing entry under the survivor.
	SurvivorPath string
	// LoserPath is the absolute path to the conflicting entry under the loser.
	LoserPath string
}

// MergeResult is the structured outcome of a MergeArtists call. The handler
// serializes this as the JSON response body for both dry-run and committed
// merges.
type MergeResult struct {
	// DryRun mirrors MergeRequest.DryRun so callers reading the response
	// in isolation can tell which mode produced it.
	DryRun bool

	// SurvivorID is echoed back so the caller has a single object to
	// store. SurvivorPath is the survivor's directory on disk.
	SurvivorID   string
	SurvivorPath string

	// SurvivorOverride is true when the caller's SurvivorID does not
	// match the precedence-recommended survivor. The UI surfaces this so
	// a user picking a non-recommended survivor sees an explicit
	// confirmation hint; the merge still proceeds.
	SurvivorOverride bool

	// Moved lists every filesystem rename performed (album subdirs and
	// loose files). Empty when DryRun is true or Conflicts is non-empty.
	Moved []MovedItem

	// Conflicts lists every would-be filesystem collision found by the
	// pre-flight walk. Non-empty implies the orchestrator halted before
	// any FS mutation and returned ErrMergeCollisions.
	Conflicts []ConflictItem

	// Removed lists the loser artist IDs whose directories were actually
	// unlinked from the filesystem during the merge. An ID appears here
	// only when executeLoserMerge confirmed the loser dir was empty after
	// the rename loop AND os.Remove succeeded. A loser whose dir was left
	// in place (destination-appeared-race, symlinks, or unexpected
	// filesystem content) does NOT appear here.
	Removed []string

	// Deleted lists loose files removed from loser directories because the
	// same filename already existed under the survivor. The survivor's copy
	// is authoritative; the loser's copy is redundant and is deleted so the
	// loser directory becomes empty and can be unlinked. In dry-run mode
	// this previews which files WOULD be deleted; in commit mode it lists
	// what WAS actually removed.
	Deleted []DeletedItem

	// Warnings collects non-fatal observations the user should see (symlinks
	// skipped, destination-appeared-race left a subdirectory in place, and
	// the standing reminder that connected platforms still index the loser
	// path until the operator triggers a refresh).
	Warnings []string

	// LosersDeleted lists the loser artist IDs whose rows were deleted
	// inside the final DB transaction.
	LosersDeleted []string
}

// MergeArtists consolidates the loser artists into the survivor per req. See
// the file header for the crash-safety contract.
//
// Error semantics: a non-nil error always wraps one of the package-level
// Err* sentinels above; the handler maps each sentinel to a specific HTTP
// status. A nil error means the merge committed (or the dry run completed)
// and the *MergeResult carries the full picture.
func (s *Service) MergeArtists(ctx context.Context, req MergeRequest) (*MergeResult, error) {
	if err := validateMergeRequest(&req); err != nil {
		return nil, err
	}

	// Singleton 409: a queued caller would block on this mutex while
	// other request goroutines pile up; returning immediately means the
	// caller knows to back off and retry once the in-flight merge ends.
	if !s.mergeMu.TryLock() {
		return nil, ErrMergeInProgress
	}
	defer s.mergeMu.Unlock()

	// renameMu serializes against Service.RenameDirectory. A concurrent
	// rename on a survivor or loser would race with the per-child
	// RenameDirAtomic loop and leave a partially-moved tree.
	s.renameMu.Lock()
	defer s.renameMu.Unlock()

	db, err := s.artistDB()
	if err != nil {
		return nil, err
	}

	// Re-validate the group against live DB state. DetectDuplicates is
	// the source of truth -- the user clicked Merge on a UI built from
	// DetectDuplicates output, and concurrent edits could have broken
	// the grouping. If the IDs no longer co-resolve, refuse rather than
	// guess.
	members, err := resolveGroupMembers(ctx, db, req.SurvivorID, req.LoserIDs)
	if err != nil {
		return nil, err
	}

	// Locked-member check. A locked artist opts out of automated and
	// destructive operations; merging is destructive (we delete the
	// loser row and unlink its directory), so any lock anywhere in the
	// group refuses.
	if err := refuseIfLocked(ctx, s.artists, members); err != nil {
		return nil, err
	}

	survivor := pickMember(members, req.SurvivorID)
	if survivor == nil {
		return nil, ErrMergeSurvivorMissing
	}

	// Survivor-override detection: ChooseSurvivor returns the
	// precedence-recommended ID; if the caller picked something else,
	// flag it so the response makes the deviation explicit.
	recommended, _ := ChooseSurvivor(members, req.ArticleMode)
	override := recommended != "" && recommended != survivor.ID

	result := &MergeResult{
		DryRun:           req.DryRun,
		SurvivorID:       survivor.ID,
		SurvivorPath:     survivor.Path,
		SurvivorOverride: override,
	}

	// Pre-flight collision walk over every loser. Album-subdir collisions
	// are halting (ErrMergeCollisions). Loose-file collisions are recorded
	// in result.Deleted as a preview of which files will be removed from
	// the loser during the commit phase (survivor wins; the loser's
	// redundant copy is deleted so the loser directory can be emptied).
	losers := lookupLosers(members, req.LoserIDs)
	if err := preflightAllLosers(losers, survivor.Path, result); err != nil {
		return result, err
	}

	if req.DryRun {
		return result, nil
	}

	// Clear the deletion preview populated by the pre-flight walk; the
	// commit loop (executeLoserMerge) will repopulate result.Deleted with
	// entries for files that were actually removed.
	result.Deleted = nil

	// Commit phase: per-loser, move each album subdir, delete each
	// colliding loose file, move each non-colliding loose file, then
	// unlink the now-empty loser dir. A failure mid-loop returns whatever
	// has already been moved in result.Moved so the caller can reason
	// about partial state. We only record a loser ID in result.Removed
	// when executeLoserMerge confirms the loser directory was actually
	// unlinked (destination-appeared-race leaves a subdir in place and
	// does NOT appear here).
	for _, loser := range losers {
		removed, err := executeLoserMerge(loser, survivor.Path, result)
		if err != nil {
			return result, fmt.Errorf("merging loser %s: %w", loser.ID, err)
		}
		if removed {
			result.Removed = append(result.Removed, loser.ID)
		}
	}

	// DB phase: fill-empty MBID forward, then delete loser rows. FKs
	// cascade (artist_provider_ids, artist_images, artist_libraries,
	// platform IDs, aliases, members, snapshots, history). One TX so the
	// MBID copy and the delete commit atomically.
	if err := s.commitMergeDB(ctx, survivor, losers, result); err != nil {
		return result, err
	}

	s.markDirtyBestEffort(ctx, survivor.ID)

	result.Warnings = append(result.Warnings,
		"Connected platforms (Emby/Jellyfin/Lidarr) still reference the deleted loser paths. Trigger a library refresh on each platform to drop the stale items.")

	slog.Info("merged near-duplicate artists",
		"survivor_id", survivor.ID,
		"survivor_path", survivor.Path,
		"loser_ids", strings.Join(result.LosersDeleted, ","),
		"moved", len(result.Moved),
		"warnings", len(result.Warnings))

	return result, nil
}

// validateMergeRequest enforces the structural request shape (non-empty
// survivor, at least one loser, no survivor ID in losers) before the
// orchestrator takes any locks or hits the DB. IDs are trimmed in place so
// downstream comparisons (against DB rows from DetectDuplicates) cannot be
// fooled by stray whitespace -- a request like {"survivor_id": " abc "}
// would otherwise pass structural validation and then fail with the less
// helpful ErrMergeStaleGroup downstream.
func validateMergeRequest(req *MergeRequest) error {
	req.SurvivorID = strings.TrimSpace(req.SurvivorID)
	if req.SurvivorID == "" {
		return fmt.Errorf("%w: survivor_id is required", ErrMergeInvalidRequest)
	}
	if len(req.LoserIDs) == 0 {
		return fmt.Errorf("%w: at least one loser_id is required", ErrMergeInvalidRequest)
	}
	seen := make(map[string]struct{}, len(req.LoserIDs))
	for i, id := range req.LoserIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			return fmt.Errorf("%w: loser_id must not be empty", ErrMergeInvalidRequest)
		}
		if trimmed == req.SurvivorID {
			return fmt.Errorf("%w: survivor_id must not appear in loser_ids", ErrMergeInvalidRequest)
		}
		if _, dup := seen[trimmed]; dup {
			return fmt.Errorf("%w: loser_ids contains duplicate %s", ErrMergeInvalidRequest, trimmed)
		}
		seen[trimmed] = struct{}{}
		req.LoserIDs[i] = trimmed
	}
	return nil
}

// artistDB is a small accessor that pulls the raw *sql.DB out of the
// Repository for use inside MergeArtists. The Service does not store a *sql.DB
// directly; it goes through the SQLite repo. Tests using NewServiceWithRepos
// pass a fake repo that may not implement the DB() accessor, so callers in
// that path hit ErrMergeStaleGroup via the re-validation step instead.
func (s *Service) artistDB() (*sql.DB, error) {
	type dbAccessor interface {
		DB() *sql.DB
	}
	if acc, ok := s.artists.(dbAccessor); ok {
		return acc.DB(), nil
	}
	return nil, fmt.Errorf("%w: artist repository has no DB accessor", ErrMergeInvalidRequest)
}

// resolveGroupMembers re-runs DetectDuplicates and confirms the survivor +
// loser IDs all co-resolve into a single group. Returns the full member
// slice so the caller can do further checks (locks, survivor lookup,
// per-loser walk) without re-querying.
func resolveGroupMembers(ctx context.Context, db *sql.DB, survivorID string, loserIDs []string) ([]NearDuplicateArtist, error) {
	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("re-detecting duplicates: %w", err)
	}

	wanted := make(map[string]bool, len(loserIDs)+1)
	wanted[survivorID] = true
	for _, id := range loserIDs {
		wanted[id] = true
	}

	for _, g := range groups {
		got := 0
		for _, m := range g.Members {
			if wanted[m.ID] {
				got++
			}
		}
		if got == len(wanted) {
			return g.Members, nil
		}
	}
	return nil, ErrMergeStaleGroup
}

// refuseIfLocked checks every group member's Locked flag. The
// NearDuplicateArtist struct does not carry Locked, so we re-load each
// artist via the repository. The N here is bounded by group size (typically
// 2-3), so the per-call cost is negligible.
func refuseIfLocked(ctx context.Context, repo Repository, members []NearDuplicateArtist) error {
	var locked []string
	for _, m := range members {
		a, err := repo.GetByID(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("loading artist %s for lock check: %w", m.ID, err)
		}
		if a.Locked {
			locked = append(locked, m.ID)
		}
	}
	if len(locked) > 0 {
		return fmt.Errorf("%w: %s", ErrMergeLocked, strings.Join(locked, ","))
	}
	return nil
}

// pickMember returns the group member with the given ID, or nil if missing.
func pickMember(members []NearDuplicateArtist, id string) *NearDuplicateArtist {
	for i := range members {
		if members[i].ID == id {
			return &members[i]
		}
	}
	return nil
}

// lookupLosers returns the group members matching the loser ID list, in the
// order they were requested. Caller has already verified every loser ID is
// present in the group (via resolveGroupMembers).
func lookupLosers(members []NearDuplicateArtist, loserIDs []string) []NearDuplicateArtist {
	byID := make(map[string]NearDuplicateArtist, len(members))
	for _, m := range members {
		byID[m.ID] = m
	}
	out := make([]NearDuplicateArtist, 0, len(loserIDs))
	for _, id := range loserIDs {
		out = append(out, byID[id])
	}
	return out
}

// ChooseSurvivor returns the recommended survivor ID from a group along
// with the reason and an override flag. Precedence:
//
//	a. MB-canonical basename: filepath.Base(path) == CanonicalDirName(name, articleMode).
//	b. Platform-mapped: tie-broken alphabetically by ID for determinism. (At this
//	   layer we only have NearDuplicateArtist, which carries no platform info, so
//	   this fallback degrades to "any member with a non-empty Path that isn't
//	   MB-canonical"; the duplicates surface already excludes platform-only
//	   artists in DetectDuplicates, so all members are filesystem-backed by
//	   construction. The platform-mapped check therefore collapses into the
//	   alphabetic tiebreak.)
//	c. Most content: highest album-subdirectory count under the artist path.
//
// Returns ("", "") when the group is empty. The caller computes override
// by comparing the recommended ID against the user-supplied SurvivorID.
func ChooseSurvivor(members []NearDuplicateArtist, articleMode string) (id, reason string) {
	if len(members) == 0 {
		return "", ""
	}

	// Precedence a: MB-canonical basename.
	var canonicals []NearDuplicateArtist
	for _, m := range members {
		if m.Path == "" {
			continue
		}
		canonical := CanonicalDirName(m.Name, articleMode)
		if canonical == "" {
			continue
		}
		if strings.EqualFold(filepath.Base(m.Path), canonical) {
			canonicals = append(canonicals, m)
		}
	}
	if len(canonicals) > 0 {
		sort.Slice(canonicals, func(i, j int) bool { return canonicals[i].ID < canonicals[j].ID })
		return canonicals[0].ID, "canonical_basename"
	}

	// Precedence c: most album-directory content under the artist path.
	// (See doc comment for why the platform-mapped precedence collapses
	// into the alphabetic tiebreak at this layer.)
	//
	// albumDirCount returns -1 for a path it cannot read (real I/O error,
	// not ENOENT); such members are excluded from the content tiebreak
	// entirely so a temporarily-unreadable survivor candidate cannot
	// silently lose to a candidate with worse data.
	bestID := ""
	bestCount := -1
	for _, m := range members {
		if m.Path == "" {
			continue
		}
		count := albumDirCount(m.Path)
		if count < 0 {
			// Read error already logged in albumDirCount; skip this
			// member so it cannot win or tiebreak.
			continue
		}
		switch {
		case count > bestCount:
			bestCount = count
			bestID = m.ID
		case count == bestCount && m.ID < bestID:
			// Deterministic tiebreak: lowest ID wins.
			bestID = m.ID
		}
	}
	if bestID != "" {
		return bestID, "most_content"
	}

	// Last-ditch: first member by ID.
	sorted := make([]NearDuplicateArtist, len(members))
	copy(sorted, members)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted[0].ID, "fallback"
}

// albumDirCount counts immediate album-style subdirectories under path,
// filtering out loose files and dotfiles. Returns 0 for a missing
// directory (ENOENT) and -1 for any other read error so the caller can
// exclude the candidate from precedence-c selection rather than letting a
// temporarily-unreadable path silently lose to a candidate with worse
// data. The non-ENOENT case is logged at warn so the operator can
// investigate (permissions, filesystem issues).
func albumDirCount(path string) int {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		slog.Warn("merge survivor candidate path unreadable; excluded from content tiebreak",
			"path", path, "error", err)
		return -1
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n++
	}
	return n
}

// enumerateChildren splits a directory's entries into album subdirs and
// loose files. Dotfiles are excluded. Symlinks are skipped (and reported
// to warnings via the caller) -- following a symlink during a destructive
// merge would extend the blast radius outside the artist tree.
func enumerateChildren(path string) (subdirs, files []os.DirEntry, symlinks []string, err error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// os.DirEntry.Type returns the mode bits without following links;
		// a symlink reports type Symlink even if it points at a directory.
		if e.Type()&os.ModeSymlink != 0 {
			symlinks = append(symlinks, e.Name())
			continue
		}
		if e.IsDir() {
			subdirs = append(subdirs, e)
		} else {
			files = append(files, e)
		}
	}
	return subdirs, files, symlinks, nil
}

// preflightAllLosers walks every loser and collects collisions before any FS
// mutation. Album-subdir collisions are halting (ErrMergeCollisions).
// Loose-file collisions are recorded in result.Deleted as a preview of what
// the commit phase will remove; symlinks are recorded in result.Warnings.
func preflightAllLosers(losers []NearDuplicateArtist, survivorPath string, result *MergeResult) error {
	for _, loser := range losers {
		if err := preflightOneLoser(loser, survivorPath, result); err != nil {
			return err
		}
	}
	if len(result.Conflicts) > 0 {
		return ErrMergeCollisions
	}
	return nil
}

// preflightOneLoser populates result.Conflicts and result.Warnings for one
// loser. Returns a non-nil error only on filesystem-read failures (e.g.
// permission denied); a missing loser directory (ENOENT) is silently
// tolerated as the crash-recovery path -- a previous attempt unlinked
// the dir but failed before the DB tx committed, leaving an orphan
// loser row. executeLoserMerge handles the corresponding ENOENT case
// during the commit phase and returns removed=true so the DB cleanup
// still runs.
func preflightOneLoser(loser NearDuplicateArtist, survivorPath string, result *MergeResult) error {
	if _, statErr := os.Lstat(loser.Path); os.IsNotExist(statErr) {
		return nil
	}
	subdirs, files, symlinks, err := enumerateChildren(loser.Path)
	if err != nil {
		return err
	}
	for _, sym := range symlinks {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("skipped symlink %q under %s (symlinks are not followed during merge)", sym, loser.Path))
	}
	for _, sd := range subdirs {
		survivorChild := filepath.Join(survivorPath, sd.Name())
		if _, statErr := os.Lstat(survivorChild); statErr == nil {
			result.Conflicts = append(result.Conflicts, ConflictItem{
				Name:         sd.Name(),
				SurvivorPath: survivorChild,
				LoserPath:    filepath.Join(loser.Path, sd.Name()),
			})
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("checking survivor child %s: %w", survivorChild, statErr)
		}
	}
	for _, f := range files {
		survivorChild := filepath.Join(survivorPath, f.Name())
		if info, statErr := os.Lstat(survivorChild); statErr == nil {
			if !info.Mode().IsRegular() {
				// Survivor has a same-named entry that is NOT a regular
				// file (e.g. a directory or a symlink). The loser's loose
				// file has no genuine authoritative survivor copy, so do
				// not treat it as colliding -- leave it in the loser dir.
				// The non-empty loser dir will trigger the "still contains"
				// warning path in executeLoserMerge rather than a delete.
				continue
			}
			// Survivor already has this file; the loser's copy is
			// redundant and will be deleted during the commit phase.
			// Record a preview entry so the caller can show what will
			// be removed before asking the user to confirm.
			result.Deleted = append(result.Deleted, DeletedItem{
				Name: f.Name(),
				Path: filepath.Join(loser.Path, f.Name()),
			})
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("checking survivor loose file %s: %w", survivorChild, statErr)
		}
	}
	return nil
}

// executeLoserMerge performs the commit-phase filesystem operations for one
// loser: move every non-colliding album subdir, delete every colliding loose
// file (survivor's copy wins; the loser's redundant copy is removed), move
// every non-colliding loose file, then remove the now-empty loser directory.
// Per-child failures abort the loop and leave the partially-moved state on
// disk -- the next merge attempt will pick up where this one stopped (the
// loser dir still exists and DetectDuplicates still groups it with the
// survivor).
//
// Returns removed=true ONLY when the loser directory was actually unlinked
// (rename/delete loop drained the directory AND os.Remove succeeded). The
// destination-appeared-race case (a subdir collision detected between
// pre-flight and commit) returns removed=false with a warning recorded in
// result.Warnings; the caller MUST NOT report the loser as "removed" in
// that case.
//
// Returns removed=true with no work performed if the loser directory does
// not exist at all (ENOENT). This is the crash-recovery path: a previous
// merge attempt unlinked the dir but failed before committing the DB tx,
// leaving an orphan loser row whose .Path points at a now-missing dir. The
// caller can proceed to delete the loser row.
func executeLoserMerge(loser NearDuplicateArtist, survivorPath string, result *MergeResult) (removed bool, err error) {
	if _, statErr := os.Lstat(loser.Path); os.IsNotExist(statErr) {
		// Crash-recovery: dir was unlinked by a previous attempt; nothing
		// to move, nothing to remove, nothing to warn about. The DB tx
		// below will clean up the orphan row.
		return true, nil
	}

	subdirs, files, symlinks, err := enumerateChildren(loser.Path)
	if err != nil {
		return false, err
	}
	// Re-warn on symlinks that appeared between pre-flight and execute:
	// the pre-flight pass warned about whatever was there at the time,
	// but a symlink added after pre-flight would otherwise be silently
	// skipped without acknowledgment.
	for _, sym := range symlinks {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("skipped symlink %q under %s during execute (not following symlinks during merge)", sym, loser.Path))
	}

	for _, sd := range subdirs {
		src := filepath.Join(loser.Path, sd.Name())
		dst := filepath.Join(survivorPath, sd.Name())
		// Defensive re-check: a concurrent process could have created the
		// destination between the pre-flight walk and now. RenameDirAtomic
		// requires the destination to not exist.
		if _, statErr := os.Lstat(dst); statErr == nil {
			// Surface as a warning rather than failing the whole merge;
			// the user can re-run after resolving the new collision.
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("destination %s appeared between pre-flight and commit; left %s in place", dst, src))
			continue
		}
		if err := filesystem.RenameDirAtomic(src, dst); err != nil {
			return false, fmt.Errorf("moving %s to %s: %w", src, dst, err)
		}
		result.Moved = append(result.Moved, MovedItem{Name: sd.Name(), From: src, To: dst})
	}

	for _, f := range files {
		src := filepath.Join(loser.Path, f.Name())
		dst := filepath.Join(survivorPath, f.Name())
		if info, statErr := os.Lstat(dst); statErr == nil {
			if !info.Mode().IsRegular() {
				// Survivor's same-named entry is not a regular file (e.g.
				// a directory or a symlink). There is no genuine
				// authoritative survivor copy, so do not delete the
				// loser's file -- leave it in place. The non-empty loser
				// dir will surface via the "still contains" warning below.
				continue
			}
			// Survivor already has a file with this name; its copy is
			// authoritative. Delete the loser's redundant copy so the
			// loser directory becomes empty and can be unlinked below.
			if err := filesystem.RemoveFileSafe(src); err != nil {
				return false, fmt.Errorf("deleting colliding loose file %s: %w", src, err)
			}
			result.Deleted = append(result.Deleted, DeletedItem{Name: f.Name(), Path: src})
			continue
		}
		// RenameFileAtomic mirrors RenameDirAtomic's EXDEV fallback so a
		// loose-file move across mount boundaries (bind mount, per-letter
		// NAS share) completes via copy+remove instead of failing.
		if err := filesystem.RenameFileAtomic(src, dst); err != nil {
			return false, fmt.Errorf("moving loose file %s to %s: %w", src, dst, err)
		}
		result.Moved = append(result.Moved, MovedItem{Name: f.Name(), From: src, To: dst})
	}

	// Defensive empty check before unlinking: os.Remove on a non-empty
	// directory returns a clear error, which we surface so the operator
	// can investigate (a real leftover should be rare given the
	// pre-flight + survivor-wins loose-file policy above).
	remaining, err := os.ReadDir(loser.Path)
	if err != nil {
		return false, fmt.Errorf("re-reading loser dir %s before removal: %w", loser.Path, err)
	}
	if len(remaining) > 0 {
		var names []string
		for _, e := range remaining {
			names = append(names, e.Name())
		}
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("loser directory %s still contains %d entries (%s); not removed", loser.Path, len(remaining), strings.Join(names, ", ")))
		return false, nil
	}
	if err := os.Remove(loser.Path); err != nil {
		return false, fmt.Errorf("removing empty loser dir %s: %w", loser.Path, err)
	}
	return true, nil
}

// commitMergeDB runs the final DB transaction: fill-empty MBID forward from
// any loser that has one (when the survivor does not), then delete every
// loser artist row. FK CASCADE handles the dependent rows.
func (s *Service) commitMergeDB(ctx context.Context, survivor *NearDuplicateArtist, losers []NearDuplicateArtist, result *MergeResult) error {
	db, err := s.artistDB()
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting merge tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// MBID fill-empty: if survivor has no MBID and any loser does, copy
	// the first loser MBID onto the survivor before deleting the losers.
	// UPSERT (not INSERT OR IGNORE) so the fill works even when the
	// survivor already has a row with an EMPTY provider_id -- which is
	// the exact condition that triggers fill-empty in the first place.
	// The WHERE guard on the UPDATE makes the UPSERT a no-op if the row
	// already has a non-empty value (the survivor's existing MBID wins).
	//
	// Warning string is deferred to a local variable; we only append it to
	// result.Warnings AFTER tx.Commit() succeeds so a failed commit does
	// not record a false-positive inheritance.
	var inheritedMBIDWarning string
	if survivor.MBID == "" {
		for _, l := range losers {
			if l.MBID == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
				 VALUES (?, 'musicbrainz', ?)
				 ON CONFLICT(artist_id, provider) DO UPDATE SET provider_id = excluded.provider_id
				 WHERE artist_provider_ids.provider_id = ''`,
				survivor.ID, l.MBID); err != nil {
				return fmt.Errorf("filling survivor mbid: %w", err)
			}
			inheritedMBIDWarning = fmt.Sprintf(
				"survivor %s inherited MusicBrainz ID %s from loser %s", survivor.ID, l.MBID, l.ID)
			break
		}
	}

	// Only delete DB rows for losers whose directory was actually removed
	// from disk (i.e. those recorded in result.Removed). A loser left on
	// disk (removed=false, e.g. blocked by a non-regular survivor child)
	// must keep its row so the scanner reconciles to the existing entry
	// rather than resurrecting it as a new artist (#2010, follow-up to #1779).
	// Metadata-forward (MBID fill above) still runs for all losers.
	removedSet := make(map[string]bool, len(result.Removed))
	for _, id := range result.Removed {
		removedSet[id] = true
	}

	// Collect deleted IDs locally; only assign to result.LosersDeleted on
	// successful commit so a failed commit does not falsely advertise
	// loser-row deletions that never persisted.
	var deletedIDs []string
	for _, l := range losers {
		if !removedSet[l.ID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM artists WHERE id = ?`, l.ID); err != nil {
			return fmt.Errorf("deleting loser %s: %w", l.ID, err)
		}
		deletedIDs = append(deletedIDs, l.ID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing merge tx: %w", err)
	}
	committed = true
	result.LosersDeleted = append(result.LosersDeleted, deletedIDs...)
	if inheritedMBIDWarning != "" {
		result.Warnings = append(result.Warnings, inheritedMBIDWarning)
	}
	return nil
}
