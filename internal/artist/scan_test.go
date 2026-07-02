package artist

import (
	"sort"
	"strings"
	"testing"
)

// TestBuildWhereClause_Empty verifies that no conditions produce no WHERE clause.
func TestBuildWhereClause_Empty(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{})
	if clause != "" {
		t.Errorf("expected empty clause, got %q", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

// TestBuildWhereClause_Search verifies LIKE predicate binding.
func TestBuildWhereClause_Search(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Search: "Beatles"})
	if !strings.Contains(clause, "name LIKE ?") {
		t.Errorf("expected LIKE clause, got %q", clause)
	}
	if len(args) != 1 || args[0] != "%Beatles%" {
		t.Errorf("unexpected args: %v", args)
	}
}

// TestBuildWhereClause_Search_EscapesWildcards verifies that LIKE metacharacters
// in the search term are escaped and the clause carries the matching ESCAPE
// clause, so a literal `%`/`_`/`\` in a search term cannot act as a wildcard.
func TestBuildWhereClause_Search_EscapesWildcards(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Search: `100%_off\path`})
	if !strings.Contains(clause, `LIKE ? ESCAPE '\'`) {
		t.Errorf("expected ESCAPE clause, got %q", clause)
	}
	want := `%100\%\_off\\path%`
	if len(args) != 1 || args[0] != want {
		t.Errorf("unexpected args: %v, want [%q]", args, want)
	}
}

// TestBuildWhereClause_LibraryID verifies single-library EXISTS clause.
func TestBuildWhereClause_LibraryID(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{LibraryID: "lib-1"})
	if !strings.Contains(clause, "EXISTS") {
		t.Errorf("expected EXISTS clause, got %q", clause)
	}
	if !strings.Contains(clause, "artist_libraries") {
		t.Errorf("expected artist_libraries reference, got %q", clause)
	}
	if len(args) != 1 || args[0] != "lib-1" {
		t.Errorf("unexpected args: %v", args)
	}
}

// TestBuildWhereClause_IDsFilter verifies IN-clause construction and argument binding.
func TestBuildWhereClause_IDsFilter(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{IDs: []string{"id-1", "id-2", "id-3"}})
	if !strings.Contains(clause, "artists.id IN (?, ?, ?)") {
		t.Errorf("expected IN clause, got %q", clause)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(args), args)
	}
}

// TestBuildWhereClause_HealthScore verifies numeric range bindings.
func TestBuildWhereClause_HealthScore(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{HealthScoreMin: 50, HealthScoreMax: 90})
	if !strings.Contains(clause, "health_score >= ?") {
		t.Errorf("expected min condition, got %q", clause)
	}
	if !strings.Contains(clause, "health_score <= ?") {
		t.Errorf("expected max condition, got %q", clause)
	}
	if len(args) != 2 || args[0] != 50 || args[1] != 90 {
		t.Errorf("unexpected args: %v", args)
	}
}

// TestBuildWhereClause_LegacyFilter covers every legacy params.Filter value to
// ensure the map-based dispatch is complete and no key was dropped.
func TestBuildWhereClause_LegacyFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		filter  string
		wantSQL string
	}{
		{"missing_nfo", "nfo_exists = 0"},
		{"missing_thumb", "image_type = 'thumb'"},
		{"missing_fanart", "image_type = 'fanart'"},
		{"missing_logo", "image_type = 'logo'"},
		{"missing_banner", "image_type = 'banner'"},
		{"missing_mbid", "provider = 'musicbrainz'"},
		{"excluded", "is_excluded = 1"},
		{"not_excluded", "is_excluded = 0"},
		{"locked", "locked = 1"},
		{"not_locked", "locked = 0"},
		{"compliant", "health_score >= 100"},
		{"non_compliant", "health_score < 100"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.filter, func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filter: tc.filter})
			if !strings.Contains(clause, tc.wantSQL) {
				t.Errorf("filter %q: expected %q in clause %q", tc.filter, tc.wantSQL, clause)
			}
			// Legacy filter keys produce no bound parameters.
			if len(args) != 0 {
				t.Errorf("filter %q: expected no args, got %v", tc.filter, args)
			}
		})
	}
}

