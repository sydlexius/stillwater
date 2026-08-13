package rule

// collision_recovery_test.go pins the #2967 detection surface added in
// collision_recovery.go. The MOST IMPORTANT property under test is that
// ResolvedCollisionViolations never decides anything on its own: it is a
// read-only report -- an operator-shaped resolved row (distinct resolved_at,
// a rule_results row present) must be SURFACED here but nothing in this file
// ever changes its status. "The system does not guess" is the whole reason
// this shipped as a report instead of a migration; the reopen half of that
// story (explicit, ID-scoped writes) is a separate follow-on PR (#2967 PR 2)
// built on top of the shared fixture defined here.

import (
	"database/sql"
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
