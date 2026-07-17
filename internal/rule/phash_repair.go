// Package rule -- phash_repair.go
//
// The destructive back-out for cross-artist backdrop pollution (#2564 PR-3),
// and the restore that makes it survivable. Consumes PR-2's detector
// (phash_mismatch.go); this file is the half that deletes a user's artwork.
//
// # The safety argument, and what each piece of it carries
//
// The detection signal is a perceptual collision, and a collision is SYMMETRIC:
// it proves artists A and B share a picture, never which of them owns it. Two
// artists can also legitimately share promo art -- a duo, a collaboration, a
// festival shot -- which lands as a true collision and a FALSE pollution
// report. So this path acts on an ambiguous signal by construction, and every
// safeguard here exists to absorb that ambiguity rather than to pretend it is
// absent:
//
//   - fresh re-detection per artist, never a stale report handed in by a caller;
//   - re-verification against the BYTES ON DISK before each removal;
//   - durable quarantine of the bytes BEFORE anything is removed;
//   - staged tombs with the commit deferred to img.RenumberFanart;
//   - a restore path that actually works (see below);
//   - artist scoping by default, and a full slog audit trail.
//
// Remove any one of them and what is left is a tool that deletes real artwork on
// a maybe.
//
// # Why restore does not trust the recorded index
//
// This is the load-bearing subtlety, and getting it wrong collapses the whole
// safety argument.
//
// Removal renumbers: img.RenumberFanart closes the gap so the survivors occupy
// contiguous ordinals. (Emby does the same thing on its own side, re-indexing
// after each delete, which is why PR-4's remote pass deletes in descending
// order.) So a recorded slot index is STALE BY CONSTRUCTION the instant the
// operation commits. By restore time, ordinal N denotes a DIFFERENT picture --
// or nothing at all.
//
// A restore that writes the quarantined bytes back "at slot N" would therefore
// overwrite a bystander backdrop with the image that was deliberately removed:
// it would CAUSE the exact cross-artist corruption this issue exists to back
// out, while reporting success. The recorded index is provenance for the audit
// trail, not an address (see image.RepairEntry.SlotIndex).
//
// The fix is not to re-derive the old position -- there is nothing to re-derive.
// The removed image is gone; its ordinal was reclaimed by a survivor, and no
// gap survives for it to return to. Position is simply not recoverable
// information, and any scheme that reconstructs one is inventing it.
//
// So restore is CONTENT-ADDRESSED and index-free:
//
//   - It re-reads the artist's fanart from disk and hashes it fresh.
//   - If the quarantined picture is already present (perceptual match within
//     tolerance), restoring is a NO-OP and the entry is consumed. This makes
//     restore idempotent and safe to retry.
//   - Otherwise it APPENDS the bytes at the next free ordinal. Appending can
//     never overwrite a bystander, which is the property that matters; the
//     artwork is what must come back, not the ordinal it used to sit on. An
//     operator who cares about ordering can reorder afterwards.
//
// The recorded index is never used to decide where the bytes go. That is what
// makes this correct under index shift rather than merely correct when nothing
// moved.
package rule

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// newRepairOpID mints an id for one back-out operation. A UUID's canonical
// string is lowercase hex with single hyphens and 36 characters, which already
// satisfies image.RepairDirName's op-id sanitizer -- no separator, no dot, no
// "..", comfortably inside its length bound -- so the quarantine path cannot be
// steered by an id minted here.
func newRepairOpID() string {
	return uuid.New().String()
}

// phashTombSuffix marks a slot staged for removal but not yet committed.
// Distinct from the duplicate fixer's ".dup_pending_delete.tmp" so a crashed
// run of either operation is attributable to the operation that made it, and so
// neither ever clears the other's staged artwork as "stale".
const phashTombSuffix = ".phash_pending_delete.tmp"

// PHashRemediateOpts controls a back-out run.
type PHashRemediateOpts struct {
	// AllArtists must be set explicitly to run without an artist scope.
	// The default is per-artist BECAUSE this deletes files: an unscoped run
	// at a badly chosen tolerance is a library-wide artwork loss, and it must
	// not be reachable by forgetting a parameter.
	AllArtists bool

	// DryRun previews without mutating anything.
	DryRun bool
}

