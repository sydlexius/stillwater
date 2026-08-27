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
//
// # THE SECOND POPULATION: THE PRE-GUARD PASS (#3079)
//
// LockDamageOpts.PreGuard selects the COMPLEMENT of the allow-list above:
// newest-per-pair damage whose source names NO rule. It drops conditions 3
// and 4 (there is no rule to name), keeps condition 1 (locked NOW), and adds
// one condition that carries the entire safety argument: the damage was
// written STRICTLY BEFORE preGuardCutoff.
//
// #3074 rejected an automatic restore of manual-sourced damage because it
// cannot tell rule damage from an operator's own edit. That objection is
// CORRECT, and it is not answered by a better predicate -- no predicate can
// separate those two on this data. It is answered by changing the shape of
// the mechanism: the population is CLOSED (a fixed past bound, so no future
// edit can enter it), every restore is REVERSIBLE (the guarded verb commits
// its history row inside the restore transaction, #3088/#3090), and the cut
// is PREVIEWED and approved by a human before anything writes.
//
// ON THIS POPULATION THE PREVIEW IS THE REAL SAFETY WORK. The time bound is a
// SCOPE LIMITER, not a discriminator: it bounds the set, it does not tell a
// thin provider record from an operator's own curation. Nothing does.
//
// So the pass applies NO direction heuristic. A shorter value looks like an
// overwrite and a longer one like curation, but a provider can return a
// longer WRONG value, and filtering on direction would silently decide the
// exact question the preview exists to put in front of a human. Direction is
// reported (LockDamageRestore.Direction) and never predicated on.

// LockDamageRestore records one repaired (artist, field) pair.
type LockDamageRestore struct {
	// ChangeID is the damage row's primary key, and the unit the approval
	// DIGEST is computed over (see LockDamageDigest).
	ChangeID   string
	ArtistID   string
	ArtistName string
	Field      string
	RuleID     string
	DamagedAt  time.Time
	// Direction is a GENERIC DESCRIPTOR of what the damage did to the field's
	// length: "emptied", "shorter", "longer", or "same-length". It lets a
	// preview group the cut for the human ruling on it -- an emptied
	// biography is unambiguous, a longer one may be curation -- without any
	// value leaving this package. Reporting only; no predicate reads it.
	Direction string
	// OldLen and NewLen are the RUNE LENGTHS of the operator's value and what
	// replaced it. Lengths, never content -- they carry no library metadata,
	// so they cost nothing against the privacy contract.
	//
	// WHY THE PREVIEW NEEDS THEM (#3079 review, MEDIUM-3). Direction alone is
	// too coarse to carry the ruling it exists for. Measured on a production
	// cut: of the pairs that shrank, 3 lost more than 90% while 74 lost under
	// 5%, and a few differ by only 1-3 characters. A near-certain operator
	// touch-up and a near-total wipe both print "shorter", so the operator
	// cannot separate them without being shown values -- which the contract
	// forbids. The magnitude does that job with no content at all.
	OldLen int
	NewLen int
	// ChainDepth is how many CONSECUTIVE damaging writes form an unbroken
	// value-linked chain ending at this one. 1 is a single clean overwrite.
	//
	// The repair restores THIS row's old_value and asks nothing about whether
	// that value was itself an earlier overwrite. One-step restore is the
	// design (it is what the history model supports, and it is reversible),
	// so this is a preview-fidelity field, not a predicate: an operator
	// ruling on "shorter" deserves to know whether it is step 1 of 1 or step
	// 4 of 5. Zero when the depth could not be read.
	ChainDepth int
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
	// PreGuardTooNew counts pre-guard-mode rows the UPPER TIME BOUND
	// excluded: newest-per-pair unattributed damage written at or after
	// preGuardCutoff. It is reported rather than silently dropped so a run
	// cannot say "nothing to repair" while the bound is quietly holding back
	// a population -- and so the bound's effect is VISIBLE in a dry run
	// instead of inferred from an absence. Zero in attributed mode.
	PreGuardTooNew int
	// PreGuardDiverged counts pre-guard-mode rows dropped because the field's
	// stored value no longer equals the damage the row recorded -- an
	// operator edit or a later writer moved it on. Restoring would overwrite
	// data newer than the damage, so these are excluded from the preview as
	// well as from the write (#3079 review, HIGH-2): before, they were shown
	// as "would restore" and then silently declined by the guarded write.
	// Zero in attributed mode.
	PreGuardDiverged int
	// PreGuardUnlocked counts pre-guard-mode rows dropped because the field
	// is not locked today. These are ordinary metadata churn on fields the
	// operator never pinned -- on a real library they outnumber the eligible
	// rows by an order of magnitude -- so they are counted, never listed.
	// Zero in attributed mode.
	PreGuardUnlocked int
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

	// PreGuard switches the pass to the pre-guard population (#3079): damage
	// whose source names no rule, on a field locked now, written before
	// preGuardCutoff. See the "SECOND POPULATION" block above for why an
	// unattributed restore is safe in this shape and was not in the shape
	// #3074 rejected.
	//
	// The two modes PARTITION the damage set, so no pair can be selected by
	// both and none falls between them.
	PreGuard bool

	// ApprovedDigest is the token the dry run printed for the candidate set
	// the operator reviewed. When set, the pass recomputes the digest over
	// the set it just selected and REFUSES TO WRITE ANYTHING unless the two
	// match, returning *LockDamageDriftError.
	//
	// REQUIRED FOR A PRE-GUARD WRITE PASS (enforced by the caller, so the
	// requirement lives where the operator-facing error message can name the
	// flag). Optional for a dry run, which writes nothing either way, and
	// unused by the attributed pass, whose per-row causation proof does not
	// depend on a human ruling.
	//
	// See lock_damage_digest.go for why the preview must bind the write on
	// this population and why a count would not be enough.
	ApprovedDigest string
}

