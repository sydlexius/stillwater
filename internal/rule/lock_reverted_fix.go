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
// The report answers "what did the guard revert". The fixer's question is
// narrower -- "was EVERY change I made reverted" -- and needs the fixer's own
// intended set, which no fixer declares. The pipeline recovers it by
// snapshotting artist.GuardedFieldSnapshot before the fixer runs and diffing
// after; see artist.ChangedGuardedFields for how that diff UNDER-reports.
//
// POST-HOC RATHER THAN A PRE-CHECK, and deliberately. The alternative is a
// rule -> lockable-fields map consulted before dispatch, which reads better but
// DRIFTS: a fixer added tomorrow that writes a lockable field and is not added
// to the map silently regains the bug, which is the failure mode #2748/#2754
// already produced twice. Reading what the guard ACTUALLY restored cannot
// drift. The cost is that the fixer still runs and its provider calls still go
// out; closing that needs the map, and it is a separate decision.
//
// "CREDIT" IS FOUR SURFACES, NOT ONE, and a correction that reaches some of them
// is worse than none -- it makes them contradict each other. All four are
// handled here: the violation ROW (resolveOrReopenRows), the run's
// fixes-succeeded COUNT and the Recent Activity ENTRY (grantFixCredits), and the
// FixResult the caller reads (lockRevertedFixResult on the click path,
// grantFixCredits rewriting the run's results entry on the auto path).

// lockRevertedFixResult reports the FixResult a fully-reverted fix should carry,
// or nil when the fix is not fully reverted and the caller should proceed with
// its normal success path.
//
// FOUR CONDITIONS, ALL REQUIRED, and the ones past the obvious two are what
// keep this from over-firing:
//
//   - fr is a Fixed result. A fixer that already reported failure or a terminal
//     dismiss has its own answer and this must not overwrite it.
//   - intended is NON-EMPTY: an empty set means the fixer changed no guarded
//     field at all (image, NFO, directory-rename fixers), so nothing it did can
//     have been reverted here.
//   - EVERY intended field appears in restored. ALL, not ANY: a rule whose
//     fixer writes two fields with one locked still did useful work on the
//     other, and treating that as a failed fix turns a per-FIELD lock into a
//     per-RULE one. #3060 made the same call for a partial provider-ID
//     backfill.
//   - the fix left NOTHING ON DISK (no saved image, removed file, or removed
//     slot). Those effects are real whatever the artists row ended up holding.
//     No shipped fixer both writes a guarded field and touches disk, so this
//     arm guards the next one rather than a live case.
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
	// NOT-FIXED AND NOT DISMISSED: the violation stays OPEN. Dismissed is for a
	// TERMINAL outcome and a lock is not terminal -- it is operator-revocable
	// state, and unlocking makes the very same fixer work next pass. A dismissed
	// row has NO un-dismiss route (UpsertViolation's ON CONFLICT pins
	// 'dismissed' per #1107; ReopenViolation is a positive allow-list on
	// 'resolved'), so dismissing here means the finding never returns after the
	// operator unlocks and the field is never repaired -- silently, permanently.
	//
	// Leaving it OPEN costs a Fix button that does nothing until the operator
	// unlocks, plus one ERROR line per pass naming the artist and field, which
	// is exactly the signal that a rule is fighting a lock.
	//
	// This DIVERGES from #3060, which dismisses when a lock refuses every field
	// of provider_id_missing. That is not being second-guessed: its gate is far
	// stricter and converging the two is its own unit.
	//
	// Fixed is cleared -- callers gate the resolve, the provenance record and
	// the platform publish on it, and each would describe a value the database
	// does not hold.
	out := revertedFixReport(fr.RuleID, intended)
	return &out
}

// revertedFixReport is the one place the reverted-fix verdict is spelled, so the
// click path (lockRevertedFixResult) and the auto path (grantFixCredits) cannot
// drift into telling the operator two different stories about the same event.
func revertedFixReport(ruleID string, intended []string) FixResult {
	return FixResult{
		RuleID: ruleID,
		Fixed:  false,
		Message: fmt.Sprintf("fix reverted: %s locked by the operator",
			strings.Join(intended, ", ")),
	}
}

