package artist

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// seedBlastChange inserts one metadata_changes row directly, bypassing
// HistoryService.Record so a test can control the exact source, values, and
// timestamp -- including source="revert" rows, which Record would accept but
// which the report must recognize as a recovery rather than damage.
//
// ts is formatted RFC3339 to match what Record writes and what migration 004
// normalized legacy rows to, so the text ordering the query relies on holds.
func seedBlastChange(t *testing.T, db *sql.DB, id, artistID, field, oldValue, newValue, source string, ts time.Time) {
	t.Helper()
	seedTestArtist(t, db, artistID)
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, artistID, field, oldValue, newValue, source, ts.UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("seedBlastChange(%q): %v", id, err)
	}
}

// blastFixtureBase is an arbitrary fixed instant. Tests offset from it so the
// per-(artist,field) ordering is deterministic and does not depend on wall
// clock resolution.
var blastFixtureBase = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// seedBlastFixture builds the canonical fixture: every attribution state and
// both damage classes, plus the rows that must NOT be reported.
//
// Returns nothing. The change ids are fixed string literals below, and tests
// assert against those ids directly.
func seedBlastFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	at := func(min int) time.Time { return blastFixtureBase.Add(time.Duration(min) * time.Minute) }

	// --- Rows that MUST be reported ---

	// Attributed automated, blanked. A scan cleared a biography.
	seedBlastChange(t, db, "c-scan-blank", "a-1", "biography", "a real bio", "", "scan", at(1))
	// Attributed automated, replaced. A provider overwrote a genre set.
	seedBlastChange(t, db, "c-prov-repl", "a-1", "genres", "Rock, Grunge", "Pop", "provider:musicbrainz", at(2))
	// Attributed automated, blanked, via a rule.
	seedBlastChange(t, db, "c-rule-blank", "a-2", "moods", "Energetic", "", "rule:r-42", at(3))
	// Attributed automated, replaced, via import.
	seedBlastChange(t, db, "c-imp-repl", "a-2", "origin", "Seattle", "Tacoma", "import", at(4))
	// UNATTRIBUTABLE, blanked. Recorded manual: this may be an operator edit or
	// may be pre-2026-07-19 scan damage, and Stillwater cannot tell.
	seedBlastChange(t, db, "c-man-blank", "a-3", "styles", "Grunge", "", "manual", at(5))
	// UNATTRIBUTABLE, replaced.
	seedBlastChange(t, db, "c-man-repl", "a-3", "type", "group", "solo", "manual", at(6))

	// --- Rows that must NOT be reported ---

	// A first-ever population: the operator had nothing, so nothing was lost.
	seedBlastChange(t, db, "x-populate", "a-4", "biography", "", "new bio", "scan", at(7))
	// A no-op rewrite: same value in and out.
	seedBlastChange(t, db, "x-noop", "a-4", "origin", "Seattle", "Seattle", "scan", at(8))
	// A recovery of a BLANKED field. The pair (a-5, born) was destroyed at
	// at(9) and recovered at at(10); the recovery is newer so it wins the
	// ranking and the pair drops out entirely.
	seedBlastChange(t, db, "x-damage-then-revert", "a-5", "born", "1967-02-20", "", "scan", at(9))
	seedBlastChange(t, db, "x-revert", "a-5", "born", "", "1967-02-20", "revert", at(10))
	// A recovery of a REPLACED field. This is the case a bare
	// "old_value != ''" test would miss: the revert row itself has a non-empty
	// old_value and a different non-empty new_value, so it looks exactly like
	// "replaced" damage unless source="revert" is excluded.
	seedBlastChange(t, db, "x-repl-then-revert", "a-6", "gender", "female", "male", "scan", at(11))
	seedBlastChange(t, db, "x-repl-revert", "a-6", "gender", "male", "female", "revert", at(12))
}

// blastIDs extracts change ids in returned order, for order-sensitive
// assertions and for readable failure messages.
func blastIDs(rows []BlastRadiusRow) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

func blastIDSet(rows []BlastRadiusRow) map[string]BlastRadiusRow {
	out := make(map[string]BlastRadiusRow, len(rows))
	for i := range rows {
		out[rows[i].ID] = rows[i]
	}
	return out
}

// TestListBlastRadius_ReportsEveryDamagedFieldAndNothingElse is the core
// behavior assertion: exactly the six damaged rows, with the right class and
// attribution on each, and none of the five rows that are not damage.
func TestListBlastRadius_ReportsEveryDamagedFieldAndNothingElse(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)

	rows, err := svc.ListBlastRadius(context.Background(), BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}

	want := map[string]struct {
		class       string
		attribution string
	}{
		"c-scan-blank": {BlastClassBlanked, BlastAttributionAutomated},
		"c-prov-repl":  {BlastClassReplaced, BlastAttributionAutomated},
		"c-rule-blank": {BlastClassBlanked, BlastAttributionAutomated},
		"c-imp-repl":   {BlastClassReplaced, BlastAttributionAutomated},
		"c-man-blank":  {BlastClassBlanked, BlastAttributionUnknown},
		"c-man-repl":   {BlastClassReplaced, BlastAttributionUnknown},
	}

	got := blastIDSet(rows)
	if len(got) != len(want) {
		t.Fatalf("row ids = %v, want exactly %d rows (%v)", blastIDs(rows), len(want), want)
	}
	for id, w := range want {
		row, ok := got[id]
		if !ok {
			t.Errorf("missing expected row %q; got %v", id, blastIDs(rows))
			continue
		}
		if row.Class != w.class {
			t.Errorf("row %q Class = %q, want %q", id, row.Class, w.class)
		}
		if row.Attribution != w.attribution {
			t.Errorf("row %q Attribution = %q, want %q", id, row.Attribution, w.attribution)
		}
		if row.ArtistName == "" {
			t.Errorf("row %q has empty ArtistName; the report cannot name the affected artist", id)
		}
	}
}