// TestBuildWhereClause_UnknownLegacyFilter verifies that an unrecognized
// params.Filter value is silently ignored (no panic, no WHERE clause).
func TestBuildWhereClause_UnknownLegacyFilter(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filter: "does_not_exist"})
	if clause != "" {
		t.Errorf("expected empty clause for unknown filter, got %q", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

// TestBuildWhereClause_MultiFilter_MissingMeta covers the missing_meta predicate
// for both include and exclude states.
func TestBuildWhereClause_MultiFilter_MissingMeta(t *testing.T) {
	t.Parallel()
	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"missing_meta": "include"}})
		if !strings.Contains(clause, "nfo_exists = 0") {
			t.Errorf("include: expected nfo_exists = 0, got %q", clause)
		}
	})
	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"missing_meta": "exclude"}})
		if !strings.Contains(clause, "nfo_exists = 1") {
			t.Errorf("exclude: expected nfo_exists = 1, got %q", clause)
		}
	})
}

// TestBuildWhereClause_MultiFilter_MissingImages verifies that the compound
// missing_images predicate generates OR clauses for include and AND for exclude.
func TestBuildWhereClause_MultiFilter_MissingImages(t *testing.T) {
	t.Parallel()
	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, args := buildWhereClause(ListParams{Filters: map[string]string{"missing_images": "include"}})
		for _, imgType := range legacyImageTypes {
			if !strings.Contains(clause, "image_type = '"+imgType+"'") {
				t.Errorf("include: expected image_type = '%s' in clause %q", imgType, clause)
			}
		}
		if !strings.Contains(clause, " OR ") {
			t.Errorf("include: expected OR logic, got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("include: expected no args, got %v", args)
		}
	})
	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, args := buildWhereClause(ListParams{Filters: map[string]string{"missing_images": "exclude"}})
		for _, imgType := range legacyImageTypes {
			if !strings.Contains(clause, "image_type = '"+imgType+"'") {
				t.Errorf("exclude: expected image_type = '%s' in clause %q", imgType, clause)
			}
		}
		// Each image type produces an EXISTS(...) fragment joined with AND.
		// Counting "EXISTS (" proves the top-level join is AND, not OR -- a bare
		// " AND " check is satisfied by ANDs inside each subquery.
		existsCount := strings.Count(clause, "EXISTS (")
		if existsCount < 2 {
			t.Errorf("exclude: expected at least 2 EXISTS ( fragments, got %d in %q", existsCount, clause)
		}
		if strings.Contains(clause, " OR ") {
			t.Errorf("exclude: expected no OR logic (top-level join must be AND), got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("exclude: expected no args, got %v", args)
		}
	})
}

// TestBuildWhereClause_MultiFilter_MissingMBID covers the missing_mbid predicate.
func TestBuildWhereClause_MultiFilter_MissingMBID(t *testing.T) {
	t.Parallel()
	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"missing_mbid": "include"}})
		if !strings.Contains(clause, "NOT EXISTS") || !strings.Contains(clause, "provider = 'musicbrainz'") {
			t.Errorf("include: expected NOT EXISTS musicbrainz, got %q", clause)
		}
		// Must gate on non-empty provider_id; a bare row-existence check
		// misclassifies artists where UpsertAll stored an empty MBID.
		if !strings.Contains(clause, "provider_id IS NOT NULL") || !strings.Contains(clause, "provider_id <> ''") {
			t.Errorf("include: expected non-empty provider_id guard, got %q", clause)
		}
	})
	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"missing_mbid": "exclude"}})
		if strings.Contains(clause, "NOT EXISTS") {
			t.Errorf("exclude: expected EXISTS (not NOT EXISTS) for musicbrainz, got %q", clause)
		}
		if !strings.Contains(clause, "provider = 'musicbrainz'") {
			t.Errorf("exclude: expected provider = 'musicbrainz', got %q", clause)
		}
		if !strings.Contains(clause, "provider_id IS NOT NULL") || !strings.Contains(clause, "provider_id <> ''") {
			t.Errorf("exclude: expected non-empty provider_id guard, got %q", clause)
		}
	})
}

// TestBuildWhereClause_MultiFilter_Excluded covers the excluded predicate.
func TestBuildWhereClause_MultiFilter_Excluded(t *testing.T) {
	t.Parallel()
	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"excluded": "include"}})
		if !strings.Contains(clause, "is_excluded = 1") {
			t.Errorf("include: expected is_excluded = 1, got %q", clause)
		}
	})
	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"excluded": "exclude"}})
		if !strings.Contains(clause, "is_excluded = 0") {
			t.Errorf("exclude: expected is_excluded = 0, got %q", clause)
		}
	})
}

