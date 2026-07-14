package rule

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// nullableString turns an empty string into a SQL NULL so nullable TEXT
// columns (violation_id, violation_message) do not round-trip as "".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ruleResultUpsertExecer is the minimum surface required to upsert a
// rule_results row. Both *sql.DB and *sql.Tx satisfy it, which lets
// UpsertRuleResultFail run inside a caller's existing transaction (see
// Service.UpsertViolation) without duplicating the SQL.
type ruleResultUpsertExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpsertRuleResultPass records that the given artist currently satisfies the
// given rule. The row is keyed by (artist_id, rule_id); on conflict we flip
// passed to 1, clear violation_id / violation_message / first_failed_at
// (the failure has been resolved), and bump last_passed_at to evaluatedAt so
// callers can see when the most recent successful evaluation happened.
//
// evaluatedAt is taken from the caller (typically the start timestamp of a
// pipeline pass or health-subscriber evaluation) so every row written during
// one artist evaluation shares a timestamp; that makes it cheap to find all
// rows touched by a single pass without joining on a separate run table.
func (s *Service) UpsertRuleResultPass(ctx context.Context, artistID, ruleID string, evaluatedAt time.Time) error {
	ts := evaluatedAt.UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rule_results (
			artist_id, rule_id, passed, violation_id, evaluated_at,
			violation_message, first_failed_at, last_passed_at
		) VALUES (?, ?, 1, NULL, ?, NULL, NULL, ?)
		ON CONFLICT(artist_id, rule_id) DO UPDATE SET
			passed            = 1,
			violation_id      = NULL,
			evaluated_at      = excluded.evaluated_at,
			violation_message = NULL,
			first_failed_at   = NULL,
			last_passed_at    = excluded.last_passed_at
	`, artistID, ruleID, ts, ts)
	if err != nil {
		return fmt.Errorf("upserting rule_result pass: %w", err)
	}
	return nil
}

// UpsertRuleResultFail records that the given artist currently violates the
// given rule. violationID is the rule_violations.id from the paired upsert
// so reports can join back to the violation row. first_failed_at is
// preserved across repeated failures via COALESCE(existing, excluded) so
// the "how long has this been broken" signal survives repeated evaluations.
//
// Accepts any execer (DB or Tx) so Service.UpsertViolation can invoke it
// inside its transaction and keep the violation + result writes atomic.
func upsertRuleResultFailExec(
	ctx context.Context,
	exec ruleResultUpsertExecer,
	artistID, ruleID, violationID, message string,
	evaluatedAt time.Time,
) error {
	ts := evaluatedAt.UTC().Format(time.RFC3339)
	_, err := exec.ExecContext(ctx, `
		INSERT INTO rule_results (
			artist_id, rule_id, passed, violation_id, evaluated_at,
			violation_message, first_failed_at, last_passed_at
		) VALUES (?, ?, 0, ?, ?, ?, ?, NULL)
		ON CONFLICT(artist_id, rule_id) DO UPDATE SET
			passed            = 0,
			violation_id      = excluded.violation_id,
			evaluated_at      = excluded.evaluated_at,
			violation_message = excluded.violation_message,
			first_failed_at   = COALESCE(rule_results.first_failed_at, excluded.first_failed_at)
	`, artistID, ruleID, nullableString(violationID), ts, nullableString(message), ts)
	if err != nil {
		return fmt.Errorf("upserting rule_result fail: %w", err)
	}
	return nil
}

// UpsertRuleResultFail is the package-level entry point used by callers that
// do not already hold a transaction. Internal transactional callers (like
// Service.UpsertViolation) use upsertRuleResultFailExec directly with their
// *sql.Tx so both writes land atomically.
func (s *Service) UpsertRuleResultFail(
	ctx context.Context,
	artistID, ruleID, violationID, message string,
	evaluatedAt time.Time,
) error {
	return upsertRuleResultFailExec(ctx, s.db, artistID, ruleID, violationID, message, evaluatedAt)
}

// DeleteRuleResultsForRule removes every stored result for the given rule.
// Called when a rule is disabled (Service.Update) so the per-rule dashboard
// does not keep surfacing stale pass/fail rows for a rule that no longer
// runs. A full delete (not status-filtered like rule_violations) is correct:
// the rule is inactive, so no row is authoritative until it runs again.
func (s *Service) DeleteRuleResultsForRule(ctx context.Context, ruleID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rule_results WHERE rule_id = ?`, ruleID); err != nil {
		return fmt.Errorf("deleting rule_results for rule: %w", err)
	}
	return nil
}