// TestListBlastRadius_UnattributableRowsAreReportedNotDropped is the
// anti-"unknown rendered as clean" guard, and the single most important test in
// this file.
//
// Before 2026-07-19 (commit 5942fa7a, PR #2641, issue #2636) scan-driven
// changes were recorded as source="manual" and are indistinguishable from
// operator edits. A report that quietly drops them, or that classifies them as
// clean operator activity, under-reports real data loss while looking complete.
// Both halves are asserted: the rows are PRESENT, and they are labeled unknown
// rather than automated.
func TestListBlastRadius_UnattributableRowsAreReportedNotDropped(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	got := blastIDSet(rows)
	for _, id := range []string{"c-man-blank", "c-man-repl"} {
		row, ok := got[id]
		if !ok {
			t.Fatalf("unattributable row %q was DROPPED from the report; "+
				"pre-attribution-cutoff damage would be invisible. got %v", id, blastIDs(rows))
		}
		if row.Attribution != BlastAttributionUnknown {
			t.Errorf("unattributable row %q Attribution = %q, want %q; "+
				"a manual-sourced row must never be presented as proven-clean",
				id, row.Attribution, BlastAttributionUnknown)
		}
	}

	// The counts must carry the unknown bucket too, so a caller rendering a
	// summary cannot report a headline number that excludes it.
	counts, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("CountBlastRadius: %v", err)
	}
	if counts.Unknown != 2 {
		t.Errorf("counts.Unknown = %d, want 2", counts.Unknown)
	}
	if counts.Automated != 4 {
		t.Errorf("counts.Automated = %d, want 4", counts.Automated)
	}
	if counts.Total != 6 {
		t.Errorf("counts.Total = %d, want 6", counts.Total)
	}
}

// TestCountBlastRadius_BothBucketsSurviveAnAttributionFilter proves the counts
// ignore the attribution filter. An operator who narrows the table to
// automated-only must still be told how many unattributable rows exist, or the
// filter becomes a way to make damage disappear.
func TestCountBlastRadius_BothBucketsSurviveAnAttributionFilter(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	counts, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{Attribution: BlastAttributionAutomated})
	if err != nil {
		t.Fatalf("CountBlastRadius: %v", err)
	}
	if counts.Unknown != 2 {
		t.Errorf("counts.Unknown = %d under an automated-only filter, want 2; "+
			"filtering the table must not zero the other bucket's count", counts.Unknown)
	}
	if counts.Automated != 4 {
		t.Errorf("counts.Automated = %d, want 4", counts.Automated)
	}
	// Total DOES follow the filter, because pagination needs it to.
	if counts.Total != 4 {
		t.Errorf("counts.Total = %d under an automated-only filter, want 4", counts.Total)
	}

	// Symmetric case: narrowing to unknown must not zero the automated count.
	counts, err = svc.CountBlastRadius(ctx, BlastRadiusFilter{Attribution: BlastAttributionUnknown})
	if err != nil {
		t.Fatalf("CountBlastRadius: %v", err)
	}
	if counts.Automated != 4 {
		t.Errorf("counts.Automated = %d under an unknown-only filter, want 4", counts.Automated)
	}
	if counts.Total != 2 {
		t.Errorf("counts.Total = %d under an unknown-only filter, want 2", counts.Total)
	}
}

// TestListBlastRadius_RecoveredFieldsDropOut proves the rn=1 frame does its
// job. Both recovery shapes are covered: recovering a blanked field, and
// recovering a replaced field (whose revert row is itself shaped like damage
// and is only excluded because source="revert" is filtered in the OUTER
// select).
//
// The precondition half is what stops this passing vacuously: it asserts the
// damage rows ARE reported before their recovery is written. Without it the
// test would also pass on a build whose query returned nothing at all.
func TestListBlastRadius_RecoveredFieldsDropOut(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()
	at := func(min int) time.Time { return blastFixtureBase.Add(time.Duration(min) * time.Minute) }

	seedBlastChange(t, db, "d-blank", "r-1", "born", "1967-02-20", "", "scan", at(1))
	seedBlastChange(t, db, "d-repl", "r-2", "gender", "female", "male", "scan", at(2))

	// PRECONDITION: both are reported as damage while unrecovered.
	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius (precondition): %v", err)
	}
	pre := blastIDSet(rows)
	if _, ok := pre["d-blank"]; !ok {
		t.Fatalf("precondition failed: blanked damage not reported; got %v", blastIDs(rows))
	}
	if _, ok := pre["d-repl"]; !ok {
		t.Fatalf("precondition failed: replaced damage not reported; got %v", blastIDs(rows))
	}

	// Recover both. Written directly rather than through a restore path,
	// which this read-only PR does not build.
	seedBlastChange(t, db, "d-blank-rev", "r-1", "born", "", "1967-02-20", "revert", at(3))
	seedBlastChange(t, db, "d-repl-rev", "r-2", "gender", "male", "female", "revert", at(4))

	rows, err = svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius (after recovery): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("recovered fields still reported as damage: %v; a recovered "+
			"field must drop out of the report, and a revert row must never be "+
			"listed as fresh damage", blastIDs(rows))
	}

	counts, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("CountBlastRadius: %v", err)
	}
	if counts.Total != 0 {
		t.Errorf("counts.Total = %d after recovery, want 0", counts.Total)
	}
}