// TestBuildWhereClause_MultiFilter_Locked covers the locked predicate.
func TestBuildWhereClause_MultiFilter_Locked(t *testing.T) {
	t.Parallel()
	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"locked": "include"}})
		if !strings.Contains(clause, "locked = 1") {
			t.Errorf("include: expected locked = 1, got %q", clause)
		}
	})
	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"locked": "exclude"}})
		if !strings.Contains(clause, "locked = 0") {
			t.Errorf("exclude: expected locked = 0, got %q", clause)
		}
	})
}

// TestBuildWhereClause_TypeFilter_Include verifies that including multiple types
// produces a single IN clause with OR logic (not multiple AND conditions).
func TestBuildWhereClause_TypeFilter_Include(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{
		"type_person": "include",
		"type_group":  "include",
	}})
	if !strings.Contains(clause, "type IN (") {
		t.Errorf("expected type IN clause, got %q", clause)
	}
	// type_person maps to "person" and "solo"; type_group maps to "group" = 3 values.
	if len(args) != 3 {
		t.Errorf("expected 3 type args (person, solo, group), got %d: %v", len(args), args)
	}
	// Must NOT produce two separate conditions, which would be impossible AND.
	if strings.Count(clause, "type IN (") > 1 {
		t.Errorf("expected single IN clause, got multiple in %q", clause)
	}
}

// TestBuildWhereClause_TypeFilter_Exclude verifies that excluding types
// produces a NOT IN clause.
func TestBuildWhereClause_TypeFilter_Exclude(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"type_orchestra": "exclude"}})
	if !strings.Contains(clause, "type NOT IN (") {
		t.Errorf("expected type NOT IN clause, got %q", clause)
	}
	// type_orchestra backs the Orchestra/Choir facet = two values.
	if len(args) != 2 || args[0] != "orchestra" || args[1] != "choir" {
		t.Errorf("expected [orchestra choir], got %v", args)
	}
}

// TestBuildWhereClause_TypeFilter_IncludeExclude verifies that mixing include
// and exclude type filters produces both IN and NOT IN clauses.
func TestBuildWhereClause_TypeFilter_IncludeExclude(t *testing.T) {
	t.Parallel()
	clause, _ := buildWhereClause(ListParams{Filters: map[string]string{
		"type_group":     "include",
		"type_orchestra": "exclude",
	}})
	if !strings.Contains(clause, "type IN (") {
		t.Errorf("expected IN clause, got %q", clause)
	}
	if !strings.Contains(clause, "type NOT IN (") {
		t.Errorf("expected NOT IN clause, got %q", clause)
	}
}

// TestBuildWhereClause_LibraryFilter_Include verifies the include-mode clause
// emitted when at least one library is set to Include (issue #1217, #1786).
// Include mode emits a single EXISTS (membership in any included library).
// Each included library ID is bound exactly once. The old "entirely within"
// NOT EXISTS guard (dropped in #1786) would exclude artists who also have a
// platform library membership, so it must not appear.
func TestBuildWhereClause_LibraryFilter_Include(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{
		"library_lib-a": "include",
		"library_lib-b": "include",
	}})
	if !strings.Contains(clause, "EXISTS") || !strings.Contains(clause, "artist_libraries") {
		t.Errorf("expected EXISTS artist_libraries clause, got %q", clause)
	}
	// The "entirely within" guard from #1217 must be gone: it over-excluded
	// artists who also hold platform library memberships (#1786).
	if strings.Contains(clause, "NOT EXISTS") {
		t.Errorf("include mode must not emit NOT EXISTS (over-excludes platform-mirrored artists); got %q", clause)
	}
	if strings.Contains(clause, "NOT IN") {
		t.Errorf("include mode must not emit NOT IN boundary guard; got %q", clause)
	}
	// Two included libraries, each bound exactly once: 2 args total.
	if len(args) != 2 {
		t.Errorf("expected 2 library args (one bind per included ID), got %d: %v", len(args), args)
	}
	gotCounts := map[string]int{}
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			t.Fatalf("expected string arg, got %T (%v)", a, a)
		}
		gotCounts[s]++
	}
	if gotCounts["lib-a"] != 1 || gotCounts["lib-b"] != 1 || len(gotCounts) != 2 {
		t.Errorf("expected lib-a/lib-b each bound once, got %v", gotCounts)
	}
}

