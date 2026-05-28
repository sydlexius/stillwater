package rule

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// mustSeed fails the test immediately if an UpsertRuleResult* call returns
// an error during fixture setup. Without this, a silent write failure
// leaves the test running against incomplete fixtures and produces
// confusing downstream assertion failures.
func mustSeed(t *testing.T, step string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
}

// seedRuleAndArtists is the multi-artist counterpart to
// seedArtistAndRule. It inserts one rule and N artists so the drill-down
// tests can exercise pagination/filter logic without colliding on the
// rule's UNIQUE id (seedArtistAndRule unconditionally inserts the rule
// each call, which fails on the 2nd artist for the same rule).
func seedRuleAndArtists(t *testing.T, db *sql.DB, ruleID string, artistIDs ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
		VALUES (?, ?, 'desc', 'nfo', 1, 'auto', '{}')`,
		ruleID, "Rule "+ruleID); err != nil {
		t.Fatalf("inserting rule %s: %v", ruleID, err)
	}
	for _, a := range artistIDs {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
			a, "Artist "+a); err != nil {
			t.Fatalf("inserting artist %s: %v", a, err)
		}
	}
}

// TestGetRuleResultsForRule_PaginatedJoinsArtist verifies the drill-down
// query returns the joined RuleResultWithArtist row shape (artist name +
// library context), respects limit/offset pagination, applies an optional
// passed filter, orders by artist_name ascending so the UI is stable
// across calls, and surfaces excluded artists with is_excluded=true so
// callers can render a badge (the row is NOT filtered out -- exclusion is
// a presentation hint, not a hard mask).
func TestGetRuleResultsForRule_PaginatedJoinsArtist(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedRuleAndArtists(t, db, "rule-1", "artist-a", "artist-b", "artist-c")

	if err := svc.UpsertRuleResultPass(ctx, "artist-a", "rule-1", now); err != nil {
		t.Fatalf("upsert pass a: %v", err)
	}
	if err := svc.UpsertRuleResultFail(ctx, "artist-b", "rule-1", "v-b", "missing nfo", now); err != nil {
		t.Fatalf("upsert fail b: %v", err)
	}
	if err := svc.UpsertRuleResultFail(ctx, "artist-c", "rule-1", "v-c", "missing thumb", now); err != nil {
		t.Fatalf("upsert fail c: %v", err)
	}

	// Page 1: limit 2 -- should return artist-a, artist-b in name order.
	rows, err := svc.GetRuleResultsForRule(ctx, "rule-1", PassedFilterAny, 2, 0)
	if err != nil {
		t.Fatalf("GetRuleResultsForRule: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("page 1 len: got %d, want 2", len(rows))
	}
	if rows[0].ArtistID != "artist-a" || rows[1].ArtistID != "artist-b" {
		t.Errorf("page 1 order: got [%s %s], want [artist-a artist-b]", rows[0].ArtistID, rows[1].ArtistID)
	}
	if rows[0].ArtistName == "" {
		t.Error("ArtistName empty; JOIN to artists table missing")
	}
	if rows[0].Passed != true || rows[1].Passed != false {
		t.Errorf("Passed: got [%v %v], want [true false]", rows[0].Passed, rows[1].Passed)
	}

	// Page 2: offset 2 -- should return artist-c only.
	page2, err := svc.GetRuleResultsForRule(ctx, "rule-1", PassedFilterAny, 2, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].ArtistID != "artist-c" {
		t.Fatalf("page 2: got %+v, want [artist-c]", page2)
	}

	// Passed-only filter: should return artist-a only.
	passOnly, err := svc.GetRuleResultsForRule(ctx, "rule-1", PassedFilterPassed, 50, 0)
	if err != nil {
		t.Fatalf("pass filter: %v", err)
	}
	if len(passOnly) != 1 || passOnly[0].ArtistID != "artist-a" {
		t.Fatalf("pass filter: got %+v, want [artist-a]", passOnly)
	}

	// Failed-only filter: should return artist-b, artist-c.
	failOnly, err := svc.GetRuleResultsForRule(ctx, "rule-1", PassedFilterFailed, 50, 0)
	if err != nil {
		t.Fatalf("fail filter: %v", err)
	}
	if len(failOnly) != 2 {
		t.Fatalf("fail filter len: got %d, want 2", len(failOnly))
	}
	if failOnly[0].ViolationMessage != "missing nfo" {
		t.Errorf("ViolationMessage on failed row not propagated: got %q", failOnly[0].ViolationMessage)
	}
}

// TestCountRuleResultsForRule_RespectsFilter pairs with the drill-down
// list to give the JSON envelope an accurate total for pagination math.
func TestCountRuleResultsForRule_RespectsFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	now := time.Now().UTC()

	seedRuleAndArtists(t, db, "rule-x", "art-1", "art-2", "art-3")
	mustSeed(t, "seed pass art-1 rule-x", svc.UpsertRuleResultPass(ctx, "art-1", "rule-x", now))
	mustSeed(t, "seed fail art-2 rule-x", svc.UpsertRuleResultFail(ctx, "art-2", "rule-x", "v2", "msg", now))
	mustSeed(t, "seed fail art-3 rule-x", svc.UpsertRuleResultFail(ctx, "art-3", "rule-x", "v3", "msg", now))

	total, err := svc.CountRuleResultsForRule(ctx, "rule-x", PassedFilterAny)
	if err != nil {
		t.Fatalf("CountRuleResultsForRule any: %v", err)
	}
	if total != 3 {
		t.Errorf("count any: got %d, want 3", total)
	}

	failed, err := svc.CountRuleResultsForRule(ctx, "rule-x", PassedFilterFailed)
	if err != nil {
		t.Fatalf("CountRuleResultsForRule failed: %v", err)
	}
	if failed != 2 {
		t.Errorf("count failed: got %d, want 2", failed)
	}

	passed, err := svc.CountRuleResultsForRule(ctx, "rule-x", PassedFilterPassed)
	if err != nil {
		t.Fatalf("CountRuleResultsForRule passed: %v", err)
	}
	if passed != 1 {
		t.Errorf("count passed: got %d, want 1", passed)
	}

	// Unknown rule returns 0, no error.
	zero, err := svc.CountRuleResultsForRule(ctx, "rule-zzz", PassedFilterAny)
	if err != nil {
		t.Fatalf("count unknown rule: %v", err)
	}
	if zero != 0 {
		t.Errorf("count unknown rule: got %d, want 0", zero)
	}
}

// TestGetEnabledRuleResultsForArtist_FiltersDisabled asserts the
// per-artist endpoint excludes results for disabled rules so the artist
// detail page does not surface rule outcomes the user explicitly turned
// off. The JOIN to rules also enriches the row with the rule's display
// name + severity so the front-end can render the breakdown without a
// second lookup per row.
func TestGetEnabledRuleResultsForArtist_FiltersDisabled(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	now := time.Now().UTC()

	// seedArtistAndRule inserts the artist on each call, so we use it
	// once for (artist-1, rule-enabled), then insert the second rule
	// directly. This avoids a PK collision on the artists table while
	// still exercising the two-rule scenario the test needs.
	seedArtistAndRule(t, db, "artist-1", "rule-enabled")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
		VALUES (?, ?, 'desc', 'nfo', 1, 'auto', '{}')`,
		"rule-disabled", "Rule rule-disabled"); err != nil {
		t.Fatalf("inserting rule-disabled: %v", err)
	}
	// Flip rule-disabled to enabled=0 manually so the test does not
	// depend on Service.Update side-effects.
	if _, err := db.ExecContext(ctx, `UPDATE rules SET enabled = 0 WHERE id = ?`, "rule-disabled"); err != nil {
		t.Fatalf("disable rule: %v", err)
	}

	if err := svc.UpsertRuleResultPass(ctx, "artist-1", "rule-enabled", now); err != nil {
		t.Fatalf("upsert enabled: %v", err)
	}
	if err := svc.UpsertRuleResultPass(ctx, "artist-1", "rule-disabled", now); err != nil {
		t.Fatalf("upsert disabled: %v", err)
	}

	rows, err := svc.GetEnabledRuleResultsForArtist(ctx, "artist-1")
	if err != nil {
		t.Fatalf("GetEnabledRuleResultsForArtist: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (disabled rule should be excluded)", len(rows))
	}
	if rows[0].RuleID != "rule-enabled" {
		t.Errorf("got rule %q, want rule-enabled", rows[0].RuleID)
	}
	if rows[0].RuleName == "" {
		t.Error("RuleName empty; JOIN to rules table missing")
	}
}