// TestListBlastRadius_ReportsLatestDamagePerField proves the report is a
// current-state view: a field damaged twice appears ONCE, showing the newer
// damage, not both events and not the older one.
func TestListBlastRadius_ReportsLatestDamagePerField(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	at := func(min int) time.Time { return blastFixtureBase.Add(time.Duration(min) * time.Minute) }

	seedBlastChange(t, db, "old-damage", "m-1", "genres", "Rock", "Pop", "scan", at(1))
	seedBlastChange(t, db, "new-damage", "m-1", "genres", "Pop", "", "scan", at(2))

	rows, err := svc.ListBlastRadius(context.Background(), BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want exactly 1 (the latest damage for the pair)", blastIDs(rows))
	}
	if rows[0].ID != "new-damage" {
		t.Errorf("reported %q, want %q (the newest change for this artist+field)",
			rows[0].ID, "new-damage")
	}
	if rows[0].Class != BlastClassBlanked {
		t.Errorf("Class = %q, want %q", rows[0].Class, BlastClassBlanked)
	}
}

// TestListBlastRadius_ClassFilter narrows to one damage class.
func TestListBlastRadius_ClassFilter(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	for _, tc := range []struct {
		class string
		want  []string
	}{
		{BlastClassBlanked, []string{"c-scan-blank", "c-rule-blank", "c-man-blank"}},
		{BlastClassReplaced, []string{"c-prov-repl", "c-imp-repl", "c-man-repl"}},
	} {
		rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{Class: tc.class})
		if err != nil {
			t.Fatalf("ListBlastRadius(class=%s): %v", tc.class, err)
		}
		got := blastIDSet(rows)
		if len(got) != len(tc.want) {
			t.Errorf("class=%s rows = %v, want %v", tc.class, blastIDs(rows), tc.want)
			continue
		}
		for _, id := range tc.want {
			if _, ok := got[id]; !ok {
				t.Errorf("class=%s missing %q; got %v", tc.class, id, blastIDs(rows))
			}
		}
	}
}

// TestListBlastRadius_ArtistAndFieldFilters narrows by artist and by field.
// These two narrow INSIDE the ranking CTE, which is safe only because removing
// whole partitions cannot change which row is newest within a surviving one.
func TestListBlastRadius_ArtistAndFieldFilters(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{ArtistID: "a-3"})
	if err != nil {
		t.Fatalf("ListBlastRadius(artist): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("artist filter rows = %v, want the 2 rows for a-3", blastIDs(rows))
	}
	for i := range rows {
		if rows[i].ArtistID != "a-3" {
			t.Errorf("artist filter leaked row for %q", rows[i].ArtistID)
		}
	}

	rows, err = svc.ListBlastRadius(ctx, BlastRadiusFilter{Field: "biography"})
	if err != nil {
		t.Fatalf("ListBlastRadius(field): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "c-scan-blank" {
		t.Errorf("field filter rows = %v, want just c-scan-blank", blastIDs(rows))
	}
}

// TestListBlastRadius_ArtistFilterDoesNotResurrectRecoveredFields is the
// regression guard for narrowing inside the CTE. Filtering to one artist must
// not change what is considered that artist's latest change.
func TestListBlastRadius_ArtistFilterDoesNotResurrectRecoveredFields(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	at := func(min int) time.Time { return blastFixtureBase.Add(time.Duration(min) * time.Minute) }

	seedBlastChange(t, db, "f-damage", "f-1", "moods", "Energetic", "", "scan", at(1))
	seedBlastChange(t, db, "f-revert", "f-1", "moods", "", "Energetic", "revert", at(2))

	rows, err := svc.ListBlastRadius(context.Background(),
		BlastRadiusFilter{ArtistID: "f-1", Field: "moods"})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("narrowing to one artist+field resurrected a recovered row: %v", blastIDs(rows))
	}
}

// TestListBlastRadius_Pagination checks limit/offset walk the full set without
// dropping or duplicating a row.
func TestListBlastRadius_Pagination(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	seen := map[string]bool{}
	for offset := 0; offset < 6; offset += 2 {
		rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("ListBlastRadius(offset=%d): %v", offset, err)
		}
		if len(rows) != 2 {
			t.Fatalf("offset=%d returned %d rows, want 2", offset, len(rows))
		}
		for i := range rows {
			if seen[rows[i].ID] {
				t.Errorf("row %q returned on more than one page", rows[i].ID)
			}
			seen[rows[i].ID] = true
		}
	}
	if len(seen) != 6 {
		t.Errorf("paged through %d distinct rows, want 6", len(seen))
	}
}

