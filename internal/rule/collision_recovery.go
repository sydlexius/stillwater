package rule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

// Package #2967 background: cleanupDisabledRuleState (service.go) used to
// soft-resolve every open/pending_choice violation for a rule in one UPDATE
// whenever that rule was saved while disabled -- not only on an explicit
// "disable this rule" click, but on ANY Update call made while the rule was
// already disabled (a settings import, a tolerance tweak), and
// cross_artist_backdrop_collision is SEEDED disabled. #2614 stopped the bug
// going forward by excluding event-driven rules from that cleanup, but it did
// not do anything for violations the pre-#2614 code had already resolved:
// those rows are indistinguishable, by status alone, from violations an
// operator resolved on purpose (dismissing a stale collision, or via the
// fixer). This file adds detection (advisory only) and an explicit,
// ID-scoped reopen -- never an automatic migration, because a migration runs
// unattended on every install and a false positive would re-arm a
// destructive back-out fixer against a row an operator deliberately closed.

// ResolvedCollisionViolation is one resolved
// cross_artist_backdrop_collision row, annotated with advisory signals that
// help an operator judge whether it looks bug-resolved or operator-resolved.
//
// ClusterSize and the report-level NoRuleResultsExist flag are ADVISORY
// ONLY. Neither can prove the cause of any single row: a large cluster is
// consistent with the pre-#2614 bulk UPDATE (which stamped every affected
// row with one byte-identical resolved_at), but an operator could also
// legitimately dismiss several collisions in the same second by hand, and
// rule_results existing again today says nothing about whether it existed at
// the moment this particular row was resolved. These signals exist to help a
// human sort a list, not to make the reopen decision for them.
type ResolvedCollisionViolation struct {
	ID         string
	ArtistID   string
	ArtistName string
	Message    string
	CreatedAt  time.Time
	ResolvedAt *time.Time
	UpdatedAt  time.Time

	// ClusterSize is the number of resolved collision rows (within this
	// result set) that share this row's exact resolved_at string. Computed
	// in Go from the same rows the query returns, not a separate query, so it
	// can never disagree with what the caller is looking at.
	ClusterSize int
}

// ResolvedCollisionReport is the return value of ResolvedCollisionViolations:
// the candidate rows plus one install-wide advisory flag.
type ResolvedCollisionReport struct {
	Violations []ResolvedCollisionViolation

	// NoRuleResultsExist is true when zero rule_results rows currently exist
	// for cross_artist_backdrop_collision. Since that rule is event-driven,
	// rule_results is not otherwise populated for it, so this only means
	// "nothing has recently upserted a rule_results row" -- it is corroborating,
	// not diagnostic on its own.
	NoRuleResultsExist bool
}

