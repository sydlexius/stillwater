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
// to it. The alternative outcome is Dismissed, and a dismissed row has no
// un-dismiss route: UpsertViolation's ON CONFLICT pins 'dismissed' (#1107) and
// ReopenViolation is a positive allow-list on 'resolved'. Wrongly returning nil
// costs the operator a Fix click on a violation that will re-raise; wrongly
// returning a dismiss costs them the finding permanently.
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
	// Dismissed rather than left open, matching #3060: the refusal is TERMINAL.
	// Re-running this fixer against the same pinned field can only produce the
	// same restoration, so an open row would hand the operator a Fix button
	// that does nothing every time they press it.
	//
	// Fixed is cleared as well as Dismissed being set. Callers gate the
	// resolve, the provenance record and the platform publish on Fixed, and
	// every one of those would be describing a value the database does not
	// hold.
	return &FixResult{
		RuleID:    fr.RuleID,
		Fixed:     false,
		Dismissed: true,
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
// restored-field report, resolves the ones whose fix survived, and dismisses
// the ones whose every guarded change the lock guard reverted (#3037).
//
// Callers gate this on the artist row having persisted, exactly as they gated
// finalizeResolvedRows: a failed write leaves the mutation in memory, and
// neither verdict below would describe the stored row.
func (p *Pipeline) resolveOrDismissRows(ctx context.Context, a *artist.Artist, acc *runForArtistAccum, restored []string, startedAt time.Time) bool {
	keep, reverted := acc.splitLockRevertedRows(restored)
	ok := p.finalizeResolvedRows(ctx, a, keep, startedAt)
	if !p.dismissLockRevertedRows(ctx, a, reverted, startedAt) {
		ok = false
	}
	return ok
}

// dismissLockRevertedRows persists each fully-reverted row as dismissed, and
// returns true when every write succeeded.
//
// TWO UPSERTS PER ROW, AND THE FIRST IS LOAD-BEARING -- the same trap
// processAutoFixViolation documents. UpsertViolation writes the paired
// rule_results FAIL row only for an open or pending row (#1107), and every
// later pass preserves 'dismissed', so going straight to dismissed on a first
// pass would leave the artist with NO rule_results row for this rule, ever.
// offlineHealthScore refuses to score an artist in that state, so its health
// would freeze permanently. Recording the open verdict first is also honest:
// the evaluation did observe the rule failing, and the dismiss is a statement
// about the FIX, not about the rule passing.
//
// Candidates are cleared: a terminal result offers no choice, and a stored list
// would be a decision that never gets presented.
func (p *Pipeline) dismissLockRevertedRows(ctx context.Context, a *artist.Artist, rows []*RuleViolation, startedAt time.Time) bool {
	ok := true
	for _, rv := range rows {
		p.logger.Info("a rule fix was fully reverted by a field lock; the violation is dismissed rather than resolved",
			"artist_id", a.ID, "rule_id", rv.RuleID)
		rv.Candidates = nil
		rv.EvaluatedAt = startedAt
		rv.Status = ViolationStatusOpen
		rv.DismissedAt = nil
		if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
			p.logger.Warn("persisting lock-reverted violation baseline",
				"rule_id", rv.RuleID, "artist", a.Name, "error", err)
			ok = false
			continue
		}
		now := time.Now().UTC()
		rv.Status = ViolationStatusDismissed
		rv.DismissedAt = &now
		if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
			p.logger.Warn("persisting lock-reverted violation dismissal",
				"rule_id", rv.RuleID, "artist", a.Name, "error", err)
			ok = false
		}
	}
	return ok
}

// dismissIfLockReverted dismisses rv and returns the replacement FixResult when
// every guarded change this fix made was restored by the lock guard. It returns
// (nil, nil) when the fix is not fully reverted and the caller should proceed
// with its normal success path.
//
// DismissViolation on the row the caller already loaded, exactly as the
// fixer-reported-Dismissed branch does. The two-upsert dance
// dismissLockRevertedRows needs does NOT apply here: that path may have no row
// yet, whereas this one loaded an OPEN row by id, so its paired rule_results
// FAIL row already exists and the artist stays scorable.
func (p *Pipeline) dismissIfLockReverted(ctx context.Context, a *artist.Artist, rv *RuleViolation, fr *FixResult, intended, restored []string) (*FixResult, error) {
	out := lockRevertedFixResult(fr, intended, restored)
	if out == nil {
		return nil, nil
	}
	p.logger.Info("a rule fix was fully reverted by a field lock; the violation is dismissed rather than resolved",
		"artist_id", a.ID, "rule_id", rv.RuleID, "fields", strings.Join(restored, ","))
	if err := p.ruleService.DismissViolation(ctx, rv.ID); err != nil {
		return nil, fmt.Errorf("dismissing violation after a lock-reverted fix: %w", err)
	}
	return out, nil
}