// TestBuildWhereClause_LibraryFilter_Include_WithPlatformMembership is the
// regression guard for issue #1786. It verifies that an artist who holds
// membership in BOTH a filesystem library (the included one) and a platform
// library (Emby/Jellyfin -- outside the included set) is NOT excluded.
//
// The old condition (b) from #1217 emitted:
//
//	NOT EXISTS (... al.library_id NOT IN (...))
//
// which disqualified any artist with an out-of-set membership. Platform
// libraries are always present for synced artists, so this excluded virtually
// everyone. The fix (dropping condition b) means Include X = HAS-membership-in-X.
func TestBuildWhereClause_LibraryFilter_Include_WithPlatformMembership(t *testing.T) {
	t.Parallel()
	// Include only the filesystem library.
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{
		"library_fs-local": "include",
	}})
	// Clause must have the EXISTS membership check.
	if !strings.Contains(clause, "EXISTS") {
		t.Errorf("expected EXISTS membership check, got %q", clause)
	}
	// The out-of-set boundary guard must be absent.
	if strings.Contains(clause, "NOT EXISTS") {
		t.Errorf("NOT EXISTS guard must be absent; it would exclude platform-mirrored artists: %q", clause)
	}
	// The library ID is bound exactly once.
	if len(args) != 1 || args[0] != "fs-local" {
		t.Errorf("expected [fs-local] args, got %v", args)
	}
}

// TestBuildWhereClause_LibraryFilter_Include_MultiUnion verifies that multiple
// Include libraries produce a single EXISTS with an IN list (UNION semantics):
// an artist belonging to ANY included library passes the filter.
func TestBuildWhereClause_LibraryFilter_Include_MultiUnion(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{
		"library_lib-1": "include",
		"library_lib-2": "include",
		"library_lib-3": "include",
	}})
	// Single EXISTS with an IN list -- not multiple separate EXISTS conditions.
	existsCount := strings.Count(clause, "EXISTS (")
	if existsCount != 1 {
		t.Errorf("expected exactly 1 EXISTS clause for multi-include union, got %d in %q", existsCount, clause)
	}
	if !strings.Contains(clause, "IN (") {
		t.Errorf("expected IN (...) for 3 included libraries, got %q", clause)
	}
	// Each library ID bound once.
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(args), args)
	}
}

// TestBuildWhereClause_LibraryFilter_Exclude verifies per-library NOT EXISTS clause.
func TestBuildWhereClause_LibraryFilter_Exclude(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"library_lib-x": "exclude"}})
	if !strings.Contains(clause, "NOT EXISTS") {
		t.Errorf("expected NOT EXISTS clause, got %q", clause)
	}
	if len(args) != 1 || args[0] != "lib-x" {
		t.Errorf("expected [lib-x], got %v", args)
	}
}

// TestBuildWhereClause_LibraryFilter_EmptyID verifies that library_ keys with
// an empty suffix are silently skipped.
func TestBuildWhereClause_LibraryFilter_EmptyID(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"library_": "include"}})
	if clause != "" {
		t.Errorf("expected empty clause for empty library ID, got %q", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

// TestBuildWhereClause_UnknownMultiFilterKey verifies that an unrecognized
// Filters key is silently skipped.
func TestBuildWhereClause_UnknownMultiFilterKey(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"totally_unknown": "include"}})
	if clause != "" {
		t.Errorf("expected empty clause for unknown filter key, got %q", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

// TestBuildWhereClause_MultiFilter_InvalidState verifies that an invalid state
// value (not "include"/"exclude") produces no condition.
func TestBuildWhereClause_MultiFilter_InvalidState(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"missing_meta": "foobar"}})
	if clause != "" {
		t.Errorf("expected empty clause for invalid state, got %q", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

// TestBuildWhereClause_Combined verifies that multiple conditions combine with AND.
func TestBuildWhereClause_Combined(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{
		Search:  "Beatles",
		Filter:  "missing_nfo",
		Filters: map[string]string{"locked": "include"},
	})
	if !strings.HasPrefix(clause, " WHERE ") {
		t.Errorf("expected clause to start with WHERE, got %q", clause)
	}
	if !strings.Contains(clause, " AND ") {
		t.Errorf("expected AND separator between conditions, got %q", clause)
	}
	if !strings.Contains(clause, "name LIKE ?") {
		t.Errorf("expected search condition, got %q", clause)
	}
	if !strings.Contains(clause, "nfo_exists = 0") {
		t.Errorf("expected nfo condition, got %q", clause)
	}
	if !strings.Contains(clause, "locked = 1") {
		t.Errorf("expected locked condition, got %q", clause)
	}
	if len(args) != 1 {
		t.Errorf("expected 1 arg (search), got %d: %v", len(args), args)
	}
}

// TestBuildWhereClause_NoUserInputInSQL verifies that SQL fragments generated by
// the filter predicate map do not contain any parameterized user data in the
// fragment text itself. Image type strings, provider names, and column names are
// hard-coded literals; only Search values and IDs are bound via args.
func TestBuildWhereClause_NoUserInputInSQL(t *testing.T) {
	t.Parallel()
	// Provide user-controlled input in every field that accepts it and verify
	// that none of the literal input text appears in the SQL fragment.
	userControlled := "'; DROP TABLE artists; --"
	clause, _ := buildWhereClause(ListParams{
		Search: userControlled,
		IDs:    []string{userControlled},
	})
	// The fragment should contain LIKE ? and IN (?) but not the literal string.
	if strings.Contains(clause, userControlled) {
		t.Errorf("user input appeared in SQL fragment: %q", clause)
	}
}

// TestBuildWhereClause_TypeFilter_AllTypes verifies that the named type filter
// keys (type_person, type_group, type_orchestra) are wired to their value sets.
// type_orchestra now backs the Orchestra/Choir facet, so it carries two values.
func TestBuildWhereClause_TypeFilter_AllTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key      string
		wantArgs []string
	}{
		{"type_person", []string{"person", "solo"}},
		{"type_group", []string{"group"}},
		{"type_orchestra", []string{"orchestra", "choir"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{tc.key: "include"}})
			if !strings.Contains(clause, "type IN (") {
				t.Errorf("%s: expected type IN clause, got %q", tc.key, clause)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("%s: expected %d args, got %d: %v", tc.key, len(tc.wantArgs), len(args), args)
			}
			// Build a set of returned args for order-independent comparison.
			got := make(map[string]bool, len(args))
			for _, a := range args {
				got[a.(string)] = true
			}
			for _, want := range tc.wantArgs {
				if !got[want] {
					t.Errorf("%s: missing expected arg %q in %v", tc.key, want, args)
				}
			}
		})
	}
}

