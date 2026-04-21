package rule

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// seedArtistAndRule inserts a minimal artist + rule pair for rule_results
// tests. rule_results has FK references to both; the rule row also has to
// exist for the FK on rule_violations (which the UpsertViolation tx needs).
//
//nolint:unparam // artistID/ruleID vary across call sites in other tests.
func seedArtistAndRule(t *testing.T, db *sql.DB, artistID, ruleID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
		artistID, "Artist "+artistID); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
		VALUES (?, ?, 'desc', 'nfo', 1, 'auto', '{}')`,
		ruleID, "Rule "+ruleID); err != nil {
		t.Fatalf("inserting rule: %v", err)
	}
}

// readResultRow reads the stored rule_results row for (artistID, ruleID).
// Returns ok=false when no row exists so tests can assert presence/absence
// without nil-pointer pitfalls.
type storedResult struct {
	passed        int
	violationID   sql.NullString
	evaluatedAt   string
	message       sql.NullString
	firstFailedAt sql.NullString
	lastPassedAt  sql.NullString
}

//nolint:unparam // artistID/ruleID vary across call sites in other tests.
func readResultRow(t *testing.T, db *sql.DB, artistID, ruleID string) (storedResult, bool) {
	t.Helper()
	var r storedResult
	err := db.QueryRowContext(context.Background(), `
		SELECT passed, violation_id, evaluated_at, violation_message,
		       first_failed_at, last_passed_at
		FROM rule_results
		WHERE artist_id = ? AND rule_id = ?`,
		artistID, ruleID).Scan(
		&r.passed, &r.violationID, &r.evaluatedAt,
		&r.message, &r.firstFailedAt, &r.lastPassedAt)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		t.Fatalf("reading rule_results row: %v", err)
	}
	return r, true
}

func TestUpsertRuleResultPass_Insert(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedArtistAndRule(t, db, "a1", "r1")
	now := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)

	if err := svc.UpsertRuleResultPass(ctx, "a1", "r1", now); err != nil {
		t.Fatalf("UpsertRuleResultPass: %v", err)
	}

	row, ok := readResultRow(t, db, "a1", "r1")
	if !ok {
		t.Fatalf("expected rule_results row, got none")
	}
	if row.passed != 1 {
		t.Errorf("passed = %d, want 1", row.passed)
	}
	if row.violationID.Valid {
		t.Errorf("violation_id = %q, want NULL", row.violationID.String)
	}
	if row.message.Valid {
		t.Errorf("violation_message = %q, want NULL", row.message.String)
	}
	if row.firstFailedAt.Valid {
		t.Errorf("first_failed_at = %q, want NULL", row.firstFailedAt.String)
	}
	if !row.lastPassedAt.Valid {
		t.Errorf("last_passed_at = NULL, want set on first pass")
	}
}

func TestUpsertRuleResultPass_FailToPass(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedArtistAndRule(t, db, "a1", "r1")
	// Seed an initial fail row so the pass upsert must flip it.
	failAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := svc.UpsertRuleResultFail(ctx, "a1", "r1", "v1", "bad", failAt); err != nil {
		t.Fatalf("seeding fail: %v", err)
	}

	passAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	if err := svc.UpsertRuleResultPass(ctx, "a1", "r1", passAt); err != nil {
		t.Fatalf("UpsertRuleResultPass: %v", err)
	}

	row, ok := readResultRow(t, db, "a1", "r1")
	if !ok {
		t.Fatalf("expected rule_results row, got none")
	}
	if row.passed != 1 {
		t.Errorf("passed = %d, want 1", row.passed)
	}
	if row.firstFailedAt.Valid {
		t.Errorf("first_failed_at = %q, want NULL after fail-to-pass", row.firstFailedAt.String)
	}
	if row.violationID.Valid {
		t.Errorf("violation_id = %q, want NULL after pass", row.violationID.String)
	}
	if !row.lastPassedAt.Valid {
		t.Errorf("last_passed_at = NULL, want set after fail-to-pass")
	}
}

func TestUpsertRuleResultFail_PassToFail(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedArtistAndRule(t, db, "a1", "r1")
	passAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := svc.UpsertRuleResultPass(ctx, "a1", "r1", passAt); err != nil {
		t.Fatalf("seeding pass: %v", err)
	}

	// Seed a violation row so the violation_id FK resolves.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status)
		VALUES ('v1', 'r1', 'a1', 'Artist a1', 'warning', 'bad', 0, 'open')`); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	failAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	if err := svc.UpsertRuleResultFail(ctx, "a1", "r1", "v1", "bad", failAt); err != nil {
		t.Fatalf("UpsertRuleResultFail: %v", err)
	}

	row, ok := readResultRow(t, db, "a1", "r1")
	if !ok {
		t.Fatalf("expected rule_results row, got none")
	}
	if row.passed != 0 {
		t.Errorf("passed = %d, want 0", row.passed)
	}
	if !row.firstFailedAt.Valid {
		t.Errorf("first_failed_at = NULL, want set on pass-to-fail transition")
	}
	wantTs := failAt.Format(time.RFC3339)
	if row.firstFailedAt.String != wantTs {
		t.Errorf("first_failed_at = %q, want %q", row.firstFailedAt.String, wantTs)
	}
	if !row.violationID.Valid || row.violationID.String != "v1" {
		t.Errorf("violation_id = %+v, want v1", row.violationID)
	}
}

