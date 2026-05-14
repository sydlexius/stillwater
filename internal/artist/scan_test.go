package artist

import (
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
	if len(args) != 1 || args[0] != "orchestra" {
		t.Errorf("expected [orchestra], got %v", args)
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

// TestBuildWhereClause_LibraryFilter_Include verifies per-library EXISTS clause.
func TestBuildWhereClause_LibraryFilter_Include(t *testing.T) {
	t.Parallel()
	clause, args := buildWhereClause(ListParams{Filters: map[string]string{
		"library_lib-a": "include",
		"library_lib-b": "include",
	}})
	if !strings.Contains(clause, "EXISTS") || !strings.Contains(clause, "artist_libraries") {
		t.Errorf("expected EXISTS artist_libraries clause, got %q", clause)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 library args, got %d: %v", len(args), args)
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

// TestBuildWhereClause_TypeFilter_AllTypes verifies that all three type filter
// keys (type_person, type_group, type_orchestra) are wired correctly.
func TestBuildWhereClause_TypeFilter_AllTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key      string
		wantArgs []string
	}{
		{"type_person", []string{"person", "solo"}},
		{"type_group", []string{"group"}},
		{"type_orchestra", []string{"orchestra"}},
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