// ResolvedCollisionViolations returns every resolved
// cross_artist_backdrop_collision violation, for operator review (#2967).
// This is READ-ONLY: it changes nothing. dismissed rows are a different,
// already-terminal status and must never appear here -- the WHERE clause
// filters status = 'resolved' explicitly, not "status != 'open'".
func (s *Service) ResolvedCollisionViolations(ctx context.Context) (*ResolvedCollisionReport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, artist_id, artist_name, message, created_at, resolved_at, updated_at
		FROM rule_violations
		WHERE rule_id = ? AND status = ?
		ORDER BY resolved_at, id
	`, RuleCrossArtistBackdropCollision, ViolationStatusResolved)
	if err != nil {
		return nil, fmt.Errorf("querying resolved collision violations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	// resolvedAtRaw is kept alongside the parsed field so ClusterSize can be
	// grouped on the exact stored string (lexical, matching what the SQL
	// would compare) rather than on a re-parsed time.Time, which could
	// silently disagree with SQL's own comparison for an unparsable value
	// (#2972's trap).
	//
	// resolvedAtRaw is sql.NullString, not string: resolved_at is a nullable
	// column (001_initial_schema.sql), and UpsertViolation can write
	// status='resolved' with resolved_at=NULL (service.go, UpsertViolation ->
	// dbutil.NilableTime(nil)). A plain string scan target crashes the ENTIRE
	// query with "converting NULL to string is unsupported" on the first such
	// row, which turns the whole report -- not just that row -- into a hard
	// error. That is the opposite of what a recovery tool should do with one
	// malformed row.
	//
	// A NULL resolved_at row is deliberately EXCLUDED from cluster counting:
	// it never contributes to another row's clusterCounts entry, and its own
	// ClusterSize is always 1. Folding it into a cluster keyed on the empty
	// string would manufacture a false cluster out of rows that share nothing
	// but a missing stamp -- exactly the kind of inflated signal an operator
	// would misread as "these were bulk-resolved together" per #2972. The row
	// is still returned (this is a report, never a silent drop); it simply
	// cannot be said to cluster with anything.
	type row struct {
		v             ResolvedCollisionViolation
		resolvedAtRaw sql.NullString
	}
	var collected []row
	clusterCounts := make(map[string]int)
	for rows.Next() {
		var r row
		var createdAtRaw, updatedAtRaw string
		if err := rows.Scan(&r.v.ID, &r.v.ArtistID, &r.v.ArtistName, &r.v.Message,
			&createdAtRaw, &r.resolvedAtRaw, &updatedAtRaw); err != nil {
			return nil, fmt.Errorf("scanning resolved collision violation: %w", err)
		}
		r.v.CreatedAt = dbutil.ParseTime(createdAtRaw)
		// ResolvedAt is non-nil only when the stored value genuinely parsed,
		// so a NULL row and an unparsable row are reported the same way and
		// neither fabricates a year-0001 timestamp (dbutil.ParseTime returns
		// the zero time, not an error, on a malformed string). ClusterSize
		// below still keys on the raw string, so an unparsable row can still
		// cluster correctly even though its ResolvedAt is nil here -- that
		// asymmetry is deliberate; do not "fix" it into agreement.
		if r.resolvedAtRaw.Valid {
			if resolvedAt, ok := dbutil.ParseTimeOK(r.resolvedAtRaw.String); ok {
				r.v.ResolvedAt = &resolvedAt
			}
		}
		r.v.UpdatedAt = dbutil.ParseTime(updatedAtRaw)
		if r.resolvedAtRaw.Valid {
			clusterCounts[r.resolvedAtRaw.String]++
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating resolved collision violations: %w", err)
	}

	// Explicitly close the cursor before the next query. The pool is
	// single-connection (SetMaxOpenConns(1)), so the next statement must not run
	// while this cursor could still hold the connection. This prevents a subtle
	// deadlock if a future code path breaks out of the loop early or falls through.
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing resolved collision cursor: %w", err)
	}

	report := &ResolvedCollisionReport{Violations: make([]ResolvedCollisionViolation, 0, len(collected))}
	for i := range collected {
		if collected[i].resolvedAtRaw.Valid {
			collected[i].v.ClusterSize = clusterCounts[collected[i].resolvedAtRaw.String]
		} else {
			collected[i].v.ClusterSize = 1
		}
		report.Violations = append(report.Violations, collected[i].v)
	}

	var resultExists bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM rule_results WHERE rule_id = ?)
	`, RuleCrossArtistBackdropCollision).Scan(&resultExists); err != nil {
		return nil, fmt.Errorf("checking rule_results for collision rule: %w", err)
	}
	report.NoRuleResultsExist = !resultExists

	return report, nil
}

// Reason codes returned by ReopenCollisionViolations for a violation ID that
// was NOT reopened.
const (
	// ReopenReasonNotFound means no rule_violations row exists with that ID.
	ReopenReasonNotFound = "not_found"
	// ReopenReasonNotResolved means the row exists but its status is not
	// "resolved" (already open or pending_choice). Distinct from
	// ReopenReasonDismissed: an open/pending_choice row was never resolved at
	// all, so "reopen" does not apply to it the same way.
	ReopenReasonNotResolved = "not_resolved"
	// ReopenReasonDismissed means the row exists but its status is
	// "dismissed" -- a deliberate operator closure, not merely "not
	// resolved". Reported separately from ReopenReasonNotResolved so an
	// operator-facing screen can say "this was dismissed on purpose" rather
	// than lumping it in with a row that simply never resolved. This changes
	// only how the refusal is REPORTED: a dismissed row is exactly as
	// unreachable for reopening as it was before this code existed (it fails
	// the same status = 'resolved' allow-list clause in the UPDATE below).
	ReopenReasonDismissed = "dismissed"
	// ReopenReasonWrongRule means the row exists but belongs to a different
	// rule than cross_artist_backdrop_collision.
	ReopenReasonWrongRule = "wrong_rule"
)