// TestBuildWhereClause_TypeFilter_Other_Include verifies the "Other" facet is a
// negation of the named types plus an explicit NULL match, so untyped artists
// stored as NULL are included rather than silently dropped by SQLite's
// `NULL NOT IN (...)` (which evaluates to NULL, not true).
func TestBuildWhereClause_TypeFilter_Other_Include(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"type_other": "include"}})
	if !strings.Contains(clause, "type NOT IN (") {
		t.Errorf("expected a type NOT IN negation, got %q", clause)
	}
	if !strings.Contains(clause, "type IS NULL") {
		t.Errorf("expected an IS NULL match for untyped artists, got %q", clause)
	}
	if len(args) != len(namedTypeValues) {
		t.Fatalf("expected %d args (namedTypeValues), got %d: %v", len(namedTypeValues), len(args), args)
	}
}

// TestBuildWhereClause_TypeFilter_Other_Exclude verifies that excluding "Other"
// keeps only the named types (type IN namedTypeValues), which correctly drops
// NULL / empty-typed artists.
func TestBuildWhereClause_TypeFilter_Other_Exclude(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{"type_other": "exclude"}})
	if !strings.Contains(clause, "type IN (") {
		t.Errorf("expected type IN (named) clause, got %q", clause)
	}
	if strings.Contains(clause, "type IS NULL") {
		t.Errorf("exclude-Other must not match NULL rows, got %q", clause)
	}
	if len(args) != len(namedTypeValues) {
		t.Fatalf("expected %d args, got %d: %v", len(namedTypeValues), len(args), args)
	}
}

// TestBuildWhereClause_TypeFilter_OtherWithNamed_Include verifies that selecting
// a named facet AND "Other" together OR-composes into one combined condition --
// "any named value OR not-a-named-value" -- rather than an impossible AND.
func TestBuildWhereClause_TypeFilter_OtherWithNamed_Include(t *testing.T) {
	t.Parallel()
	clause, _ := buildWhereClause(ListParams{Filters: map[string]string{
		"type_group": "include",
		"type_other": "include",
	}})
	if !strings.Contains(clause, " OR ") {
		t.Errorf("expected an OR-composed include clause, got %q", clause)
	}
	if !strings.Contains(clause, "type IN (") || !strings.Contains(clause, "type NOT IN (") {
		t.Errorf("expected both IN (group) and NOT IN (named) branches, got %q", clause)
	}
}

