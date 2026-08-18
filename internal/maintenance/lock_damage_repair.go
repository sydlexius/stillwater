package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// lock_damage_repair.go -- the repair half of #3037 (#3038, impl #3075).
//
// Restores locked fields a past rule run overwrote. Prevention now holds at the
// persist chokepoint (internal/artist/lockguard.go), but prevention does
// nothing for an artist already damaged.
//
// ROW SELECTION IS A POSITIVE ALLOW-LIST, in four parts. The query
// (artist.HistoryRepository.LockDamageCandidates) answers 2 and 3; this file
// answers 1 and 4:
//
//  1. the field is CURRENTLY in the artist's locked_fields
//  2. the row is the newest change for its (artist, field) pair AND reads as damage
//  3. the damage row's OWN source is rule:<id> -- per-row causation, not an
//     artist-wide coincidence
//  4. rule.RuleFields(<id>) contains the damaged field
//
// Anything unrecognized -- an unknown rule id, a field absent from the
// catalogue -- falls through to NOT restored and is counted as unrecoverable. A
// predicate safe for deciding whether to WRITE turns destructive when inverted
// to decide what to overwrite, which is why this direction is not negotiable.

// LockDamageRestore records one repaired (artist, field) pair.
type LockDamageRestore struct {
	ArtistID   string
	ArtistName string
	Field      string
	RuleID     string
	DamagedAt  time.Time
}

// LockDamageSkip records a pair that was NOT repaired, and why. Reason is a
// hand-authored literal and never carries a field value.
type LockDamageSkip struct {
	ArtistID string
	Field    string
	RuleID   string
	Reason   string
}

// LockDamageResult reports what a pass did. Unrecoverable is as load-bearing as
// Restored: a run that repairs nothing and says so is correct, but a run
// reporting zero because the mechanism cannot SEE the damage is the "unknown
// rendered as clean" defect the blast-radius work exists to prevent.
type LockDamageResult struct {
	Restored      []LockDamageRestore
	Unrecoverable []LockDamageSkip
	// Failed holds TRANSIENT failures (a read error, a write error): outcomes
	// that may differ on the next boot, so they block completion and retry.
	Failed []LockDamageSkip
	// FailedPermanent holds DETERMINISTIC failures: a restore the write layer
	// refused for a reason that recurs identically every boot (the old value
	// fails today's validation, or restoring a name would recreate a
	// collision a later rename removed). Retrying cannot change the answer,
	// so these are reported in their own category and do NOT block
	// completion -- a pass blocked forever by an unretriable row re-runs the
	// full repair on every start with no operator-visible way to retire it.
	FailedPermanent []LockDamageSkip
	// UnattributableAll counts EVERY newest-per-pair damage row whose source
	// names no rule, locked or not. Unrecoverable lists only the rows on a
	// field locked NOW (the set the feature is scoped to act on); this wider
	// number is reported alongside it so the filtered list can never read as
	// the whole population. The blast-radius pane surfaces the full set.
	UnattributableAll int
}

// LockDamageOpts controls a pass.
type LockDamageOpts struct {
	// DryRun selects and reports without writing. Used for the
	// production-clone validation described in the design doc
	// (docs/architecture/lock-damage-repair.md).
	DryRun bool
}

// ErrLockDamageDepsNotSet reports a RepairLockDamage call before
// SetLockDamageDeps attached the artist service and history repository. A
// sentinel rather than an ad-hoc fmt.Errorf so callers and tests can identify
// the condition with errors.Is instead of matching message text (the repo's
// established pattern; compare ErrNoProviderIDRepository).
var ErrLockDamageDepsNotSet = errors.New("lock damage repair: dependencies not set (SetLockDamageDeps)")

// SetLockDamageDeps attaches the dependencies the locked-field damage repair
// needs. Setter form rather than a NewService widening: the artist service and
// history repository exist only after wireAuth builds them, while the
// maintenance service is built earlier in wireInfraServices, and every other
// maintenance feature is indifferent to them. Matches the repo's established
// pattern (artist.Service.SetHistoryService and siblings).
//
// RepairLockDamage returns an error when called before this.
func (s *Service) SetLockDamageDeps(history artist.HistoryRepository, artistSvc *artist.Service) {
	s.lockDamageHistory = history
	s.artistService = artistSvc
}