// TestListBlastRadius_SortOrder checks EVERY sort key produces the documented
// ordering, ascending and descending. created_at descending is the default.
//
// All three keys are covered because each selects a different ORDER BY clause
// naming a different projected column, so exercising only created_at would
// leave the artist_name and field branches unvalidated: a clause that named the
// wrong column would compile, run, and silently sort by something else.
//
// It uses its own three-row fixture rather than the shared one, and that is the
// load-bearing detail. In the shared fixture the artists happen to sort into the
// same order as the fields, so a clause sorting by the WRONG column still looks
// correct (mutation-tested: pointing the artist_name branch at "field" kept a
// shared-fixture version of this test green). Here artist name, field, and
// timestamp are deliberately three DIFFERENT permutations of the same three
// rows, so only the right column produces the expected sequence.
func TestListBlastRadius_SortOrder(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	// Three damaged rows. Read down each column: the artist names ascend as
	// zz, mm, aa; the fields ascend as biography, type, moods; the timestamps
	// ascend as +3, +1, +2. No two of those agree.
	seedBlastChange(t, db, "s-1", "zz-artist", "biography", "a real bio", "", "scan",
		blastFixtureBase.Add(3*time.Minute))
	seedBlastChange(t, db, "s-2", "mm-artist", "type", "group", "solo", "scan",
		blastFixtureBase.Add(1*time.Minute))
	seedBlastChange(t, db, "s-3", "aa-artist", "moods", "Energetic", "", "scan",
		blastFixtureBase.Add(2*time.Minute))

	// Sanity check on the default: newest first, without naming a sort key.
	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius (default): %v", err)
	}
	if got := blastIDs(rows); len(got) != 3 || got[0] != "s-1" {
		t.Errorf("default sort gave %v, want newest-first starting with s-1", got)
	}

	for _, tc := range []struct {
		name string
		sort string
		// wantAsc is the expected ascending row-id sequence for this key.
		wantAsc []string
	}{
		// Timestamps +1, +2, +3 belong to s-2, s-3, s-1.
		{name: "created_at", sort: BlastSortCreatedAt, wantAsc: []string{"s-2", "s-3", "s-1"}},
		// Artist names aa, mm, zz belong to s-3, s-2, s-1.
		{name: "artist_name", sort: BlastSortArtistName, wantAsc: []string{"s-3", "s-2", "s-1"}},
		// Fields biography, moods, type belong to s-1, s-3, s-2.
		{name: "field", sort: BlastSortField, wantAsc: []string{"s-1", "s-3", "s-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, order := range []string{"asc", "desc"} {
				want := make([]string, len(tc.wantAsc))
				copy(want, tc.wantAsc)
				if order == "desc" {
					for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
						want[i], want[j] = want[j], want[i]
					}
				}

				rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{Sort: tc.sort, Order: order})
				if err != nil {
					t.Fatalf("ListBlastRadius(sort=%s, order=%s): %v", tc.sort, order, err)
				}
				got := blastIDs(rows)
				if len(got) != len(want) {
					t.Fatalf("sort=%s order=%s returned %d rows (%v), want %d",
						tc.sort, order, len(got), got, len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("sort=%s order=%s gave %v, want %v", tc.sort, order, got, want)
						break
					}
				}
			}
		})
	}
}

// TestBlastAttribution_SQLAndGoClassifierAgree is the guard against the SQL
// predicates and the Go classifier drifting apart.
//
// They used to be two hand-written copies, and their "unknown" halves
// disagreed: SQL said unknown meant source = 'manual' exactly, while Go said
// unknown meant anything not recognized as automated. A source that was neither
// automated nor literally "manual" was labeled unknown on the row but matched
// NEITHER count predicate, so it appeared in the list and vanished from the
// totals. Both are now derived from one pair of collections, and the SQL
// unknown predicate is the exact complement of the automated one.
//
// The two cases that would have caught the original defect are "revert" (a
// source this codebase really writes) and "webhook" (standing in for any source
// value a future writer might introduce).
func TestBlastAttribution_SQLAndGoClassifierAgree(t *testing.T) {
	t.Parallel()
	_, db := setupHistoryTestDB(t)
	ctx := context.Background()

	// evalPredicate runs a SQL predicate against a single literal source value
	// by wrapping it in a one-row subquery that exposes that value as a column
	// named "source". This tests the predicate builder itself, independently of
	// any seeded rows.
	evalPredicate := func(t *testing.T, predicate, source string) bool {
		t.Helper()
		var got int
		q := `SELECT CASE WHEN ` + predicate + ` THEN 1 ELSE 0 END FROM (SELECT ? AS source)`
		if err := db.QueryRowContext(ctx, q, source).Scan(&got); err != nil {
			t.Fatalf("evaluating %q for source %q: %v", predicate, source, err)
		}
		return got == 1
	}

	automatedPred := blastAttributionPredicate(BlastAttributionAutomated, "source")
	unknownPred := blastAttributionPredicate(BlastAttributionUnknown, "source")

	for _, tc := range []struct {
		source string
		want   string
	}{
		{"scan", BlastAttributionAutomated},
		{"import", BlastAttributionAutomated},
		{"provider:musicbrainz", BlastAttributionAutomated},
		{"rule:r-42", BlastAttributionAutomated},
		{"manual", BlastAttributionUnknown},
		// The two that exposed the divergence. Neither is automated, so both
		// must be unknown on BOTH sides.
		{"revert", BlastAttributionUnknown},
		{"webhook", BlastAttributionUnknown},
		// A source nobody has thought of yet must still land in a bucket.
		{"", BlastAttributionUnknown},
	} {
		if got := classifyBlastAttribution(tc.source); got != tc.want {
			t.Errorf("classifyBlastAttribution(%q) = %q, want %q", tc.source, got, tc.want)
		}

		inAutomated := evalPredicate(t, automatedPred, tc.source)
		inUnknown := evalPredicate(t, unknownPred, tc.source)

		// Exhaustive and mutually exclusive: exactly one bucket, always. This
		// is the property that makes a row-vs-count disagreement impossible.
		if inAutomated == inUnknown {
			t.Errorf("source %q matched automated=%v unknown=%v; every source must "+
				"match exactly one bucket or it can be listed and counted nowhere",
				tc.source, inAutomated, inUnknown)
			continue
		}
		sqlBucket := BlastAttributionUnknown
		if inAutomated {
			sqlBucket = BlastAttributionAutomated
		}
		if sqlBucket != tc.want {
			t.Errorf("SQL put source %q in bucket %q, want %q (Go classifier says %q)",
				tc.source, sqlBucket, tc.want, classifyBlastAttribution(tc.source))
		}
	}
}

