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
	defer rows.Close() //nolint:errcheck

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
	defer rows.Close() //nolint:errcheck

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
	defer rows.Close() //nolint:errcheck

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
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