// RepairLockDamage restores every locked field a rule overwrote.
//
// Each pair is restored INDEPENDENTLY through artist.Service.UpdateField. That
// verb is deliberately lock-blind (see the comment block in
// internal/artist/lockguard.go): the operator's history revert and blast-radius
// restore use it for exactly this reason. Routing a restore through
// Service.Update instead would hit enforceLocksBeforeUpdate and have the
// restore reverted by the very guard that stopped the damage.
//
// The write records source="revert", which is also what makes a second pass a
// no-op: the revert row becomes the newest row for the pair, so the damage
// predicate stops matching it. The settings flag consulted by the startup
// wiring is an optimization; this convergence is the real idempotence.
func (s *Service) RepairLockDamage(ctx context.Context, opts LockDamageOpts) (*LockDamageResult, error) {
	if s.lockDamageHistory == nil || s.artistService == nil {
		return nil, ErrLockDamageDepsNotSet
	}

	candidates, err := s.lockDamageHistory.LockDamageCandidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("selecting locked-field damage candidates: %w", err)
	}

	res := &LockDamageResult{}
	for i := range candidates {
		c := candidates[i]

		// Condition 4. An unknown rule id yields no fields, so an unrecognized
		// value restores nothing rather than everything: the allow-list
		// direction holding. Pseudo-sources ("rule:multiple_rules" and the
		// bulk/maintenance operation names) are separated first: those rows
		// ARE rule-engine damage, but the responsible rule is not recoverable
		// from the row, so "the attributing rule does not write this field"
		// would be a false reason -- no rule by that name exists to consult.
		// Same outcome (never restored, counted), accurate why.
		if rule.IsPseudoRuleSource(c.RuleID) {
			res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
				ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
				Reason: "a rule-engine batch write; the responsible rule is not recorded on the row",
			})
			continue
		}
		if !slices.Contains(rule.RuleFields(c.RuleID), c.Field) {
			res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
				ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
				Reason: "the attributing rule does not write this field",
			})
			continue
		}

		// Condition 1. Read the STORED artist, never a cached struct: the lock
		// set is the operator's current intent, not what it was at damage time.
		// locked_fields lives on the artists row itself, so side-table
		// hydration is skipped -- WHICH IS SAFE ONLY WHILE NO trackableFields
		// ENTRY LIVES IN A SIDE TABLE. Provider IDs are hydrated from
		// artist_provider_ids, so on an unhydrated struct
		// FieldValueFromArtist("musicbrainz_id") returns "" regardless of the
		// stored value. Unreachable today (no provider ID is in
		// trackableFields, so no candidate can carry one), and the guarded
		// write compares against the COLUMN, not this struct -- but if
		// trackableFields ever gains a side-table field, this read must
		// hydrate it or every such candidate misclassifies here.
		a, err := s.artistService.GetByID(ctx, c.ArtistID, artist.HydrateOpts{})
		if err != nil {
			// A CANCELED PASS HAS DECIDED NOTHING. Filing the remaining
			// candidates as per-row failures would return a partial result
			// that looks like a completed pass; abort with the cause instead,
			// so the startup wiring logs it and the next boot retries the
			// whole pass (the completion key is only stamped by the caller on
			// a returned result).
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("lock damage repair aborted: %w", ctxErr)
			}
			res.Failed = append(res.Failed, LockDamageSkip{
				ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
				Reason: "could not read the artist",
			})
			continue
		}
		if !s.artistService.IsFieldLocked(a, artist.FieldName(c.Field)) {
			res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
				ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
				Reason: "the field is not currently locked",
			})
			continue
		}

		if opts.DryRun {
			res.Restored = append(res.Restored, LockDamageRestore{
				ArtistID: c.ArtistID, ArtistName: c.ArtistName,
				Field: c.Field, RuleID: c.RuleID, DamagedAt: c.DamagedAt,
			})
			continue
		}

		if err := s.attemptLockDamageRestore(ctx, c, res); err != nil {
			return nil, err
		}
	}

	// The unrecoverable REPORT: newest-per-pair damage whose source names no
	// rule (all pre-#3048 damage looks like this). These rows are filtered out
	// by the candidate query itself, so without this second query they would
	// never reach any tally and the run would render unknown as clean.
	//
	// THE PER-ROW LIST IS FILTERED TO FIELDS LOCKED NOW; THE WIDER COUNT IS
	// KEPT AS ONE NUMBER (UnattributableAll). Unfiltered, the list is
	// dominated by ordinary manual edits to unlocked fields -- on a production
	// clone, 3234 rows of which only 216 sat on a field locked today -- which
	// buries the rows the feature is scoped to act on. Nothing is hidden: the
	// blast-radius pane already surfaces the full unfiltered set, and the
	// logged summary carries both numbers.
	//
	// The lock check stays in GO. locked_fields is a JSON array and the lock
	// predicate lives on artist.Service.IsFieldLocked; re-implementing it in
	// SQL is the drift 024_retract_false_duplicate_passes.sql warns against.
	unattributed, err := s.lockDamageHistory.LockDamageUnattributed(ctx)
	if err != nil {
		return nil, fmt.Errorf("selecting unattributed locked-field damage: %w", err)
	}
	if err := s.reportUnattributedLockDamage(ctx, unattributed, res); err != nil {
		return nil, err
	}

	return res, nil
}

