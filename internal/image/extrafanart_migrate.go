package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// extraFanartDir is the subdirectory the migration drains (#3178).
const extraFanartDir = "extrafanart"

// MigrationDisposition is what the plan intends for one extrafanart/ file.
// There is deliberately NO "delete" disposition: the engine never removes an
// operator file.
type MigrationDisposition string

const (
	// DispositionMove renames Source to Dest.
	DispositionMove MigrationDisposition = "move"
	// DispositionSkipIdentical marks a Source that is byte-identical (sha256) to a file
	// already at the artist root. Reported and left exactly where it is.
	DispositionSkipIdentical MigrationDisposition = "skip-identical"
	// DispositionBlocked marks a file that cannot be planned or moved safely.
	DispositionBlocked MigrationDisposition = "blocked"
)

// MigrationEntry is one file in a plan. It is a value the caller inspects.
type MigrationEntry struct {
	Source      string
	Dest        string // empty unless Disposition is move
	Disposition MigrationDisposition
	Reason      string // why skipped or blocked
}

// ExtraFanartPlan is the read-only plan for one artist.
type ExtraFanartPlan struct {
	ArtistDir string
	Primary   string // convention resolved from the directory, not the profile
	Entries   []MigrationEntry
}

// PlanExtraFanartMigration computes, without writing anything, where each file
// under artistDir/extrafanart/ would go (#3178). names are the candidate primary
// names in preference order; the convention is resolved from the directory's
// contents with ResolveFanart, never from a profile. kodi selects the numbering
// offset (base1.ext vs base2.ext).
//
// Destination indices come from disk (MaxFanartIndex) and advance per planned
// move, so files in one plan never share a destination. Identity is sha256: it
// decides only what is REPORTED, never what is removed.
func PlanExtraFanartMigration(ctx context.Context, artistDir string, names []string, kodi bool) (*ExtraFanartPlan, error) {
	if fi, lerr := os.Lstat(filepath.Join(artistDir, extraFanartDir)); lerr == nil && !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory (symlink?); refusing to plan", extraFanartDir)
	}
	sources, err := ListArtworkSubdirFiles(ctx, artistDir, extraFanartDir)
	if err != nil {
		return nil, err
	}
	primary, _, err := ResolveFanart(ctx, artistDir, names)
	if err != nil {
		return nil, err
	}
	plan := &ExtraFanartPlan{ArtistDir: artistDir, Primary: primary}
	if len(sources) == 0 {
		return plan, nil
	}
	if primary == "" {
		return nil, errors.New("no fanart primary name to plan against")
	}

	// Hash every root image under ANY candidate name, not only the chosen convention.
	rootHashes := map[string]bool{}
	for _, n := range names {
		rootFiles, derr := DiscoverFanart(ctx, artistDir, n)
		if derr != nil {
			return nil, derr
		}
		for _, p := range rootFiles {
			h, herr := HashFile(ctx, p, false)
			if herr != nil {
				return nil, fmt.Errorf("hashing root fanart %s: %w", p, herr)
			}
			rootHashes[h.Content] = true
		}
	}
	maxSuffix, err := MaxFanartIndex(ctx, artistDir, primary)
	if err != nil {
		return nil, err
	}

	for _, src := range sources {
		if fi, lerr := os.Lstat(src); lerr != nil || !fi.Mode().IsRegular() {
			plan.Entries = append(plan.Entries, MigrationEntry{Source: src, Disposition: DispositionBlocked, Reason: "not a regular file (symlink?)"})
			continue
		}
		h, herr := HashFile(ctx, src, false)
		if herr != nil {
			plan.Entries = append(plan.Entries, MigrationEntry{Source: src, Disposition: DispositionBlocked, Reason: herr.Error()})
			continue
		}
		if rootHashes[h.Content] {
			plan.Entries = append(plan.Entries, MigrationEntry{Source: src, Disposition: DispositionSkipIdentical,
				Reason: "byte-identical to a file already at the artist root"})
			continue
		}
		rootHashes[h.Content] = true // a second identical source is a duplicate too
		idx := NextFanartIndex(maxSuffix, kodi)
		// Advance the running maximum to the suffix this index occupies.
		switch {
		case idx == 0:
			maxSuffix = 0
		case kodi:
			maxSuffix = idx
		default:
			maxSuffix = idx + 1
		}
		name := FanartFilename(primary, idx, kodi)
		// The source's own extension is kept: renaming png bytes to .jpg
		// would lie about the format.
		name = strings.TrimSuffix(name, filepath.Ext(name)) + strings.ToLower(filepath.Ext(src))
		plan.Entries = append(plan.Entries, MigrationEntry{Source: src, Dest: filepath.Join(artistDir, name), Disposition: DispositionMove})
	}
	return plan, nil
}

// MigrationOutcome is what apply did with one entry.
type MigrationOutcome string

