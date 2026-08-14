package rule

import (
	"context"
	"database/sql"
	"fmt"
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
// fixer). This file adds detection (advisory only) -- a read-only report of
// candidate rows for a human to judge. An explicit, ID-scoped reopen of those
// rows is a separate follow-on (#2967 PR 2), never an automatic migration,
// because a migration runs unattended on every install and a false positive
// would re-arm a destructive back-out fixer against a row an operator
// deliberately closed.

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
