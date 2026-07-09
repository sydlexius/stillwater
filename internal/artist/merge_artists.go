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

	// Moved lists filesystem relocations. On a dry-run it lists the PLANNED
	// moves (each non-colliding album subdir and loose file that a real merge
	// would relocate). On a committed merge it lists the moves ACTUALLY
	// performed. Note Moved can be non-empty even when Conflicts halted the
	// merge: a loser that has both a colliding child and non-colliding
	// siblings previews the siblings here while the collision is recorded in
	// Conflicts. Callers must treat Conflicts (not an empty Moved) as the
	// halt signal.
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

	// SurvivorName is the survivor artist's stored display name at merge time.
	// Used by MergeAndReconcile to compute the canonical directory; not part
	// of the core FS/DB merge. Populated on BOTH dry-run and committed merges
	// (the survivor is already resolved when the result is built, so this is
	// free). Contrast AffectedConnectionIDs, which requires a pre-delete DB
	// capture and is therefore empty on dry-run.
	SurvivorName string

	// AffectedConnectionIDs is the distinct, sorted union of platform
	// connection IDs that mapped the survivor or ANY loser at merge time.
	// Captured before commitMergeDB deletes the loser rows (whose platform_ids
	// FK-cascade away). MergeAndReconcile refreshes exactly this set so the
	// survivor's absorbed albums are indexed and stale loser items are dropped.
	// Empty on dry-run.
	AffectedConnectionIDs []string

	// CanonicalRename is non-nil when MergeAndReconcile relocated the survivor
	// to its canonical directory after the merge committed. Nil when the
	// survivor was already canonical, on dry-run, or when the rename failed
	// (a warning is recorded instead). Populated only by MergeAndReconcile.
	CanonicalRename *CanonicalRenameResult

	// PlatformRefresh lists the post-merge per-connection refresh outcomes
	// (survivor re-index + stale loser eviction). Populated only by
	// MergeAndReconcile when a refresher is wired; empty otherwise.
	PlatformRefresh []PlatformRefreshResult
}

// CanonicalRenameResult records the survivor's post-merge relocation to its
// canonical directory, including the per-platform path-sync outcomes returned
// by the chained RenameDirectory call.
type CanonicalRenameResult struct {
	OldPath   string
	NewPath   string
	Platforms []PlatformRemapResult
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
		SurvivorName:     survivor.Name,
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

	// Clear the previews populated by the pre-flight walk; the commit loop
	// (executeLoserMerge) repopulates result.Moved and result.Deleted with
	// entries for children that were ACTUALLY moved/removed.
	result.Deleted = nil
	result.Moved = nil

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

	// Capture affected platform connections BEFORE the loser rows (and their
	// platform_ids) are deleted by commitMergeDB. MergeAndReconcile refreshes
	// this set post-commit.
	result.AffectedConnectionIDs = s.collectAffectedConnectionIDs(ctx, survivor.ID, losers)

	// DB phase: fill-empty MBID forward, then delete loser rows. FKs
	// cascade (artist_provider_ids, artist_images, artist_libraries,
	// platform IDs, aliases, members, snapshots, history). One TX so the
	// MBID copy and the delete commit atomically.
	if err := s.commitMergeDB(ctx, survivor, losers, result); err != nil {
		return result, err
	}

	s.markDirtyBestEffort(ctx, survivor.ID)

	slog.Info("merged near-duplicate artists",
		"survivor_id", survivor.ID,
		"survivor_path", survivor.Path,
		"loser_ids", strings.Join(result.LosersDeleted, ","),
		"moved", len(result.Moved),
		"warnings", len(result.Warnings))

	return result, nil
}

// MergeAndReconcile runs a merge and then reconciles the survivor's directory
// and connected platforms. It exists as a wrapper (not inline in MergeArtists)
// because MergeArtists holds renameMu for its whole duration and the chained
// RenameDirectory also takes renameMu; the reconcile steps therefore run only
// after MergeArtists has returned and released the lock. Nesting the rename
// inside MergeArtists would self-deadlock on the non-reentrant renameMu.
//
// Order: MergeArtists -> (if survivor non-canonical) RenameDirectory to the
// canonical basename (which also re-issues the path to platforms) -> refresh
// every affected connection so the survivor's absorbed albums are indexed and
// stale loser items drop. Dry-runs and failed merges return straight from
// MergeArtists with no reconcile. Reconcile steps are best-effort: their
// failures never fail the merge; they record warnings / structured outcomes.
func (s *Service) MergeAndReconcile(ctx context.Context, req MergeRequest) (*MergeResult, error) {
	result, err := s.MergeArtists(ctx, req)
	if err != nil || req.DryRun {
		return result, err
	}
	s.reconcileSurvivorCanonicalPath(ctx, req.ArticleMode, result)
	s.refreshAffectedPlatforms(ctx, result)
	return result, nil
}