// RetractRuleVerdict withdraws the stored verdicts a skipped rule has no right to
// keep for one (artist, rule) pair: the rule_results row, and an OPEN
// rule_violations row. It reports whether anything was actually withdrawn.
//
// It deliberately does NOT touch a violation awaiting an operator decision
// (pending_choice), nor one the operator dismissed. See the note below.
//
// This is the retraction half of the capability gate (#2509). When a rule is
// SKIPPED for an artist -- it cannot be evaluated because the artist lacks the
// data the rule needs -- any row left behind from an earlier evaluation is a
// claim the code can no longer stand behind. Declining to write a new row is
// not enough: the old rows survive. A stale rule_results row keeps every reader
// that does not consult the capability gate (the artist rule-result breakdown,
// the compliance grid, the per-rule pass-rate dashboards) reporting a rule that
// never examined the artist as a genuine outcome. A stale OPEN violation is
// worse: persistPassResults only resolves violations for rules in
// RulesConsidered, and a skipped rule is never in that set, so the finding keeps
// counting against compliance and showing in "needs attention" with no
// evaluation left that could ever clear it. Both must go, and both must go in
// the same place, or the artist keeps a verdict in one table after losing it in
// the other. A DISMISSED violation is left alone: dismissal is the operator's
// terminal decision (#1107), not a verdict this code made.
//
// A PENDING_CHOICE violation is left alone for the same reason, and this is why
// retraction resolves only OPEN violations rather than reusing
// ResolveViolationIfActive (which clears both). pending_choice means an image
// violation is parked awaiting the OPERATOR's decision on which candidate to keep
// (see the fixer's manual-choice paths). Auto-resolving it on a transient
// capability loss -- an artist whose hashes were cleared by a rescan, say --
// destroys a queued human decision to save a stale row, and automation must never
// overrule a human decision. The row stays; the next evaluation that CAN run the
// rule either re-raises the finding or resolves it honestly.
//
// Retracting nothing is not an error: retraction runs on every evaluation of a
// skipped rule, so on all but the first pass there is nothing left to withdraw.
// That steady state is the common one -- a library with many API-only artists
// re-walks the same skipped rules on every incremental pass -- so the existence
// check comes first and the writes are issued only when there is something to
// write. An unconditional DELETE plus UPDATE would open two write transactions
// per skipped rule per pass against SQLite's single writer, matching zero rows
// every time. A row that appears between the check and the writes is simply
// retracted by the next pass; retraction is idempotent.
func (s *Service) RetractRuleVerdict(ctx context.Context, artistID, ruleID string) (bool, error) {
	var hasResult, hasOpenViolation int
	err := s.db.QueryRowContext(ctx, `
		SELECT
			EXISTS(SELECT 1 FROM rule_results
			        WHERE artist_id = ? AND rule_id = ?),
			EXISTS(SELECT 1 FROM rule_violations
			        WHERE artist_id = ? AND rule_id = ? AND status = ?)
	`, artistID, ruleID, artistID, ruleID,
		ViolationStatusOpen).Scan(&hasResult, &hasOpenViolation)
	if err != nil {
		return false, fmt.Errorf("checking stored verdict for skipped rule: %w", err)
	}
	if hasResult == 0 && hasOpenViolation == 0 {
		return false, nil
	}

	if hasResult != 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
			artistID, ruleID); err != nil {
			return false, fmt.Errorf("deleting rule_result for skipped rule: %w", err)
		}
	}
	if hasOpenViolation != 0 {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := s.db.ExecContext(ctx, `
			UPDATE rule_violations
			   SET status = ?, resolved_at = ?, updated_at = ?
			 WHERE artist_id = ? AND rule_id = ? AND status = ?
		`, ViolationStatusResolved, now, now, artistID, ruleID, ViolationStatusOpen); err != nil {
			return false, fmt.Errorf("resolving open violation for skipped rule: %w", err)
		}
	}
	return true, nil
}

