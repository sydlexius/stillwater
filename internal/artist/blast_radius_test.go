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
// Returned map is change id -> a short description, used only to make failure
// messages readable.
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

// ids extracts change ids in returned order, for order-sensitive assertions.
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

// TestListBlastRadius_SortOrder checks the sort keys produce the documented
// ordering. created_at descending is the default.
func TestListBlastRadius_SortOrder(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedBlastFixture(t, db)
	ctx := context.Background()

	rows, err := svc.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].CreatedAt.Before(rows[i].CreatedAt) {
			t.Fatalf("default sort is not newest-first: %v", blastIDs(rows))
		}
	}

	rows, err = svc.ListBlastRadius(ctx, BlastRadiusFilter{Sort: BlastSortCreatedAt, Order: "asc"})
	if err != nil {
		t.Fatalf("ListBlastRadius(asc): %v", err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].CreatedAt.After(rows[i].CreatedAt) {
			t.Fatalf("asc sort is not oldest-first: %v", blastIDs(rows))
		}
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

	rows, err := svc.ListBlastRadius(context.Background(), BlastRadiusFilter{
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

	// The table is still there.
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM metadata_changes`).Scan(&n); err != nil {
		t.Fatalf("metadata_changes is gone: %v", err)
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
	for _, f := range []string{"biography", "genres", "styles", "moods", "type", "gender", "origin"} {
		if !covered[f] {
			t.Errorf("%q is not in TrackableFields(); the report's coverage claim is wrong", f)
		}
	}

	// Fields the report CANNOT see. Damage to these leaves no metadata_changes
	// row on the scan path, so the report must never imply it covers them.
	// disambiguation is the sharpest case: issue #2748 names it among the worst
	// fields to clear, and it has no audit trail at all.
	for _, f := range []string{"disambiguation", "name", "sort_name", "musicbrainz_id"} {
		if covered[f] {
			t.Errorf("%q is now tracked; the coverage caveat claims it is NOT visible "+
				"to this report and must be revisited", f)
		}
	}
}
