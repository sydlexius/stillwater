package artist

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// The type-consistency repair (applyTypeConsistency) mutates the artist but
// must never on its own make ApplyMetadata report true.
//
// WHY THAT MATTERS, IN THE CONSUMER'S TERMS: ApplyMetadata's bool is a
// persist-and-publish signal, not a "the struct moved" flag.
// internal/rule/bulk_executor.go skips an artist outright when it is false; on
// true it performs a DB write, a metadata_changes audit row, an NFO rewrite on
// disk, and a push to Emby/Jellyfin. The repair fires on the artist's RESULTING
// state, so it fires just as readily on an inconsistency that was already
// sitting in the database. If it fed the return value, a bulk sweep carrying no
// new metadata would still persist and PUBLISH every pre-existing bad row it
// walked past, for artists the operator never touched.
//
// The separation between "the merge changed the type, so clearing gender is
// part of that change" and "nothing changed, we merely repaired a bad row" is
// made upstream and needs no bookkeeping: a genuine type FLIP moves the Type
// field, so applyFields has already set changed=true for an independent reason.
// The two halves of that distinction are the "real change" and "type flip" rows
// of the table below versus the "repair only" rows.

// TestApplyMetadata_TypeRepairAloneDoesNotReportChanged is the four-case table
// the fix is defined by. Each case asserts BOTH halves -- the returned signal
// AND the resulting in-memory field -- because either alone is satisfied by a
// broken implementation:
//
//   - asserting only changed==false passes on an implementation that dropped
//     the repair entirely (regressing #2748);
//   - asserting only the repaired gender passes on the pre-fix implementation
//     that reported changed==true.
func TestApplyMetadata_TypeRepairAloneDoesNotReportChanged(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		artist      *Artist
		update      *MetadataUpdate
		strategy    MergeStrategy
		wantChanged bool
		wantGender  string
		wantType    string
		why         string
	}{
		// FillEmpty is the strategy the bulk sweep uses
		// (internal/rule/bulk_executor.go), so the repair-only cases are
		// measured on exactly the path whose publish behavior is at stake.
		"repair only: inconsistent artist, empty update": {
			artist:      &Artist{Type: "group", Gender: "female"},
			update:      &MetadataUpdate{},
			strategy:    FillEmpty,
			wantChanged: false,
			wantGender:  "",
			wantType:    "group",
			why: "the update carried no information; the row was ALREADY inconsistent, " +
				"so repairing it is not a reason to persist and publish",
		},
		"real change on an inconsistent artist": {
			artist:      &Artist{Type: "group", Gender: "female"},
			update:      &MetadataUpdate{Biography: "a newly fetched biography"},
			strategy:    FillEmpty,
			wantChanged: true,
			wantGender:  "",
			wantType:    "group",
			why: "the biography genuinely arrived, so this merge persists -- and the " +
				"repair rides along on the same write",
		},
		// NFOImport, not FillEmpty: FillEmpty's Type mode is fill-empty, so it
		// cannot overwrite a stored non-empty type and no flip could occur.
		// The flip must be exercised on a strategy that actually moves Type.
		"genuine type flip makes gender inapplicable": {
			artist:      &Artist{Type: "solo", Gender: "female"},
			update:      &MetadataUpdate{Type: "group"},
			strategy:    NFOImport,
			wantChanged: true,
			wantGender:  "",
			wantType:    "group",
			why: "the TYPE itself changed, which is a real change on its own; clearing " +
				"the now-inapplicable gender is part of it (this is the original #2748 case)",
		},
		"consistent artist, empty update": {
			artist:      &Artist{Type: "solo", Gender: "female"},
			update:      &MetadataUpdate{},
			strategy:    FillEmpty,
			wantChanged: false,
			wantGender:  "female",
			wantType:    "solo",
			why:         "nothing to do at all; the baseline that proves the pass is not simply always-off",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Precondition: without this the "repair only" cases would pass
			// vacuously on a fixture that never carried a gender to clear.
			if tc.artist.Gender == "" {
				t.Fatalf("fixture precondition: artist must start with a gender, got %q", tc.artist.Gender)
			}

			got := ApplyMetadata(tc.artist, tc.update, tc.strategy, MergeOptions{})

			if got != tc.wantChanged {
				t.Errorf("ApplyMetadata changed = %v, want %v; %s", got, tc.wantChanged, tc.why)
			}
			if tc.artist.Gender != tc.wantGender {
				t.Errorf("gender = %q, want %q; the repair must still MUTATE the artist "+
					"even when it does not report a change", tc.artist.Gender, tc.wantGender)
			}
			if tc.artist.Type != tc.wantType {
				t.Errorf("type = %q, want %q", tc.artist.Type, tc.wantType)
			}
		})
	}
}