func TestUpsertRuleResultFail_FailToFail(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedArtistAndRule(t, db, "a1", "r1")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status)
		VALUES ('v1', 'r1', 'a1', 'Artist a1', 'warning', 'bad', 0, 'open')`); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := svc.UpsertRuleResultFail(ctx, "a1", "r1", "v1", "still bad", first); err != nil {
		t.Fatalf("first UpsertRuleResultFail: %v", err)
	}

	// Second fail: first_failed_at must be preserved by the COALESCE.
	second := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	if err := svc.UpsertRuleResultFail(ctx, "a1", "r1", "v1", "still bad", second); err != nil {
		t.Fatalf("second UpsertRuleResultFail: %v", err)
	}

	row, ok := readResultRow(t, db, "a1", "r1")
	if !ok {
		t.Fatalf("expected rule_results row, got none")
	}
	wantFirst := first.Format(time.RFC3339)
	if row.firstFailedAt.String != wantFirst {
		t.Errorf("first_failed_at = %q, want %q (must be preserved across repeated fails)",
			row.firstFailedAt.String, wantFirst)
	}
	// evaluated_at should be the latest fail timestamp.
	wantEval := second.Format(time.RFC3339)
	if row.evaluatedAt != wantEval {
		t.Errorf("evaluated_at = %q, want %q", row.evaluatedAt, wantEval)
	}
}

func TestCountRuleResultsByRule(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Two rules, three artists, mixed outcomes.
	for _, a := range []string{"a1", "a2", "a3"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
			a, "Artist "+a); err != nil {
			t.Fatalf("inserting %s: %v", a, err)
		}
	}
	for _, r := range []string{"rA", "rB"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
			VALUES (?, ?, 'd', 'nfo', 1, 'auto', '{}')`,
			r, "Rule "+r); err != nil {
			t.Fatalf("inserting rule %s: %v", r, err)
		}
	}

	now := time.Now().UTC()
	// rA: a1 pass, a2 pass, a3 fail. Expected Passed=2, Evaluated=3.
	if err := svc.UpsertRuleResultPass(ctx, "a1", "rA", now); err != nil {
		t.Fatalf("pass a1/rA: %v", err)
	}
	if err := svc.UpsertRuleResultPass(ctx, "a2", "rA", now); err != nil {
		t.Fatalf("pass a2/rA: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status)
		VALUES ('vA3', 'rA', 'a3', 'Artist a3', 'warning', 'bad', 0, 'open')`); err != nil {
		t.Fatalf("inserting rA violation: %v", err)
	}
	if err := svc.UpsertRuleResultFail(ctx, "a3", "rA", "vA3", "bad", now); err != nil {
		t.Fatalf("fail a3/rA: %v", err)
	}
	// rB: a1 fail only. Expected Passed=0, Evaluated=1.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status)
		VALUES ('vB1', 'rB', 'a1', 'Artist a1', 'warning', 'bad', 0, 'open')`); err != nil {
		t.Fatalf("inserting rB violation: %v", err)
	}
	if err := svc.UpsertRuleResultFail(ctx, "a1", "rB", "vB1", "bad", now); err != nil {
		t.Fatalf("fail a1/rB: %v", err)
	}

	counts, err := svc.CountRuleResultsByRule(ctx)
	if err != nil {
		t.Fatalf("CountRuleResultsByRule: %v", err)
	}
	if got := counts["rA"]; got.Passed != 2 || got.Evaluated != 3 {
		t.Errorf("rA counts = %+v, want {Passed:2, Evaluated:3}", got)
	}
	if got := counts["rB"]; got.Passed != 0 || got.Evaluated != 1 {
		t.Errorf("rB counts = %+v, want {Passed:0, Evaluated:1}", got)
	}
}

