package rule

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// A FIXER MUST NOT CLAIM CREDIT FOR A WRITE THE LOCK GUARD REVERTED (#3037).
//
// artist.Service's persist chokepoint RESTORES-AND-CONTINUES: a locked field's
// incoming value is put back to the stored one and the write proceeds, so
// Update returns nil even though the caller's change did not land. #3060 closed
// that for the single-field provider-ID verb, which can REFUSE. The whole-row
// verb cannot -- refusing would discard the legitimate unlocked changes riding
// in the same struct -- so it reports instead, via
// Service.UpdateReportingLocks. This file is the consumer.
//
// The report answers "what did the guard revert". A fixer's honesty question is
// narrower: "was EVERY change I made reverted". Answering it needs the fixer's
// own intended set, which no fixer declares, so the pipeline recovers it by
// snapshotting artist.GuardedFieldSnapshot before the fixer runs and diffing
// after. See artist.ChangedGuardedFields for the ways that diff deliberately
// UNDER-reports.

// lockRevertedFixResult reports the FixResult a fully-reverted fix should carry,
// or nil when the fix is not fully reverted and the caller should proceed with
// its normal success path.
//
// FIVE CONDITIONS, ALL REQUIRED, and the extra ones past the obvious two are
// what keep this from over-firing:
//
//   - fr is a Fixed result. A fixer that already reported failure or a terminal
//     dismiss has its own answer and this must not overwrite it.
//   - intended is NON-EMPTY. An empty set means the fixer changed no guarded
//     field at all (an image fixer, the NFO fixer, the directory-rename fixer),
//     so nothing it did can have been reverted here.
//   - every intended field appears in restored. One surviving change is a real
//     repair; #3060 made the same call for a partial provider-ID backfill.
//   - the fix left NOTHING ON DISK: no saved image, no removed files, no
//     removed slots. Those effects are real and permanent whatever the artists
//     row ended up holding, so a result carrying one is not "fully reverted"
//     even if its guarded-field changes were. No shipped fixer both writes a
//     guarded field and touches disk, so this arm is a guard against the next
//     one rather than a live case.
//
// A NIL RETURN IS THE RECOVERABLE DIRECTION and every ambiguity above resolves
// to it. The alternative outcome leaves the violation OPEN, which re-raises
// harmlessly; wrongly reporting a repair CLOSES a finding that was never made,
// and the operator has no signal that anything went wrong.
func lockRevertedFixResult(fr *FixResult, intended, restored []string) *FixResult {
	if fr == nil || !fr.Fixed || fr.Dismissed {
		return nil
	}
	if len(intended) == 0 || len(restored) == 0 {
		return nil
	}
	if fr.SavedPath != "" || fr.RemovedFiles || fr.SlotsRemoved > 0 {
		return nil
	}
	for _, f := range intended {
		if !slices.Contains(restored, f) {
			return nil
		}
	}
	// NOT-FIXED AND NOT DISMISSED: the violation stays OPEN. This is the one
	// place this unit deliberately DIVERGES from #3060, which dismisses when a
	// lock refuses every field of provider_id_missing, and the divergence is
	// argued rather than accidental.
	//
	// Dismissed is for a TERMINAL outcome, and a lock is not terminal: it is
	// operator-revocable state. Unlock the field and the very same fixer works
	// on the next pass. A dismissed row has NO un-dismiss route -- UpsertViolation's
	// ON CONFLICT pins 'dismissed' (#1107), ReopenViolation is a positive
	// allow-list on 'resolved' -- so dismissing here means that after the
	// operator unlocks, the finding never comes back and the field is never
	// repaired. That failure is silent and permanent.
	//
	// Leaving it OPEN costs a Fix button that does nothing until the operator
	// unlocks, plus an ERROR log line per pass naming the artist and field --
	// which is the signal an operator needs to notice they have a rule fighting
	// a lock. That is the recoverable direction.
	//
	// (#3060's dismiss is not being second-guessed here: it ships, its gate is
	// far stricter, and converging the two is its own unit. This function is
	// about the whole-row path, which is the one that covers every other fixer.)
	//
	// Fixed is cleared. Callers gate the resolve, the provenance record and the
	// platform publish on Fixed, and every one of those would be describing a
	// value the database does not hold.
	return &FixResult{
		RuleID: fr.RuleID,
		Fixed:  false,
		Message: fmt.Sprintf("fix reverted: %s locked by the operator",
			strings.Join(intended, ", ")),
	}
}

// noteIntendedGuardedFields records, per rule, the guarded fields that rule's
// fixer changed on this artist during this pass. The auto-fix path persists
// every violation's changes in ONE whole-row write at the end of the pass, so
// the per-rule intent has to survive until then to be intersected with that
// write's restored-field report.
func (acc *runForArtistAccum) noteIntendedGuardedFields(ruleID string, fields []string) {
	if ruleID == "" || len(fields) == 0 {
		return
	}
	if acc.intendedGuarded == nil {
		acc.intendedGuarded = make(map[string][]string, 1)
	}
	// Union rather than overwrite: two fixers for the same rule id in one pass
	// would otherwise leave only the last one's intent, and a field dropped
	// from the intended set makes the "every change reverted" test EASIER to
	// pass, which is the direction that wrongly dismisses.
	for _, f := range fields {
		if !slices.Contains(acc.intendedGuarded[ruleID], f) {
			acc.intendedGuarded[ruleID] = append(acc.intendedGuarded[ruleID], f)
		}
	}
}

