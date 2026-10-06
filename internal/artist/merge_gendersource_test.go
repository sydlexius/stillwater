package artist

import "testing"

// A stored gender source must follow the gender it describes: cleared with it
// for a non-individual type, kept when a lock or an individual type keeps the
// gender. The type source is never touched (#3292).
func TestApplyMetadata_GenderSourceFollowsGenderClear(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		typ        string
		locked     []string
		wantGender string
		wantSrc    bool
	}{
		"group clears gender and source": {typ: "group", wantGender: "", wantSrc: false},
		"locked gender keeps both":       {typ: "group", locked: []string{"gender"}, wantGender: "female", wantSrc: true},
		"individual type keeps both":     {typ: "solo", wantGender: "female", wantSrc: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &Artist{
				Type: tc.typ, Gender: "female", LockedFields: tc.locked,
				MetadataSources: map[string]string{"gender": "audiodb", "type": "audiodb"},
			}
			ApplyMetadata(a, &MetadataUpdate{}, FillEmpty, MergeOptions{})
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