// TestCountBlastRadius_UnrecognizedSourceIsListedAndCounted is the end-to-end
// half of the same guard: an unrecognized source must not appear in the rows
// while being missing from the totals.
//
// "webhook" is not a source this codebase writes today; it stands in for any
// future writer's source value. Before the fix it was labeled unknown on the
// row but matched neither count predicate, so the operator saw one row and a
// total of zero.
func TestCountBlastRadius_UnrecognizedSourceIsListedAndCounted(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	seedBlastChange(t, db, "u-webhook", "u-1", "biography", "a real bio", "", "webhook", blastFixtureBase)
	seedBlastChange(t, db, "u-scan", "u-2", "moods", "Energetic", "", "scan", blastFixtureBase.Add(time.Minute))

	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	got := blastIDSet(rows)
	row, ok := got["u-webhook"]
	if !ok {
		t.Fatalf("unrecognized-source damage was dropped from the report; got %v", blastIDs(rows))
	}
	if row.Attribution != BlastAttributionUnknown {
		t.Errorf("row %q Attribution = %q, want %q", row.ID, row.Attribution, BlastAttributionUnknown)
	}

	counts, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("CountBlastRadius: %v", err)
	}
	if counts.Unknown != 1 {
		t.Errorf("counts.Unknown = %d, want 1; the unrecognized-source row is listed "+
			"but missing from the totals", counts.Unknown)
	}
	if counts.Automated != 1 {
		t.Errorf("counts.Automated = %d, want 1", counts.Automated)
	}
	// The invariant that matters to an operator: the buckets account for every
	// row the report shows.
	if counts.Automated+counts.Unknown != len(rows) {
		t.Errorf("counts split %d automated + %d unknown = %d, but the report lists %d rows (%v); "+
			"a total that omits listed rows is the defect this report exists to avoid",
			counts.Automated, counts.Unknown, counts.Automated+counts.Unknown, len(rows), blastIDs(rows))
	}

	// The unknown-only filter must also surface it, or narrowing becomes a way
	// to hide the rows Stillwater cannot attribute.
	rows, err = svc.ListBlastRadius(ctx, BlastRadiusFilter{Attribution: BlastAttributionUnknown})
	if err != nil {
		t.Fatalf("ListBlastRadius(unknown): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "u-webhook" {
		t.Errorf("unknown-only filter returned %v, want just u-webhook", blastIDs(rows))
	}
}

// TestBlastRadiusFilter_ValidateCoercesUnknownValues proves no caller-supplied
// text reaches SQL. Every free-text axis that feeds a clause is coerced to a
// known-good value rather than interpolated.
func TestBlastRadiusFilter_ValidateCoercesUnknownValues(t *testing.T) {
	t.Parallel()

	f := BlastRadiusFilter{
		Class:       "'; DROP TABLE artists; --",
		Attribution: "bogus",
		Sort:        "created_at) --",
		Order:       "sideways",
		Limit:       -5,
		Offset:      -1,
	}
	f.Validate()

	if f.Class != BlastScopeAll {
		t.Errorf("Class = %q, want %q", f.Class, BlastScopeAll)
	}
	if f.Attribution != BlastScopeAll {
		t.Errorf("Attribution = %q, want %q", f.Attribution, BlastScopeAll)
	}
	if f.Sort != BlastSortCreatedAt {
		t.Errorf("Sort = %q, want %q", f.Sort, BlastSortCreatedAt)
	}
	if f.Order != "desc" {
		t.Errorf("Order = %q, want %q", f.Order, "desc")
	}
	if f.Limit != 50 {
		t.Errorf("Limit = %d, want the default 50", f.Limit)
	}
	if f.Offset != 0 {
		t.Errorf("Offset = %d, want 0", f.Offset)
	}

	over := BlastRadiusFilter{Limit: 5000}
	over.Validate()
	if over.Limit != 500 {
		t.Errorf("Limit = %d, want the 500 cap", over.Limit)
	}
}