// Per-file outcomes of an apply.
const (
	OutcomeMoved   MigrationOutcome = "moved"
	OutcomeSkipped MigrationOutcome = "skipped" // plan said skip-identical
	OutcomeBlocked MigrationOutcome = "blocked" // plan blocked, or dest occupied at apply time
	OutcomeGone    MigrationOutcome = "source-gone"
	OutcomeFailed  MigrationOutcome = "failed"
)

// DirOutcome is the fate of the extrafanart/ directory itself. It is a
// directory outcome, not a per-file disposition.
type DirOutcome string

// Outcomes for the extrafanart/ directory after an apply.
const (
	DirUntouched    DirOutcome = ""
	DirRemoved      DirOutcome = "removed"
	DirKeptNotEmpty DirOutcome = "kept-not-empty"
	DirKeptError    DirOutcome = "kept-error"
)

// MigrationResult reports one entry's outcome; Err is set for blocked/failed.
type MigrationResult struct {
	Entry   MigrationEntry
	Outcome MigrationOutcome
	Err     error
}

// ExtraFanartApplyResult is the truthful per-file account of an apply.
type ExtraFanartApplyResult struct {
	Results    []MigrationResult
	Moved      int
	Dir        DirOutcome
	DirErr     error
	InvalidErr error // hash/geometry invalidation failure, if any
}

// ApplyExtraFanartMigration executes a plan. The migration is ONE-WAY: there
// is no rollback and no manifest; the dry run (the plan) is the safety net.
//
// The only filesystem-mutating calls are os.Rename of a file and, at the end,
// os.Remove of the extrafanart/ directory (which fails if non-empty; never
// RemoveAll). No operator file is deleted or overwritten.
//
// A failure on one file is recorded and the rest proceed. A second apply of
// the same plan finds every source gone and does nothing.
func ApplyExtraFanartMigration(ctx context.Context, inv HashInvalidator, artistID string, plan *ExtraFanartPlan) (*ExtraFanartApplyResult, error) {
	res := &ExtraFanartApplyResult{}
	allMoved := len(plan.Entries) > 0
	var ctxErr error
	for _, e := range plan.Entries {
		if ctxErr = ctx.Err(); ctxErr != nil {
			allMoved = false
			break // still invalidate whatever already moved
		}
		r := MigrationResult{Entry: e}
		switch e.Disposition {
		case DispositionSkipIdentical:
			r.Outcome = OutcomeSkipped
		case DispositionMove:
			r.Outcome, r.Err = moveNoOverwrite(e)
		default:
			r.Outcome = OutcomeBlocked
		}
		switch r.Outcome {
		case OutcomeMoved:
			res.Moved++
		case OutcomeGone:
			// Already migrated by an earlier apply; neither a failure nor
			// a reason to keep the directory.
		default:
			allMoved = false
		}
		res.Results = append(res.Results, r)
	}

	if res.Moved > 0 {
		// Files moved INTO fanart slots: stored per-slot hashes and geometry
		// now describe different files (see RenumberFanart).
		ictx := context.WithoutCancel(ctx) // moved files need invalidating even after a cancel
		if err := inv.InvalidateImageHashes(ictx, artistID, "fanart"); err != nil {
			res.InvalidErr = err
		} else if err := inv.InvalidateImageGeometry(ictx, artistID, "fanart"); err != nil {
			res.InvalidErr = err
		}
	}

	if allMoved && res.Moved > 0 {
		dir := filepath.Join(plan.ArtistDir, extraFanartDir)
		if fi, lerr := os.Lstat(dir); lerr != nil || !fi.IsDir() {
			res.Dir, res.DirErr = DirKeptError, fmt.Errorf("%s is not a real directory; leaving it", dir)
		} else if left, err := os.ReadDir(dir); err == nil && len(left) > 0 {
			res.Dir = DirKeptNotEmpty // a dotfile or non-image file remains
		} else if err := os.Remove(dir); err != nil {
			res.Dir, res.DirErr = DirKeptError, err
		} else {
			res.Dir = DirRemoved
		}
	}
	return res, ctxErr
}

// moveNoOverwrite renames e.Source to e.Dest unless anything is at Dest.
// os.Rename silently replaces on POSIX, so Dest is Lstat'd first; the small
// check-to-rename window is accepted because closing it (link-then-remove,
// copy-then-delete) would add a delete this engine forbids.
func moveNoOverwrite(e MigrationEntry) (MigrationOutcome, error) {
	if _, err := os.Lstat(e.Source); errors.Is(err, fs.ErrNotExist) {
		return OutcomeGone, nil
	} else if err != nil {
		return OutcomeFailed, err
	}
	if _, err := os.Lstat(e.Dest); err == nil {
		return OutcomeBlocked, fmt.Errorf("destination %s is occupied; refusing to overwrite", e.Dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return OutcomeFailed, err
	}
	if err := os.Rename(e.Source, e.Dest); err != nil {
		return OutcomeFailed, err
	}
	return OutcomeMoved, nil
}
