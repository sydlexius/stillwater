package components

import "testing"

// TestMarshalLanguages_PreservesEmpty locks in the invariant the picker's
// init JS depends on: when the user has no stored preference (or has
// explicitly cleared it via the picker's remove-all UI), the component
// renders `data-languages="[]"` rather than coercing back to the default
// `["en"]`. Coercion would silently re-render an English pill after a
// reset and contradict what the user just did (see issue #1138).
func TestMarshalLanguages_PreservesEmpty(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil slice preserves empty", nil, `[]`},
		{"empty slice preserves empty", []string{}, `[]`},
		{"single language serializes normally", []string{"en"}, `["en"]`},
		{"multiple languages preserve order", []string{"en-US", "en-GB", "en"}, `["en-US","en-GB","en"]`},
		{"non-Latin tag marshals unchanged", []string{"ja"}, `["ja"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := marshalLanguages(tt.input)
			if got != tt.want {
				t.Errorf("marshalLanguages(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
