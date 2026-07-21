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
// So restore is CONTENT-ADDRESSED and index-free. It re-reads the artist's
// fanart from disk, hashes it fresh, and lands in exactly one of three states:
//
//   - A BYTE-IDENTICAL copy is on disk. Restoring is a no-op and the entry is
//     consumed -- the bytes are provably recoverable from the artist directory,
//     so dropping the quarantine copy destroys nothing. This makes restore
//     idempotent and safe to retry.
//   - A survivor merely RESEMBLES it. Restore is DECLINED and the entry is
//     RETAINED for review; see restoreNeedsReview.
//   - Otherwise the bytes are APPENDED at the next free ordinal. Appending can
//     never overwrite a bystander, which is the property that matters; the
//     artwork is what must come back, not the ordinal it used to sit on. An
//     operator who cares about ordering can reorder afterwards.
//
// The recorded index is never used to decide where the bytes go. That is what
// makes this correct under index shift rather than merely correct when nothing
// moved.
//
// # Why a resemblance is not a license to delete
//
// The middle state exists because conflating it with the first destroyed
// artwork. A perceptual hash says two pictures LOOK alike at 64 bits of
// resolution; it never says one IS the other. Treating a resemblance as
// "already present" meant consuming the entry -- unlinking the quarantined
// bytes -- while the original was ALREADY GONE, so the only surviving copy of a
// picture this tool had deleted was destroyed by the path meant to recover it,
// and the run reported success. Only byte equality proves the bytes are
// somewhere else; only that may authorize the delete. A resemblance may
// suppress an append, and nothing more.
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
	"sync"

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