// reconcileSurvivorCanonicalPath renames the survivor to CanonicalDirName when
// its current basename differs, reusing RenameDirectory (which propagates the
// new path to platforms). Directory-match only: it never mutates survivor.Name
// or resolves localized aliases (that is RuleNameLanguagePref's job). Best-
// effort: a rename error is recorded as a warning and the merged-but-non-
// canonical state is left for the directory-name rule to flag later.
func (s *Service) reconcileSurvivorCanonicalPath(ctx context.Context, articleMode string, result *MergeResult) {
	canonicalDir := CanonicalDirName(result.SurvivorName, articleMode)
	if canonicalDir == "" || strings.EqualFold(filepath.Base(result.SurvivorPath), canonicalDir) {
		return // cannot compute, or already canonical (case-insensitive).
	}
	oldPath := result.SurvivorPath
	newPath, platforms, err := s.RenameDirectory(ctx, result.SurvivorID, canonicalDir)
	if err != nil {
		if errors.Is(err, ErrRenameNoChange) {
			return // already canonical by RenameDirectory's exact check.
		}
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("could not move survivor to canonical directory %q: %v", canonicalDir, err))
		return
	}
	result.SurvivorPath = newPath
	result.CanonicalRename = &CanonicalRenameResult{OldPath: oldPath, NewPath: newPath, Platforms: platforms}
}