// TestListBlastRadius_HostileFilterValuesReturnTheUnnarrowedReport is the
// end-to-end companion to the Validate unit test: a hostile Class value must
// produce the normal report, not an error and not an empty one. A read-only
// report that 500s or silently empties on a malformed query param is a way to
// make damage invisible.
func TestListBlastRadius_HostileFilterValuesReturnTheUnnarrowedReport(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	// The seeded row total, read BEFORE the hostile query. Counting rather than
	// hardcoding keeps this honest if the fixture grows.
	var seeded int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes`).Scan(&seeded); err != nil {
		t.Fatalf("counting seeded rows: %v", err)
	}
	if seeded == 0 {
		t.Fatal("precondition failed: the fixture seeded no rows, so the " +
			"after-query count below would prove nothing")
	}

	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{
		Class:       "' OR 1=1 --",
		Attribution: "' OR 1=1 --",
		Sort:        "'; DROP TABLE metadata_changes; --",
	})
	if err != nil {
		t.Fatalf("hostile filter values errored: %v", err)
	}
	if len(rows) != 6 {
		t.Errorf("hostile filter returned %d rows, want the full 6-row report", len(rows))
	}

	// The table is still there AND still intact. Asserting the count matches
	// what was seeded is the part with teeth: a query that merely SUCCEEDS
	// proves nothing, because a hostile value that deleted some rows would
	// leave a perfectly queryable table behind.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes`).Scan(&n); err != nil {
		t.Fatalf("metadata_changes is gone: %v", err)
	}
	if n != seeded {
		t.Errorf("metadata_changes holds %d rows after the hostile query, want the %d seeded; "+
			"rows were destroyed", n, seeded)
	}
}

// TestListBlastRadius_EmptyLibrary returns an empty slice, not nil, so JSON
// encoders emit [] rather than null.
func TestListBlastRadius_EmptyLibrary(t *testing.T) {
	t.Parallel()
	svc, _ := setupHistoryTestDB(t)
	ctx := context.Background()

	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	if rows == nil {
		t.Error("rows is nil; want an empty slice so JSON encodes [] not null")
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want empty", blastIDs(rows))
	}

	counts, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("CountBlastRadius: %v", err)
	}
	if counts != (BlastRadiusCounts{}) {
		t.Errorf("counts = %+v, want all zero", counts)
	}
}

// TestListBlastRadius_OrphanedRowsAreExcluded proves the JOIN to artists is
// load-bearing. metadata_changes.artist_id carries ON DELETE CASCADE, so
// deleting an artist takes its history with it -- which is exactly why the
// operator docs must say the report is retained until an artist is deleted or
// merged away, not "indefinitely".
func TestListBlastRadius_OrphanedRowsAreExcluded(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	seedBlastChange(t, db, "o-damage", "o-1", "biography", "a real bio", "", "scan", blastFixtureBase)

	// PRECONDITION: reported while the artist exists.
	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius (precondition): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("precondition failed: damage not reported; got %v", blastIDs(rows))
	}

	// Deleting the artist cascades the history away. This is the real retention
	// boundary, and it applies to a merge too: merging an artist deletes the
	// loser row, so its blast-radius history goes with it.
	if _, err := db.ExecContext(ctx, `DELETE FROM artists WHERE id = ?`, "o-1"); err != nil {
		t.Fatalf("deleting artist: %v", err)
	}

	rows, err = svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius (after delete): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v after the artist was deleted, want none", blastIDs(rows))
	}
}

// TestBlastRadiusCoverageIsTrackableFields pins the report's coverage to the
// set of fields metadata_changes actually records, and asserts the two facts
// the operator-facing caveat rests on.
//
// This is the guard against someone hand-typing the field list into the UI: if
// trackableFields gains or loses a member, a hardcoded list silently starts
// lying about what the report can see.
func TestBlastRadiusCoverageIsTrackableFields(t *testing.T) {
	t.Parallel()

	covered := map[string]bool{}
	for _, f := range TrackableFields() {
		covered[f] = true
	}

	// Fields the report CAN see. These are the ones Service.update diffs.
	// "name" and "sort_name" MOVED here from the CANNOT-see list below in
	// #3037: the name_language_pref rule fixer overwrites both on a non-empty
	// artist, so damage to them had to become observable before it could be
	// recovered. The move is the assertion, inverted deliberately.
	for _, f := range []string{"biography", "genres", "styles", "moods", "type", "gender", "origin", "name", "sort_name"} {
		if !covered[f] {
			t.Errorf("%q is not in TrackableFields(); the report's coverage claim is wrong", f)
		}
	}

	// Fields the report CANNOT see. Damage to these leaves no metadata_changes
	// row on the scan path, so the report must never imply it covers them.
	// disambiguation is the sharpest case: issue #2748 names it among the worst
	// fields to clear, and it has no audit trail at all.
	for _, f := range []string{"disambiguation", "musicbrainz_id"} {
		if covered[f] {
			t.Errorf("%q is now tracked; the coverage caveat claims it is NOT visible "+
				"to this report and must be revisited", f)
		}
	}
}