// TestBuildWhereClause_TypeFilter_NamedAndOther_Exclude verifies that excluding
// a named facet AND "Other" together emits BOTH clauses AND-joined: the named
// exclusion (type NOT IN (group...)) and the Other exclusion (type IN (named)).
// Excluding Other keeps only named types, so the two conditions coexist as
// independent AND predicates (keep named types, but not the group ones).
func TestBuildWhereClause_TypeFilter_NamedAndOther_Exclude(t *testing.T) {
	t.Parallel()
	clause, _ := buildWhereClause(ListParams{Filters: map[string]string{
		"type_group": "exclude",
		"type_other": "exclude",
	}})
	// The named exclusion: type NOT IN (group...).
	if !strings.Contains(clause, "type NOT IN (") {
		t.Errorf("expected the named exclusion (type NOT IN (group)), got %q", clause)
	}
	// The Other exclusion: keep only the named types (type IN (named)).
	if !strings.Contains(clause, "type IN (") {
		t.Errorf("expected the Other exclusion (type IN (named)), got %q", clause)
	}
	// Two EXCLUDE facets are AND-joined as separate conditions, never OR-composed
	// (OR composition is for INCLUDE facets only).
	if strings.Contains(clause, " OR ") {
		t.Errorf("two EXCLUDE facets must be AND-joined, not OR-composed, got %q", clause)
	}
}

// TestNamedTypeValuesMatchFacets guards namedTypeValues against drift from
// typeFilterKeys: the "Other" negation must be the exact complement of every
// value exposed by a named facet, or untyped/other artists would leak into a
// named facet or be missed by Other.
func TestNamedTypeValuesMatchFacets(t *testing.T) {
	t.Parallel()
	var union []string
	for _, vals := range typeFilterKeys {
		union = append(union, vals...)
	}
	sort.Strings(union)
	got := append([]string(nil), namedTypeValues...)
	sort.Strings(got)
	if len(union) != len(got) {
		t.Fatalf("namedTypeValues has %d values, union of typeFilterKeys has %d: %v vs %v", len(got), len(union), got, union)
	}
	for i := range union {
		if union[i] != got[i] {
			t.Errorf("namedTypeValues %v != union of typeFilterKeys values %v; update one to match", got, union)
		}
	}
}

// TestBuildWhereClause_MetadataFieldPresence verifies that each metadata-field
// presence filter key produces the correct column predicate for both include
// (has value) and exclude (lacks value) states, with no bound args. The
// assertions check operator-level semantics (IS NOT NULL vs IS NULL) so an
// include/exclude regression cannot pass on column-name presence alone.
func TestBuildWhereClause_MetadataFieldPresence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key     string
		wantCol string // column name that must appear in the predicate
	}{
		{"has_biography", "biography"},
		{"has_years_active", "years_active"},
		{"has_formed", "formed"},
		{"has_disbanded", "disbanded"},
		{"has_born", "born"},
		{"has_died", "died"},
		{"has_gender", "gender"},
		{"has_type", "type"},
		{"has_country", "origin"},
		{"has_genres", "genres"},
		{"has_styles", "styles"},
		{"has_moods", "moods"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.key+"_include", func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{tc.key: "include"}})
			// include must assert the column has a value: IS NOT NULL.
			if !strings.Contains(clause, tc.wantCol+" IS NOT NULL") {
				t.Errorf("include: expected %q IS NOT NULL predicate, got %q", tc.wantCol, clause)
			}
			// It must not assert the inverse (a bare IS NULL on the column).
			if strings.Contains(clause, tc.wantCol+" IS NULL") {
				t.Errorf("include: clause must not assert %q IS NULL, got %q", tc.wantCol, clause)
			}
			// nonEmptyStringPredicate uses no bound args.
			if len(args) != 0 {
				t.Errorf("include: expected no args, got %v", args)
			}
		})
		t.Run(tc.key+"_exclude", func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{tc.key: "exclude"}})
			// exclude must assert the column lacks a value: IS NULL.
			if !strings.Contains(clause, tc.wantCol+" IS NULL") {
				t.Errorf("exclude: expected %q IS NULL predicate, got %q", tc.wantCol, clause)
			}
			// It must not assert the include-side IS NOT NULL.
			if strings.Contains(clause, "IS NOT NULL") {
				t.Errorf("exclude: clause must not assert IS NOT NULL, got %q", clause)
			}
			if len(args) != 0 {
				t.Errorf("exclude: expected no args, got %v", args)
			}
		})
	}
}

// TestBuildWhereClause_HasMembers verifies the band_members EXISTS sub-select.
func TestBuildWhereClause_HasMembers(t *testing.T) {
	t.Parallel()

	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, args := buildWhereClause(ListParams{Filters: map[string]string{"has_members": "include"}})
		if !strings.Contains(clause, "EXISTS") {
			t.Errorf("include: expected EXISTS, got %q", clause)
		}
		if !strings.Contains(clause, "band_members") {
			t.Errorf("include: expected band_members table reference, got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("include: expected no args, got %v", args)
		}
	})

	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, args := buildWhereClause(ListParams{Filters: map[string]string{"has_members": "exclude"}})
		if !strings.Contains(clause, "NOT EXISTS") {
			t.Errorf("exclude: expected NOT EXISTS, got %q", clause)
		}
		if !strings.Contains(clause, "band_members") {
			t.Errorf("exclude: expected band_members table reference, got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("exclude: expected no args, got %v", args)
		}
	})
}