// splitLockRevertedRows partitions the pass's deferred resolved rows into the
// ones that may still be resolved and the ones whose every guarded change the
// lock guard reverted. It is a pure function of the accumulated intent and the
// write's restored-field report; the caller does the writing.
//
// An empty restored list short-circuits to "resolve everything", which is the
// overwhelmingly common case and costs nothing.
func (acc *runForArtistAccum) splitLockRevertedRows(restored []string) (keep, reverted []*RuleViolation) {
	if len(restored) == 0 || len(acc.intendedGuarded) == 0 {
		return acc.resolvedRows, nil
	}
	for _, rv := range acc.resolvedRows {
		intended := acc.intendedGuarded[rv.RuleID]
		if lockRevertedFixResult(&FixResult{RuleID: rv.RuleID, Fixed: true}, intended, restored) != nil {
			reverted = append(reverted, rv)
			continue
		}
		keep = append(keep, rv)
	}
	return keep, reverted
}

// resolveOrDismissRows is the run paths' replacement for a bare
// finalizeResolvedRows call. It splits the pass's deferred rows on the write's
// restored-field report, resolves the ones whose fix survived, and re-opens the
// ones whose every guarded change the lock guard reverted (#3037).
//
// Callers gate this on the artist row having persisted, exactly as they gated
// finalizeResolvedRows: a failed write leaves the mutation in memory, and
// neither verdict below would describe the stored row.
func (p *Pipeline) resolveOrDismissRows(ctx context.Context, a *artist.Artist, acc *runForArtistAccum, restored []string, startedAt time.Time) bool {
	keep, reverted := acc.splitLockRevertedRows(restored)
	ok := p.finalizeResolvedRows(ctx, a, keep, startedAt)
	if !p.reopenLockRevertedRows(ctx, a, reverted, startedAt) {
		ok = false
	}
	return ok
}

// reopenLockRevertedRows persists each fully-reverted row as OPEN, and returns
// true when every write succeeded.
//
// ONE UPSERT, AND IT MUST BE AN OPEN ONE. The row is written rather than merely
// left alone because on a FIRST pass no row exists yet: processAutoFixViolation
// returns early on a Fixed result and defers the write, so dropping the row
// here without replacing it would leave the artist with no violation row and no
// rule_results row at all for a rule that genuinely failed.
//
// Writing it OPEN also sidesteps, rather than manages, the trap that a
// dismissing version would have to handle: UpsertViolation emits the paired
// rule_results FAIL row only for an open or pending row (#1107), and later
// passes preserve 'dismissed', so a straight-to-dismissed write would leave the
// artist permanently unscorable by offlineHealthScore. An open row gets its
// FAIL row on the first write.
//
// Candidates are cleared: a fix the lock reverted offers the operator no
// choice, and a stored list would be a decision that never gets presented.
func (p *Pipeline) reopenLockRevertedRows(ctx context.Context, a *artist.Artist, rows []*RuleViolation, startedAt time.Time) bool {
	ok := true
	for _, rv := range rows {
		p.logger.Error("a rule fix was fully reverted by a field lock; the violation stays OPEN rather than being resolved",
			"artist_id", a.ID, "rule_id", rv.RuleID)
		rv.Candidates = nil
		rv.EvaluatedAt = startedAt
		rv.Status = ViolationStatusOpen
		rv.ResolvedAt = nil
		rv.DismissedAt = nil
		if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
			p.logger.Warn("persisting lock-reverted violation as open",
				"rule_id", rv.RuleID, "artist", a.Name, "error", err)
			ok = false
		}
	}
	return ok
}

// reportIfLockReverted returns the replacement FixResult when every guarded
// change this fix made was restored by the lock guard, or nil when the caller
// should proceed with its normal success path.
//
// IT WRITES NOTHING. The row this path loaded is already open, and the caller
// simply declines to resolve it, so there is no status to change -- which is
// why it needs neither DismissViolation nor an upsert. The auto path DOES write
// (see reopenLockRevertedRows) only because on a first pass its row does not
// exist yet.
func (p *Pipeline) reportIfLockReverted(a *artist.Artist, rv *RuleViolation, fr *FixResult, intended, restored []string) *FixResult {
	out := lockRevertedFixResult(fr, intended, restored)
	if out == nil {
		return nil
	}
	p.logger.Error("a rule fix was fully reverted by a field lock; the violation stays OPEN rather than being resolved",
		"artist_id", a.ID, "rule_id", rv.RuleID, "fields", strings.Join(restored, ","))
	return out
}