// noteIntendedGuardedFields records, per rule, the guarded fields that rule's
// fixer changed on this artist. The auto path persists every violation's
// changes in ONE whole-row write at the end, so per-rule intent has to survive
// until then to be intersected with that write's restored-field report.
func (acc *runForArtistAccum) noteIntendedGuardedFields(ruleID string, fields []string) {
	if ruleID == "" || len(fields) == 0 {
		return
	}
	if acc.intendedGuarded == nil {
		acc.intendedGuarded = make(map[string][]string, 1)
	}
	// Union rather than overwrite: two fixers for one rule id would otherwise
	// leave only the last one's intent, and a dropped field makes the
	// "every change reverted" test EASIER to pass -- the wrong direction.
	for _, f := range fields {
		if !slices.Contains(acc.intendedGuarded[ruleID], f) {
			acc.intendedGuarded[ruleID] = append(acc.intendedGuarded[ruleID], f)
		}
	}
}

// splitLockRevertedRows partitions the pass's deferred resolved rows into the
// ones that may still be resolved and the ones whose every guarded change the
// lock guard reverted. It is a pure function of the accumulated state (intent
// and the pass's pending credits) and the write's restored-field report; the
// caller does the writing.
//
// An empty restored list short-circuits to "resolve everything", which is the
// overwhelmingly common case and costs nothing.
func (acc *runForArtistAccum) splitLockRevertedRows(restored []string) (keep, reverted []*RuleViolation) {
	if len(restored) == 0 || len(acc.intendedGuarded) == 0 {
		return acc.resolvedRows, nil
	}
	for _, rv := range acc.resolvedRows {
		intended := acc.intendedGuarded[rv.RuleID]
		// THE FIXER'S REAL RESULT, never a synthetic stand-in. A synthesized
		// FixResult{Fixed: true} has no SavedPath, no RemovedFiles and no
		// SlotsRemoved, so the disk-effects arm of lockRevertedFixResult could
		// never fire here while grantFixCredits -- which passes the real one --
		// saw it and granted the credit. The two surfaces would then disagree
		// about the same fix: the count says repaired, the row says still open.
		// That is precisely the split-surface failure this file exists to close.
		if lockRevertedFixResult(acc.fixResultForRule(rv.RuleID), intended, restored) != nil {
			reverted = append(reverted, rv)
			continue
		}
		keep = append(keep, rv)
	}
	return keep, reverted
}

// fixResultForRule returns the FixResult the pass recorded for ruleID, or nil
// when no pending credit carries one.
//
// NIL RATHER THAN A SYNTHETIC STAND-IN. lockRevertedFixResult is a positive
// allow-list -- four conditions, ALL required -- and two of them (Fixed, and the
// disk effects) are properties of the fixer's result. With no result there is
// nothing to test them against, so the row cannot be CLASSIFIED as reverted and
// is left alone; lockRevertedFixResult's own nil guard already spells that.
// Inventing a Fixed result to stand in would be asserting the very thing that
// could not be read.
//
// Unreachable today, and provably so: a resolvedRow is stashed only inside the
// `out.fixed` arm of mergeOutcome / mergeIntoContrib, which is entered only when
// fr.Fixed, and the same arm calls notePendingFixCredit with that same non-nil fr
// and the same rule id. The lookup is by rule id because that is the key both
// sides already agree on (violationOutcome.fixedRuleID, never fr.RuleID, which a
// fixer may leave empty). Two violations of one rule for one artist share the
// first credit's result -- they also share the intended-fields union, so the two
// surfaces still read the same input, which is the property that matters here.
func (acc *runForArtistAccum) fixResultForRule(ruleID string) *FixResult {
	for _, c := range acc.pendingCredits {
		if c.ruleID == ruleID && c.fr != nil {
			return c.fr
		}
	}
	return nil
}

