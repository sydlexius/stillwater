package artist

import (
	"strconv"
	"testing"
)

// TestListParams_Validate_CapsIDs pins the IDs-filter safety cap from #1227.
// The "Show selected" affordance can in theory ship up to MAX_BULK_SELECTION
// IDs, but a hand-crafted query string is not bound by the JS-side cap.
// Validate() truncates so the SQL `IN (?, ...)` clause cannot exhaust the
// bound-parameter limit and degrade the page handler to a 500. We pin the
// truncation explicitly because a future "tighten cap" change should be a
// deliberate decision, not silent slack.
func TestListParams_Validate_CapsIDs(t *testing.T) {
	ids := make([]string, MaxListIDs+5)
	for i := range ids {
		ids[i] = "id-" + strconv.Itoa(i)
	}
	p := ListParams{IDs: ids}
	p.Validate()
	if len(p.IDs) != MaxListIDs {
		t.Fatalf("Validate did not cap IDs to MaxListIDs: got %d, want %d", len(p.IDs), MaxListIDs)
	}
}

// TestListParams_Validate_DropsEmptyIDs guards against the case where the
// query-string parser leaves a stray empty string in the slice (e.g. from
// a trailing comma in `ids=a,`). The repo-level WHERE clause uses
// `IN (?, ?, ?)` and an empty placeholder argument would silently match a
// row whose primary key is the empty string. We assert the empty entries
// are filtered out.
func TestListParams_Validate_DropsEmptyIDs(t *testing.T) {
	p := ListParams{IDs: []string{"a", "", "b", "", "c"}}
	p.Validate()
	want := []string{"a", "b", "c"}
	if len(p.IDs) != len(want) {
		t.Fatalf("filtered len = %d, want %d (got %v)", len(p.IDs), len(want), p.IDs)
	}
	for i, id := range want {
		if p.IDs[i] != id {
			t.Errorf("IDs[%d] = %q, want %q", i, p.IDs[i], id)
		}
	}
}

// TestListParams_Validate_PreservesNilIDs documents that the absent-filter
// case (nil IDs) survives Validate() unchanged. This is the path every
// existing caller takes today; a regression here would silently shift the
// list query into "filter to nothing" via an empty IN-clause.
func TestListParams_Validate_PreservesNilIDs(t *testing.T) {
	p := ListParams{}
	p.Validate()
	if len(p.IDs) != 0 {
		t.Errorf("Validate populated IDs from nil; got %v", p.IDs)
	}
}