// preGuardCutoff is the UPPER TIME BOUND on the pre-guard population, and the
// property that makes an unattributed restore safe at all. Damage written at
// or after this instant is NEVER eligible, whatever else is true of it.
//
// WHY THIS INSTANT AND NOT ANOTHER. It is the commit timestamp of v1.6.2
// (`git log -1 --format=%cI v1.6.2` -> 2026-08-18T18:56:23-07:00, commit
// ec8c8100), the FIRST RELEASE carrying the persist chokepoint that ended
// this damage (#3052 / #3053 / #3055 / #3060 / #3063 / #3065, merged
// 2026-08-16..17). Before that release no shipped build refused a rule write
// to a locked field; from it on, one does.
//
// The date is DERIVED, not chosen. Two alternatives were rejected: the merge
// date of the last chokepoint PR (when the code landed on main, not when any
// operator could be RUNNING it, so it would exclude damage from builds that
// genuinely had no guard), and "now, at first boot" (not reproducible, not
// auditable, and it makes the population depend on when a deployment happened
// to upgrade -- the unbounded-future shape #3074 rejected).
//
// The second-level precision is a claim about AUDITABILITY, not accuracy: a
// future reader can re-derive this exact value from the tag.
//
// NOT configurable, deliberately. An operator-supplied cutoff would reopen
// the closed population, which is the one property the mechanism rests on.
var preGuardCutoff = time.Date(2026, time.August, 19, 1, 56, 23, 0, time.UTC)