// refreshAffectedPlatforms triggers the post-merge platform refresh over the
// connections captured before the loser rows were deleted. When no refresher
// is wired it records the manual-refresh reminder (the behavior the removed
// unconditional warning used to provide, now gated on refresh being absent).
func (s *Service) refreshAffectedPlatforms(ctx context.Context, result *MergeResult) {
	if len(result.AffectedConnectionIDs) == 0 {
		return // no connected platforms indexed any member.
	}
	if s.mergeRefresher == nil {
		result.Warnings = append(result.Warnings,
			"Connected platforms still reference the merged directories. Trigger a library refresh on each so they pick up the new location.")
		return
	}
	refreshed, err := s.mergeRefresher.SyncMergeRefresh(ctx, result.SurvivorID, result.AffectedConnectionIDs)
	if err != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("post-merge platform refresh could not start: %v", err))
		return
	}
	result.PlatformRefresh = refreshed
	for _, r := range refreshed {
		if r.Result == PlatformRemapFailed {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("platform refresh failed for connection %s: %s", r.ConnectionID, r.Error))
		}
	}
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
// merge would extend the blast radius outside the artist tree. OS/NAS junk
// entries ($RECYCLE.BIN, @eaDir, Thumbs.db, ...) are returned in the
// separate `ignored` bucket so the collision walk never treats them as a
// mergeable child (a stray @eaDir on both sides must not halt the merge);
// the commit phase removes them so the loser directory can still be
// unlinked (#30).
func enumerateChildren(path string) (subdirs, files []os.DirEntry, symlinks []string, ignored []os.DirEntry, err error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for _, e := range entries {
		// OS/NAS junk ($RECYCLE.BIN, @eaDir, Thumbs.db, desktop.ini,
		// .DS_Store, .Trashes, ...): never a real album/loose file, must not
		// trip collision gating. Classify junk BEFORE the generic dot-prefix
		// skip below -- several junk names are themselves dot-prefixed
		// (.DS_Store, .Trash, .Trashes), and if the hidden-file skip ran
		// first they would be silently dropped instead of entering the
		// `ignored` bucket, so removeIgnoredJunk would never sweep them and
		// the leftover would block the final loser-dir unlink (#30).
		if IsIgnoredSystemName(e.Name()) {
			ignored = append(ignored, e)
			continue
		}
		// Non-junk hidden entries (dotfiles/dotdirs that are not OS/NAS junk)
		// are skipped: not album content, not a mergeable child.
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
	return subdirs, files, symlinks, ignored, nil
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
	subdirs, files, symlinks, _, err := enumerateChildren(loser.Path)
	if err != nil {
		return err
	}
	for _, sym := range symlinks {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("skipped symlink %q under %s (symlinks are not followed during merge)", sym, loser.Path))
	}
	for _, sd := range subdirs {
		survivorChild := filepath.Join(survivorPath, sd.Name())
		// extrafanart/ and extrathumbs/ are additive: both artists' extra
		// images can coexist under the survivor, so a same-named collision
		// here must NOT halt the merge -- BUT only when the survivor's
		// same-named entry is itself a directory (or absent, i.e. a plain
		// whole-dir move). If the survivor entry is a FILE or SYMLINK, the
		// additive content-merge cannot descend into it, so report it as a
		// collision like any other rather than silently skipping (which would
		// let the commit phase fail inside mergeAdditiveDir) (#28).
		if isAdditiveMergeDir(sd.Name()) {
			info, statErr := os.Lstat(survivorChild)
			switch {
			case os.IsNotExist(statErr):
				// Survivor lacks it entirely: commit does a whole-dir move
				// (moveLoserSubdirs falls through to RenameDirAtomic when
				// mergeAdditiveSubdirIfPresent sees no destination). Preview
				// the same single-entry whole-dir move.
				result.Moved = append(result.Moved, MovedItem{
					Name: sd.Name(),
					From: filepath.Join(loser.Path, sd.Name()),
					To:   survivorChild,
				})
				continue
			case statErr == nil && info.Mode().IsDir():
				// Both sides are real directories: commit does a per-file
				// content-merge (mergeAdditiveDir), not a directory rename.
				// Mirror that here so the dry-run Moved preview has the same
				// shape as what commit will actually produce, instead of a
				// single misleading whole-dir "Moved" entry.
				// (os.Lstat + IsDir treats a symlink as NOT a directory.)
				if err := previewMergeAdditiveDir(filepath.Join(loser.Path, sd.Name()), survivorChild, result); err != nil {
					return err
				}
				continue
			case statErr != nil:
				return fmt.Errorf("checking survivor additive dir %s: %w", survivorChild, statErr)
			default:
				// Survivor entry exists but is a file or symlink: cannot merge
				// the loser's additive dir into it. Surface as a conflict.
				result.Conflicts = append(result.Conflicts, ConflictItem{
					Name:         sd.Name(),
					SurvivorPath: survivorChild,
					LoserPath:    filepath.Join(loser.Path, sd.Name()),
				})
				continue
			}
		}
		if _, statErr := os.Lstat(survivorChild); statErr == nil {
			result.Conflicts = append(result.Conflicts, ConflictItem{
				Name:         sd.Name(),
				SurvivorPath: survivorChild,
				LoserPath:    filepath.Join(loser.Path, sd.Name()),
			})
		} else if os.IsNotExist(statErr) {
			// Survivor lacks this subdir; a real merge moves it whole.
			result.Moved = append(result.Moved, MovedItem{
				Name: sd.Name(),
				From: filepath.Join(loser.Path, sd.Name()),
				To:   survivorChild,
			})
		} else {
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
		} else if os.IsNotExist(statErr) {
			// Survivor lacks this loose file; a real merge moves it.
			result.Moved = append(result.Moved, MovedItem{
				Name: f.Name(),
				From: filepath.Join(loser.Path, f.Name()),
				To:   survivorChild,
			})
		} else {
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

	subdirs, files, symlinks, ignored, err := enumerateChildren(loser.Path)
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

	if err := moveLoserSubdirs(loser.Path, survivorPath, subdirs, result); err != nil {
		return false, err
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

	if err := removeIgnoredJunk(loser.Path, ignored); err != nil {
		return false, err
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

// moveLoserSubdirs moves every album subdirectory from the loser into the
// survivor during the commit phase. extrafanart/ and extrathumbs/ are additive:
// when the survivor already has one, the loser's extra images are merged into
// it (keeping both on a basename clash) instead of colliding; otherwise the
// whole directory moves via an atomic rename. A destination that appeared
// between pre-flight and commit is left in place with a warning (#28).
func moveLoserSubdirs(loserPath, survivorPath string, subdirs []os.DirEntry, result *MergeResult) error {
	for _, sd := range subdirs {
		src := filepath.Join(loserPath, sd.Name())
		dst := filepath.Join(survivorPath, sd.Name())
		if isAdditiveMergeDir(sd.Name()) {
			merged, err := mergeAdditiveSubdirIfPresent(src, dst, result)
			if err != nil {
				return err
			}
			if merged {
				continue
			}
			// Survivor has no such dir: fall through to the whole-dir move.
		}
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
			return fmt.Errorf("moving %s to %s: %w", src, dst, err)
		}
		result.Moved = append(result.Moved, MovedItem{Name: sd.Name(), From: src, To: dst})
	}
	return nil
}

// mergeAdditiveSubdirIfPresent merges src into dst when dst already exists,
// returning merged=true; when dst does not exist it returns merged=false so
// the caller falls back to a whole-directory move. After a merge the drained
// loser dir (only junk / empties remain) is removed.
func mergeAdditiveSubdirIfPresent(src, dst string, result *MergeResult) (merged bool, err error) {
	info, statErr := os.Lstat(dst)
	if os.IsNotExist(statErr) {
		return false, nil
	} else if statErr != nil {
		return false, fmt.Errorf("checking survivor additive dir %s: %w", dst, statErr)
	}
	// Defensive guard: pre-flight (preflightOneLoser) reports a conflict when
	// the survivor's same-named entry is a file or symlink, so the commit
	// phase should never reach this with a non-directory dst. Fail loudly with
	// a clear error rather than letting mergeAdditiveDir's os.ReadDir(dst)
	// produce an opaque failure if that invariant is ever violated.
	if !info.Mode().IsDir() {
		// Record the collision so result.Conflicts stays consistent with the
		// preflight path (which appends a ConflictItem for a file/symlink
		// survivor entry); consumers/UI otherwise see ErrMergeCollisions with
		// no corresponding conflict entry.
		result.Conflicts = append(result.Conflicts, ConflictItem{
			Name:         filepath.Base(dst),
			SurvivorPath: dst,
			LoserPath:    src,
		})
		return false, fmt.Errorf("%w: survivor entry %s exists but is not a directory; cannot merge additive dir into it",
			ErrMergeCollisions, dst)
	}
	if err := mergeAdditiveDir(src, dst, result); err != nil {
		return false, err
	}
	if err := os.RemoveAll(src); err != nil {
		return false, fmt.Errorf("removing merged additive dir %s: %w", src, err)
	}
	return true, nil
}

// removeIgnoredJunk deletes the OS/NAS junk entries ($RECYCLE.BIN, @eaDir,
// Thumbs.db, ...) that enumerateChildren deliberately set aside. It is never
// authoritative content, and leaving it behind would keep the loser directory
// non-empty and block the unlink at the end of the merge (#30). Every entry is
// inside loserPath, which the merge is unlinking wholesale, so removing it is
// in-scope.
func removeIgnoredJunk(loserPath string, ignored []os.DirEntry) error {
	for _, e := range ignored {
		p := filepath.Join(loserPath, e.Name())
		if e.IsDir() {
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("removing ignored junk dir %s: %w", p, err)
			}
			continue
		}
		if err := filesystem.RemoveFileSafe(p); err != nil {
			return fmt.Errorf("removing ignored junk file %s: %w", p, err)
		}
	}
	return nil
}

// mergeAdditiveDir merges the contents of an additive loser subdirectory
// (extrafanart/ or extrathumbs/) into the survivor's same-named directory.
// Each entry moves under the survivor; on a basename clash the loser's file is
// preserved under a de-duplicated name so BOTH artists' extra images survive
// (the additive contract for #28). Junk (dotfiles, OS/NAS caches) is left in
// place for the caller's os.RemoveAll to sweep. Symlinks are skipped for the
// same blast-radius reason as the top-level walk.
func mergeAdditiveDir(src, dst string, result *MergeResult) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading additive merge dir %s: %w", src, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || IsIgnoredSystemName(e.Name()) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("skipped symlink %q under %s (symlinks are not followed during merge)", e.Name(), src))
			continue
		}
		srcChild := filepath.Join(src, e.Name())
		dstChild := filepath.Join(dst, e.Name())
		if _, statErr := os.Lstat(dstChild); statErr == nil {
			// Basename clash: keep both by relocating the loser's copy to a
			// free name rather than overwriting or refusing.
			unique, uErr := uniqueDestName(dst, e.Name())
			if uErr != nil {
				return uErr
			}
			dstChild = filepath.Join(dst, unique)
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("checking additive destination %s: %w", dstChild, statErr)
		}
		if e.IsDir() {
			if err := filesystem.RenameDirAtomic(srcChild, dstChild); err != nil {
				return fmt.Errorf("moving additive dir %s to %s: %w", srcChild, dstChild, err)
			}
		} else {
			if err := filesystem.RenameFileAtomic(srcChild, dstChild); err != nil {
				return fmt.Errorf("moving additive file %s to %s: %w", srcChild, dstChild, err)
			}
		}
		result.Moved = append(result.Moved, MovedItem{Name: filepath.Base(dstChild), From: srcChild, To: dstChild})
	}
	return nil
}

