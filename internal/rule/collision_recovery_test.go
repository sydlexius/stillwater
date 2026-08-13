package rule

// collision_recovery_test.go pins the #2967 detection/reopen surface added in
// collision_recovery.go. The MOST IMPORTANT property under test is that
// ResolvedCollisionViolations never decides anything on its own: it is a
// read-only report, and ReopenCollisionViolations reopens ONLY the exact IDs
// an operator passes -- an operator-shaped resolved row (distinct resolved_at,
// a rule_results row present) must be SURFACED by detection but stay resolved
// forever unless its ID is explicitly named. "The system does not guess" is
// the whole reason this shipped as a report instead of a migration.

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

// resolveViolationAt backdates a seeded violation to look exactly like a row
// resolved at a specific instant -- either by the pre-#2614 bulk cleanup
// (several rows sharing one byte-identical stamp) or by a normal one-off
// resolve. A raw UPDATE, not a service call: no service method sets an
// arbitrary resolved_at, and the fixture needs exact control over which rows
// cluster.
func resolveViolationAt(t *testing.T, svc *Service, artistID, ruleID string, at time.Time) {
	t.Helper()
	ts := at.UTC().Format(time.RFC3339)
	res, err := svc.db.ExecContext(t.Context(), `
		UPDATE rule_violations
		   SET status = ?, resolved_at = ?, updated_at = ?
		 WHERE artist_id = ? AND rule_id = ?
	`, ViolationStatusResolved, ts, ts, artistID, ruleID)
	if err != nil {
		t.Fatalf("backdating resolved violation for %s: %v", ruleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("reading rows affected while backdating %s: %v", ruleID, err)
	}
	if n != 1 {
		t.Fatalf("backdating %s updated %d rows, want 1 (fixture did not land)", ruleID, n)
	}
}

// resolveViolationNullAt sets a seeded violation to status='resolved' with
// resolved_at explicitly NULL -- reproducing the row UpsertViolation writes
// when Status is ViolationStatusResolved and ResolvedAt is nil
// (dbutil.NilableTime(nil) returns nil, and the upsert's resolved_at column
// is set unconditionally to excluded.resolved_at). No service method
// exposes this shape as a public call, so this is a raw UPDATE like its
// sibling resolveViolationAt.
func resolveViolationNullAt(t *testing.T, svc *Service, artistID, ruleID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := svc.db.ExecContext(t.Context(), `
		UPDATE rule_violations
		   SET status = ?, resolved_at = NULL, updated_at = ?
		 WHERE artist_id = ? AND rule_id = ?
	`, ViolationStatusResolved, now, artistID, ruleID)
	if err != nil {
		t.Fatalf("backdating NULL-resolved_at violation for %s: %v", ruleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("reading rows affected while backdating NULL-resolved_at %s: %v", ruleID, err)
	}
	if n != 1 {
		t.Fatalf("backdating NULL-resolved_at %s updated %d rows, want 1 (fixture did not land)", ruleID, n)
	}
}

// resolveViolationRawAt sets a seeded violation's resolved_at to an
// arbitrary raw string, bypassing resolveViolationAt's RFC3339 formatting.
// resolveViolationAt can only ever produce well-formed timestamps (it goes
// through time.RFC3339), so it cannot reproduce a malformed resolved_at
// value; this sibling helper writes the raw string directly, the same shape
// as resolveViolationNullAt but for a non-NULL malformed value.
func resolveViolationRawAt(t *testing.T, svc *Service, artistID, ruleID, raw string) {
	t.Helper()
	res, err := svc.db.ExecContext(t.Context(), `
		UPDATE rule_violations
		   SET status = ?, resolved_at = ?, updated_at = ?
		 WHERE artist_id = ? AND rule_id = ?
	`, ViolationStatusResolved, raw, time.Now().UTC().Format(time.RFC3339), artistID, ruleID)
	if err != nil {
		t.Fatalf("backdating raw resolved_at for %s: %v", ruleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("reading rows affected while backdating raw resolved_at %s: %v", ruleID, err)
	}
	if n != 1 {
		t.Fatalf("backdating raw resolved_at %s updated %d rows, want 1 (fixture did not land)", ruleID, n)
	}
}

// violationRow is a full snapshot of one rule_violations row, used to assert
// "byte-unchanged" -- that a row ReopenCollisionViolations refused to touch
// really was left alone in every column, not merely in the one column a
// narrower assertion happened to check.
type violationRow struct {
	status     string
	resolvedAt sql.NullString
	updatedAt  string
	message    string
}

func readViolationRow(t *testing.T, db *sql.DB, id string) violationRow {
	t.Helper()
	var r violationRow
	err := db.QueryRowContext(t.Context(),
		`SELECT status, resolved_at, updated_at, message FROM rule_violations WHERE id = ?`, id,
	).Scan(&r.status, &r.resolvedAt, &r.updatedAt, &r.message)
	if err != nil {
		t.Fatalf("reading violation row %s: %v", id, err)
	}
	return r
}

// deleteRuleResultRow removes the rule_results row for (artistID, ruleID).
// UpsertViolation (which seedViolation calls to raise the open violation)
// writes a rule_results fail row for ANY open/pending violation, collision
// included -- writeResultRow keys only on status, not on IsEventDriven. So
// every seeded collision violation starts WITH a rule_results row, exactly as
// production does when RaiseBackdropCollision calls UpsertViolation.
//
// The bug-shaped signature this test reproduces is what the pre-#2614
// cleanupDisabledRuleState left behind: its second statement was an
// unconditional `DELETE FROM rule_results WHERE rule_id = ?`, run in the same
// operation that soft-resolved the violations. This helper reproduces exactly
// that half of the old bug so the fixture's "no rule_results row" case means
// what it claims to mean, rather than "a row was never created".
func deleteRuleResultRow(t *testing.T, db *sql.DB, artistID, ruleID string) {
	t.Helper()
	res, err := db.ExecContext(t.Context(),
		`DELETE FROM rule_results WHERE artist_id = ? AND rule_id = ?`, artistID, ruleID)
	if err != nil {
		t.Fatalf("deleting rule_results row for (%s, %s): %v", artistID, ruleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("reading rows affected while deleting rule_results (%s, %s): %v", artistID, ruleID, err)
	}
	if n != 1 {
		t.Fatalf("deleting rule_results for (%s, %s) removed %d rows, want 1 (fixture did not have the row UpsertViolation should have written)", artistID, ruleID, n)
	}
}

// collisionFixture seeds a six-row population covering every status/cluster
// shape the detection (and, in the follow-on reopen PR, reopen) surface must
// distinguish, each on its own artist (rule_violations is UNIQUE(rule_id,
// artist_id), so two resolved collision rows cannot share an artist).
type collisionFixture struct {
	bugA, bugB   *artist.Artist // clustered resolved_at, no rule_results row (bug-shaped)
	operator     *artist.Artist // distinct resolved_at, rule_results row present (operator-shaped)
	dismissed    *artist.Artist // status=dismissed, must never surface or be reopenable
	otherRule    *artist.Artist // resolved violation for a different rule
	alreadyOpen  *artist.Artist // status=open collision violation
	clusterStamp time.Time
	soloStamp    time.Time
}

func newCollisionFixture(t *testing.T, db *sql.DB, svc *Service) *collisionFixture {
	t.Helper()
	f := &collisionFixture{
		bugA:        apiOnlyArtist(t, db, "Collision Bug A"),
		bugB:        apiOnlyArtist(t, db, "Collision Bug B"),
		operator:    apiOnlyArtist(t, db, "Collision Operator Resolved"),
		dismissed:   apiOnlyArtist(t, db, "Collision Dismissed"),
		otherRule:   apiOnlyArtist(t, db, "Other Rule Resolved"),
		alreadyOpen: apiOnlyArtist(t, db, "Collision Still Open"),
		// Distinct instants, one hour apart, so cluster and non-cluster rows
		// cannot collide by accident.
		clusterStamp: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		soloStamp:    time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC),
	}

	seedViolation(t, svc, f.bugA, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationAt(t, svc, f.bugA.ID, RuleCrossArtistBackdropCollision, f.clusterStamp)
	deleteRuleResultRow(t, db, f.bugA.ID, RuleCrossArtistBackdropCollision)

	seedViolation(t, svc, f.bugB, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationAt(t, svc, f.bugB.ID, RuleCrossArtistBackdropCollision, f.clusterStamp)
	deleteRuleResultRow(t, db, f.bugB.ID, RuleCrossArtistBackdropCollision)

	// Operator-shaped: the rule_results row seedViolation wrote is left in
	// place, matching a row nothing has hard-deleted.
	seedViolation(t, svc, f.operator, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationAt(t, svc, f.operator.ID, RuleCrossArtistBackdropCollision, f.soloStamp)

	seedViolation(t, svc, f.dismissed, RuleCrossArtistBackdropCollision, ViolationStatusDismissed)

	seedViolation(t, svc, f.otherRule, RuleBioExists, ViolationStatusOpen)
	resolveViolationAt(t, svc, f.otherRule.ID, RuleBioExists, f.soloStamp)

	seedViolation(t, svc, f.alreadyOpen, RuleCrossArtistBackdropCollision, ViolationStatusOpen)

	return f
}

// assertPreconditions checks the fixture landed exactly as intended, straight
// off the table, before any test trusts it.
func (f *collisionFixture) assertPreconditions(t *testing.T, db *sql.DB) {
	t.Helper()
	cases := []struct {
		artist *artist.Artist
		ruleID string
		status string
	}{
		{f.bugA, RuleCrossArtistBackdropCollision, ViolationStatusResolved},
		{f.bugB, RuleCrossArtistBackdropCollision, ViolationStatusResolved},
		{f.operator, RuleCrossArtistBackdropCollision, ViolationStatusResolved},
		{f.dismissed, RuleCrossArtistBackdropCollision, ViolationStatusDismissed},
		{f.otherRule, RuleBioExists, ViolationStatusResolved},
		{f.alreadyOpen, RuleCrossArtistBackdropCollision, ViolationStatusOpen},
	}
	for _, c := range cases {
		if got := violationStatus(t, db, c.artist.ID, c.ruleID); got != c.status {
			t.Fatalf("precondition: %s/%s should be %q, got %q", c.artist.Name, c.ruleID, c.status, got)
		}
	}
	// The cluster precondition: bugA and bugB really do share one stamp, or
	// the ClusterSize assertion below would prove nothing.
	var stampA, stampB string
	if err := db.QueryRowContext(t.Context(),
		`SELECT resolved_at FROM rule_violations WHERE artist_id = ?`, f.bugA.ID).Scan(&stampA); err != nil {
		t.Fatalf("reading bugA resolved_at: %v", err)
	}
	if err := db.QueryRowContext(t.Context(),
		`SELECT resolved_at FROM rule_violations WHERE artist_id = ?`, f.bugB.ID).Scan(&stampB); err != nil {
		t.Fatalf("reading bugB resolved_at: %v", err)
	}
	if stampA != stampB {
		t.Fatalf("precondition: bugA (%s) and bugB (%s) must share resolved_at for the cluster test to mean anything", stampA, stampB)
	}
}

// TestResolvedCollisionViolations_DetectionAndCluster is the read-only
// detection test.
//
// Mutants this kills: dropping the `status = ?` filter (would surface the
// dismissed or already-open rows); dropping the `rule_id = ?` filter (would
// surface the other-rule row); computing ClusterSize from a parsed time.Time
// instead of the raw string (would silently disagree with the SQL grouping
// for an unparsable stamp -- #2972's trap).
func TestResolvedCollisionViolations_DetectionAndCluster(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	report, err := svc.ResolvedCollisionViolations(ctx)
	if err != nil {
		t.Fatalf("ResolvedCollisionViolations: %v", err)
	}

	got := make(map[string]ResolvedCollisionViolation, len(report.Violations))
	for _, v := range report.Violations {
		got[v.ArtistID] = v
	}

	if len(got) != 3 {
		t.Fatalf("ResolvedCollisionViolations returned %d rows, want 3 (bugA, bugB, operator); got IDs %v",
			len(got), keysOf(got))
	}
	for _, want := range []*artist.Artist{f.bugA, f.bugB, f.operator} {
		if _, ok := got[want.ID]; !ok {
			t.Errorf("missing expected resolved collision row for %s", want.Name)
		}
	}
	for _, unwanted := range []*artist.Artist{f.dismissed, f.otherRule, f.alreadyOpen} {
		if _, ok := got[unwanted.ID]; ok {
			t.Errorf("ResolvedCollisionViolations returned %s, which must never appear (dismissed/other-rule/open)", unwanted.Name)
		}
	}

	if cs := got[f.bugA.ID].ClusterSize; cs != 2 {
		t.Errorf("bugA ClusterSize = %d, want 2 (shares resolved_at with bugB)", cs)
	}
	if cs := got[f.bugB.ID].ClusterSize; cs != 2 {
		t.Errorf("bugB ClusterSize = %d, want 2 (shares resolved_at with bugA)", cs)
	}
	if cs := got[f.operator.ID].ClusterSize; cs != 1 {
		t.Errorf("operator ClusterSize = %d, want 1 (distinct resolved_at)", cs)
	}

	if report.NoRuleResultsExist {
		t.Errorf("NoRuleResultsExist = true, want false: a rule_results row for the collision rule was seeded on the operator artist")
	}
}

// TestResolvedCollisionViolations_NoRuleResultsExist_TrueWhenNoneSeeded pins
// the other side of the advisory flag: after the rule_results row is removed
// (reproducing the pre-#2614 cleanup's hard DELETE), the flag flips to true.
func TestResolvedCollisionViolations_NoRuleResultsExist_TrueWhenNoneSeeded(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	a := apiOnlyArtist(t, db, "Collision Solo")
	seedViolation(t, svc, a, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationAt(t, svc, a.ID, RuleCrossArtistBackdropCollision, time.Now())
	deleteRuleResultRow(t, db, a.ID, RuleCrossArtistBackdropCollision)

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rule_results WHERE rule_id = ?`,
		RuleCrossArtistBackdropCollision).Scan(&count); err != nil {
		t.Fatalf("counting rule_results: %v", err)
	}
	if count != 0 {
		t.Fatalf("precondition: no rule_results row should exist for the collision rule, found %d", count)
	}

	report, err := svc.ResolvedCollisionViolations(ctx)
	if err != nil {
		t.Fatalf("ResolvedCollisionViolations: %v", err)
	}
	if !report.NoRuleResultsExist {
		t.Errorf("NoRuleResultsExist = false, want true: zero rule_results rows exist for the collision rule")
	}
}

// TestReopenCollisionViolations_ScopedAndRefusals is the reopen test, and its
// central assertion is the "system does not guess" property: the operator-
// shaped row (f.operator) is surfaced by detection but is NEVER reopened
// because its ID was never passed.
//
// Mutants this kills: reopening every candidate row instead of only the
// passed IDs (would flip f.bugB or f.operator too); treating a missing ID as
// a silent no-op instead of a not_found outcome; reporting a dismissed row
// under the generic not_resolved code instead of the dedicated dismissed
// code.
//
// Deliberately NOT covered here: dropping the `status = 'resolved'` clause
// from the reopen UPDATE. That mutant is killed by
// TestReopenCollisionViolations_RowsAffectedMismatchIsAnError below, which
// drives the UPDATE with an eligible list whose row no longer qualifies by
// the time the UPDATE runs -- the only way to make the SQL clause the
// deciding factor rather than the (already correct) Go pre-check. Likewise
// dropping the `rule_id = ?` clause: f.otherRule is already filtered out by
// the Go pre-check before the UPDATE runs, so the SQL clause never decides
// anything in this test. That mutant is killed by
// TestReopenCollisionViolations_RuleIDMismatchIsAnError below, for the same
// reason.
func TestReopenCollisionViolations_ScopedAndRefusals(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	bugAID := violationID(t, db, f.bugA.ID, RuleCrossArtistBackdropCollision)
	bugBID := violationID(t, db, f.bugB.ID, RuleCrossArtistBackdropCollision)
	operatorID := violationID(t, db, f.operator.ID, RuleCrossArtistBackdropCollision)
	dismissedID := violationID(t, db, f.dismissed.ID, RuleCrossArtistBackdropCollision)
	otherRuleID := violationID(t, db, f.otherRule.ID, RuleBioExists)
	alreadyOpenID := violationID(t, db, f.alreadyOpen.ID, RuleCrossArtistBackdropCollision)
	const missingID = "does-not-exist-12345"

	// Snapshot every row we expect to be UNTOUCHED, before the call.
	before := map[string]violationRow{
		bugBID:        readViolationRow(t, db, bugBID),
		operatorID:    readViolationRow(t, db, operatorID),
		dismissedID:   readViolationRow(t, db, dismissedID),
		otherRuleID:   readViolationRow(t, db, otherRuleID),
		alreadyOpenID: readViolationRow(t, db, alreadyOpenID),
	}

	outcomes, err := svc.ReopenCollisionViolations(ctx, []string{
		bugAID, dismissedID, otherRuleID, alreadyOpenID, missingID,
	})
	if err != nil {
		t.Fatalf("ReopenCollisionViolations: %v", err)
	}

	reasons := make(map[string]ReopenOutcome, len(outcomes))
	for _, o := range outcomes {
		reasons[o.ID] = o
	}
	if len(reasons) != 5 {
		t.Fatalf("got %d outcomes, want 5 (one per requested ID)", len(reasons))
	}

	// POSITIVE CONTROL: a legitimately-eligible row (bugA) still reopens
	// successfully and reports Reopened=true truthfully.
	if o := reasons[bugAID]; !o.Reopened || o.Reason != "" {
		t.Errorf("bugA outcome = %+v, want Reopened=true Reason=\"\"", o)
	}
	if o := reasons[dismissedID]; o.Reopened || o.Reason != ReopenReasonDismissed {
		t.Errorf("dismissed outcome = %+v, want Reopened=false Reason=%q", o, ReopenReasonDismissed)
	}
	if o := reasons[otherRuleID]; o.Reopened || o.Reason != ReopenReasonWrongRule {
		t.Errorf("other-rule outcome = %+v, want Reopened=false Reason=%q", o, ReopenReasonWrongRule)
	}
	if o := reasons[alreadyOpenID]; o.Reopened || o.Reason != ReopenReasonNotResolved {
		t.Errorf("already-open outcome = %+v, want Reopened=false Reason=%q", o, ReopenReasonNotResolved)
	}
	if o := reasons[missingID]; o.Reopened || o.Reason != ReopenReasonNotFound {
		t.Errorf("missing outcome = %+v, want Reopened=false Reason=%q", o, ReopenReasonNotFound)
	}

	// THE POSITIVE CHANGE: bugA is now open with resolved_at cleared.
	got := readViolationRow(t, db, bugAID)
	if got.status != ViolationStatusOpen {
		t.Errorf("bugA status = %q, want %q", got.status, ViolationStatusOpen)
	}
	if got.resolvedAt.Valid {
		t.Errorf("bugA resolved_at = %q, want NULL", got.resolvedAt.String)
	}

	// THE MOST IMPORTANT ASSERTION: every row NOT explicitly passed is
	// byte-unchanged, including bugB (same cluster as bugA, but its ID was
	// never in the request), operator (surfaced by detection, still never
	// reopened without its ID being named), and dismissed (still never
	// modified -- see also the dedicated positive control below).
	for id, want := range before {
		got := readViolationRow(t, db, id)
		if got != want {
			t.Errorf("row %s changed despite not being in the reopen request: before=%+v after=%+v", id, want, got)
		}
	}
}

// TestReopenCollisionViolations_DismissedReasonCode is a narrow positive
// control on I4/dismissed-vs-not-resolved: a dismissed row is refused with
// the DEDICATED dismissed reason, distinct from a merely-not-resolved row
// (already-open), and -- the actual behavior guarantee, not just the label --
// the dismissed row is never modified.
func TestReopenCollisionViolations_DismissedReasonCode(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	dismissedID := violationID(t, db, f.dismissed.ID, RuleCrossArtistBackdropCollision)
	alreadyOpenID := violationID(t, db, f.alreadyOpen.ID, RuleCrossArtistBackdropCollision)
	before := readViolationRow(t, db, dismissedID)

	outcomes, err := svc.ReopenCollisionViolations(ctx, []string{dismissedID, alreadyOpenID})
	if err != nil {
		t.Fatalf("ReopenCollisionViolations: %v", err)
	}
	reasons := make(map[string]ReopenOutcome, len(outcomes))
	for _, o := range outcomes {
		reasons[o.ID] = o
	}

	if o := reasons[dismissedID]; o.Reopened || o.Reason != ReopenReasonDismissed {
		t.Errorf("dismissed outcome = %+v, want Reopened=false Reason=%q", o, ReopenReasonDismissed)
	}
	if o := reasons[alreadyOpenID]; o.Reopened || o.Reason != ReopenReasonNotResolved {
		t.Errorf("already-open outcome = %+v, want Reopened=false Reason=%q", o, ReopenReasonNotResolved)
	}
	// The two reason codes must actually differ -- a test that let them
	// collapse to the same string would prove nothing about I4.
	if reasons[dismissedID].Reason == reasons[alreadyOpenID].Reason {
		t.Fatalf("dismissed and already-open share reason %q, want distinct codes", reasons[dismissedID].Reason)
	}

	// POSITIVE CONTROL: the dismissed row was never modified.
	after := readViolationRow(t, db, dismissedID)
	if after != before {
		t.Errorf("dismissed row changed: before=%+v after=%+v", before, after)
	}
}

// TestReopenCollisionViolations_EmptyListIsNoOp pins the SQLite `IN ()`
// guard: an empty ID slice must never reach the query builder.
func TestReopenCollisionViolations_EmptyListIsNoOp(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	outcomes, err := svc.ReopenCollisionViolations(ctx, nil)
	if err != nil {
		t.Fatalf("ReopenCollisionViolations(nil): %v", err)
	}
	if len(outcomes) != 0 {
		t.Errorf("got %d outcomes for an empty request, want 0", len(outcomes))
	}
}

// violationID looks up the persisted id for a (artist, rule) pair.
func violationID(t *testing.T, db *sql.DB, artistID, ruleID string) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(t.Context(),
		`SELECT id FROM rule_violations WHERE artist_id = ? AND rule_id = ?`, artistID, ruleID,
	).Scan(&id); err != nil {
		t.Fatalf("reading violation id for (%s, %s): %v", artistID, ruleID, err)
	}
	return id
}

// keysOf is a small debug helper used to render a mismatch's actual IDs in a
// test failure message.
func keysOf(m map[string]ResolvedCollisionViolation) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestResolvedCollisionViolations_QueryErrorPropagates exercises the error
// return path of the SELECT: closing the underlying DB forces
// QueryContext to fail, matching the established pattern in
// coverage_targets_test.go for driving a real database error rather than a
// mock.
func TestResolvedCollisionViolations_QueryErrorPropagates(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.SeedDefaults(t.Context()); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	if _, err := svc.ResolvedCollisionViolations(t.Context()); err == nil {
		t.Error("ResolvedCollisionViolations on a closed db returned nil error, want the query failure")
	}
}

// TestReopenCollisionViolations_BeginTxErrorPropagates exercises the
// BeginTx error path the same way.
func TestReopenCollisionViolations_BeginTxErrorPropagates(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.SeedDefaults(t.Context()); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	if _, err := svc.ReopenCollisionViolations(t.Context(), []string{"whatever"}); err == nil {
		t.Error("ReopenCollisionViolations on a closed db returned nil error, want the BeginTx failure")
	}
}

// TestResolvedCollisionViolations_NullResolvedAt is C1: a resolved collision
// row with a NULL resolved_at (the shape UpsertViolation can write; see
// resolveViolationNullAt) must not crash the whole report, must not inflate
// any OTHER row's ClusterSize, and -- the positive control -- the other rows
// in the same result must still come back correctly.
func TestResolvedCollisionViolations_NullResolvedAt(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	nullRow := apiOnlyArtist(t, db, "Collision Null ResolvedAt")
	seedViolation(t, svc, nullRow, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationNullAt(t, svc, nullRow.ID, RuleCrossArtistBackdropCollision)

	// A SECOND NULL-resolved_at row. One NULL row cannot distinguish "NULL
	// rows never cluster" from "NULL rows fold into a cluster keyed on the
	// empty string" -- both hypotheses report ClusterSize=1 for a single row.
	// Two NULL rows do distinguish them: the correct code still reports 1 for
	// each (never clusters), while the folding mutant reports 2 for each.
	nullRow2 := apiOnlyArtist(t, db, "Collision Null ResolvedAt 2")
	seedViolation(t, svc, nullRow2, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationNullAt(t, svc, nullRow2.ID, RuleCrossArtistBackdropCollision)

	// Precondition: both rows really are status=resolved with a NULL
	// resolved_at, straight off the table -- otherwise the crash/cluster
	// claims below prove nothing.
	for _, id := range []string{nullRow.ID, nullRow2.ID} {
		var status string
		var resolvedAt sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT status, resolved_at FROM rule_violations WHERE artist_id = ?`, id,
		).Scan(&status, &resolvedAt); err != nil {
			t.Fatalf("reading nullRow %s: %v", id, err)
		}
		if status != ViolationStatusResolved {
			t.Fatalf("precondition: nullRow %s status = %q, want resolved", id, status)
		}
		if resolvedAt.Valid {
			t.Fatalf("precondition: nullRow %s resolved_at = %q, want NULL", id, resolvedAt.String)
		}
	}

	// (a) returned, not crashed on.
	report, err := svc.ResolvedCollisionViolations(ctx)
	if err != nil {
		t.Fatalf("ResolvedCollisionViolations returned an error on a NULL resolved_at row: %v", err)
	}

	got := make(map[string]ResolvedCollisionViolation, len(report.Violations))
	for _, v := range report.Violations {
		got[v.ArtistID] = v
	}
	nullV, ok := got[nullRow.ID]
	if !ok {
		t.Fatalf("NULL-resolved_at row missing from report; it must be surfaced, not dropped")
	}
	if nullV.ResolvedAt != nil {
		t.Errorf("NULL-resolved_at row ResolvedAt = %v, want nil (NULL in the database must not flatten to a zero time.Time)", *nullV.ResolvedAt)
	}
	null2V, ok := got[nullRow2.ID]
	if !ok {
		t.Fatalf("second NULL-resolved_at row missing from report; it must be surfaced, not dropped")
	}
	if null2V.ResolvedAt != nil {
		t.Errorf("second NULL-resolved_at row ResolvedAt = %v, want nil", *null2V.ResolvedAt)
	}

	// (b) does not inflate any OTHER row's ClusterSize: bugA/bugB must still
	// read 2, not 3, and BOTH NULL rows must read 1, never joining a cluster
	// keyed on the empty string. Two NULL rows is the case that actually
	// discriminates this: a mutant that folds NULL rows into an
	// empty-string-keyed cluster would report ClusterSize=2 for both, which a
	// single NULL row cannot distinguish from the correct ClusterSize=1.
	if cs := got[nullRow.ID].ClusterSize; cs != 1 {
		t.Errorf("NULL-resolved_at row ClusterSize = %d, want 1 (never clusters)", cs)
	}
	if cs := got[nullRow2.ID].ClusterSize; cs != 1 {
		t.Errorf("second NULL-resolved_at row ClusterSize = %d, want 1 (never clusters)", cs)
	}
	if cs := got[f.bugA.ID].ClusterSize; cs != 2 {
		t.Errorf("bugA ClusterSize = %d, want 2 -- the NULL rows must not have inflated this cluster", cs)
	}
	if cs := got[f.bugB.ID].ClusterSize; cs != 2 {
		t.Errorf("bugB ClusterSize = %d, want 2 -- the NULL rows must not have inflated this cluster", cs)
	}

	// (c) POSITIVE CONTROL: the other rows in the same result set are still
	// returned correctly alongside the NULL rows -- same population and
	// cluster sizes as the plain detection test asserts.
	if len(got) != 5 {
		t.Fatalf("report has %d rows, want 5 (bugA, bugB, operator, nullRow, nullRow2); got IDs %v", len(got), keysOf(got))
	}
	for _, want := range []*artist.Artist{f.bugA, f.bugB, f.operator} {
		if _, ok := got[want.ID]; !ok {
			t.Errorf("missing expected resolved collision row for %s alongside the NULL row", want.Name)
		}
	}
	if cs := got[f.operator.ID].ClusterSize; cs != 1 {
		t.Errorf("operator ClusterSize = %d, want 1", cs)
	}
	if operatorV := got[f.operator.ID]; operatorV.ResolvedAt == nil {
		t.Errorf("operator row ResolvedAt = nil, want a non-nil pointer to %v", f.soloStamp)
	} else if !operatorV.ResolvedAt.Equal(f.soloStamp) {
		t.Errorf("operator row ResolvedAt = %v, want %v", *operatorV.ResolvedAt, f.soloStamp)
	}
}

// TestResolvedCollisionViolations_LexicalClusteringOnMalformedStamps pins the
// deliberate choice, documented on ResolvedCollisionViolations, to cluster on
// the RAW stored resolved_at string rather than a re-parsed time.Time. Every
// other fixture in this file goes through resolveViolationAt, which formats
// via time.RFC3339, so every stored value it produces is well-formed --
// raw-string grouping and parsed-time.Time grouping agree on that fixture and
// this exact defect could ship undetected. Two DISTINCT malformed strings,
// each of which parses to the zero time.Time under dbutil.ParseTime, are the
// only fixture shape that can tell the two implementations apart: lexical
// clustering reports each as its own cluster of 1, while a
// parse-then-compare implementation collapses both onto the zero time and
// reports 2 for each (#2972's trap).
func TestResolvedCollisionViolations_LexicalClusteringOnMalformedStamps(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	const malformedA = "not-a-timestamp-A"
	const malformedB = "not-a-timestamp-B"

	rowA := apiOnlyArtist(t, db, "Collision Malformed A")
	seedViolation(t, svc, rowA, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationRawAt(t, svc, rowA.ID, RuleCrossArtistBackdropCollision, malformedA)

	rowB := apiOnlyArtist(t, db, "Collision Malformed B")
	seedViolation(t, svc, rowB, RuleCrossArtistBackdropCollision, ViolationStatusOpen)
	resolveViolationRawAt(t, svc, rowB.ID, RuleCrossArtistBackdropCollision, malformedB)

	// Precondition: the stored strings really are the distinct malformed
	// values intended, and are NOT NULL -- read straight off the raw table,
	// not through the code under test, or the ClusterSize assertions below
	// prove nothing.
	for _, c := range []struct {
		id   string
		want string
	}{{rowA.ID, malformedA}, {rowB.ID, malformedB}} {
		var resolvedAt sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT resolved_at FROM rule_violations WHERE artist_id = ?`, c.id,
		).Scan(&resolvedAt); err != nil {
			t.Fatalf("reading resolved_at for %s: %v", c.id, err)
		}
		if !resolvedAt.Valid {
			t.Fatalf("precondition: resolved_at for %s is NULL, want %q", c.id, c.want)
		}
		if resolvedAt.String != c.want {
			t.Fatalf("precondition: resolved_at for %s = %q, want %q", c.id, resolvedAt.String, c.want)
		}
	}
	// Both malformed strings really do parse to the zero time under
	// dbutil.ParseTime -- otherwise a parsed-time.Time implementation would
	// not collapse them together and this fixture would not discriminate.
	if zt := dbutil.ParseTime(malformedA); !zt.IsZero() {
		t.Fatalf("precondition: %q parsed to %v, want the zero time", malformedA, zt)
	}
	if zt := dbutil.ParseTime(malformedB); !zt.IsZero() {
		t.Fatalf("precondition: %q parsed to %v, want the zero time", malformedB, zt)
	}

	report, err := svc.ResolvedCollisionViolations(ctx)
	if err != nil {
		t.Fatalf("ResolvedCollisionViolations: %v", err)
	}
	got := make(map[string]ResolvedCollisionViolation, len(report.Violations))
	for _, v := range report.Violations {
		got[v.ArtistID] = v
	}

	rowAV, ok := got[rowA.ID]
	if !ok {
		t.Fatalf("malformed-A row missing from report")
	}
	rowBV, ok := got[rowB.ID]
	if !ok {
		t.Fatalf("malformed-B row missing from report")
	}
	if rowAV.ClusterSize != 1 {
		t.Errorf("malformed-A ClusterSize = %d, want 1 (lexically distinct from malformed-B, must not cluster via a shared zero-time parse)", rowAV.ClusterSize)
	}
	if rowBV.ClusterSize != 1 {
		t.Errorf("malformed-B ClusterSize = %d, want 1 (lexically distinct from malformed-A, must not cluster via a shared zero-time parse)", rowBV.ClusterSize)
	}
	// A malformed resolved_at must never surface as a fabricated year-0001
	// ResolvedAt: it must be nil, exactly like a NULL resolved_at would be.
	// dbutil.ParseTime returns the zero time (not an error) on a malformed
	// string, so a naive "always dereference" implementation produces a
	// non-nil pointer to 0001-01-01 here -- a different bug than "some other
	// wrong value", so the failure messages below say so explicitly.
	if rowAV.ResolvedAt != nil {
		if rowAV.ResolvedAt.IsZero() {
			t.Errorf("malformed-A ResolvedAt = non-nil pointer to year 0001 (%v), want nil", *rowAV.ResolvedAt)
		} else {
			t.Errorf("malformed-A ResolvedAt = %v, want nil", *rowAV.ResolvedAt)
		}
	}
	if rowBV.ResolvedAt != nil {
		if rowBV.ResolvedAt.IsZero() {
			t.Errorf("malformed-B ResolvedAt = non-nil pointer to year 0001 (%v), want nil", *rowBV.ResolvedAt)
		} else {
			t.Errorf("malformed-B ResolvedAt = %v, want nil", *rowBV.ResolvedAt)
		}
	}

	// Positive control: a well-formed clustered pair in the same result set
	// still reports its correct cluster size alongside the malformed rows.
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)
	report2, err := svc.ResolvedCollisionViolations(ctx)
	if err != nil {
		t.Fatalf("ResolvedCollisionViolations (with clustered fixture added): %v", err)
	}
	got2 := make(map[string]ResolvedCollisionViolation, len(report2.Violations))
	for _, v := range report2.Violations {
		got2[v.ArtistID] = v
	}
	if cs := got2[f.bugA.ID].ClusterSize; cs != 2 {
		t.Errorf("bugA ClusterSize = %d, want 2 (positive control: well-formed clustering still works alongside malformed rows)", cs)
	}
	if cs := got2[f.bugB.ID].ClusterSize; cs != 2 {
		t.Errorf("bugB ClusterSize = %d, want 2 (positive control: well-formed clustering still works alongside malformed rows)", cs)
	}
	if cs := got2[rowA.ID].ClusterSize; cs != 1 {
		t.Errorf("malformed-A ClusterSize = %d, want 1 (still distinct after adding the clustered fixture)", cs)
	}
	if cs := got2[rowB.ID].ClusterSize; cs != 1 {
		t.Errorf("malformed-B ClusterSize = %d, want 1 (still distinct after adding the clustered fixture)", cs)
	}
}

// TestResolvedCollisionViolations_CursorClosedBeforeFollowupQuery verifies
// that the method properly releases the first query's cursor before executing
// the second query. With a single-connection pool, a still-held cursor during
// the second query would deadlock. The test seeds multiple rows to force the
// cursor to be held during iteration, then verifies the method completes and
// correctly computes NoRuleResultsExist (the value from the second query).
func TestResolvedCollisionViolations_CursorClosedBeforeFollowupQuery(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Seed multiple resolved collision rows so the cursor spans multiple
	// iterations -- the precondition that the cursor would have been held
	// during the second query if not explicitly closed.
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	// Verify precondition: multiple resolved rows exist.
	var resolvedCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rule_violations
		WHERE rule_id = ? AND status = ?
	`, RuleCrossArtistBackdropCollision, ViolationStatusResolved).Scan(&resolvedCount); err != nil {
		t.Fatalf("counting resolved collision violations: %v", err)
	}
	if resolvedCount < 2 {
		t.Fatalf("precondition: need at least 2 resolved violation rows, fixture seeded %d", resolvedCount)
	}

	// The method must complete without deadlock and correctly report
	// NoRuleResultsExist based on the second query.
	report, err := svc.ResolvedCollisionViolations(ctx)
	if err != nil {
		t.Fatalf("ResolvedCollisionViolations: %v", err)
	}

	// Verify the report has the expected rows from the first query.
	if len(report.Violations) != resolvedCount {
		t.Errorf("report has %d violations, want %d", len(report.Violations), resolvedCount)
	}

	// Verify NoRuleResultsExist reflects the actual state: the fixture leaves
	// operator's rule_results row in place and deletes bugA/bugB's rows,
	// so one rule_results row exists.
	if report.NoRuleResultsExist {
		t.Errorf("NoRuleResultsExist = true, want false: the operator row's rule_results was seeded and not deleted")
	}
}

// TestReopenCollisionViolations_RowsAffectedMismatchIsAnError is the direct
// test for I1 and, as a consequence, for C2: it drives the reopen UPDATE
// with a row that the Go pre-check judged eligible but that no longer
// qualifies by the time the UPDATE executes (its status flips to dismissed
// inside the same transaction, via the reopenPreUpdateHook test seam, between
// the SELECT-based eligibility check and the UPDATE). If RowsAffected is not
// checked, this looks like a normal successful reopen; with the check, it
// must surface as an error and the row must be left exactly as the
// interloper set it -- never silently reported Reopened: true.
//
// This is also what proves the doc-comment fix on this test file's earlier
// claims: dropping the `status = 'resolved'` clause from the UPDATE (mutation
// a) makes THIS test fail, not the scoped-and-refusals test above, because
// only here does the SQL clause -- rather than the Go pre-check -- decide
// anything. See TestReopenCollisionViolations_RuleIDMismatchIsAnError below
// for the `rule_id = ?` clause (mutation b) counterpart.
func TestReopenCollisionViolations_RowsAffectedMismatchIsAnError(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	bugAID := violationID(t, db, f.bugA.ID, RuleCrossArtistBackdropCollision)
	before := readViolationRow(t, db, bugAID)

	// Simulate a status change landing between the SELECT and the UPDATE:
	// the row Go judged eligible is flipped to dismissed inside the same
	// transaction just before the UPDATE runs.
	svc.reopenPreUpdateHook = func(tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rule_violations SET status = ? WHERE id = ?`, ViolationStatusDismissed, bugAID,
		); err != nil {
			t.Fatalf("test hook: flipping bugA status: %v", err)
		}
	}

	outcomes, err := svc.ReopenCollisionViolations(ctx, []string{bugAID})
	if err == nil {
		t.Fatalf("ReopenCollisionViolations = (%v, nil), want an error when RowsAffected disagrees with the eligible count", outcomes)
	}

	// The WHOLE transaction must have rolled back, including the hook's own
	// interloping write -- not just the reopen UPDATE. The row must be
	// byte-identical to its state before the call: never silently reopened,
	// and not left in the hook's intermediate "dismissed" state either,
	// since that write happened inside the same transaction this error
	// unwinds.
	after := readViolationRow(t, db, bugAID)
	if after != before {
		t.Errorf("bugA row after failed reopen = %+v, want unchanged from before (%+v) -- the whole transaction, including the hook's own write, must roll back", after, before)
	}
}

// TestReopenCollisionViolations_RuleIDMismatchIsAnError is the rule_id
// counterpart of the test above, and specifically what kills mutation (b)
// (dropping the `rule_id = ?` clause from the reopen UPDATE): a row Go's
// pre-check judged eligible is switched to a DIFFERENT rule between the
// SELECT and the UPDATE. With the rule_id allow-list clause intact, the
// UPDATE's WHERE no longer matches the row, RowsAffected comes back short,
// and the call must error and roll back -- exactly like the status case,
// but exercising the other clause.
func TestReopenCollisionViolations_RuleIDMismatchIsAnError(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	bugAID := violationID(t, db, f.bugA.ID, RuleCrossArtistBackdropCollision)
	before := readViolationRow(t, db, bugAID)

	svc.reopenPreUpdateHook = func(tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rule_violations SET rule_id = ? WHERE id = ?`, RuleBioExists, bugAID,
		); err != nil {
			t.Fatalf("test hook: flipping bugA rule_id: %v", err)
		}
	}

	outcomes, err := svc.ReopenCollisionViolations(ctx, []string{bugAID})
	if err == nil {
		t.Fatalf("ReopenCollisionViolations = (%v, nil), want an error when RowsAffected disagrees with the eligible count", outcomes)
	}

	// The whole transaction, including the hook's own write, rolled back:
	// status/resolved_at/updated_at/message are untouched, and rule_id is
	// back to the collision rule.
	after := readViolationRow(t, db, bugAID)
	if after != before {
		t.Errorf("bugA row after failed reopen = %+v, want unchanged from before (%+v)", after, before)
	}
	var ruleID string
	if err := db.QueryRowContext(ctx, `SELECT rule_id FROM rule_violations WHERE id = ?`, bugAID).Scan(&ruleID); err != nil {
		t.Fatalf("reading bugA rule_id: %v", err)
	}
	if ruleID != RuleCrossArtistBackdropCollision {
		t.Errorf("bugA rule_id after failed reopen = %q, want %q (hook's write rolled back)", ruleID, RuleCrossArtistBackdropCollision)
	}
}

// TestReopenCollisionViolations_UpdatedAtIsStamped is the positive control
// for mutation (g) (dropping the `updated_at = ?` assignment from the reopen
// UPDATE): a legitimately-reopened row's updated_at must match the clock
// value the call used, not be left at its pre-reopen stamp. Uses frozenClock
// (not FakeClock) because this asserts an EXACT stamp equality, and FakeClock
// advances on every call including ones this test does not control the count
// of.
func TestReopenCollisionViolations_UpdatedAtIsStamped(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)
	bugAID := violationID(t, db, f.bugA.ID, RuleCrossArtistBackdropCollision)

	preReopen := readViolationRow(t, db, bugAID)

	reopenAt := time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)
	svc.clock = frozenClock{at: reopenAt}

	if _, err := svc.ReopenCollisionViolations(ctx, []string{bugAID}); err != nil {
		t.Fatalf("ReopenCollisionViolations: %v", err)
	}

	after := readViolationRow(t, db, bugAID)
	want := reopenAt.Format(time.RFC3339)
	if after.updatedAt != want {
		t.Errorf("bugA updated_at after reopen = %q, want %q (the reopen clock stamp)", after.updatedAt, want)
	}
	if after.updatedAt == preReopen.updatedAt {
		t.Errorf("bugA updated_at unchanged by reopen (%q); a stale updated_at poisons later staleness comparisons (#2972)", after.updatedAt)
	}
}

// TestReopenCollisionViolations_DuplicateIDsDeduplicated is I2: passing the
// same ID more than once must produce exactly one outcome for it, and the
// outcome order for the surviving IDs must match first-occurrence order in
// the input so a caller aligning outcomes back to its own display list gets a
// stable, unsurprising result.
func TestReopenCollisionViolations_DuplicateIDsDeduplicated(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	f := newCollisionFixture(t, db, svc)
	f.assertPreconditions(t, db)

	bugAID := violationID(t, db, f.bugA.ID, RuleCrossArtistBackdropCollision)
	bugBID := violationID(t, db, f.bugB.ID, RuleCrossArtistBackdropCollision)

	outcomes, err := svc.ReopenCollisionViolations(ctx, []string{bugAID, bugBID, bugAID, bugAID, bugBID})
	if err != nil {
		t.Fatalf("ReopenCollisionViolations: %v", err)
	}

	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes for [bugA, bugB, bugA, bugA, bugB], want 2 (deduplicated, first-occurrence order)", len(outcomes))
	}
	if outcomes[0].ID != bugAID || outcomes[1].ID != bugBID {
		t.Fatalf("outcome order = [%s, %s], want [%s, %s] (first-occurrence order preserved)",
			outcomes[0].ID, outcomes[1].ID, bugAID, bugBID)
	}
	for _, o := range outcomes {
		if !o.Reopened {
			t.Errorf("outcome %+v: want Reopened=true for a legitimately-eligible row", o)
		}
	}

	// Both rows actually reopened exactly once each -- the dedup must not
	// have suppressed the real write for the second distinct ID.
	for _, id := range []string{bugAID, bugBID} {
		got := readViolationRow(t, db, id)
		if got.status != ViolationStatusOpen {
			t.Errorf("row %s status = %q, want %q", id, got.status, ViolationStatusOpen)
		}
	}
}

// TestReopenCollisionViolations_MaxIDsBoundary is I3: at reopenMaxIDs the
// call must proceed normally (bounded by a named, documented limit rather
// than failing on the raw SQLite "too many SQL variables" driver string);
// one ID over the limit must be refused by ErrTooManyReopenIDs before any
// query runs.
func TestReopenCollisionViolations_MaxIDsBoundary(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// One over the limit: refused by our own error, never reaches the SELECT.
	tooMany := make([]string, reopenMaxIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("nonexistent-%d", i)
	}
	outcomes, err := svc.ReopenCollisionViolations(ctx, tooMany)
	if !errors.Is(err, ErrTooManyReopenIDs) {
		t.Fatalf("ReopenCollisionViolations(%d ids) error = %v, want ErrTooManyReopenIDs", len(tooMany), err)
	}
	if outcomes != nil {
		t.Errorf("ReopenCollisionViolations(%d ids) outcomes = %v, want nil alongside the error", len(tooMany), outcomes)
	}

	// At the limit exactly: proceeds (all not_found, since none of these IDs
	// exist, but that is a normal outcome, not a rejection).
	atLimit := make([]string, reopenMaxIDs)
	for i := range atLimit {
		atLimit[i] = fmt.Sprintf("nonexistent-%d", i)
	}
	outcomes, err = svc.ReopenCollisionViolations(ctx, atLimit)
	if err != nil {
		t.Fatalf("ReopenCollisionViolations(%d ids, at the limit): %v", len(atLimit), err)
	}
	if len(outcomes) != reopenMaxIDs {
		t.Fatalf("got %d outcomes at the limit, want %d", len(outcomes), reopenMaxIDs)
	}
	for _, o := range outcomes {
		if o.Reopened || o.Reason != ReopenReasonNotFound {
			t.Fatalf("outcome %+v at the limit, want Reopened=false Reason=%q for a nonexistent id", o, ReopenReasonNotFound)
		}
	}
}