// PreGuardCutoff exposes the upper time bound for reporting. The dry-run
// preview prints it so an operator reading the candidate list can see the
// bound that produced it, rather than having to trust that one was applied.
// A getter and not an exported var: the bound must not be reassignable from
// outside this package.
func PreGuardCutoff() time.Time { return preGuardCutoff }

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

	res := &LockDamageResult{}

	var candidates []artist.LockDamageCandidate
	var err error
	if opts.PreGuard {
		// The pre-guard population also PRODUCES the unrecoverable report for
		// this mode (rows too new, or whose lock state could not be read), so
		// the trailing LockDamageUnattributed call below is skipped: running
		// it would file every candidate a second time as unrecoverable.
		candidates, err = s.selectPreGuardCandidates(ctx, res)
	} else {
		candidates, err = s.lockDamageHistory.LockDamageCandidates(ctx)
		if err != nil {
			err = fmt.Errorf("selecting locked-field damage candidates: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}

	// THE DIGEST GATE, BEFORE ANY WRITE (#3079 review, HIGH-1). The candidate
	// set is fully decided by this point, so comparing it against the token
	// the operator approved happens while the pass has still written nothing
	// and can abort cleanly. Placed here rather than inside the per-row loop
	// deliberately: a mid-loop refusal would leave a partially-restored
	// database, which is the outcome the gate exists to prevent.
	if err := verifyApprovedDigest(opts, candidates); err != nil {
		return nil, err
	}

	// Chain depths are read ONCE for the whole pass, not per candidate: it is
	// one recursive query over the history table, and a per-row form would
	// re-walk it 215 times. Reporting only -- a failure to read it must never
	// block a repair, so the error is dropped to a nil map and every candidate
	// reports depth 0 rather than the pass aborting over a preview field.
	depths, depthErr := s.lockDamageHistory.LockDamageChainDepths(ctx)
	if depthErr != nil {
		s.logger.Warn("could not read locked-field damage chain depths; "+
			"the preview will report depth 0", "error", depthErr)
		depths = nil
	}

	for i := range candidates {
		if err := s.processLockDamageCandidate(ctx, candidates[i], opts, depths, res); err != nil {
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
	//
	// SKIPPED IN PRE-GUARD MODE. There the unattributed rows are not the
	// report, they are the CANDIDATES: selectPreGuardCandidates has already
	// consumed the same query and set UnattributableAll from it. Running this
	// as well would file every candidate a second time, as unrecoverable, in
	// the same result that restored it.
	if !opts.PreGuard {
		unattributed, err := s.lockDamageHistory.LockDamageUnattributed(ctx)
		if err != nil {
			return nil, fmt.Errorf("selecting unattributed locked-field damage: %w", err)
		}
		if err := s.reportUnattributedLockDamage(ctx, unattributed, res); err != nil {
			return nil, err
		}
	}

	return res, nil
}

// processLockDamageCandidate decides ONE candidate: the attribution checks
// (attributed mode only), the lock check, and then either the dry-run record
// or the guarded write. It returns an error ONLY for a condition that ends
// the whole pass -- a canceled context. Every per-row outcome is filed into
// res and reported as nil.
func (s *Service) processLockDamageCandidate(ctx context.Context, c artist.LockDamageCandidate, opts LockDamageOpts, depths map[artist.LockDamagePairKey]int, res *LockDamageResult) error {
	if !opts.PreGuard && !s.attributionHolds(c, res) {
		return nil
	}

	// Condition 1. Read the STORED artist, never a cached struct: the lock
	// set is the operator's current intent, not what it was at damage time.
	// locked_fields lives on the artists row itself, so side-table hydration
	// is skipped -- WHICH IS SAFE ONLY WHILE NO trackableFields ENTRY LIVES
	// IN A SIDE TABLE. Provider IDs are hydrated from artist_provider_ids, so
	// on an unhydrated struct FieldValueFromArtist("musicbrainz_id") returns
	// "" regardless of the stored value. Unreachable today (no provider ID is
	// in trackableFields, so no candidate can carry one), and the guarded
	// write compares against the COLUMN, not this struct -- but if
	// trackableFields ever gains a side-table field, this read must hydrate
	// it or every such candidate misclassifies here.
	a, err := s.artistService.GetByID(ctx, c.ArtistID, artist.HydrateOpts{})
	if err != nil {
		// A CANCELED PASS HAS DECIDED NOTHING. Filing the remaining
		// candidates as per-row failures would return a partial result that
		// looks like a completed pass; abort with the cause instead. The
		// completion key is stamped only on a pass that returned no error,
		// so a caller honoring that cannot stamp over work it never did.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("lock damage repair aborted: %w", ctxErr)
		}
		res.Failed = append(res.Failed, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "could not read the artist",
		})
		return nil
	}
	if !s.artistService.IsFieldLocked(a, artist.FieldName(c.Field)) {
		res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "the field is not currently locked",
		})
		return nil
	}

	if opts.DryRun {
		res.Restored = append(res.Restored, lockDamageRestoreOf(c, depths[chainKey(c)]))
		return nil
	}
	return s.attemptLockDamageRestore(ctx, c, depths, res)
}