// TestCountBlastRadius_BucketsFollowEveryAxisExceptAttribution pins the counting
// contract that BlastRadiusCounts' doc comment states.
//
// The rule is easy to misread as a bug, which is why it is pinned rather than
// only described: Class, Field and ArtistID all narrow the bucket counts, and
// Attribution alone does not. Class reaches the damage clause; Field and
// ArtistID sit inside the ranking CTE, which the counting query builds too.
//
// Only the attribution exemption is a HAZARD requiring protection: it is the one
// filter whose purpose is to hide a bucket, so letting it shrink the counts would
// let an operator narrow to "automated" and read "0 of unknown origin" over a
// library where nothing is attributable. The other axes carry no such hazard.
//
// Mutation proving teeth: passing BlastScopeAll for class in CountBlastRadius's
// damage clause fails the class case; dropping the attribution neutralization
// fails the attribution cases.
func TestCountBlastRadius_BucketsFollowEveryAxisExceptAttribution(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	base, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("CountBlastRadius(neutral): %v", err)
	}
	// Precondition: the fixture has rows in BOTH buckets, or "the counts did not
	// move" is satisfied by a fixture that never had anything to move.
	if base.Automated == 0 || base.Unknown == 0 {
		t.Fatalf("precondition: fixture reports automated=%d unknown=%d; both must be non-zero",
			base.Automated, base.Unknown)
	}

	// NARROWING axes: each must shrink at least one bucket.
	for _, tc := range []struct {
		name string
		f    BlastRadiusFilter
	}{
		{"class", BlastRadiusFilter{Class: BlastClassBlanked}},
		{"field", BlastRadiusFilter{Field: "biography"}},
		// a-1, a SEEDED artist. An unseeded id (this read "art-1", which
		// seedBlastFixture never creates -- it seeds a-1..a-6) matches ZERO
		// rows, and zero rows satisfies "the buckets moved" for the wrong
		// reason: the case then proves only that an empty result is empty, and
		// would keep passing if artist_id narrowing broke entirely. The
		// dedicated no-match case below covers the empty path deliberately.
		{"artist_id", BlastRadiusFilter{ArtistID: "a-1"}},
	} {
		t.Run(tc.name+" narrows the buckets", func(t *testing.T) {
			got, err := svc.CountBlastRadius(ctx, tc.f)
			if err != nil {
				t.Fatalf("CountBlastRadius: %v", err)
			}
			// PRECONDITION: the narrowing MATCHED SOMETHING. Without this, an
			// axis that matches nothing passes the "buckets moved" assertion
			// below by collapsing both to zero -- which is what an unseeded id
			// did here. A narrowing axis has to be shown to narrow, not to
			// annihilate.
			if got.Automated == 0 && got.Unknown == 0 {
				t.Fatalf("%s narrowing matched NO rows (automated=0 unknown=0), so the assertion below would "+
					"pass whether or not this axis narrows correctly; the filter value is not in the fixture",
					tc.name)
			}
			if got.Automated == base.Automated && got.Unknown == base.Unknown {
				t.Errorf("%s narrowing left the buckets at automated=%d unknown=%d, unchanged from the "+
					"unfiltered %d/%d. The doc comment says this axis narrows them; if that changed, the "+
					"comment and the empty-state sentence that depends on it must change too.",
					tc.name, got.Automated, got.Unknown, base.Automated, base.Unknown)
			}
		})
	}

	// ATTRIBUTION: must NEVER move either bucket. This is the honesty guarantee.
	for _, attr := range []string{BlastAttributionAutomated, BlastAttributionUnknown} {
		t.Run("attribution="+attr+" does NOT narrow the buckets", func(t *testing.T) {
			got, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{Attribution: attr})
			if err != nil {
				t.Fatalf("CountBlastRadius: %v", err)
			}
			if got.Automated != base.Automated || got.Unknown != base.Unknown {
				t.Errorf("attribution=%s moved the buckets to automated=%d unknown=%d from %d/%d. The "+
					"attribution filter must never be able to hide a bucket: an operator narrowing to one "+
					"attribution would read the other as zero over a library that is full of it.",
					attr, got.Automated, got.Unknown, base.Automated, base.Unknown)
			}
		})
	}
}

// TestCountBlastRadius_TotalUnfilteredIsLibraryWide pins the one count that is
// genuinely library-wide.
//
// The empty-state sentence quotes it ("the report still records N change(s)
// overall"), and that sentence renders only when a filter matched nothing. Every
// other count in the struct is filter-scoped, so quoting one there produces a
// number that is structurally zero on the exact view that displays it -- while
// the caveat band directly above reports the real damage. The pane contradicting
// itself in adjacent elements is what this field exists to prevent.
//
// Mutation proving teeth: returning counts.Total, or Automated+Unknown, for
// TotalUnfiltered fails every narrowed case below.
func TestCountBlastRadius_TotalUnfilteredIsLibraryWide(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	base, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("CountBlastRadius(neutral): %v", err)
	}
	want := base.TotalUnfiltered
	// Precondition: the library holds damage, or "unchanged" is trivially true.
	if want == 0 {
		t.Fatalf("precondition: fixture reports TotalUnfiltered=0; there is nothing for a filter to hide")
	}
	if want != base.Automated+base.Unknown {
		t.Fatalf("on an unfiltered request TotalUnfiltered=%d must equal Automated+Unknown=%d",
			want, base.Automated+base.Unknown)
	}

	for _, tc := range []struct {
		name string
		f    BlastRadiusFilter
	}{
		{"class", BlastRadiusFilter{Class: BlastClassBlanked}},
		{"field", BlastRadiusFilter{Field: "biography"}},
		// a-1 is SEEDED. This read "art-1", which seedBlastFixture never
		// creates, so the case narrowed to nothing -- and TotalUnfiltered is
		// library-wide precisely BECAUSE it ignores the filter, which makes
		// "unchanged" true for a no-match filter whether or not the field is
		// computed correctly. A seeded id means the surrounding counts really
		// do move while TotalUnfiltered stays put, which is the property.
		{"artist_id", BlastRadiusFilter{ArtistID: "a-1"}},
		{"attribution", BlastRadiusFilter{Attribution: BlastAttributionAutomated}},
		// KEPT DELIBERATELY: a filter that matches nothing is the case the
		// empty-state sentence actually renders on, so it is worth covering on
		// purpose -- just not by accident, which is what the unseeded id did.
		{"a filter matching nothing", BlastRadiusFilter{Field: "no_such_field_xyz"}},
		{"every axis at once", BlastRadiusFilter{
			Class: BlastClassBlanked, Attribution: BlastAttributionUnknown,
			Field: "biography", ArtistID: "a-1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.CountBlastRadius(ctx, tc.f)
			if err != nil {
				t.Fatalf("CountBlastRadius: %v", err)
			}
			if got.TotalUnfiltered != want {
				t.Errorf("TotalUnfiltered=%d under a %s filter, want the library-wide %d. The empty-state "+
					"sentence quotes this to tell an operator how much the report still holds behind their "+
					"filter; a filter-scoped value there is zero on the very view that renders it.",
					got.TotalUnfiltered, tc.name, want)
			}
		})
	}
}

