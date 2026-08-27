package artist

import (
	"strings"
	"time"
)

// lock_damage.go -- candidate selection for the automated repair of locked
// fields a past rule run overwrote (#3038, the repair half of #3037).
//
// WHY ATTRIBUTION IS THE DAMAGE ROW'S OWN SOURCE (#3074 review).
// An earlier design joined each damage row to any earlier rule_fix row on the
// same artist. That proves a rule ran on the artist AT SOME POINT, never that
// it caused THIS row, so an operator's own edit made after any rule run matched
// the join and would have been restored over -- destroying the operator data
// the lock exists to protect. The per-row source is the only key that separates
// a rule write from an operator edit: persistHealthAfterRun stamps it via
// withRuleHistorySource (internal/rule/fixer.go).
//
// The accepted cost is coverage. A damage row carries a rule: source only on a
// build shipping #3048 (fdeb1b6f), which no release does, so this repairs
// nothing on existing databases and exists to catch a FUTURE write path that
// escapes the chokepoint. Pre-#3048 damage is reported as unrecoverable via
// LockDamageUnattributed.
//
// THIS QUERY IS DELIBERATELY INCOMPLETE. It answers conditions 2 and 3 of the
// spec's four-part allow-list. Conditions 1 (the field is CURRENTLY locked) and
// 4 (the naming rule declares that field) are answered in Go by the caller,
// because locked_fields is a JSON array and rule.RuleFields is a static Go map
// -- re-implementing either in SQL is the drift that
// 024_retract_false_duplicate_passes.sql documents as the reason to avoid a
// migration. internal/artist must not import internal/rule, so the rule id
// crosses the boundary as data.

// LockDamageCandidate is one (artist, field) pair whose newest change reads as
// damage and whose own source names the rule that wrote it.
//
// It is a CANDIDATE, never a decision: the caller still applies the lock check
// and the rule-capability check before restoring anything.
type LockDamageCandidate struct {
	// ChangeID is the damage row's primary key.
	ChangeID string
	// ArtistID and ArtistName identify the affected artist.
	ArtistID   string
	ArtistName string
	// Field is the damaged metadata field.
	Field string
	// OldValue is the operator's value, and what a restore writes back.
	OldValue string
	// NewValue is what replaced it. Carried for the staleness recheck and the
	// report, never restored.
	NewValue string
	// RuleID is the attributing rule, taken from THIS ROW'S OWN source with the
	// "rule:" prefix removed. The caller resolves it against rule.RuleFields;
	// an id absent from the catalogue yields no fields and so restores nothing.
	RuleID string
	// DamagedAt is the damage row's created_at.
	DamagedAt time.Time
}

// LockDamageUnattributedRow is one (artist, field) pair whose newest change
// reads as damage but whose source does NOT name a rule, so no safe predicate
// can attribute it. All pre-#3048 damage has this shape (source = "manual",
// byte-identical to an operator edit).
//
// These rows exist so the repair can REPORT what it cannot fix. A run that
// says "nothing to repair" because it cannot see the damage is the "unknown
// rendered as clean" defect the blast-radius work exists to prevent.
type LockDamageUnattributedRow struct {
	// ChangeID is the damage row's primary key.
	ChangeID string
	ArtistID string
	Field    string
	// OldValue is the operator's value; NewValue is what replaced it.
	//
	// THESE ARRIVED WITH #3079 AND CHANGED THIS TYPE'S ROLE. Before it, this
	// row was a pure REPORT ("we saw damage we cannot attribute") and carried
	// no values at all, deliberately. The pre-guard repair pass restores from
	// exactly this set, so it needs the value to write back and the value to
	// compare against -- the same two the attributed pass takes from
	// LockDamageCandidate. Nothing else changed: the query is unchanged in
	// which ROWS it returns.
	//
	// PRIVATE LIBRARY CONTENT. Neither ever reaches a log line or a report;
	// see the reporting rules in docs/architecture/lock-damage-repair.md.
	OldValue string
	NewValue string
	// Source is the damage row's recorded source ("manual", "scan",
	// "provider:<name>", ...). Carried so the report can say WHY the row is
	// unattributable. Never a value.
	Source string
	// DamagedAt is the damage row's created_at, and the pre-guard pass's
	// UPPER TIME BOUND is applied against it (#3079). An unparsable timestamp
	// resolves to time.Now (parseHistoryTimestamp), which is always AFTER the
	// cutoff, so a row whose timestamp cannot be read is EXCLUDED rather than
	// admitted -- the allow-list direction holding on the one field the bound
	// rests on.
	DamagedAt time.Time
}

// StoredFieldValue converts a raw artists-column value into the VALUE FORM
// history rows carry. Slice fields store a JSON array in the column but
// metadata_changes records the joined "a, b, c" representation, so a raw
// comparison of the two is a type error wearing a string's clothes.
//
// Exported because #3079's preview must ask the same question the write path
// asks. Before this, the dry run short-circuited before reaching the guarded
// verb, so a candidate whose stored value had already diverged was previewed
// as "would restore" and then silently declined at write time -- the preview
// overstating in the direction nothing was checking.
func StoredFieldValue(field, storedRaw string) string {
	if sliceFields[field] {
		return strings.Join(UnmarshalStringSlice(storedRaw), ", ")
	}
	return storedRaw
}

// FieldValueStillDamaged reports whether stored still equals the damaged
// value a candidate was selected for, under the same normalization the
// repository uses for no-op detection.
//
// THIS IS THE DIVERGENCE PREDICATE, AND IT HAS EXACTLY TWO CALLERS BY DESIGN:
// RestoreLockedFieldGuarded (which decides the write) and the #3079 preview
// (which decides what the operator is shown). They MUST NOT drift: a preview
// answering a different question than the write is a preview that cannot bind
// it, and the whole safety argument for the pre-guard pass is that the human
// ruled on the set that actually gets written.
//
// False means the field moved on after the damage -- an operator edit, or a
// later writer -- so restoring would overwrite data newer than the damage.
func FieldValueStillDamaged(field, stored, damagedValue string) bool {
	return normalizeFieldValue(field, stored) == normalizeFieldValue(field, damagedValue)
}