// PHashSlotOutcome is one slot's fate, for the audit trail and the response.
type PHashSlotOutcome struct {
	ArtistID   string  `json:"artist_id"`
	ArtistName string  `json:"artist_name"`
	SlotIndex  int     `json:"slot_index"`
	FileName   string  `json:"file_name"`
	Similarity float64 `json:"similarity"`

	MatchedArtistID   string `json:"matched_artist_id,omitempty"`
	MatchedArtistName string `json:"matched_artist_name,omitempty"`

	// Action is "removed", "skipped", or "would-remove" (dry run).
	Action string `json:"action"`

	// Reason explains a skip. Empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// PHashRemediateResult summarizes a back-out run.
type PHashRemediateResult struct {
	OpID   string `json:"op_id"`
	DryRun bool   `json:"dry_run"`

	ArtistsProcessed int `json:"artists_processed"`
	SlotsRemoved     int `json:"slots_removed"`
	Quarantined      int `json:"quarantined"`

	// SlotsSkipped counts flagged slots deliberately NOT removed --
	// re-verification disagreed with the report. Held apart from Failures:
	// a skip is the safeguard working, a failure is the operation breaking.
	SlotsSkipped int `json:"slots_skipped"`

	// Failures counts artists whose remediation errored. Their staged files
	// were rolled back; their quarantined bytes are deliberately RETAINED.
	Failures int `json:"failures"`

	Outcomes []PHashSlotOutcome `json:"outcomes"`
}

// PHashRestoreResult summarizes a restore run.
type PHashRestoreResult struct {
	OpID string `json:"op_id"`

	// Restored counts entries whose bytes were written back. AlreadyPresent
	// counts entries whose picture was found still on disk, so restoring was
	// a no-op -- reported separately because "nothing to do" and "put it
	// back" are different facts and collapsing them would make a restore
	// that silently did nothing indistinguishable from one that worked.
	Restored       int `json:"restored"`
	AlreadyPresent int `json:"already_present"`

	Failures []string `json:"failures,omitempty"`
}

// RemediatePHashMismatches backs out cross-artist backdrop pollution for the
// scoped artist, locally on disk.
//
// It RE-DETECTS from the live database rather than accepting a report from the
// caller. A report is a snapshot; between rendering it and confirming it the
// library can be rescanned, re-hashed, or repaired by another path, and acting
// on the stale copy would delete whatever has since moved into the flagged
// ordinal. The dry run an operator approves and the run that executes are
// therefore two independent detections, and the second one is authoritative.
//
// The Emby-side per-slot deletion and its restore are PR-4. This pass is local
// only: it never contacts a platform.
func (p *Pipeline) RemediatePHashMismatches(ctx context.Context, scope PHashMismatchScope, opts PHashRemediateOpts) (PHashRemediateResult, error) {
	if p.engine == nil || p.engine.db == nil || p.artistService == nil {
		return PHashRemediateResult{}, fmt.Errorf("remediate phash mismatches: pipeline not fully wired")
	}
	if scope.ArtistID == "" && !opts.AllArtists {
		return PHashRemediateResult{}, fmt.Errorf("remediate phash mismatches: an artist scope is required; set AllArtists to run library-wide")
	}
	// Reject a NaN tolerance HERE as well as in ScanPHashMismatches, rather
	// than relying on the scan to normalize it. The scan's guard silently
	// falls back to the default, which is right for a read-only report but
	// wrong for a deletion: an operator who asked for a specific tolerance
	// and got the default instead would be confirming a suspect set that is
	// not the one they configured. On a path that deletes files, an
	// unusable tolerance is an error, not a nudge back to the default.
	if scope.Tolerance != 0 && (math.IsNaN(scope.Tolerance) || scope.Tolerance <= 0 || scope.Tolerance > 1) {
		return PHashRemediateResult{}, fmt.Errorf("remediate phash mismatches: tolerance must be within (0, 1], got %v", scope.Tolerance)
	}

	primaryName := resolveFanartPrimaryName(ctx, p.engine.platformService)
	if primaryName == "" {
		return PHashRemediateResult{}, fmt.Errorf("remediate phash mismatches: no fanart naming convention")
	}
	kodiNumbering := p.kodiFanartNumbering(ctx)

	report, err := p.ScanPHashMismatches(ctx, scope)
	if err != nil {
		return PHashRemediateResult{}, fmt.Errorf("re-detecting before remediation: %w", err)
	}

	result := PHashRemediateResult{OpID: newRepairOpID(), DryRun: opts.DryRun}
	p.logger.Info("phash back-out starting",
		slog.String("op_id", result.OpID),
		slog.Bool("dry_run", opts.DryRun),
		slog.String("scoped_artist_id", scope.ArtistID),
		slog.Bool("all_artists", opts.AllArtists),
		slog.Float64("tolerance", report.Tolerance),
		slog.Int("artists_affected", report.ArtistsAffected),
		slog.Int("suspect_slots", report.SuspectSlots),
		slog.Int("indeterminate_slots", report.IndeterminateSlots))

	for _, am := range report.PerArtist {
		if len(am.Suspects) == 0 {
			continue
		}
		a, err := p.artistService.GetByID(ctx, am.ArtistID)
		if err != nil {
			p.logger.Error("phash back-out could not load artist",
				slog.String("op_id", result.OpID),
				slog.String("artist_id", am.ArtistID),
				slog.String("error", err.Error()))
			result.Failures++
			continue
		}
		if a.Path == "" {
			p.logger.Warn("phash back-out skipping artist with no path",
				slog.String("op_id", result.OpID), slog.String("artist_id", am.ArtistID))
			continue
		}
		result.ArtistsProcessed++
		if err := p.remediateArtistPHash(ctx, a, am, result.OpID, primaryName, kodiNumbering, opts, &result); err != nil {
			p.logger.Error("phash back-out failed for artist",
				slog.String("op_id", result.OpID),
				slog.String("artist_id", am.ArtistID),
				slog.String("artist", am.Name),
				slog.String("error", err.Error()))
			result.Failures++
		}
	}

	p.logger.Info("phash back-out finished",
		slog.String("op_id", result.OpID),
		slog.Bool("dry_run", opts.DryRun),
		slog.Int("artists_processed", result.ArtistsProcessed),
		slog.Int("slots_removed", result.SlotsRemoved),
		slog.Int("quarantined", result.Quarantined),
		slog.Int("slots_skipped", result.SlotsSkipped),
		slog.Int("failures", result.Failures))
	return result, nil
}

// remediateArtistPHash quarantines and removes one artist's confirmed suspect
// slots, then renumbers the survivors.
//
// The sequence is quarantine -> stage -> renumber(commit) -> unlink tombs, and
// the order is the crash-safety contract: bytes are durably copied elsewhere
// before the original is touched, the original is staged rather than unlinked,
// and the staging is only made permanent once the renumber that closes the gap
// has succeeded. A crash at any point leaves the artwork readable from the
// quarantine, the original, or the tomb -- never from nowhere.
func (p *Pipeline) remediateArtistPHash(
	ctx context.Context,
	a *artist.Artist,
	am ArtistPHashMismatch,
	opID, primaryName string,
	kodiNumbering bool,
	opts PHashRemediateOpts,
	result *PHashRemediateResult,
) error {
	paths, err := img.DiscoverFanart(a.Path, primaryName)
	if err != nil {
		return fmt.Errorf("discovering fanart for %s: %w", a.Name, err)
	}

	confirmed := make(map[int]bool)
	for _, s := range am.Suspects {
		outcome := PHashSlotOutcome{
			ArtistID: a.ID, ArtistName: a.Name, SlotIndex: s.SlotIndex,
			Similarity:        s.Similarity,
			MatchedArtistID:   s.MatchedArtistID,
			MatchedArtistName: s.MatchedArtistName,
		}
		if s.SlotIndex < 0 || s.SlotIndex >= len(paths) {
			outcome.Action, outcome.Reason = "skipped", "slot no longer exists on disk"
			p.logger.Warn("phash back-out skipping vanished slot",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.Int("slot_index", s.SlotIndex))
			result.SlotsSkipped++
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		path := paths[s.SlotIndex]
		outcome.FileName = filepath.Base(path)

		// Re-verify against the BYTES ON DISK, not the row the detector
		// read. The detector works from artist_images.phash, which is a
		// cache; if it is stale -- the file was replaced since the last
		// hash write -- the ordinal now holds a picture nobody flagged,
		// and deleting it would destroy artwork on the strength of a
		// hash that no longer describes it. Re-hashing from the file is
		// the only check that binds the decision to what is actually
		// about to be deleted.
		ok, reason := p.reverifySlotPHash(path, s.PHash)
		if !ok {
			outcome.Action, outcome.Reason = "skipped", reason
			p.logger.Warn("phash back-out skipping slot that failed re-verification",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.Int("slot_index", s.SlotIndex),
				slog.String("file", outcome.FileName), slog.String("reason", reason))
			result.SlotsSkipped++
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}

		if opts.DryRun {
			outcome.Action = "would-remove"
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}

		entry := img.RepairEntry{
			ArtistID: a.ID, ArtistName: a.Name, ImageType: "fanart",
			SlotIndex: s.SlotIndex, FileName: filepath.Base(path),
			PHash:             s.PHash,
			MatchedArtistID:   s.MatchedArtistID,
			MatchedArtistName: s.MatchedArtistName,
			Similarity:        s.Similarity,
		}
		if err := img.QuarantineImage(a.Path, opID, path, entry); err != nil {
			return fmt.Errorf("quarantining slot %d for %s: %w", s.SlotIndex, a.Name, err)
		}
		result.Quarantined++
		p.logger.Info("phash back-out quarantined slot",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.Int("slot_index", s.SlotIndex),
			slog.String("file", entry.FileName),
			slog.String("matched_artist_id", s.MatchedArtistID),
			slog.String("matched_artist", s.MatchedArtistName),
			slog.Float64("similarity", s.Similarity))

		outcome.Action = "removed"
		result.Outcomes = append(result.Outcomes, outcome)
		confirmed[s.SlotIndex] = true
	}

	if opts.DryRun || len(confirmed) == 0 {
		return nil
	}

	removed, err := p.stageAndCommitPHashRemoval(ctx, a, primaryName, kodiNumbering, paths, confirmed, opID)
	if err != nil {
		return err
	}
	result.SlotsRemoved += len(removed)

	resyncFanartFields(a, primaryName)
	if err := p.artistService.Update(ctx, a); err != nil {
		return fmt.Errorf("updating artist %s after back-out: %w", a.Name, err)
	}
	return nil
}

// reverifySlotPHash re-hashes the file at path and reports whether it still
// carries the hash the detector flagged.
//
// An empty flagged hash is refused rather than treated as a wildcard. A zero or
// absent phash is UNKNOWN, not a value: it is Hamming-distance-0 from every
// other unknown, so admitting it here would let "we do not know what this is"
// match "we do not know what that is" and manufacture a confident deletion out
// of two absences. Unknown never matches unknown.
func (p *Pipeline) reverifySlotPHash(path, flagged string) (bool, string) {
	if flagged == "" {
		return false, "flagged slot carries no stored phash; refusing to remove on an unknown hash"
	}
	f, err := os.Open(path) //nolint:gosec // path is a DiscoverFanart result under the artist dir
	if err != nil {
		return false, fmt.Sprintf("re-reading slot: %v", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close after read

	h, err := img.PerceptualHash(f)
	if err != nil {
		return false, fmt.Sprintf("re-hashing slot: %v", err)
	}
	actual := img.HashHex(h)
	if !strings.EqualFold(actual, flagged) {
		return false, "on-disk image no longer matches the flagged hash; the slot changed since detection"
	}
	return true, ""
}

// stageAndCommitPHashRemoval stages each confirmed slot to a tomb, commits by
// renumbering the survivors, then unlinks the tombs. On any failure before the
// commit, staged tombs are restored to their original paths.
//
// Mirrors ImageDuplicateFixer.deleteDuplicateFanartWithRollback, including its
// refusal to restore onto an occupied path: RenumberFanart's own best-effort
// rollback can leave a renumbered survivor sitting exactly where a tombed slot
// used to be, and renaming the tomb over it would silently overwrite live
// artwork while "rolling back". The tomb is left in place instead -- recoverable
// and inert, since discovery ignores its suffix -- and the error says so.
//
// Quarantined bytes are deliberately NOT cleaned up on the failure path. If a
// tomb restore was refused, the quarantine is the ONLY remaining copy of that
// artwork, and tidying it away to keep the manifest neat would destroy the very
// thing the quarantine exists to preserve. A manifest entry for an image that
// is still on disk is harmless: RestorePHashQuarantine is content-addressed, so
// it recognizes the picture as already present and consumes the entry without
// writing anything.
func (p *Pipeline) stageAndCommitPHashRemoval(
	ctx context.Context,
	a *artist.Artist,
	primaryName string,
	kodiNumbering bool,
	paths []string,
	confirmed map[int]bool,
	opID string,
) ([]string, error) {
	type stagedSlot struct{ origPath, tombPath string }
	var staged []stagedSlot
	var removedNames []string
	survivors := make([]string, 0, len(paths))

	restoreStaged := func() []string {
		var rollbackErrs []string
		for _, s := range staged {
			if _, statErr := os.Lstat(s.origPath); statErr == nil {
				p.logger.Error("refusing to restore a staged backdrop onto an occupied path",
					slog.String("op_id", opID), slog.String("artist", a.Name),
					slog.String("path", s.origPath), slog.String("tomb", filepath.Base(s.tombPath)))
				rollbackErrs = append(rollbackErrs, fmt.Sprintf(
					"restore %s: refused -- path is occupied (tomb left at %s)",
					filepath.Base(s.origPath), filepath.Base(s.tombPath)))
				continue
			} else if !os.IsNotExist(statErr) {
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("restore %s: checking occupancy: %v", filepath.Base(s.origPath), statErr))
				continue
			}
			if rbErr := os.Rename(s.tombPath, s.origPath); rbErr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("restore %s: %v", filepath.Base(s.origPath), rbErr))
			}
		}
		return rollbackErrs
	}

	for i, path := range paths {
		if !confirmed[i] {
			survivors = append(survivors, path)
			continue
		}
		tombPath := path + phashTombSuffix
		if rmErr := os.Remove(tombPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, wrapWithRollbackErrs(restoreStaged(),
				fmt.Errorf("clearing stale tomb %s for %s: %w", filepath.Base(tombPath), a.Name, rmErr))
		}
		if stageErr := os.Rename(path, tombPath); stageErr != nil {
			return nil, wrapWithRollbackErrs(restoreStaged(),
				fmt.Errorf("staging backdrop %s for removal for %s: %w", filepath.Base(path), a.Name, stageErr))
		}
		staged = append(staged, stagedSlot{origPath: path, tombPath: tombPath})
		removedNames = append(removedNames, filepath.Base(path))
	}

	if err := img.RenumberFanart(ctx, p.engine.imageHashRecorder, a.ID, a.Path, primaryName, survivors, kodiNumbering); err != nil {
		return nil, wrapWithRollbackErrs(restoreStaged(),
			fmt.Errorf("renumbering fanart after removing %d slot(s) (%s) for %s: %w",
				len(removedNames), strings.Join(removedNames, ", "), a.Name, err))
	}

	for _, s := range staged {
		if rmErr := os.Remove(s.tombPath); rmErr != nil {
			p.logger.Warn("removing staged backdrop tomb after renumber",
				slog.String("op_id", opID), slog.String("artist", a.Name),
				slog.String("tomb", filepath.Base(s.tombPath)), slog.String("error", rmErr.Error()))
		}
	}
	p.logger.Info("phash back-out removed slots locally",
		slog.String("op_id", opID), slog.String("artist_id", a.ID),
		slog.String("artist", a.Name), slog.Int("slots_removed", len(removedNames)),
		slog.String("files", strings.Join(removedNames, ", ")))
	return removedNames, nil
}

// RestorePHashQuarantine puts a repair operation's quarantined backdrops back.
//
// Content-addressed and index-free -- see this file's package comment for why
// the recorded slot index must never be used as a write target. Each entry is
// either recognized as already present (no-op, consumed) or APPENDED at the next
// free ordinal. Appending cannot overwrite a bystander, which is the property
// that makes a restore safe to run against a library that has moved on.
//
// Entries are consumed only after their bytes are back, so an interrupted
// restore can be re-run and picks up where it stopped.
func (p *Pipeline) RestorePHashQuarantine(ctx context.Context, artistID, opID string) (PHashRestoreResult, error) {
	if p.engine == nil || p.artistService == nil {
		return PHashRestoreResult{}, fmt.Errorf("restore phash quarantine: pipeline not fully wired")
	}
	a, err := p.artistService.GetByID(ctx, artistID)
	if err != nil {
		return PHashRestoreResult{}, fmt.Errorf("loading artist %s: %w", artistID, err)
	}
	if a.Path == "" {
		return PHashRestoreResult{}, fmt.Errorf("restore phash quarantine: artist %s has no path", artistID)
	}
	primaryName := resolveFanartPrimaryName(ctx, p.engine.platformService)
	if primaryName == "" {
		return PHashRestoreResult{}, fmt.Errorf("restore phash quarantine: no fanart naming convention")
	}
	kodiNumbering := p.kodiFanartNumbering(ctx)

	m, err := img.ReadRepairManifest(a.Path, opID)
	if err != nil {
		return PHashRestoreResult{}, err
	}
	if m == nil {
		return PHashRestoreResult{}, fmt.Errorf("restore phash quarantine: no repair operation %q for artist %s", opID, artistID)
	}

	result := PHashRestoreResult{OpID: opID}
	// Snapshot the entries: each restore mutates the manifest via
	// ConsumeRepairEntry, so iterating the live copy would be a
	// read-during-write over the same file.
	entries := make([]img.RepairEntry, len(m.Entries))
	copy(entries, m.Entries)

	for i := range entries {
		entry := &entries[i]
		restored, err := p.restoreOneQuarantined(ctx, a, entry, opID, primaryName, kodiNumbering)
		if err != nil {
			p.logger.Error("restoring quarantined backdrop",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.String("file", entry.FileName),
				slog.String("error", err.Error()))
			result.Failures = append(result.Failures, fmt.Sprintf("%s: %v", entry.FileName, err))
			continue
		}
		if restored {
			result.Restored++
		} else {
			result.AlreadyPresent++
		}
	}

	resyncFanartFields(a, primaryName)
	if err := p.artistService.Update(ctx, a); err != nil {
		return result, fmt.Errorf("updating artist %s after restore: %w", a.Name, err)
	}
	p.logger.Info("phash quarantine restore finished",
		slog.String("op_id", opID), slog.String("artist_id", a.ID),
		slog.String("artist", a.Name), slog.Int("restored", result.Restored),
		slog.Int("already_present", result.AlreadyPresent),
		slog.Int("failures", len(result.Failures)))
	return result, nil
}

// restoreOneQuarantined reinstates a single entry. Returns true when bytes were
// written, false when the picture was already present.
func (p *Pipeline) restoreOneQuarantined(
	ctx context.Context,
	a *artist.Artist,
	entry *img.RepairEntry,
	opID, primaryName string,
	kodiNumbering bool,
) (bool, error) {
	data, err := img.RepairEntryBytes(a.Path, opID, *entry)
	if err != nil {
		return false, err
	}

	present, err := p.quarantinedImagePresent(a.Path, primaryName, data)
	if err != nil {
		return false, err
	}
	if present {
		p.logger.Info("quarantined backdrop already present; restore is a no-op",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("file", entry.FileName))
		return false, img.ConsumeRepairEntry(a.Path, opID, *entry)
	}

	// Append at the next free ordinal. Re-discovered per entry rather than
	// counted once outside the loop: each restore adds a slot, so a cached
	// length would aim the second entry at the ordinal the first just took
	// and clobber it.
	paths, err := img.DiscoverFanart(a.Path, primaryName)
	if err != nil {
		return false, fmt.Errorf("discovering fanart: %w", err)
	}
	target := filepath.Join(a.Path, img.FanartFilename(primaryName, len(paths), kodiNumbering))
	if _, statErr := os.Lstat(target); statErr == nil {
		// Refuse rather than clobber. Discovery only counts recognized
		// artwork names, so a stray file can occupy the computed target
		// without being a slot; overwriting it here would destroy a file
		// this feature never took.
		return false, fmt.Errorf("refusing to restore onto occupied path %s", filepath.Base(target))
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("checking restore target %s: %w", filepath.Base(target), statErr)
	}

	if err := img.WriteFanartBytes(target, data); err != nil {
		return false, err
	}
	// Invalidate the artist's stored fanart hashes: a new slot exists and
	// the cached rows no longer describe the directory. Same reasoning as
	// RenumberFanart's unconditional invalidation -- a stale hash left
	// pointing at a reshuffled slot is exactly the starved/wrong-hash state
	// that makes the detector unreliable.
	if p.engine.imageHashRecorder != nil {
		if err := p.engine.imageHashRecorder.InvalidateImageHashes(ctx, a.ID, "fanart"); err != nil {
			p.logger.Warn("invalidating fanart hashes after restore",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("error", err.Error()))
		}
	}

	p.logger.Info("restored quarantined backdrop",
		slog.String("op_id", opID), slog.String("artist_id", a.ID),
		slog.String("artist", a.Name), slog.String("file", entry.FileName),
		slog.String("restored_as", filepath.Base(target)),
		slog.Int("original_slot_index", entry.SlotIndex),
		slog.Int("restored_slot_index", len(paths)))
	return true, img.ConsumeRepairEntry(a.Path, opID, *entry)
}

// quarantinedImagePresent reports whether the artist already holds the
// quarantined picture, making a restore a no-op.
//
// Byte equality is checked first (the common case: nothing touched the file
// since it was quarantined) and is exact. The perceptual fallback catches a
// re-encode of the same picture. Both are content comparisons; neither consults
// a slot index.
func (p *Pipeline) quarantinedImagePresent(dir, primaryName string, data []byte) (bool, error) {
	paths, err := img.DiscoverFanart(dir, primaryName)
	if err != nil {
		return false, fmt.Errorf("discovering fanart: %w", err)
	}
	if len(paths) == 0 {
		return false, nil
	}

	want, wantErr := img.PerceptualHash(bytes.NewReader(data))
	for _, path := range paths {
		onDisk, readErr := os.ReadFile(path) //nolint:gosec // path is a DiscoverFanart result under the artist dir
		if readErr != nil {
			continue
		}
		if bytes.Equal(onDisk, data) {
			return true, nil
		}
		if wantErr != nil {
			continue
		}
		got, hashErr := img.PerceptualHash(bytes.NewReader(onDisk))
		if hashErr != nil {
			continue
		}
		// Compared at the same tolerance the removal was decided at, so
		// restore recognizes "the same picture" by the identical
		// standard that called it pollution. A stricter test here would
		// re-append a near-duplicate the operator already has; a looser
		// one would refuse to restore a picture that is genuinely absent.
		if img.Similarity(want, got) >= defaultPHashMismatchTolerance {
			return true, nil
		}
	}
	return false, nil
}

// kodiFanartNumbering reports whether the active platform profile uses Kodi's
// fanart numbering convention.
func (p *Pipeline) kodiFanartNumbering(ctx context.Context) bool {
	if p.engine == nil || p.engine.platformService == nil {
		return false
	}
	profile, err := p.engine.platformService.GetActive(ctx)
	return err == nil && profile != nil && strings.EqualFold(profile.ID, "kodi")
}