// GetRuleResultsForArtist returns every persisted rule outcome for a single
// artist. Rows are ordered by rule_id so callers can render a stable list.
func (s *Service) GetRuleResultsForArtist(ctx context.Context, artistID string) ([]RuleResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT artist_id, rule_id, passed, violation_id, evaluated_at,
		       violation_message, first_failed_at, last_passed_at
		FROM rule_results
		WHERE artist_id = ?
		ORDER BY rule_id
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("querying rule_results for artist: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var results []RuleResult
	for rows.Next() {
		var (
			rr               RuleResult
			passedInt        int
			violationID      sql.NullString
			evaluatedAt      string
			violationMessage sql.NullString
			firstFailedAt    sql.NullString
			lastPassedAt     sql.NullString
		)
		if err := rows.Scan(&rr.ArtistID, &rr.RuleID, &passedInt, &violationID,
			&evaluatedAt, &violationMessage, &firstFailedAt, &lastPassedAt); err != nil {
			return nil, fmt.Errorf("scanning rule_result row: %w", err)
		}
		rr.Passed = passedInt != 0
		if violationID.Valid {
			rr.ViolationID = violationID.String
		}
		if violationMessage.Valid {
			rr.ViolationMessage = violationMessage.String
		}
		if ts, ok := parseNullableTime(evaluatedAt); ok {
			rr.EvaluatedAt = ts
		}
		if firstFailedAt.Valid {
			if ts, ok := parseNullableTime(firstFailedAt.String); ok {
				rr.FirstFailedAt = &ts
			}
		}
		if lastPassedAt.Valid {
			if ts, ok := parseNullableTime(lastPassedAt.String); ok {
				rr.LastPassedAt = &ts
			}
		}
		results = append(results, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rule_result rows: %w", err)
	}
	return results, nil
}

// CountRuleResultsByRule returns per-rule pass / fail counts across every
// artist. Intended for the rule-results dashboard (slice 2) and for the
// TestCountRuleResultsByRule unit test that asserts the aggregate counts.
func (s *Service) CountRuleResultsByRule(ctx context.Context) (map[string]RuleResultCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rule_id, passed, COUNT(*)
		FROM rule_results
		GROUP BY rule_id, passed
	`)
	if err != nil {
		return nil, fmt.Errorf("counting rule_results by rule: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	counts := map[string]RuleResultCount{}
	for rows.Next() {
		var (
			ruleID    string
			passedInt int
			count     int
		)
		if err := rows.Scan(&ruleID, &passedInt, &count); err != nil {
			return nil, fmt.Errorf("scanning rule_results count row: %w", err)
		}
		c := counts[ruleID]
		c.Evaluated += count
		if passedInt != 0 {
			c.Passed += count
		}
		counts[ruleID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rule_results count rows: %w", err)
	}
	return counts, nil
}

// GetRuleResultCounts returns per-artist pass / evaluated counts for the
// given artist IDs. Empty input returns an empty map. IDs are processed in
// chunks of 500 so the per-query parameter count stays well under SQLite's
// 999-parameter limit, mirroring GetViolationsForArtists.
func (s *Service) GetRuleResultCounts(ctx context.Context, artistIDs []string) (map[string]RuleResultCount, error) {
	if len(artistIDs) == 0 {
		return map[string]RuleResultCount{}, nil
	}

	result := make(map[string]RuleResultCount, len(artistIDs))
	const chunkSize = 500

	for i := 0; i < len(artistIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(artistIDs) {
			end = len(artistIDs)
		}
		if err := s.queryRuleResultCountsChunk(ctx, artistIDs[i:end], result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// queryRuleResultCountsChunk executes a single chunk of artist IDs and
// merges its aggregated counts into dest.
func (s *Service) queryRuleResultCountsChunk(ctx context.Context, chunk []string, dest map[string]RuleResultCount) error {
	placeholders := make([]string, len(chunk))
	args := make([]any, 0, len(chunk))
	for j, id := range chunk {
		placeholders[j] = "?"
		args = append(args, id)
	}

	//nolint:gosec // G202: only "?" placeholders concatenated, no user input
	query := `SELECT artist_id, passed, COUNT(*)
		FROM rule_results
		WHERE artist_id IN (` + strings.Join(placeholders, ",") + `)
		GROUP BY artist_id, passed`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying rule_result counts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	for rows.Next() {
		var (
			artistID  string
			passedInt int
			count     int
		)
		if err := rows.Scan(&artistID, &passedInt, &count); err != nil {
			return fmt.Errorf("scanning rule_result count row: %w", err)
		}
		c := dest[artistID]
		c.Evaluated += count
		if passedInt != 0 {
			c.Passed += count
		}
		dest[artistID] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating rule_result count rows: %w", err)
	}
	return nil
}

// PassedFilter is the tri-state filter for the per-rule drill-down list.
// Any -> no WHERE clause on passed.
// Passed -> WHERE passed = 1.
// Failed -> WHERE passed = 0.
// Strongly-typed so the handler layer can't accidentally pass a stray
// integer that would break the SQL.
type PassedFilter int

// PassedFilter values for the per-rule drill-down list. Any means no
// WHERE clause on passed; Passed and Failed restrict to passed=1 / =0
// respectively.
const (
	PassedFilterAny PassedFilter = iota
	PassedFilterPassed
	PassedFilterFailed
)

// passedWhere returns the AND-prefixed WHERE-clause fragment for the
// given filter. Returns "" for Any so callers don't have to AND in a
// no-op. Keeping the predicate construction here means every consumer of
// PassedFilter applies the same SQL convention. The fragment never takes
// a bind parameter, so callers append the empty string directly to their
// query template.
func passedWhere(f PassedFilter) string {
	switch f {
	case PassedFilterPassed:
		return " AND rr.passed = 1"
	case PassedFilterFailed:
		return " AND rr.passed = 0"
	default:
		return ""
	}
}

// GetRuleResultsForRule returns a paginated list of artist-joined
// rule_results rows for the given rule, ordered by artist name so the UI
// is stable across calls. Limit must be > 0; offset must be >= 0; both
// are caller-validated (the handler clamps to sensible bounds via
// intQuery before calling).
//
// The query JOINs to artists for display name and is_excluded; LEFT JOINs
// artist_libraries + libraries so an artist that is not in any library
// still appears (library_name is empty in that case). Excluded artists
// are NOT filtered out -- the row carries is_excluded so the UI can render
// a "(excluded)" badge, which matches the per-artist drill-down behavior
// and lets reviewers explicitly see which excluded artists failed.
func (s *Service) GetRuleResultsForRule(
	ctx context.Context,
	ruleID string,
	filter PassedFilter,
	limit, offset int,
) ([]RuleResultWithArtist, error) {
	extraWhere := passedWhere(filter)
	//nolint:gosec // G202: only the constant filter fragment is concatenated; no user input.
	query := `
		SELECT rr.artist_id, rr.rule_id, rr.passed, rr.violation_id,
		       rr.evaluated_at, rr.violation_message,
		       rr.first_failed_at, rr.last_passed_at,
		       a.name, a.is_excluded,
		       COALESCE(MIN(l.name), '') AS library_name
		FROM rule_results rr
		JOIN artists a ON a.id = rr.artist_id
		LEFT JOIN artist_libraries al
		     ON al.artist_id = a.id
		LEFT JOIN libraries l
		     ON l.id = al.library_id
		WHERE rr.rule_id = ?` + extraWhere + `
		GROUP BY rr.artist_id
		ORDER BY a.name ASC, rr.artist_id ASC
		LIMIT ? OFFSET ?`

	rows, err := s.db.QueryContext(ctx, query, ruleID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("querying rule_results drill-down: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []RuleResultWithArtist
	for rows.Next() {
		var (
			rr               RuleResultWithArtist
			passedInt        int
			isExcludedInt    int
			violationID      sql.NullString
			evaluatedAt      string
			violationMessage sql.NullString
			firstFailedAt    sql.NullString
			lastPassedAt     sql.NullString
		)
		if err := rows.Scan(
			&rr.ArtistID, &rr.RuleID, &passedInt, &violationID,
			&evaluatedAt, &violationMessage, &firstFailedAt, &lastPassedAt,
			&rr.ArtistName, &isExcludedInt, &rr.LibraryName,
		); err != nil {
			return nil, fmt.Errorf("scanning rule_results drill-down row: %w", err)
		}
		rr.Passed = passedInt != 0
		rr.IsExcluded = isExcludedInt != 0
		if violationID.Valid {
			rr.ViolationID = violationID.String
		}
		if violationMessage.Valid {
			rr.ViolationMessage = violationMessage.String
		}
		if ts, ok := parseNullableTime(evaluatedAt); ok {
			rr.EvaluatedAt = ts
		}
		if firstFailedAt.Valid {
			if ts, ok := parseNullableTime(firstFailedAt.String); ok {
				rr.FirstFailedAt = &ts
			}
		}
		if lastPassedAt.Valid {
			if ts, ok := parseNullableTime(lastPassedAt.String); ok {
				rr.LastPassedAt = &ts
			}
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rule_results drill-down rows: %w", err)
	}
	return out, nil
}

// CountRuleResultsForRule pairs with GetRuleResultsForRule; the handler
// emits both as {rows, total} so the front-end can render pagination
// without a second round-trip.
func (s *Service) CountRuleResultsForRule(ctx context.Context, ruleID string, filter PassedFilter) (int, error) {
	// Mirror the INNER JOIN artists in GetRuleResultsForRule so the total
	// excludes orphaned rule_results rows whose artist has been hard-deleted;
	// otherwise the paginated rows can never sum to the reported total and
	// the UI's page math breaks.
	query := `SELECT COUNT(*) FROM rule_results rr JOIN artists a ON a.id = rr.artist_id WHERE rr.rule_id = ?` + passedWhere(filter)
	var n int
	if err := s.db.QueryRowContext(ctx, query, ruleID).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting rule_results for rule: %w", err)
	}
	return n, nil
}

// GetEnabledRuleResultsForArtist returns one row per enabled rule with the
// artist's current pass/fail state for that rule. Rules without any
// rule_results row for this artist are omitted (the artist has not been
// evaluated against that rule yet); rules that are disabled at query time
// are filtered out so the artist detail page only shows rules the user is
// currently asking the engine to enforce.
//
// Rows are ordered by rule_id so the breakdown is stable across calls.
// Severity is pulled from the rule's JSON config (rules.config has no
// dedicated severity column; the rule engine decodes it via RuleConfig
// on the Go side). The COALESCE falls back to "warning" so the UI's
// color-coding always has a valid value even for legacy rules whose
// config did not specify a severity. modernc.org/sqlite ships JSON1
// built in, so json_extract is safe to use without a driver flag.
func (s *Service) GetEnabledRuleResultsForArtist(ctx context.Context, artistID string) ([]RuleResultWithRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rr.artist_id, rr.rule_id, rr.passed, rr.violation_id,
		       rr.evaluated_at, rr.violation_message,
		       rr.first_failed_at, rr.last_passed_at,
		       r.name, r.category,
		       COALESCE(json_extract(r.config, '$.severity'), 'warning') AS severity
		FROM rule_results rr
		JOIN rules r ON r.id = rr.rule_id AND r.enabled = 1
		WHERE rr.artist_id = ?
		ORDER BY rr.rule_id ASC`,
		artistID)
	if err != nil {
		return nil, fmt.Errorf("querying enabled rule_results for artist: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []RuleResultWithRule
	for rows.Next() {
		var (
			rr               RuleResultWithRule
			passedInt        int
			violationID      sql.NullString
			evaluatedAt      string
			violationMessage sql.NullString
			firstFailedAt    sql.NullString
			lastPassedAt     sql.NullString
		)
		if err := rows.Scan(
			&rr.ArtistID, &rr.RuleID, &passedInt, &violationID,
			&evaluatedAt, &violationMessage, &firstFailedAt, &lastPassedAt,
			&rr.RuleName, &rr.RuleCategory, &rr.Severity,
		); err != nil {
			return nil, fmt.Errorf("scanning enabled rule_result row: %w", err)
		}
		rr.Passed = passedInt != 0
		if violationID.Valid {
			rr.ViolationID = violationID.String
		}
		if violationMessage.Valid {
			rr.ViolationMessage = violationMessage.String
		}
		if ts, ok := parseNullableTime(evaluatedAt); ok {
			rr.EvaluatedAt = ts
		}
		if firstFailedAt.Valid {
			if ts, ok := parseNullableTime(firstFailedAt.String); ok {
				rr.FirstFailedAt = &ts
			}
		}
		if lastPassedAt.Valid {
			if ts, ok := parseNullableTime(lastPassedAt.String); ok {
				rr.LastPassedAt = &ts
			}
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating enabled rule_result rows: %w", err)
	}
	return out, nil
}

// TopFailingRuleResults is the rule_results-based replacement for
// TopViolationSummaries. It returns the rules with the most failing
// (passed=0) results across non-excluded artists, ordered by failure
// count descending and rule_id ascending as a stable tie-breaker. The
// caller-passed limit is clamped to [1, 100]: zero or negative becomes
// 10 (the dashboard default), and very large requests are capped to
// keep the response shape predictable.
//
// Returns the same ViolationSummary shape as TopViolationSummaries so the
// handler can swap one call site for the other without touching the JSON
// envelope or the health_summary templ widget. Severity is sourced via
// json_extract on rules.config (rules has no severity column; the COALESCE
// falls back to 'warning' to match the existing UI convention).
func (s *Service) TopFailingRuleResults(ctx context.Context, limit int) ([]ViolationSummary, error) {
	switch {
	case limit <= 0:
		limit = 10
	case limit > 100:
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT rr.rule_id,
		       r.name,
		       COUNT(*) AS cnt,
		       COALESCE(json_extract(r.config, '$.severity'), 'warning') AS severity
		FROM rule_results rr
		JOIN rules r   ON r.id = rr.rule_id AND r.enabled = 1
		JOIN artists a ON a.id = rr.artist_id AND a.is_excluded = 0
		WHERE rr.passed = 0
		GROUP BY rr.rule_id
		ORDER BY cnt DESC, rr.rule_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying top failing rule_results: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []ViolationSummary
	for rows.Next() {
		var vs ViolationSummary
		if err := rows.Scan(&vs.RuleID, &vs.RuleName, &vs.Count, &vs.Severity); err != nil {
			return nil, fmt.Errorf("scanning top failing rule_result row: %w", err)
		}
		out = append(out, vs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating top failing rule_result rows: %w", err)
	}
	return out, nil
}

// GetRulePassRates returns one row per enabled rule with aggregate pass /
// fail counts and a 0.0-1.0 PassRate. Ordered by PassRate ascending then
// rule_id ascending so the dashboard widget shows the worst-performing
// rules first (matches the TopViolations card's "biggest problems on top"
// intent). Rules with no rule_results rows are omitted because the COUNT
// runs on the rule_results table; the widget treats absent rules as "no
// data yet" rather than rendering them at 0% (which would imply 100%
// failing). Excluded artists are filtered out so the rates match the
// dashboard's other counts.
//
// Severity is sourced from rules.config JSON via json_extract, falling
// back to 'warning' when the config has no severity field. This matches
// the source-of-truth pattern used in TopFailingRuleResults (rules has
// no dedicated severity column; the rule engine reads it as
// RuleConfig.Severity on the Go side).
func (s *Service) GetRulePassRates(ctx context.Context) ([]RulePassRate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rr.rule_id,
		       r.name,
		       COALESCE(json_extract(r.config, '$.severity'), 'warning') AS severity,
		       SUM(CASE WHEN rr.passed = 1 THEN 1 ELSE 0 END) AS passed,
		       SUM(CASE WHEN rr.passed = 0 THEN 1 ELSE 0 END) AS failed,
		       COUNT(*) AS evaluated
		FROM rule_results rr
		JOIN rules r   ON r.id = rr.rule_id AND r.enabled = 1
		JOIN artists a ON a.id = rr.artist_id AND a.is_excluded = 0
		GROUP BY rr.rule_id
		ORDER BY (CAST(SUM(CASE WHEN rr.passed = 1 THEN 1 ELSE 0 END) AS REAL) / COUNT(*)) ASC,
		         rr.rule_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("querying rule pass rates: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []RulePassRate
	for rows.Next() {
		var pr RulePassRate
		if err := rows.Scan(&pr.RuleID, &pr.RuleName, &pr.Severity, &pr.Passed, &pr.Failed, &pr.Evaluated); err != nil {
			return nil, fmt.Errorf("scanning rule pass rate row: %w", err)
		}
		if pr.Evaluated > 0 {
			pr.PassRate = float64(pr.Passed) / float64(pr.Evaluated)
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rule pass rate rows: %w", err)
	}
	return out, nil
}

// parseNullableTime parses a stored RFC3339 timestamp, returning false if the
// input is empty or unparsable. Kept here (rather than pulling dbutil) so
// the rule_results accessor doesn't bleed its time-format assumption into
// every caller.
func parseNullableTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// SQLite's datetime('now') default produces "YYYY-MM-DD HH:MM:SS"
	// (no timezone). Accept that form too so backfill rows stay readable.
	if t, err := time.Parse(time.DateTime, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