// TestBuildWhereClause_HasDiscography verifies the discography filter maps to
// nfo_exists (the DB-level proxy for an NFO file that may contain albums).
func TestBuildWhereClause_HasDiscography(t *testing.T) {
	t.Parallel()

	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"has_discography": "include"}})
		if !strings.Contains(clause, "nfo_exists = 1") {
			t.Errorf("include: expected nfo_exists = 1, got %q", clause)
		}
	})

	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, _ := buildWhereClause(ListParams{Filters: map[string]string{"has_discography": "exclude"}})
		if !strings.Contains(clause, "nfo_exists = 0") {
			t.Errorf("exclude: expected nfo_exists = 0, got %q", clause)
		}
	})
}

// TestBuildWhereClause_PerImageTypeFilters verifies that each individual image
// filter (has_thumb, has_fanart, has_logo, has_banner) produces an EXISTS or
// NOT EXISTS sub-select against the artist_images table.
func TestBuildWhereClause_PerImageTypeFilters(t *testing.T) {
	t.Parallel()
	imgTypes := []string{"has_thumb", "has_fanart", "has_logo", "has_banner"}
	for _, key := range imgTypes {
		key := key
		imgType := key[len("has_"):]
		t.Run(key+"_include", func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{key: "include"}})
			if !strings.Contains(clause, "EXISTS") {
				t.Errorf("%s include: expected EXISTS, got %q", key, clause)
			}
			if !strings.Contains(clause, imgType) {
				t.Errorf("%s include: expected image type %q in clause, got %q", key, imgType, clause)
			}
			if !strings.Contains(clause, "artist_images") {
				t.Errorf("%s include: expected artist_images table reference, got %q", key, clause)
			}
			if len(args) != 0 {
				t.Errorf("%s include: expected no args, got %v", key, args)
			}
		})
		t.Run(key+"_exclude", func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{key: "exclude"}})
			if !strings.Contains(clause, "NOT EXISTS") {
				t.Errorf("%s exclude: expected NOT EXISTS, got %q", key, clause)
			}
			if !strings.Contains(clause, imgType) {
				t.Errorf("%s exclude: expected image type %q in clause, got %q", key, imgType, clause)
			}
			if len(args) != 0 {
				t.Errorf("%s exclude: expected no args, got %v", key, args)
			}
		})
	}
}

// TestBuildWhereClause_PlatformFilters verifies that in_emby, in_jellyfin, and
// has_lidarr each produce a sub-select that joins artist_libraries -> libraries
// -> connections and checks the correct connection type.
func TestBuildWhereClause_PlatformFilters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key      string
		connType string
	}{
		{"in_emby", "emby"},
		{"in_jellyfin", "jellyfin"},
		{"has_lidarr", "lidarr"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.key+"_include", func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{tc.key: "include"}})
			if !strings.Contains(clause, "EXISTS") {
				t.Errorf("%s include: expected EXISTS, got %q", tc.key, clause)
			}
			if !strings.Contains(clause, tc.connType) {
				t.Errorf("%s include: expected connection type %q in clause, got %q", tc.key, tc.connType, clause)
			}
			if !strings.Contains(clause, "connections") {
				t.Errorf("%s include: expected connections table reference, got %q", tc.key, clause)
			}
			if len(args) != 0 {
				t.Errorf("%s include: expected no args, got %v", tc.key, args)
			}
		})
		t.Run(tc.key+"_exclude", func(t *testing.T) {
			t.Parallel()
			clause, args := buildWhereClause(ListParams{Filters: map[string]string{tc.key: "exclude"}})
			if !strings.Contains(clause, "NOT EXISTS") {
				t.Errorf("%s exclude: expected NOT EXISTS, got %q", tc.key, clause)
			}
			if !strings.Contains(clause, tc.connType) {
				t.Errorf("%s exclude: expected connection type %q in clause, got %q", tc.key, tc.connType, clause)
			}
			if len(args) != 0 {
				t.Errorf("%s exclude: expected no args, got %v", tc.key, args)
			}
		})
	}
}