// grantFixCredits awards -- or withholds -- every credit the pass deferred while
// waiting to learn what the end-of-run write actually stored (#3037).
//
// WHY THE CREDIT IS DEFERRED RATHER THAN GRANTED AND REVERSED. Correcting only
// the violation row left the auto path claiming the repair on every OTHER
// surface: the run-complete toast's "N fixed" (handlers_rule.go reads
// RunResult.FixesSucceeded), the metadata_changes audit row, and the
// activity.recent dashboard event. processAutoFixViolation wrote all three the
// moment the fixer returned -- BEFORE the pass's single write, so before anything
// could know the guard would revert it. Reversing them afterwards is not an
// equivalent repair: an SSE event cannot be recalled (the rail row is already on
// the operator's dashboard), and a metadata_changes row written then deleted is
// WORSE audit than one never written, because the deletion leaves no trace that
// it was retracted. Deferring has no such asymmetry; it costs only that a new
// orchestrator must remember to call this.
//
// A REVERTED FIX IS NOT A FAILURE. FixesAttempted counted it and stays counted:
// the run did attempt the write and the guard refused it, which is a state the
// operator should see. Only the SUCCESS credit is withheld, and the fix's entry
// in results is rewritten in place to say so.
//
// results and succeeded are the run-level tallies to correct: RunResult's for
// the single-artist path, artistContribution's for the walker paths. results is
// indexed, never appended to, so the caller's slice header stays valid.
func (p *Pipeline) grantFixCredits(ctx context.Context, a *artist.Artist, acc *runForArtistAccum, restored []string, results []FixResult, succeeded *int) {
	for _, c := range acc.pendingCredits {
		if reverted := lockRevertedFixResult(c.fr, acc.intendedGuarded[c.ruleID], restored); reverted != nil {
			// fields is the RULE-scoped intent, not the artist-wide restored set.
			// The line is keyed by one rule_id and the operator-visible Message
			// below is built from the same intent, so logging the artist-wide set
			// would name locks this fixer never touched in a multi-rule pass.
			p.logger.Error("a rule fix was fully reverted by a field lock; it is not counted as a repair and no history is recorded",
				"artist_id", a.ID, "rule_id", c.ruleID, "fields", strings.Join(acc.intendedGuarded[c.ruleID], ","))
			if c.resultIdx >= 0 && c.resultIdx < len(results) {
				// Keep the fixer's own RuleID: reverted.RuleID is copied from the
				// FixResult, which a fixer may leave empty, and blanking a
				// populated field here would lose information the entry had.
				results[c.resultIdx].Fixed = false
				results[c.resultIdx].Message = revertedFixReport(c.ruleID, acc.intendedGuarded[c.ruleID]).Message
			}
			continue
		}
		*succeeded++
		// Issue #1106: the Recent Activity entry and the metadata_changes audit
		// row. Deferred to here from processAutoFixViolation so a reverted fix
		// never emits either. recordRuleFixHistory warn-logs on failure and never
		// fails the surrounding flow.
		p.recordRuleFixHistory(ctx, a.ID, c.fr)
	}
}

// resolveOrReopenRows is the run paths' replacement for a bare
// finalizeResolvedRows call. It splits the pass's deferred rows on the write's
// restored-field report, resolves the ones whose fix survived, and re-opens the
// ones whose every guarded change the lock guard reverted (#3037).
//
// Callers gate this on the artist row having persisted, exactly as they gated
// finalizeResolvedRows: a failed write leaves the mutation in memory, and
// neither verdict below would describe the stored row.
func (p *Pipeline) resolveOrReopenRows(ctx context.Context, a *artist.Artist, acc *runForArtistAccum, restored []string, startedAt time.Time) bool {
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
// ONE UPSERT, AND IT MUST BE AN OPEN ONE. The row is written rather than left
// alone because on a FIRST pass none exists: processAutoFixViolation returns
// early on a Fixed result and defers the write, so dropping the row without
// replacing it leaves the artist with no violation row and no rule_results row
// for a rule that genuinely failed.
//
// Writing it OPEN also sidesteps the trap a dismissing version would have to
// manage: UpsertViolation emits the paired rule_results FAIL row only for an
// open/pending row (#1107) and later passes preserve 'dismissed', so a
// straight-to-dismissed write leaves the artist permanently unscorable by
// offlineHealthScore. An open row gets its FAIL row on the first write.
//
// Candidates are cleared: a reverted fix offers no choice to present.
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
	// intended, not restored: this line names one rule, and the FixResult
	// revertedFixReport just built names the same rule-scoped set. See the
	// matching site in grantFixCredits.
	p.logger.Error("a rule fix was fully reverted by a field lock; the violation stays OPEN rather than being resolved",
		"artist_id", a.ID, "rule_id", rv.RuleID, "fields", strings.Join(intended, ","))
	return out
}