// attributionHolds answers conditions 3 and 4 for the ATTRIBUTED mode: the
// row's own source names a rule, and that rule declares this field. It files
// the skip and returns false when either fails.
//
// An unknown rule id yields no fields, so an unrecognized value restores
// nothing rather than everything: the allow-list direction holding.
// Pseudo-sources ("rule:multiple_rules" and the bulk/maintenance operation
// names) are separated first -- those rows ARE rule-engine damage, but the
// responsible rule is not recoverable from the row, so "the attributing rule
// does not write this field" would be a false reason: no rule by that name
// exists to consult. Same outcome, accurate why.
func (s *Service) attributionHolds(c artist.LockDamageCandidate, res *LockDamageResult) bool {
	if rule.IsPseudoRuleSource(c.RuleID) {
		res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "a rule-engine batch write; the responsible rule is not recorded on the row",
		})
		return false
	}
	if !slices.Contains(rule.RuleFields(c.RuleID), c.Field) {
		res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
			ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
			Reason: "the attributing rule does not write this field",
		})
		return false
	}
	return true
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
func (s *Service) attemptLockDamageRestore(ctx context.Context, c artist.LockDamageCandidate, depths map[artist.LockDamagePairKey]int, res *LockDamageResult) error {
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
	//
	// The message does NOT claim a rule wrote the damage. In pre-guard mode
	// nothing did -- the row's source names no rule, which is the whole
	// reason that population exists -- and rule_id is empty there. A log line
	// asserting an attribution the pass does not have would be the same false
	// causation #3074 caught in the predicate.
	s.logger.Info("restored a locked field that had been overwritten",
		slog.String("artist_id", c.ArtistID),
		slog.String("field", c.Field),
		slog.String("rule_id", c.RuleID),
		slog.Time("damaged_at", c.DamagedAt))

	res.Restored = append(res.Restored, lockDamageRestoreOf(c, depths[chainKey(c)]))
	return nil
}

// lockDamageRestoreOf projects a candidate into the reported form. The
// candidate's values are read here and NEVER carried out: only their LENGTHS
// escape, as damageDirection's fixed vocabulary and as rune counts.
func lockDamageRestoreOf(c artist.LockDamageCandidate, chainDepth int) LockDamageRestore {
	return LockDamageRestore{
		ChangeID: c.ChangeID, ArtistID: c.ArtistID, ArtistName: c.ArtistName,
		Field: c.Field, RuleID: c.RuleID, DamagedAt: c.DamagedAt,
		Direction:  damageDirection(c.OldValue, c.NewValue),
		OldLen:     len([]rune(c.OldValue)),
		NewLen:     len([]rune(c.NewValue)),
		ChainDepth: chainDepth,
	}
}

// damageDirection describes what the damage did to the field's LENGTH, in a
// fixed four-word vocabulary. It is the only thing about a field's content
// this package emits, and it exists so the human ruling on the cut can
// separate ambiguous rows from unambiguous ones without being shown values.
//
// A DESCRIPTOR, never a predicate -- see this file's header for why "it grew,
// so it was curation" is false. Length is in runes, not bytes, so a value
// that gained an accented character does not read as having grown.
func damageDirection(oldValue, newValue string) string {
	o, n := len([]rune(oldValue)), len([]rune(newValue))
	switch {
	case n == 0:
		return "emptied"
	case n < o:
		return "shorter"
	case n > o:
		return "longer"
	default:
		return "same-length"
	}
}