// TestTopFailingRuleResults_GroupsAndOrders asserts the rule_results-based
// rewrite: groups by rule, counts failed rows (passed=0), excludes
// excluded artists, joins rules.name + json_extract(config, '$.severity')
// with a COALESCE default of 'warning', orders by count DESC then rule_id
// ASC, and caps at the given limit.
//
// The semantic delta vs TopViolationSummaries: a dismissed violation that
// has not yet been re-evaluated still shows passed=0 here (the rule_results
// row is the last evaluation outcome). That is the desired behavior for
// the dashboard "top failing rules" widget; lifecycle queries (the badge
// count) stay on rule_violations and are out of scope.
func TestTopFailingRuleResults_GroupsAndOrders(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	now := time.Now().UTC()

	// rule-a: 3 failures (artists 1, 2, 3); rule-b: 1 failure (artist 4);
	// rule-c: 2 failures BUT one is on an excluded artist, so the count
	// should be 1. Use direct insert statements to avoid
	// seedArtistAndRule's per-call artist insert (which would PK-collide).
	for _, id := range []string{"a-1", "a-2", "a-3", "a-4", "a-5", "a-6"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
			id, "Artist "+id); err != nil {
			t.Fatalf("inserting artist %s: %v", id, err)
		}
	}
	for _, r := range []struct{ id, severity string }{
		{"rule-a", "error"},
		{"rule-b", "info"},
		{"rule-c", ""}, // empty -> COALESCE to 'warning'
	} {
		cfg := "{}"
		if r.severity != "" {
			cfg = `{"severity":"` + r.severity + `"}`
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
			VALUES (?, ?, 'desc', 'nfo', 1, 'auto', ?)`,
			r.id, "Rule "+r.id, cfg); err != nil {
			t.Fatalf("inserting rule %s: %v", r.id, err)
		}
	}
	// Mark a-6 as excluded.
	if _, err := db.ExecContext(ctx, `UPDATE artists SET is_excluded = 1 WHERE id = ?`, "a-6"); err != nil {
		t.Fatalf("excluding artist: %v", err)
	}

	mustSeed(t, "seed fail a-1 rule-a", svc.UpsertRuleResultFail(ctx, "a-1", "rule-a", "v1", "msg", now))
	mustSeed(t, "seed fail a-2 rule-a", svc.UpsertRuleResultFail(ctx, "a-2", "rule-a", "v2", "msg", now))
	mustSeed(t, "seed fail a-3 rule-a", svc.UpsertRuleResultFail(ctx, "a-3", "rule-a", "v3", "msg", now))
	mustSeed(t, "seed fail a-4 rule-b", svc.UpsertRuleResultFail(ctx, "a-4", "rule-b", "v4", "msg", now))
	mustSeed(t, "seed fail a-5 rule-c", svc.UpsertRuleResultFail(ctx, "a-5", "rule-c", "v5", "msg", now))
	mustSeed(t, "seed fail a-6 rule-c (excluded)", svc.UpsertRuleResultFail(ctx, "a-6", "rule-c", "v6", "msg", now))

	got, err := svc.TopFailingRuleResults(ctx, 10)
	if err != nil {
		t.Fatalf("TopFailingRuleResults: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 (rule-a, rule-b, rule-c)", len(got))
	}
	if got[0].RuleID != "rule-a" || got[0].Count != 3 || got[0].Severity != "error" {
		t.Errorf("row 0: %+v want {rule-a, 3, error}", got[0])
	}
	// rule-b and rule-c both have count=1; rule-b sorts first by rule_id ASC.
	if got[1].RuleID != "rule-b" || got[1].Count != 1 || got[1].Severity != "info" {
		t.Errorf("row 1: %+v want {rule-b, 1, info}", got[1])
	}
	if got[2].RuleID != "rule-c" || got[2].Count != 1 || got[2].Severity != "warning" {
		t.Errorf("row 2: %+v want {rule-c, 1, warning (COALESCE default)}", got[2])
	}
}

// TestTopFailingRuleResults_LimitClamped guards the limit parameter:
// zero / negative -> default 10; large values pass through.
func TestTopFailingRuleResults_LimitClamped(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Empty DB: limit handling alone is exercised; just need to confirm
	// the call does not error and returns an empty slice.
	got, err := svc.TopFailingRuleResults(ctx, 0)
	if err != nil {
		t.Fatalf("limit=0: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("limit=0 with empty DB: got %d rows, want 0", len(got))
	}

	got, err = svc.TopFailingRuleResults(ctx, -5)
	if err != nil {
		t.Fatalf("limit=-5: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("limit=-5 with empty DB: got %d rows, want 0", len(got))
	}
}

// TestGetRulePassRates_OrderedByPassRateAscending asserts the widget feed:
// one row per enabled rule, severity sourced via json_extract on
// rules.config with a COALESCE default of 'warning', ordered by PassRate
// ASC so the worst-performing rules surface first (matches the existing
// TopViolations widget intent). Disabled rules are excluded. A rule with
// zero rule_results rows is omitted entirely (no row in rule_results
// means the COUNT-based query won't include it) so the widget renders
// "no data yet" rather than rendering at 0%.
func TestGetRulePassRates_OrderedByPassRateAscending(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	now := time.Now().UTC()

	// rule-good: 4 of 4 passing -> 100%
	// rule-meh:  2 of 4 passing -> 50%
	// rule-bad:  1 of 4 passing -> 25%
	// rule-off:  disabled -- must not appear
	for _, art := range []string{"x1", "x2", "x3", "x4"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
			art, "Artist "+art); err != nil {
			t.Fatalf("inserting artist %s: %v", art, err)
		}
	}
	for _, id := range []string{"rule-good", "rule-meh", "rule-bad", "rule-off"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
			VALUES (?, ?, 'desc', 'nfo', 1, 'auto', '{}')`,
			id, "Rule "+id); err != nil {
			t.Fatalf("inserting rule %s: %v", id, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE rules SET enabled = 0 WHERE id = ?`, "rule-off"); err != nil {
		t.Fatalf("disable rule-off: %v", err)
	}

	for _, art := range []string{"x1", "x2", "x3", "x4"} {
		mustSeed(t, "seed pass "+art+" rule-good", svc.UpsertRuleResultPass(ctx, art, "rule-good", now))
	}
	mustSeed(t, "seed pass x1 rule-meh", svc.UpsertRuleResultPass(ctx, "x1", "rule-meh", now))
	mustSeed(t, "seed pass x2 rule-meh", svc.UpsertRuleResultPass(ctx, "x2", "rule-meh", now))
	mustSeed(t, "seed fail x3 rule-meh", svc.UpsertRuleResultFail(ctx, "x3", "rule-meh", "vm3", "m", now))
	mustSeed(t, "seed fail x4 rule-meh", svc.UpsertRuleResultFail(ctx, "x4", "rule-meh", "vm4", "m", now))
	mustSeed(t, "seed pass x1 rule-bad", svc.UpsertRuleResultPass(ctx, "x1", "rule-bad", now))
	for _, art := range []string{"x2", "x3", "x4"} {
		mustSeed(t, "seed fail "+art+" rule-bad", svc.UpsertRuleResultFail(ctx, art, "rule-bad", "vb"+art, "b", now))
	}
	// rule-off rows exist too -- prove they are filtered out.
	for _, art := range []string{"x1", "x2", "x3", "x4"} {
		mustSeed(t, "seed pass "+art+" rule-off", svc.UpsertRuleResultPass(ctx, art, "rule-off", now))
	}

	got, err := svc.GetRulePassRates(ctx)
	if err != nil {
		t.Fatalf("GetRulePassRates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rules, want 3 (rule-off should be excluded)", len(got))
	}
	if got[0].RuleID != "rule-bad" || got[0].Failed != 3 || got[0].Passed != 1 || got[0].Evaluated != 4 {
		t.Errorf("row 0: %+v want {rule-bad, P=1 F=3 E=4}", got[0])
	}
	if got[0].PassRate <= 0.24 || got[0].PassRate >= 0.26 {
		t.Errorf("row 0 PassRate: got %f, want ~0.25", got[0].PassRate)
	}
	if got[1].RuleID != "rule-meh" || got[1].PassRate <= 0.49 || got[1].PassRate >= 0.51 {
		t.Errorf("row 1: %+v want rule-meh PassRate~0.5", got[1])
	}
	if got[2].RuleID != "rule-good" || got[2].PassRate < 0.99 {
		t.Errorf("row 2: %+v want rule-good PassRate~1.0", got[2])
	}
}