// previewMergeAdditiveDir mirrors mergeAdditiveDir's per-file merge decisions
// during pre-flight, without touching the filesystem, so the dry-run Moved
// preview matches what the commit phase actually does for the
// both-dirs-exist additive-merge case (extrafanart/extrathumbs). A symlink
// is warned about exactly as mergeAdditiveDir does at commit; a basename
// clash previews the same de-duplicated destination name via uniqueDestName
// (itself read-only -- it only os.Lstats candidate names, it does not
// create anything).
func previewMergeAdditiveDir(loserDir, survivorDir string, result *MergeResult) error {
	entries, err := os.ReadDir(loserDir)
	if err != nil {
		return fmt.Errorf("reading additive merge dir %s: %w", loserDir, err)
	}
	// claimed tracks destination basenames already assigned to an earlier
	// entry within THIS preview pass. uniqueDestName only os.Lstats the
	// survivor dir, which a dry-run never actually writes to, so without this
	// set two distinct loser entries that both resolve to the same
	// de-duplicated name (e.g. one clashes to "foo-1.jpg", and a second loser
	// entry is itself literally named "foo-1.jpg") would both preview moving
	// to "foo-1.jpg" -- a false collision the real commit-time
	// mergeAdditiveDir never produces, because it renames one file at a time
	// so the on-disk check for the second file already sees the first file's
	// new name (#2322 CR-2). Marking each chosen dstName as claimed mirrors
	// that sequential effect without touching the filesystem.
	claimed := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || IsIgnoredSystemName(e.Name()) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("skipped symlink %q under %s (symlinks are not followed during merge)", e.Name(), loserDir))
			continue
		}
		srcChild := filepath.Join(loserDir, e.Name())
		dstName := e.Name()
		_, alreadyClaimed := claimed[dstName]
		_, statErr := os.Lstat(filepath.Join(survivorDir, dstName))
		switch {
		case statErr == nil || alreadyClaimed:
			// Basename clash (on disk, or against an earlier entry in this
			// same pass): preview the same de-duplicated name mergeAdditiveDir
			// will pick at commit time.
			unique, uErr := uniquePreviewDestName(survivorDir, e.Name(), claimed)
			if uErr != nil {
				return uErr
			}
			dstName = unique
		case !os.IsNotExist(statErr):
			return fmt.Errorf("checking additive destination %s: %w", filepath.Join(survivorDir, dstName), statErr)
		}
		claimed[dstName] = struct{}{}
		dstChild := filepath.Join(survivorDir, dstName)
		result.Moved = append(result.Moved, MovedItem{Name: dstName, From: srcChild, To: dstChild})
	}
	return nil
}