// TestBuildWhereClause_HasViolations verifies that the has_violations filter
// checks the rule_violations table for open violations.
func TestBuildWhereClause_HasViolations(t *testing.T) {
	t.Parallel()

	t.Run("include", func(t *testing.T) {
		t.Parallel()
		clause, args := buildWhereClause(ListParams{Filters: map[string]string{"has_violations": "include"}})
		if !strings.Contains(clause, "EXISTS") {
			t.Errorf("include: expected EXISTS, got %q", clause)
		}
		if !strings.Contains(clause, "rule_violations") {
			t.Errorf("include: expected rule_violations table reference, got %q", clause)
		}
		if !strings.Contains(clause, "status = 'open'") {
			t.Errorf("include: expected status = 'open' predicate, got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("include: expected no args, got %v", args)
		}
	})

	t.Run("exclude", func(t *testing.T) {
		t.Parallel()
		clause, args := buildWhereClause(ListParams{Filters: map[string]string{"has_violations": "exclude"}})
		if !strings.Contains(clause, "NOT EXISTS") {
			t.Errorf("exclude: expected NOT EXISTS, got %q", clause)
		}
		if !strings.Contains(clause, "rule_violations") {
			t.Errorf("exclude: expected rule_violations table reference, got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("exclude: expected no args, got %v", args)
		}
	})
}

// TestValidatedOrderClause_AllSortKeys verifies that every accepted sort key
// produces a SQL ORDER BY expression containing the expected column name, and
// that the id tiebreaker is always appended for stable pagination.
func TestValidatedOrderClause_AllSortKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sort    string
		wantCol string
	}{
		{"name", "name"},
		{"sort_name", "sort_name"},
		{"type", "type"},
		{"origin", "origin"},
		{"health_score", "health_score"},
		{"updated_at", "updated_at"},
		{"created_at", "created_at"},
		// Empty/unknown keys normalize to name (Validate() defense-in-depth).
		{"", "name"},
		{"totally_unknown", "name"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("sort="+tc.sort, func(t *testing.T) {
			t.Parallel()
			clause := validatedOrderClause(ListParams{Sort: tc.sort, Order: "asc"})
			if !strings.Contains(clause, tc.wantCol) {
				t.Errorf("sort=%q: expected column %q in ORDER BY %q", tc.sort, tc.wantCol, clause)
			}
			// Stable pagination requires an id tiebreaker on every sort path.
			if !strings.Contains(clause, "id ASC") {
				t.Errorf("sort=%q: expected id ASC tiebreaker in ORDER BY %q", tc.sort, clause)
			}
		})
	}
}

// TestValidatedOrderClause_Direction verifies that "asc" and "desc" map to the
// correct SQL direction keyword for an arbitrary column.
func TestValidatedOrderClause_Direction(t *testing.T) {
	t.Parallel()
	asc := validatedOrderClause(ListParams{Sort: "name", Order: "asc"})
	if !strings.HasPrefix(asc, "name ASC,") {
		t.Errorf("order=asc: expected primary direction ASC in %q", asc)
	}
	desc := validatedOrderClause(ListParams{Sort: "name", Order: "desc"})
	if !strings.HasPrefix(desc, "name DESC,") {
		t.Errorf("order=desc: expected primary direction DESC in %q", desc)
	}
}

// TestBuildWhereClause_NewFilters_AllHavePredicates verifies that every key
// added in issue #1125 is registered in artistFilterPredicates and that both
// include and exclude states produce a non-empty WHERE clause. This is a
// coverage guard: if a new key is added to parseFlyoutFilters but not wired
// into the predicate map, the filter would silently be a no-op.
func TestBuildWhereClause_NewFilters_AllHavePredicates(t *testing.T) {
	t.Parallel()
	newKeys := []string{
		"has_biography", "has_years_active", "has_formed", "has_disbanded",
		"has_born", "has_died", "has_gender", "has_type", "has_country",
		"has_genres", "has_styles", "has_moods", "has_members", "has_discography",
		"has_thumb", "has_fanart", "has_logo", "has_banner",
		"in_emby", "in_jellyfin", "has_lidarr",
		"has_violations",
	}
	for _, key := range newKeys {
		key := key
		for _, state := range []string{"include", "exclude"} {
			state := state
			t.Run(key+"_"+state, func(t *testing.T) {
				t.Parallel()
				clause, _ := buildWhereClause(ListParams{Filters: map[string]string{key: state}})
				if clause == "" {
					t.Errorf("key %q state %q: expected a non-empty WHERE clause, got empty (predicate missing or broken)", key, state)
				}
			})
		}
	}
}