// reopenMaxIDs bounds one ReopenCollisionViolations call. SQLite refuses a
// statement with more than ~32766 bound parameters ("too many SQL
// variables"), and this method's UPDATE -- the larger of its two queries --
// binds one parameter per ID plus 4 fixed ones (status, updated_at, rule_id,
// status), so an unbounded list eventually fails with that raw driver string
// instead of a message a recovery screen can show an operator. Set well
// under the SQLite ceiling, matching the shape (not the exact value) of
// blastRestoreMaxIDs (handlers_blast_radius_restore.go): a request this
// large is already far beyond anything a checkbox-driven UI would submit in
// one call, so refusing early costs nothing real.
const reopenMaxIDs = 5000

// ErrTooManyReopenIDs is returned by ReopenCollisionViolations when the
// caller passes more than reopenMaxIDs IDs in one call.
var ErrTooManyReopenIDs = errors.New("too many violation ids in one reopen request")

// ReopenOutcome reports what happened to one requested violation ID.
type ReopenOutcome struct {
	ID       string
	Reopened bool
	// Reason is empty when Reopened is true; otherwise one of the
	// ReopenReason* constants.
	Reason string
}

// ReopenCollisionViolations durably reopens the given
// cross_artist_backdrop_collision violations, by explicit ID list only.
//
// This is the scoped counterpart to ReopenViolation (service.go), which is
// reachable only inside the 30-second undo window via handleUndoFix and so
// cannot serve a durable, operator-initiated recovery flow (#2967).
//
// The UPDATE clause is a POSITIVE ALLOW-LIST -- id IN (...) AND rule_id = ?
// AND status = 'resolved' -- so it structurally refuses a dismissed row, an
// already-open row, a row for any other rule, and any row not in the
// caller's list. There is no "reopen everything resolved" mode: the whole
// point of shipping a report instead of a migration is that reopening stays
// an explicit, per-row operator decision.
//
// Runs inside a transaction so the per-ID reason codes returned describe
// exactly what the UPDATE did, not a stale read taken before or after it. The
// outcomes are additionally verified against RowsAffected before the
// transaction commits: if fewer rows were updated than the Go pre-check
// judged eligible, that means a row changed state between the SELECT and the
// UPDATE (or a bug removed one of the UPDATE's allow-list clauses), and this
// method returns an error rather than reporting a Reopened: true it cannot
// back up.
//
// Duplicate input IDs are deduplicated (first occurrence kept, order
// preserved) before any query runs, so passing the same ID twice returns one
// outcome, not two -- a caller counting Reopened: true outcomes must not be
// able to inflate a count by repeating an ID.
//
// Limitation: cleanupDisabledRuleState (service.go) resolves BOTH open and
// pending_choice violations in one UPDATE, so in principle the resolved
// population this method can reopen could include rows that were parked in
// pending_choice (a candidate awaiting an operator's pick) at the moment they
// were bulk-resolved. In practice that never happens for THIS rule:
// pending_choice is set only by the evaluation pipeline (fixer.go,
// processManualViolation / processAutoFixViolation), and
// cross_artist_backdrop_collision is event-driven with a registered checker
// that always returns nil (engine.go), so the pipeline never persists a
// status for it -- the sole production writer, RaiseBackdropCollision, always
// passes ViolationStatusOpen. If that ever changed, the original pre-resolve
// status is not recorded anywhere on the row, so there would be no way to
// tell which resolved rows were pending_choice versus open, and reopening
// always lands a row at ViolationStatusOpen -- never back at pending_choice.
func (s *Service) ReopenCollisionViolations(ctx context.Context, ids []string) ([]ReopenOutcome, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		// Returning early rather than building an empty IN () clause, which
		// is a SQLite syntax error (mirrors the guard in
		// ClearResolvedViolations).
		return nil, nil
	}
	if len(ids) > reopenMaxIDs {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrTooManyReopenIDs, len(ids), reopenMaxIDs)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning reopen-collision transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after commit success is a no-op; on error path the original error is what callers act on

	placeholders := make([]string, len(ids))
	selectArgs := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		selectArgs[i] = id
	}
	inClause := strings.Join(placeholders, ", ")

	//nolint:gosec // G202: only "?" placeholders are interpolated; every value is parameterized
	selectQuery := fmt.Sprintf(
		`SELECT id, rule_id, status FROM rule_violations WHERE id IN (%s)`,
		inClause,
	)
	// queryCandidates is an inline function so defer rows.Close() fires the
	// moment the scan is done, satisfying sqlclosecheck, rather than living
	// past it inside the outer function body.
	found := make(map[string]struct{ ruleID, status string })
	queryErr := func() error {
		rows, err := tx.QueryContext(ctx, selectQuery, selectArgs...)
		if err != nil {
			return fmt.Errorf("reading candidate collision violations: %w", err)
		}
		defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup
		for rows.Next() {
			var id, ruleID, status string
			if err := rows.Scan(&id, &ruleID, &status); err != nil {
				return fmt.Errorf("scanning candidate collision violation: %w", err)
			}
			found[id] = struct{ ruleID, status string }{ruleID, status}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating candidate collision violations: %w", err)
		}
		return nil
	}()
	if queryErr != nil {
		return nil, queryErr
	}

	// Determine, per input ID, whether it is eligible before issuing the
	// UPDATE -- this is what lets the reason codes below match reality rather
	// than being inferred after the fact from a second read.
	eligible := make([]string, 0, len(ids))
	outcomes := make([]ReopenOutcome, len(ids))
	for i, id := range ids {
		row, ok := found[id]
		switch {
		case !ok:
			outcomes[i] = ReopenOutcome{ID: id, Reason: ReopenReasonNotFound}
		case row.ruleID != RuleCrossArtistBackdropCollision:
			outcomes[i] = ReopenOutcome{ID: id, Reason: ReopenReasonWrongRule}
		case row.status == ViolationStatusDismissed:
			outcomes[i] = ReopenOutcome{ID: id, Reason: ReopenReasonDismissed}
		case row.status != ViolationStatusResolved:
			outcomes[i] = ReopenOutcome{ID: id, Reason: ReopenReasonNotResolved}
		default:
			outcomes[i] = ReopenOutcome{ID: id, Reopened: true}
			eligible = append(eligible, id)
		}
	}

	if s.reopenPreUpdateHook != nil {
		s.reopenPreUpdateHook(tx)
	}

	if len(eligible) > 0 {
		updatePlaceholders := make([]string, len(eligible))
		updateArgs := make([]any, 0, len(eligible)+4)
		now := s.clock.Now().UTC().Format(time.RFC3339)
		updateArgs = append(updateArgs, ViolationStatusOpen, now)
		for i, id := range eligible {
			updatePlaceholders[i] = "?"
			updateArgs = append(updateArgs, id)
		}
		updateArgs = append(updateArgs, RuleCrossArtistBackdropCollision, ViolationStatusResolved)

		//nolint:gosec // G202: only "?" placeholders are interpolated; every value is parameterized
		updateQuery := fmt.Sprintf(
			`UPDATE rule_violations SET status = ?, resolved_at = NULL, updated_at = ?
			 WHERE id IN (%s) AND rule_id = ? AND status = ?`,
			strings.Join(updatePlaceholders, ", "),
		)
		res, err := tx.ExecContext(ctx, updateQuery, updateArgs...)
		if err != nil {
			return nil, fmt.Errorf("reopening collision violations: %w", err)
		}
		// Reconcile against the Go pre-check: if the UPDATE's own allow-list
		// clauses (rule_id, status) ever disagree with what the SELECT above
		// judged eligible -- a row's status changed between the two reads, or
		// a future edit drops one of those clauses -- fewer rows are affected
		// than eligible. The outcomes slice above already marked every
		// eligible ID Reopened: true before this UPDATE ran, so a silent
		// mismatch here would report a reopen that never happened. Fail loudly
		// and roll back (via the deferred tx.Rollback()) instead.
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("reopening collision violations (rows affected): %w", err)
		}
		if int(affected) != len(eligible) {
			return nil, fmt.Errorf("reopening collision violations: expected to update %d rows, updated %d -- refusing to report an unverified outcome",
				len(eligible), affected)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing reopen-collision transaction: %w", err)
	}

	return outcomes, nil
}

// dedupeIDs returns ids with duplicates removed, keeping the first occurrence
// of each and preserving input order, so the returned outcome list stays
// deterministic and positionally meaningful to a caller that dedupes its own
// display list the same way. Unlike dedupeChangeIDs
// (handlers_blast_radius_restore.go) this does NOT drop empty strings: an
// empty ID is a legitimate (if useless) input that must still produce its own
// not_found outcome, not silently vanish from the result.
func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
