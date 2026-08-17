package artist

import (
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
	ArtistID string
	Field    string
	// Source is the damage row's recorded source ("manual", "scan",
	// "provider:<name>", ...). Carried so the report can say WHY the row is
	// unattributable. Never a value.
	Source string
}