func TestDeleteRuleResultsForRule(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for _, a := range []string{"a1", "a2"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`, a, "A"); err != nil {
			t.Fatalf("inserting %s: %v", a, err)
		}
	}
	for _, r := range []string{"target", "sibling"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
			VALUES (?, ?, 'd', 'nfo', 1, 'auto', '{}')`, r, r); err != nil {
			t.Fatalf("inserting rule %s: %v", r, err)
		}
	}
	now := time.Now().UTC()
	// Seed pass rows for both rules against both artists.
	for _, r := range []string{"target", "sibling"} {
		for _, a := range []string{"a1", "a2"} {
			if err := svc.UpsertRuleResultPass(ctx, a, r, now); err != nil {
				t.Fatalf("seeding %s/%s: %v", r, a, err)
			}
		}
	}

	if err := svc.DeleteRuleResultsForRule(ctx, "target"); err != nil {
		t.Fatalf("DeleteRuleResultsForRule: %v", err)
	}

	countFor := func(ruleID string) int {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM rule_results WHERE rule_id = ?`, ruleID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", ruleID, err)
		}
		return n
	}
	if got := countFor("target"); got != 0 {
		t.Errorf("target rule rows = %d, want 0", got)
	}
	if got := countFor("sibling"); got != 2 {
		t.Errorf("sibling rule rows = %d, want 2 (delete must be rule-scoped)", got)
	}
}

func TestUpsertViolation_WritesResultRowTransactionally(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedArtistAndRule(t, db, "a1", "r1")

	// Happy path: open violation upsert should write both rows atomically.
	v := &RuleViolation{
		RuleID:     "r1",
		ArtistID:   "a1",
		ArtistName: "Artist a1",
		Severity:   "warning",
		Message:    "bad",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	if err := svc.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("UpsertViolation: %v", err)
	}

	// Violation row exists.
	var vCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		"r1", "a1").Scan(&vCount); err != nil {
		t.Fatalf("counting violation rows: %v", err)
	}
	if vCount != 1 {
		t.Errorf("violation rows = %d, want 1", vCount)
	}

	// rule_results row exists and links to the violation.
	row, ok := readResultRow(t, db, "a1", "r1")
	if !ok {
		t.Fatalf("rule_results row missing after UpsertViolation")
	}
	if row.passed != 0 {
		t.Errorf("passed = %d, want 0", row.passed)
	}
	if !row.violationID.Valid || row.violationID.String != v.ID {
		t.Errorf("violation_id = %+v, want %s", row.violationID, v.ID)
	}
	if !row.message.Valid || row.message.String != "bad" {
		t.Errorf("violation_message = %+v, want 'bad'", row.message)
	}
	if !row.firstFailedAt.Valid {
		t.Errorf("first_failed_at = NULL, want set on first open violation")
	}

	// Resolved status should NOT upsert a fail row (it should leave the
	// existing row alone; the paired ResolveViolation or next pipeline
	// pass is responsible for flipping to passed=1).
	resolved := *v
	resolved.Status = ViolationStatusResolved
	now := time.Now().UTC()
	resolved.ResolvedAt = &now
	if err := svc.UpsertViolation(ctx, &resolved); err != nil {
		t.Fatalf("UpsertViolation (resolved): %v", err)
	}
	row2, ok := readResultRow(t, db, "a1", "r1")
	if !ok {
		t.Fatalf("rule_results row disappeared")
	}
	if row2.passed != 0 {
		t.Errorf("passed = %d after resolve upsert, want 0 (row should be left alone)", row2.passed)
	}
}

// TestUpsertViolation_RollsBackWhenResultWriteFails proves the atomicity
// contract behind UpsertViolation (CR comment 3114386792): if the
// rule_results write inside the same transaction fails, the sibling
// rule_violations insert must also roll back. Without the transactional
// pairing, a caller would see an "active" violation with no corresponding
// rule_results fail row, breaching the "both tables agree" invariant.
//
// We force a failure by installing a BEFORE INSERT trigger on rule_results
// that raises via SQLite's RAISE(ABORT, ...). The trigger is dropped at
// the end of the test so the shared DB remains usable for sibling cases.
// TestGetRuleResultCounts covers the compliance-report aggregator used by
// handlers_report to attach rules_passed_count / rules_evaluated_count to
// each artist row. Exercises the chunked IN-clause path with a single
// under-chunk batch, mixed pass/fail outcomes, and the empty-input fast path.
func TestGetRuleResultCounts(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Empty input returns an empty map without querying.
	empty, err := svc.GetRuleResultCounts(ctx, nil)
	if err != nil {
		t.Fatalf("GetRuleResultCounts(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("GetRuleResultCounts(nil) len = %d, want 0", len(empty))
	}

	// Two artists, two rules: a1 passes both, a2 fails one and passes the other.
	for _, a := range []string{"a1", "a2"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
			a, "Artist "+a); err != nil {
			t.Fatalf("inserting %s: %v", a, err)
		}
	}
	for _, r := range []string{"rA", "rB"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
			VALUES (?, ?, 'd', 'nfo', 1, 'auto', '{}')`, r, r); err != nil {
			t.Fatalf("inserting rule %s: %v", r, err)
		}
	}
	now := time.Now().UTC()
	if err := svc.UpsertRuleResultPass(ctx, "a1", "rA", now); err != nil {
		t.Fatalf("pass a1/rA: %v", err)
	}
	if err := svc.UpsertRuleResultPass(ctx, "a1", "rB", now); err != nil {
		t.Fatalf("pass a1/rB: %v", err)
	}
	if err := svc.UpsertRuleResultPass(ctx, "a2", "rA", now); err != nil {
		t.Fatalf("pass a2/rA: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status)
		VALUES ('v2B', 'rB', 'a2', 'Artist a2', 'warning', 'bad', 0, 'open')`); err != nil {
		t.Fatalf("inserting violation: %v", err)
	}
	if err := svc.UpsertRuleResultFail(ctx, "a2", "rB", "v2B", "bad", now); err != nil {
		t.Fatalf("fail a2/rB: %v", err)
	}

	// "missing" is included in the input slice so we actually exercise the
	// requested-but-unknown-artist path: the map lookup alone would just
	// return Go's zero-value whether or not the service supports it.
	counts, err := svc.GetRuleResultCounts(ctx, []string{"a1", "a2", "missing"})
	if err != nil {
		t.Fatalf("GetRuleResultCounts: %v", err)
	}
	if got := counts["a1"]; got.Passed != 2 || got.Evaluated != 2 {
		t.Errorf("a1 counts = %+v, want {Passed:2, Evaluated:2}", got)
	}
	if got := counts["a2"]; got.Passed != 1 || got.Evaluated != 2 {
		t.Errorf("a2 counts = %+v, want {Passed:1, Evaluated:2}", got)
	}

	// Unknown artist id returns the zero-value count rather than an error.
	// The current implementation omits keys that have no rule_result rows,
	// so we assert on the map lookup's zero-value (Go semantics) rather
	// than on key presence.
	if got := counts["missing"]; got.Passed != 0 || got.Evaluated != 0 {
		t.Errorf("missing counts = %+v, want zero", got)
	}
}

func TestUpsertViolation_RollsBackWhenResultWriteFails(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedArtistAndRule(t, db, "a1", "r1")

	// Install a trigger that aborts any INSERT into rule_results. The
	// UpsertViolation call below must then roll back the in-flight
	// rule_violations insert as well.
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER rule_results_abort
		BEFORE INSERT ON rule_results
		BEGIN
			SELECT RAISE(ABORT, 'forced-abort for rollback test');
		END`); err != nil {
		t.Fatalf("installing abort trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DROP TRIGGER IF EXISTS rule_results_abort`); err != nil {
			t.Logf("dropping abort trigger: %v", err)
		}
	})

	v := &RuleViolation{
		RuleID:     "r1",
		ArtistID:   "a1",
		ArtistName: "Artist a1",
		Severity:   "warning",
		Message:    "bad",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	err := svc.UpsertViolation(ctx, v)
	if err == nil {
		t.Fatalf("UpsertViolation returned nil, want error from abort trigger")
	}

	// Both tables must be empty: the tx rolled back.
	var violationCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		"r1", "a1").Scan(&violationCount); err != nil {
		t.Fatalf("counting rule_violations: %v", err)
	}
	if violationCount != 0 {
		t.Errorf("rule_violations rows = %d after rolled-back UpsertViolation, want 0", violationCount)
	}

	var resultCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM rule_results WHERE rule_id = ? AND artist_id = ?`,
		"r1", "a1").Scan(&resultCount); err != nil {
		t.Fatalf("counting rule_results: %v", err)
	}
	if resultCount != 0 {
		t.Errorf("rule_results rows = %d after rolled-back UpsertViolation, want 0", resultCount)
	}
}
