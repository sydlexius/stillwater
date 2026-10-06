package artist

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// A gender source must follow the gender it describes: cleared with it for a
// non-individual type (even when the caller supplies one), kept when a lock or
// an individual type keeps the gender. The type source is never touched (#3292).
func TestApplyMetadata_GenderSourceFollowsGenderClear(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		typ, stored string
		locked      []string
		supplied    bool
		wantGender  string
		wantSrc     bool
	}{
		"group clears gender and source": {typ: "group", stored: "female"},
		"locked gender keeps both":       {typ: "group", stored: "female", locked: []string{"gender"}, wantGender: "female", wantSrc: true},
		"individual type keeps both":     {typ: "solo", stored: "female", wantGender: "female", wantSrc: true},
		"group, supplied source":         {typ: "group", stored: "female", supplied: true},
		"group, empty gender, supplied":  {typ: "group", supplied: true},
		"solo, supplied source":          {typ: "solo", stored: "female", supplied: true, wantGender: "female", wantSrc: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &Artist{
				Type: tc.typ, Gender: tc.stored, LockedFields: tc.locked,
				MetadataSources: map[string]string{"gender": "audiodb", "type": "audiodb"},
			}
			var opts MergeOptions
			if tc.supplied {
				opts.Sources = []provider.FieldSource{{Field: "gender", Provider: provider.NameAudioDB}}
			}
			ApplyMetadata(a, &MetadataUpdate{}, FillEmpty, opts)
			if a.Gender != tc.wantGender {
				t.Errorf("Gender = %q, want %q", a.Gender, tc.wantGender)
			}
			if _, ok := a.MetadataSources["gender"]; ok != tc.wantSrc {
				t.Errorf("gender source present = %v, want %v", ok, tc.wantSrc)
			}
			if a.MetadataSources["type"] != "audiodb" {
				t.Errorf("type source = %q, want audiodb", a.MetadataSources["type"])
			}
		})
	}
}