// attemptLockDamageRestore performs the guarded write for one candidate and
// files the outcome into res.
//
// THE WRITE IS CONDITIONAL, DECIDED ATOMICALLY IN THE REPOSITORY LAYER
// (#3074 review; window closed in the #3075 fix round). The candidate was
// decided from a list read at the top of the pass, and this repair runs in a
// goroutine at startup while the server is already serving, so an operator
// holding a grant can edit -- or unlock -- the field between the caller's
// reads and this write. RestoreLockedFieldGuarded re-verifies BOTH inside one
// transaction and applies the update only while the stored value still equals
// the damaged value the candidate was selected for, so the restore can never
// overwrite data newer than the damage it attributed. A Go-side
// read-then-write here is exactly the window the verb closes; do not
// reintroduce one.
//
// NOT Service.UpdateField: that verb is deliberately unconditional (the
// operator's history revert and blast-radius restore write on the operator's
// say-so), and its callers rely on that contract.
func (s *Service) attemptLockDamageRestore(ctx context.Context, c artist.LockDamageCandidate, res *LockDamageResult) error {
	writeCtx := artist.ContextWithSource(ctx, "revert")
	outcome, err := s.artistService.RestoreLockedFieldGuarded(
		writeCtx, c.ArtistID, c.Field, c.NewValue, c.OldValue)
	if err != nil {
		// A CANCELED PASS HAS DECIDED NOTHING: abort rather than filing the
		// remaining candidates as failures a completed-looking result carries.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("lock damage repair aborted: %w", ctxErr)
		}
		// A DETERMINISTIC refusal recurs identically on every boot: retrying
		// cannot change the answer, so it must not block completion forever
		// (the pass would re-run on every start with no way to retire it).
		// Typed checks, never string matching: the validator refusing the OLD
		// value under a rule added after the row was written, and a name
		// restore that would recreate a collision a later rename removed.
		if errors.Is(err, artist.ErrInvalidFieldValue) || errors.Is(err, artist.ErrNameCollision) {
			res.FailedPermanent = append(res.FailedPermanent, LockDamageSkip{
				ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
				Reason: "the restore was refused and will be refused identically on retry",
			})
			return nil
		}
		// Anything else is TRANSIENT (a read or write error): counted, the
		// pass continues, and the unstamped completion key retries it next
		// boot. One bad row never aborts the pass.
		res.Failed = append(res.Failed, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "the restore write failed",
		})
		return nil
	}
	switch outcome {
	case artist.LockedFieldRestoreValueDiverged:
		// The stored value moved on after selection (an operator edit, or a
		// concurrent writer inside the pass). The pair's newest state is no
		// longer the damage this candidate attributed, so the outcome is
		// DECIDED, not failed: it neither blocks completion nor retries.
		res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "the field changed after the candidate was selected",
		})
		return nil
	case artist.LockedFieldRestoreUnlocked:
		res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "the field was unlocked after the candidate was selected",
		})
		return nil
	case artist.LockedFieldRestoreApplied:
		// Fall through to record the restore.
	}

	// Values are never logged: an old biography is user library content.
	s.logger.Info("restored a locked field a rule had overwritten",
		slog.String("artist_id", c.ArtistID),
		slog.String("field", c.Field),
		slog.String("rule_id", c.RuleID),
		slog.Time("damaged_at", c.DamagedAt))

	res.Restored = append(res.Restored, LockDamageRestore{
		ArtistID: c.ArtistID, ArtistName: c.ArtistName,
		Field: c.Field, RuleID: c.RuleID, DamagedAt: c.DamagedAt,
	})
	return nil
}

// reportUnattributedLockDamage files the unattributed damage rows into res:
// the per-row actionable list filtered to fields locked now, and the wider
// count kept whole in UnattributableAll.
func (s *Service) reportUnattributedLockDamage(ctx context.Context, unattributed []artist.LockDamageUnattributedRow, res *LockDamageResult) error {
	res.UnattributableAll = len(unattributed)
	lockedByArtist := make(map[string]*artist.Artist)
	for _, u := range unattributed {
		a, ok := lockedByArtist[u.ArtistID]
		if !ok {
			// locked_fields lives on the artists row itself, so side-table
			// hydration is skipped, as in the candidate loop.
			var err error
			a, err = s.artistService.GetByID(ctx, u.ArtistID, artist.HydrateOpts{})
			if err != nil {
				// A CANCELED PASS HAS DECIDED NOTHING -- and this category
				// is worse than Failed: Unrecoverable is treated as a FINAL
				// outcome, so rows filed here under a canceled read would be
				// permanently reported as unrecoverable without ever being
				// examined. Abort with the cause instead.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fmt.Errorf("lock damage repair aborted: %w", ctxErr)
				}
				// Undecidable rather than decided-unlocked: keep the row in
				// the actionable list so a read hiccup cannot silently shrink
				// it. Reported with its own reason.
				res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
					ArtistID: u.ArtistID, Field: u.Field,
					Reason: "the damage row's source (" + u.Source + ") names no rule; " +
						"the artist's lock state could not be read",
				})
				continue
			}
			lockedByArtist[u.ArtistID] = a
		}
		if !s.artistService.IsFieldLocked(a, artist.FieldName(u.Field)) {
			continue
		}
		res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
			ArtistID: u.ArtistID, Field: u.Field,
			Reason: "the damage row's source (" + u.Source + ") names no rule; " +
				"indistinguishable from an operator edit",
		})
	}
	return nil
}