// TestApplyMetadata_TypeRepairIsIdempotentSignal pins the property the bulk
// sweep actually depends on: running the same no-op merge twice over an
// inconsistent row reports false BOTH times.
//
// The first call is the one the pre-fix code got wrong. The second call would
// have been false even before the fix (the repair had already run), so a test
// that only checked the second call would pass on the broken implementation.
func TestApplyMetadata_TypeRepairIsIdempotentSignal(t *testing.T) {
	t.Parallel()

	a := &Artist{Type: "group", Gender: "female"}

	first := ApplyMetadata(a, &MetadataUpdate{}, FillEmpty, MergeOptions{})
	if first {
		t.Errorf("first no-op merge over an already-inconsistent row reported changed=true; "+
			"that is the bulk-publish trigger this fix removes (gender is now %q)", a.Gender)
	}
	if a.Gender != "" {
		t.Errorf("gender = %q, want cleared; the repair must still run", a.Gender)
	}

	second := ApplyMetadata(a, &MetadataUpdate{}, FillEmpty, MergeOptions{})
	if second {
		t.Error("second no-op merge reported changed=true")
	}
}

// TestApplyMetadata_DateRepairAloneDoesNotReportChanged is the date-pass
// sibling of the gender table. The date pass has the identical structure and
// had the identical defect, so it gets the identical treatment: it runs (when
// the caller opts in via FilterDatesByType) and mutates, but does not by itself
// report a change.
//
// The date pass is opt-in, so each case also asserts the opts.FilterDatesByType
// precondition implicitly by checking the field actually got cleared -- without
// that assertion a "changed==false" result would be satisfied by a pass that
// never ran.
func TestApplyMetadata_DateRepairAloneDoesNotReportChanged(t *testing.T) {
	t.Parallel()

	t.Run("repair only: group carrying born, empty update", func(t *testing.T) {
		t.Parallel()

		a := &Artist{Type: "group", Born: "1965-01-02", Formed: "1965"}
		if a.Born == "" {
			t.Fatal("fixture precondition: artist must start with a born date")
		}

		got := ApplyMetadata(a, &MetadataUpdate{}, FillEmpty, MergeOptions{FilterDatesByType: true})

		if got {
			t.Error("changed = true, want false; the born date was already wrong on the " +
				"stored row, so clearing it is a repair and not new information")
		}
		if a.Born != "" {
			t.Errorf("born = %q, want cleared; the date pass must still MUTATE", a.Born)
		}
		if a.Formed != "1965" {
			t.Errorf("formed = %q, want %q; a group's formed date is valid and must survive", a.Formed, "1965")
		}
	})

	t.Run("genuine type flip clearing dates still reports changed", func(t *testing.T) {
		t.Parallel()

		a := &Artist{Type: "solo", Born: "1965-01-02"}

		got := ApplyMetadata(a, &MetadataUpdate{Type: "group"}, NFOImport, MergeOptions{FilterDatesByType: true})

		if !got {
			t.Error("changed = false, want true; the TYPE genuinely changed, which is a " +
				"real change independent of the date repair riding along with it")
		}
		if a.Born != "" {
			t.Errorf("born = %q, want cleared for a group type", a.Born)
		}
	})

	t.Run("real change on a date-inconsistent artist", func(t *testing.T) {
		t.Parallel()

		a := &Artist{Type: "group", Born: "1965-01-02"}

		got := ApplyMetadata(a, &MetadataUpdate{Biography: "new bio"}, FillEmpty, MergeOptions{FilterDatesByType: true})

		if !got {
			t.Error("changed = false, want true; the biography genuinely arrived")
		}
		if a.Biography != "new bio" {
			t.Errorf("biography = %q, want the merged value", a.Biography)
		}
		if a.Born != "" {
			t.Errorf("born = %q, want cleared; the repair rides along on the real write", a.Born)
		}
	})
}

// TestApplyMetadata_MetadataSourcesStillReportChanged guards the other
// contributor to the return value, which the fix must not have touched. Without
// it, a change that made ApplyMetadata always return false would still pass
// every assertion above.
func TestApplyMetadata_MetadataSourcesStillReportChanged(t *testing.T) {
	t.Parallel()

	a := &Artist{Type: "group", Gender: "female"}
	got := ApplyMetadata(a, &MetadataUpdate{}, FillEmpty, MergeOptions{
		Sources: []provider.FieldSource{{Field: "biography", Provider: provider.NameAudioDB}},
	})
	if !got {
		t.Error("changed = false, want true; a new metadata-source attribution is a real change")
	}
}