// phashArtistMutex returns the one mutex guarding an artist's back-out/restore
// critical section. LoadOrStore guarantees every caller for a given artist id
// gets the SAME mutex even when several arrive at once. See Pipeline.phashArtistMu
// for why this serialization is load-bearing and invisible to -race.
//
// Only ONE such mutex is ever held at a time by design: remediate and restore
// each lock exactly one artist for the duration of that artist's work, so there
// is no multi-lock acquisition and no lock-ordering hazard with image.repairOpMutex,
// which is always taken strictly inside this one.
func (p *Pipeline) phashArtistMutex(artistID string) *sync.Mutex {
	mu, _ := p.phashArtistMu.LoadOrStore(artistID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// reconcileArtistMutex returns the one mutex serializing reconcileAfterFix for
// an artist id. LoadOrStore guarantees every caller for a given artist id gets
// the SAME mutex even when several arrive at once. See Pipeline.reconcileArtistMu
// for why this serialization is load-bearing, invisible to -race, and a separate
// lock from phashArtistMu (reusing that one would self-deadlock on the phash
// back-out path, which holds it while calling reconcileAfterFix).
func (p *Pipeline) reconcileArtistMutex(artistID string) *sync.Mutex {
	mu, _ := p.reconcileArtistMu.LoadOrStore(artistID, &sync.Mutex{})
	return mu.(*sync.Mutex)
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

	// Action is "removed", "skipped", "would-remove" (dry run), or "failed"
	// (staged for removal, but the renumber/commit did not confirm, so the
	// staged tombs were rolled back and nothing was removed).
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

// restoreOutcome is what happened to ONE quarantined entry.
//
// It is an enum rather than a bool BECAUSE a bool is what collapsed the
// reporting once already: restoreOneQuarantined used to return
// (restored bool, error), the caller dispatched `if restored {...} else {...}`,
// and the third outcome -- "declined to act, a human must decide" -- had nowhere
// to go but the bucket meaning "nothing to do". Three facts do not fit in two
// values, and the type is where that gets enforced rather than remembered.
type restoreOutcome int

const (
	// restoreOutcomeUnset is the ZERO VALUE and is deliberately not an
	// outcome. It is what the error returns carry, so a caller that ever
	// forgot to check err would count the entry as NOTHING rather than
	// silently bank a failure as a success -- the switch has no case for it.
	// Making the zero value inert is the same lesson as the enum itself: do
	// not leave a type able to express a wrong answer.
	restoreOutcomeUnset restoreOutcome = iota

	// restoreWrote: the bytes were written back to disk.
	restoreWrote

	// restoreAlreadyPresent: a byte-identical copy was already on disk, so
	// there was nothing to do and the quarantine entry was consumed. Benign
	// and terminal -- no one needs to look.
	restoreAlreadyPresent

	// restoreNeedsReview: a surviving slot RESEMBLES the quarantined picture
	// but is not it, so restoring was declined and the entry was RETAINED.
	// The artwork is NOT on disk, the operation will never empty on its own,
	// and only a human can settle it. Terminal for this run, unresolved for
	// the operator.
	restoreNeedsReview
)

// PHashRestoreResult summarizes a restore run.
type PHashRestoreResult struct {
	OpID string `json:"op_id"`

	// The three outcomes are counted separately because they are three
	// different facts and any two of them collapsed together produce a lie:
	//
	//   Restored       -- bytes written back. The artwork is on disk.
	//   AlreadyPresent -- a byte-identical copy was already there, so nothing
	//                     was needed and the entry was consumed. Benign.
	//   NeedsReview    -- restoring was DECLINED because a survivor merely
	//                     resembles the quarantined picture. The artwork is
	//                     NOT on disk, the entry is still quarantined, and a
	//                     human must decide. THIS IS NOT SUCCESS.
	//
	// NeedsReview exists because collapsing it into AlreadyPresent reported
	// "we deliberately did nothing and you must intervene" as "nothing to do"
	// -- an operator reading Restored=0 AlreadyPresent=1 Failures=[] would see
	// unambiguous success while their artwork sat undelivered in quarantine.
	// That is this repo's dominant bug class (reports success while doing
	// nothing) on the recovery path, and a slog.Warn is not a substitute: the
	// returned result is what the API surfaces and what a caller branches on.
	//
	// It is deliberately NOT a Failure either. Nothing broke, nothing was lost,
	// and the run should not read as errored; the entry is intact and waiting.
	// "Needs a decision" is its own state.
	Restored       int `json:"restored"`
	AlreadyPresent int `json:"already_present"`
	NeedsReview    int `json:"needs_review"`

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
		// Serialize this artist's whole back-out against a concurrent restore
		// (or another back-out) of the same artist: the two rewrite the same
		// on-disk slots and manifest, a lost update -race cannot see. See
		// Pipeline.phashArtistMu. A dry run mutates nothing, but it still reads
		// the fanart directory the restore renumbers, so it holds the lock too
		// rather than reason about which reads are safe to leave unguarded.
		err = func() error {
			mu := p.phashArtistMutex(am.ArtistID)
			mu.Lock()
			defer mu.Unlock()
			return p.remediateArtistPHash(ctx, a, am, result.OpID, primaryName, kodiNumbering, report.Tolerance, opts, &result)
		}()
		if err != nil {
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
	tolerance float64,
	opts PHashRemediateOpts,
	result *PHashRemediateResult,
) error {
	paths, err := img.DiscoverFanart(a.Path, primaryName)
	if err != nil {
		return fmt.Errorf("discovering fanart for %s: %w", a.Name, err)
	}

	confirmed := make(map[int]bool)
	// Outcomes for slots quarantined and staged for removal are held here,
	// NOT stamped "removed" or appended to result.Outcomes, until the renumber
	// commit confirms. Recording "removed" before the commit would leave a
	// lying audit trail on the failure path, where the staged tombs are rolled
	// back to their original paths and nothing is actually removed.
	var pending []PHashSlotOutcome
	// quarantined tracks the entries staged for removal so the platform-side
	// delete (below, after the on-disk commit confirms) can re-resolve each by
	// its content hash and record where it was deleted from. Kept in lockstep
	// with pending: both are appended only for a slot that quarantined and
	// staged successfully.
	var quarantined []img.RepairEntry
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

		// Re-verify the MATCHED COUNTERPART against its own bytes on disk too,
		// not just the suspect. The suspect re-verification above binds the
		// deletion to what is about to be deleted, but the collision that
		// justifies deleting it rests on the OTHER side of the pair -- and that
		// side's hash came from the scan's cache (artist_images.phash), never
		// re-read here. If the counterpart changed or was removed on disk after
		// the scan but before this commit, the collision no longer holds, yet
		// the stale cache would still authorize a destructive removal. Re-read
		// the counterpart, re-hash it, and require the perceptual match to still
		// hold at the removal's own tolerance. A stale or absent counterpart is
		// strictly a SKIP: an ambiguous, no-longer-reproducible signal must not
		// authorize an exact delete.
		if ok, reason := p.reverifyMatchedCounterpart(ctx, primaryName, s.PHash, s.MatchedArtistID, s.MatchedSlotIndex, tolerance); !ok {
			outcome.Action, outcome.Reason = "skipped", reason
			p.logger.Warn("phash back-out skipping slot whose matched counterpart no longer confirms the collision",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.Int("slot_index", s.SlotIndex),
				slog.String("file", outcome.FileName),
				slog.String("matched_artist_id", s.MatchedArtistID),
				slog.Int("matched_slot_index", s.MatchedSlotIndex),
				slog.String("reason", reason))
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

		// Deliberately NOT stamped "removed" or appended yet: the removal is
		// not confirmed until stageAndCommitPHashRemoval succeeds below.
		pending = append(pending, outcome)
		quarantined = append(quarantined, entry)
		confirmed[s.SlotIndex] = true
	}

	if opts.DryRun || len(confirmed) == 0 {
		return nil
	}

	removed, err := p.stageAndCommitPHashRemoval(ctx, a, primaryName, kodiNumbering, paths, confirmed, opID)
	if err != nil {
		// The commit failed: the staged tombs were rolled back to their
		// original paths, so nothing was removed. Record the pending slots as
		// "failed" rather than "removed" so the audit trail matches the disk,
		// then propagate the error (the caller bumps result.Failures).
		for i := range pending {
			pending[i].Action = "failed"
			pending[i].Reason = "renumber/commit failed; staged tombs were rolled back and nothing was removed"
			result.Outcomes = append(result.Outcomes, pending[i])
		}
		return err
	}
	// Confirmed: stamp the pending outcomes "removed" and bump the counter.
	result.SlotsRemoved += len(removed)
	for i := range pending {
		pending[i].Action = "removed"
		result.Outcomes = append(result.Outcomes, pending[i])
	}

	// Now that the removal is COMMITTED on disk, back the same picture out of
	// the connected platforms. Deliberately AFTER the authoritative on-disk
	// removal: the platform match is perceptual (fuzzy) and must never authorize
	// a delete on its own -- the local, byte-exact removal is what licenses
	// removing the mirrored copy.
	p.deleteRemovedSlotsOnPlatforms(ctx, a, opID, tolerance, quarantined)

	// Resolve across every configured convention, not the single primary this
	// operation renumbered under: the resync feeds a count the operator sees,
	// and a profile/library convention mismatch would resync it to zero against
	// a directory that still holds artwork.
	if names, namesErr := resolveFanartNames(ctx, p.engine.platformService); namesErr != nil {
		p.logger.Warn("resolving fanart naming convention after phash back-out; artist fields left as-is",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("error", namesErr.Error()))
	} else {
		resyncFanartFields(a, names)
	}
	if err := p.artistService.Update(ctx, a); err != nil {
		// The destructive on-disk work is already committed and IRREVERSIBLE
		// at this point. A metadata sync miss is a cache problem the next scan
		// re-derives; reporting it as the operation failing would invite an
		// operator to re-run a destructive path that already succeeded. Warn
		// and preserve the successful result instead of returning the error.
		p.logger.Warn("phash back-out removed slots on disk but the artist metadata sync failed; "+
			"the removal is committed and must not be re-run",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("error", err.Error()))
		return nil
	}
	// The back-out REMOVED files, so the persist must retire their rows.
	// Update is declarative and deletes nothing (#2635).
	p.reconcileAfterFix(ctx, a, true)
	return nil
}

// deleteRemovedSlotsOnPlatforms removes each just-removed slot's picture from
// the connected platforms and records, on the quarantine entry, the items it
// was deleted from so a later restore can re-upload the bytes to the same
// items.
//
// It runs only after the on-disk removal has committed (see the call site) and
// is entirely best-effort: a nil publisher (many test pipelines) is a clean
// no-op, and a per-connection failure is logged, not propagated -- the on-disk
// quarantine still holds the bytes, so a stranded platform copy is recoverable,
// never lost data. Each slot is re-resolved on the platform by its content hash
// (the recorded ordinal is stale by construction); DeletePollutedBackdropOnPlatforms
// owns that resolution.
func (p *Pipeline) deleteRemovedSlotsOnPlatforms(ctx context.Context, a *artist.Artist, opID string, tolerance float64, quarantined []img.RepairEntry) {
	if p.publisher == nil {
		return
	}
	for i := range quarantined {
		e := &quarantined[i]
		delRes, delErr := p.publisher.DeletePollutedBackdropOnPlatforms(ctx, a.ID, e.PHash, tolerance)
		if delErr != nil {
			p.logger.Error("phash back-out: platform delete failed after local removal committed",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.String("file", e.FileName),
				slog.String("error", delErr.Error()))
			continue
		}
		for _, f := range delRes.Failures {
			p.logger.Warn("phash back-out: platform delete reported a per-connection failure",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("connection_id", f.ConnectionID), slog.String("error", f.Err))
		}
		if len(delRes.Targets) > 0 {
			if err := img.SetRepairEntryPlatformTargets(a.Path, opID, *e, delRes.Targets); err != nil {
				// The picture is already off the platform, but we could not
				// record where from. A restore will still put the bytes back on
				// disk; it just will not re-upload to the platform. Non-fatal,
				// and the safe direction to miss in -- warn.
				p.logger.Warn("phash back-out: recording platform delete targets on the quarantine entry failed",
					slog.String("op_id", opID), slog.String("artist_id", a.ID),
					slog.String("file", e.FileName), slog.String("error", err.Error()))
			}
		}
	}
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

// reverifyMatchedCounterpart re-reads the matched artist's fanart slot from
// disk, re-hashes it, and reports whether the perceptual collision that
// justifies removing the suspect still holds at the removal's tolerance.
//
// This closes the other half of the re-verification gap. reverifySlotPHash
// binds the decision to the suspect's OWN bytes, but a removal is authorized by
// a PAIR: the suspect looks like some other artist's fanart. That other side's
// hash was read from the scan's cache and is never otherwise re-read, so a
// counterpart that changed or vanished on disk between the scan and this commit
// would leave the cache pointing at a picture that is no longer there while
// still authorizing a delete. Re-hashing the counterpart from disk is the only
// check that binds the removal to a collision that is still real.
//
// suspectPHash is the suspect's flagged hash, which reverifySlotPHash has
// already confirmed matches the suspect's current on-disk bytes -- so it is the
// live suspect hash, not a stale one. Every failure to re-confirm the match is
// a refusal: a stale or absent counterpart must NOT authorize a delete.
func (p *Pipeline) reverifyMatchedCounterpart(ctx context.Context, primaryName, suspectPHash, matchedArtistID string, matchedSlotIndex int, tolerance float64) (bool, string) {
	if matchedArtistID == "" {
		return false, "collision has no matched counterpart to re-verify; refusing to remove"
	}
	want, err := img.ParseHashHex(suspectPHash)
	if err != nil {
		return false, fmt.Sprintf("parsing flagged suspect hash: %v", err)
	}
	ma, err := p.artistService.GetByID(ctx, matchedArtistID)
	if err != nil {
		return false, fmt.Sprintf("loading matched artist %s to re-verify the counterpart: %v", matchedArtistID, err)
	}
	if ma.Path == "" {
		return false, "matched artist has no path; cannot re-verify the counterpart on disk"
	}
	paths, err := img.DiscoverFanart(ma.Path, primaryName)
	if err != nil {
		return false, fmt.Sprintf("discovering matched artist fanart: %v", err)
	}
	if matchedSlotIndex < 0 || matchedSlotIndex >= len(paths) {
		return false, "matched counterpart slot no longer exists on disk; the collision cannot be re-confirmed"
	}
	f, err := os.Open(paths[matchedSlotIndex])
	if err != nil {
		return false, fmt.Sprintf("re-reading matched counterpart: %v", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close after read

	got, err := img.PerceptualHash(f)
	if err != nil {
		return false, fmt.Sprintf("re-hashing matched counterpart: %v", err)
	}
	if img.Similarity(want, got) < tolerance {
		return false, "matched counterpart on disk no longer resembles the suspect; the collision that justified removal is gone"
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

	// Serialize this artist's whole restore against a concurrent back-out (or
	// another restore) of the same artist. The restore appends fanart slots and
	// consumes manifest entries while a back-out renumbers those same slots and
	// rewrites the same manifest -- a file-level lost update -race cannot see.
	// See Pipeline.phashArtistMu.
	//
	// The lock is taken BEFORE the manifest read below, not after, and held
	// across the occupancy checks, the on-disk writes, the entry consumption,
	// and the metadata resync. Reading the manifest outside the lock was a
	// TOCTOU: a concurrent remediation could rewrite an entry's PlatformTargets
	// (see deleteRemovedSlotsOnPlatforms -> SetRepairEntryPlatformTargets)
	// between this snapshot and the restore that acts on it, so the restore
	// would re-upload to a stale set of platform items. Snapshotting the
	// manifest under the lock makes the read and the entry consumption one
	// atomic critical section. Only this one mutex is held (image.repairOpMutex,
	// which ReadRepairManifest/RepairEntryBytes/ConsumeRepairEntry take, is
	// always acquired strictly inside it -- see phashArtistMutex), so there is
	// no lock-ordering hazard or double-lock.
	mu := p.phashArtistMutex(artistID)
	mu.Lock()
	defer mu.Unlock()

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
		outcome, err := p.restoreOneQuarantined(ctx, a, entry, opID, primaryName, kodiNumbering)
		if err != nil {
			p.logger.Error("restoring quarantined backdrop",
				slog.String("op_id", opID), slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.String("file", entry.FileName),
				slog.String("error", err.Error()))
			result.Failures = append(result.Failures, fmt.Sprintf("%s: %v", entry.FileName, err))
			continue
		}
		switch outcome {
		case restoreWrote:
			result.Restored++
		case restoreAlreadyPresent:
			result.AlreadyPresent++
		case restoreNeedsReview:
			result.NeedsReview++
		case restoreOutcomeUnset:
			// Unreachable today: every path returning it also returns a
			// non-nil err, which the check above already handled. Named
			// explicitly anyway so this switch stays exhaustive and the
			// zero value keeps counting as NOTHING -- if a future edit
			// ever lets it through, the entry goes uncounted rather than
			// silently banked as a success.
		}
	}

	if names, namesErr := resolveFanartNames(ctx, p.engine.platformService); namesErr != nil {
		p.logger.Warn("resolving fanart naming convention after phash restore; artist fields left as-is",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("error", namesErr.Error()))
	} else {
		resyncFanartFields(a, names)
	}
	if err := p.artistService.Update(ctx, a); err != nil {
		// The bytes are already back on disk at this point -- the restore
		// succeeded. A metadata sync miss is a cache problem the next scan
		// re-derives; returning it as an error would report a successful
		// recovery as a failure, and an operator re-running "the failed
		// restore" would find the artwork already present and be told nothing
		// happened. Warn and preserve the successful result.
		p.logger.Warn("phash quarantine restore wrote the artwork back but the artist metadata sync failed; "+
			"the restore is complete and must not be re-run as a failure",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("error", err.Error()))
	}
	p.logger.Info("phash quarantine restore finished",
		slog.String("op_id", opID), slog.String("artist_id", a.ID),
		slog.String("artist", a.Name), slog.Int("restored", result.Restored),
		slog.Int("already_present", result.AlreadyPresent),
		slog.Int("needs_review", result.NeedsReview),
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
) (restoreOutcome, error) {
	data, err := img.RepairEntryBytes(a.Path, opID, *entry)
	if err != nil {
		return restoreOutcomeUnset, err
	}

	exact, similar, err := p.quarantinedImagePresence(a.Path, primaryName, data)
	if err != nil {
		return restoreOutcomeUnset, err
	}
	switch {
	case exact:
		// A byte-identical copy is on disk, so the quarantine's copy is
		// redundant and consuming it destroys nothing: the exact bytes
		// remain recoverable from the artist directory itself. This is
		// the ONLY evidence that justifies unlinking them.
		p.logger.Info("quarantined backdrop already present byte-for-byte; consuming the entry",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("file", entry.FileName))
		// Re-upload to the platforms it was deleted from even though the local
		// copy is already present: the platform delete removed the mirrored
		// copy during remediation, so the artist may be missing it there while
		// the local disk still has it. Content-addressed and idempotent -- a
		// copy already present on the platform is a no-op.
		p.restorePHashToPlatforms(ctx, a, entry, data, opID)
		return restoreAlreadyPresent, img.ConsumeRepairEntry(a.Path, opID, *entry)

	case similar:
		// A surviving slot RESEMBLES this picture but is not it. Do
		// neither thing: appending would hand the operator a
		// near-duplicate, and consuming would unlink the last exact copy
		// of artwork whose original this tool already deleted.
		//
		// The entry is RETAINED deliberately. A perceptual match is
		// evidence about similarity, never about recoverability, and this
		// is the path whose entire job is holding the last copy -- so the
		// two decisions are decoupled: a resemblance may suppress an
		// append, only byte equality may authorize a delete. The
		// operation stays un-empty until a human looks at it, which is
		// the correct end state for an ambiguous signal.
		p.logger.Warn("a surviving backdrop resembles the quarantined one; not restoring, and KEEPING the quarantined copy",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("file", entry.FileName),
			slog.String("action", "entry retained for manual review"))
		return restoreNeedsReview, nil
	}

	// Append at the next free ordinal. Re-discovered per entry rather than
	// counted once outside the loop: each restore adds a slot, so a cached
	// length would aim the second entry at the ordinal the first just took
	// and clobber it.
	paths, err := img.DiscoverFanart(a.Path, primaryName)
	if err != nil {
		return restoreOutcomeUnset, fmt.Errorf("discovering fanart: %w", err)
	}
	target := filepath.Join(a.Path, img.FanartFilename(primaryName, len(paths), kodiNumbering))
	if _, statErr := os.Lstat(target); statErr == nil {
		// Refuse rather than clobber. Discovery only counts recognized
		// artwork names, so a stray file can occupy the computed target
		// without being a slot; overwriting it here would destroy a file
		// this feature never took.
		return restoreOutcomeUnset, fmt.Errorf("refusing to restore onto occupied path %s", filepath.Base(target))
	} else if !os.IsNotExist(statErr) {
		return restoreOutcomeUnset, fmt.Errorf("checking restore target %s: %w", filepath.Base(target), statErr)
	}

	if err := img.WriteFanartBytes(target, data); err != nil {
		return restoreOutcomeUnset, err
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
	// Put the picture back on the platforms it was deleted from during
	// remediation. Runs after the on-disk restore so the local recovery -- the
	// thing that must not be lost -- is committed first.
	p.restorePHashToPlatforms(ctx, a, entry, data, opID)
	return restoreWrote, img.ConsumeRepairEntry(a.Path, opID, *entry)
}

// restorePHashToPlatforms re-uploads a restored backdrop's bytes to each
// platform item the remediation deleted it from (entry.PlatformTargets),
// after the on-disk restore has committed.
//
// It is deliberately best-effort and NON-FATAL: the local restore is the
// authoritative recovery and has already succeeded by the time this runs, so a
// platform that is unreachable, disabled, or slow must not turn a successful
// on-disk restore into a failure the operator is told to re-run. Per-connection
// failures are logged and surfaced in the platform result, not propagated.
//
// It uses defaultPHashMismatchTolerance -- the same cutoff the on-disk
// already-present check uses -- because the manifest records no cutoff and the
// only thing this tolerance gates on the restore side is whether an upload is
// suppressed as already-present (the conservative, non-destructive direction).
// RestoreBackdropToPlatforms only ever APPENDS or no-ops; it deletes nothing, so
// a looser tolerance here can at worst suppress a redundant upload, never harm.
//
// A nil publisher (many test pipelines) or an entry with no recorded targets (a
// pre-platform-wiring manifest, or a removal where nothing matched on any
// platform) is a clean no-op.
func (p *Pipeline) restorePHashToPlatforms(ctx context.Context, a *artist.Artist, entry *img.RepairEntry, data []byte, opID string) {
	if p.publisher == nil || len(entry.PlatformTargets) == 0 {
		return
	}
	res, err := p.publisher.RestoreBackdropToPlatforms(ctx, entry.PlatformTargets, data, defaultPHashMismatchTolerance)
	if err != nil {
		p.logger.Error("phash restore: platform restore failed after local restore committed",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("file", entry.FileName),
			slog.String("error", err.Error()))
		return
	}
	for _, f := range res.Failures {
		p.logger.Warn("phash restore: platform restore reported a per-connection failure",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("connection_id", f.ConnectionID), slog.String("error", f.Err))
	}
	if res.Appended > 0 || res.AlreadyPresent > 0 {
		p.logger.Info("phash restore: platform restore finished",
			slog.String("op_id", opID), slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("file", entry.FileName),
			slog.Int("appended", res.Appended), slog.Int("already_present", res.AlreadyPresent))
	}
}

// quarantinedImagePresence reports whether the artist already holds the
// quarantined picture.
//
// It reports two DIFFERENT facts and the caller must not conflate them:
//
//   - exact -- some slot is byte-identical. The quarantined bytes are provably
//     recoverable from the artist directory, so the quarantine copy may be
//     consumed.
//   - similar -- some slot is within the perceptual tolerance but is NOT those
//     bytes. Evidence about resemblance only. It may suppress an append; it may
//     NEVER authorize unlinking the quarantined copy.
//
// Collapsing them is what made a resemblance destroy artwork: consuming on a
// perceptual match unlinked the last exact copy of a picture whose original this
// tool had already removed, and reported success. A perceptual hash says two
// pictures LOOK alike at 64 bits of resolution; it never says one IS the other,
// and only the latter makes a deletion safe.
//
// exact wins over similar, and the scan does not stop at the first resemblance:
// a later slot may still be the byte-identical copy that makes consuming safe.
//
// Both comparisons are content-addressed; neither consults a slot index.
//
// TOLERANCE: this uses defaultPHashMismatchTolerance, NOT the operator's
// PHashMismatchScope.Tolerance, and it cannot do otherwise --
// RestorePHashQuarantine takes no tolerance and the manifest records none
// (image.RepairEntry carries the per-suspect Similarity, not the cutoff it was
// judged against). That mismatch is now harmless BECAUSE of the split above: the
// only thing this tolerance still gates is whether an append is suppressed, and
// a looser value there is the conservative direction -- it withholds a
// near-duplicate and keeps the quarantined bytes, rather than authorizing
// anything destructive. It must not be reconnected to a delete without also
// plumbing the operator's real tolerance through the manifest.
func (p *Pipeline) quarantinedImagePresence(dir, primaryName string, data []byte) (exact, similar bool, err error) {
	paths, err := img.DiscoverFanart(dir, primaryName)
	if err != nil {
		return false, false, fmt.Errorf("discovering fanart: %w", err)
	}
	if len(paths) == 0 {
		return false, false, nil
	}

	want, wantErr := img.PerceptualHash(bytes.NewReader(data))
	for _, path := range paths {
		onDisk, readErr := os.ReadFile(path) //nolint:gosec // path is a DiscoverFanart result under the artist dir
		if readErr != nil {
			continue
		}
		if bytes.Equal(onDisk, data) {
			return true, true, nil
		}
		if wantErr != nil {
			continue
		}
		got, hashErr := img.PerceptualHash(bytes.NewReader(onDisk))
		if hashErr != nil {
			continue
		}
		if img.Similarity(want, got) >= defaultPHashMismatchTolerance {
			similar = true
		}
	}
	return false, similar, nil
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