// selectPreGuardCandidates builds the pre-guard population (#3079): the
// newest-per-pair damage rows whose source names NO rule, narrowed by the
// UPPER TIME BOUND and by the field being locked today.
//
// It reuses LockDamageUnattributed -- the query the attributed pass uses to
// REPORT what it cannot fix -- rather than adding a second SQL predicate over
// the same rows, so the two modes partition one row set by construction and
// a change to the shared damage predicate cannot make them disagree.
//
// THE POPULATION IS "SOURCE NAMES NO RULE", NOT "SOURCE = manual". #3079
// measured its deployment as entirely manual-sourced, but the complement also
// admits scan, import and provider: rows -- and a clone measured while
// building this carried 10 scan-sourced rows alongside 11 manual ones.
// Widening to the whole complement is the CONSERVATIVE direction, not a
// loosening: a scan or provider write that replaced a locked field is
// unambiguously an automated writer, whereas manual is the one source that
// might be the operator's own hand. Narrowing to manual would have excluded
// the LEAST ambiguous rows in the set. Every row is previewed either way.
//
// THE TIME BOUND IS APPLIED FIRST, before any per-artist read, so a row
// outside the closed population cannot reach the lock check or the write
// path. It is the safety property; it does not sit behind another test that
// might be edited later. The comparison is STRICT -- a row exactly at the
// boundary is excluded, the allow-list direction holding on an ambiguous
// instant.
func (s *Service) selectPreGuardCandidates(ctx context.Context, res *LockDamageResult) ([]artist.LockDamageCandidate, error) {
	rows, err := s.lockDamageHistory.LockDamageUnattributed(ctx)
	if err != nil {
		return nil, fmt.Errorf("selecting pre-guard locked-field damage: %w", err)
	}
	res.UnattributableAll = len(rows)

	out := make([]artist.LockDamageCandidate, 0)
	byArtist := make(map[string]*artist.Artist)
	for _, u := range rows {
		if !u.DamagedAt.Before(preGuardCutoff) {
			res.PreGuardTooNew++
			continue
		}
		a, ok := byArtist[u.ArtistID]
		if !ok {
			// locked_fields lives on the artists row itself, so side-table
			// hydration is skipped, as in the candidate loop.
			a, err = s.artistService.GetByID(ctx, u.ArtistID, artist.HydrateOpts{})
			if err != nil {
				// A CANCELED PASS HAS DECIDED NOTHING.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, fmt.Errorf("lock damage repair aborted: %w", ctxErr)
				}
				// TRANSIENT, so it blocks completion and retries next boot.
				// NOT Unrecoverable: that category is final, and a row whose
				// lock state was never read has not been decided -- filing it
				// there would retire a restorable pair on a read hiccup.
				res.Failed = append(res.Failed, LockDamageSkip{
					ArtistID: u.ArtistID, Field: u.Field,
					Reason: "could not read the artist to check its lock state",
				})
				continue
			}
			byArtist[u.ArtistID] = a
		}
		if !s.artistService.IsFieldLocked(a, artist.FieldName(u.Field)) {
			res.PreGuardUnlocked++
			continue
		}
		// THE DIVERGENCE CHECK, RUN DURING SELECTION SO THE PREVIEW MEANS IT
		// (#3079 review, HIGH-2). Before this, the dry run short-circuited
		// ahead of RestoreLockedFieldGuarded, so a pair whose stored value had
		// already moved on was previewed as "would restore" and then silently
		// declined at write time -- preview 215, repair 214, and nothing
		// compared the two.
		//
		// It asks the SAME question the write asks, through the same exported
		// predicate (artist.FieldValueStillDamaged), so the two cannot drift.
		// This does NOT replace the guarded compare-and-set: that one runs
		// inside the write transaction and is what closes the race between
		// this read and the write. This one makes the PREVIEW honest; that one
		// makes the WRITE safe.
		// FieldValueFromArtist already yields the JOINED value form for slice
		// fields (it reads the hydrated []string, not the raw JSON column), so
		// it is NOT wrapped in artist.StoredFieldValue here. Wrapping it would
		// feed "a, b" to a JSON decoder, which fails and yields "", making
		// every locked slice-field candidate read as diverged.
		stored := artist.FieldValueFromArtist(a, u.Field)
		if !artist.FieldValueStillDamaged(u.Field, stored, u.NewValue) {
			res.PreGuardDiverged++
			continue
		}
		out = append(out, artist.LockDamageCandidate{
			ChangeID: u.ChangeID, ArtistID: u.ArtistID,
			Field: u.Field, OldValue: u.OldValue, NewValue: u.NewValue,
			// RuleID is deliberately EMPTY: no rule is named on these rows,
			// and inventing a placeholder would put a fake attribution into
			// the report the maintainer rules on. ArtistName is left empty
			// for the same reason the dry-run report never prints one: for a
			// field="name" row the artist name IS the damaged value.
			DamagedAt: u.DamagedAt,
		})
	}
	return out, nil
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