// TestCountBlastRadius_QueryFailurePropagates covers the database-error branch
// in CountBlastRadius: a failed count must surface as an error, never as a zero.
//
// That distinction is the whole point on this report. A swallowed error returns
// a zero-valued BlastRadiusCounts, which the pane renders as "0 automated, 0 of
// unknown origin" -- an all-clear over a library whose damage could not be
// counted. The buckets exist precisely so an unknown state is never displayed as
// a clean one.
//
// BOTH error branches are covered, one per subtest, and each is injected where
// it is being asserted rather than upstream. That is why countAllBlastRadius is
// its own method: inline as the second of two queries against the same tables,
// no fault could isolate it -- a closed handle or a dropped table fails the
// FIRST query, so execution never arrived at the second and a test claiming to
// cover it would have passed for the wrong reason.
func TestCountBlastRadius_QueryFailurePropagates(t *testing.T) {
	t.Parallel()

	t.Run("the bucket-count query", func(t *testing.T) {
		svc, db := setupHistoryTestDB(t)
		seedBlastFixture(t, db)
		ctx := context.Background()

		// Precondition: the count succeeds before the fault, so a later error is
		// attributable to the closed handle rather than to a malformed filter,
		// and the fixture has damage, so a zero-valued result after the fault is
		// distinguishable from a correct one.
		before, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
		if err != nil {
			t.Fatalf("precondition: CountBlastRadius failed before the fault was injected: %v", err)
		}
		if before.Automated+before.Unknown == 0 {
			t.Fatalf("precondition: the fixture reports no damage")
		}

		if err := db.Close(); err != nil {
			t.Fatalf("closing db to inject the query failure: %v", err)
		}

		counts, err := svc.CountBlastRadius(ctx, BlastRadiusFilter{})
		if err == nil {
			t.Fatalf("CountBlastRadius returned a nil error over a closed database, with counts %+v. A "+
				"swallowed error yields a zero-valued result, which this report renders as an all-clear "+
				"over a library it could not count.", counts)
		}
		if counts.Automated != 0 || counts.Unknown != 0 || counts.TotalUnfiltered != 0 {
			t.Errorf("the error path returned partially populated counts %+v; a caller that ignores the "+
				"error would render them as real numbers", counts)
		}
	})

	t.Run("the library-wide count", func(t *testing.T) {
		svc, db := setupHistoryTestDB(t)
		seedBlastFixture(t, db)
		ctx := context.Background()

		// Reached through the repository directly, which is the point of the
		// extraction: called via CountBlastRadius this branch sits behind
		// another query over the same tables, so any fault big enough to break
		// it breaks that one first.
		repo, ok := svc.repo.(*sqliteHistoryRepo)
		if !ok {
			t.Fatalf("history service is not backed by sqliteHistoryRepo (%T); this test cannot reach the "+
				"branch it names", svc.repo)
		}

		// Precondition: it succeeds before the fault and reports real damage.
		total, err := repo.countAllBlastRadius(ctx)
		if err != nil {
			t.Fatalf("precondition: countAllBlastRadius failed before the fault: %v", err)
		}
		if total == 0 {
			t.Fatalf("precondition: the fixture reports no damage, so a zero after the fault would be " +
				"indistinguishable from a correct answer")
		}

		if err := db.Close(); err != nil {
			t.Fatalf("closing db: %v", err)
		}

		got, err := repo.countAllBlastRadius(ctx)
		if err == nil {
			t.Fatalf("countAllBlastRadius returned a nil error over a closed database, with total %d. The "+
				"empty-state sentence quotes this number to tell an operator how much the report still "+
				"holds behind their filter; a swallowed error renders that as zero.", got)
		}
		if got != 0 {
			t.Errorf("the error path returned total=%d; it must return a zero the caller cannot mistake "+
				"for an answer", got)
		}
	})
}