// uniquePreviewDestName mirrors uniqueDestName's "{stem}-{n}{ext}" probing but
// also treats names already claimed within the current previewMergeAdditiveDir
// pass as taken, since uniqueDestName's disk-only check cannot see them (a
// dry-run never writes, so the filesystem never reflects an earlier preview
// entry's chosen name).
func uniquePreviewDestName(dir, name string, claimed map[string]struct{}) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, taken := claimed[candidate]; taken {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("probing unique name %s in %s: %w", candidate, dir, err)
		}
	}
	return "", fmt.Errorf("%w: could not find a free name for %s in %s", ErrMergeInvalidRequest, name, dir)
}

// uniqueDestName returns a base name of the form "{stem}-{n}{ext}" that does
// not yet exist in dir, used to preserve a colliding additive image alongside
// the survivor's copy. The bounded loop guards against an unbounded scan on a
// pathological directory; in practice the first candidate is free.
func uniqueDestName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Lstat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("probing unique name %s in %s: %w", candidate, dir, err)
		}
	}
	return "", fmt.Errorf("%w: could not find a free name for %s in %s", ErrMergeInvalidRequest, name, dir)
}

// collectAffectedConnectionIDs returns the distinct, sorted union of platform
// connection IDs mapping the survivor or any loser. Called before the loser
// rows are deleted (their platform_ids cascade away on delete), so the
// post-merge platform refresh can reach every connection that indexed a member.
// Best-effort: a per-artist enumeration error is logged and skipped rather than
// failing the merge, since the merge itself has already moved files on disk.
func (s *Service) collectAffectedConnectionIDs(ctx context.Context, survivorID string, losers []NearDuplicateArtist) []string {
	seen := make(map[string]struct{})
	add := func(id string) {
		pids, err := s.GetPlatformIDs(ctx, id)
		if err != nil {
			slog.Warn("merge: enumerating platform IDs for affected-connection capture",
				"artist_id", id, "error", err)
			return
		}
		for _, p := range pids {
			if p.ConnectionID != "" {
				seen[p.ConnectionID] = struct{}{}
			}
		}
	}
	add(survivorID)
	for _, l := range losers {
		add(l.ID)
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
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
