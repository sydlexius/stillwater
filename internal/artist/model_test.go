package artist

import "testing"

func TestMarshalStringSlice(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil slice", nil, "[]"},
		{"empty slice", []string{}, "[]"},
		{"single element", []string{"Rock"}, `["Rock"]`},
		{"multiple elements", []string{"Rock", "Pop", "Jazz"}, `["Rock","Pop","Jazz"]`},
		{"unicode", []string{"Electr\u00f3nica", "\u30ed\u30c3\u30af"}, "[\"Electr\u00f3nica\",\"\u30ed\u30c3\u30af\"]"},
		{"special characters", []string{`"quoted"`, "with spaces"}, `["\"quoted\"","with spaces"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarshalStringSlice(tt.input)
			if got != tt.want {
				t.Errorf("MarshalStringSlice(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnmarshalStringSlice(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantNil bool
		wantLen int
	}{
		{"empty string", "", true, 0},
		{"empty array", "[]", true, 0},
		{"single element", `["Rock"]`, false, 1},
		{"multiple elements", `["Rock","Pop","Jazz"]`, false, 3},
		{"invalid json", "not json", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnmarshalStringSlice(tt.input)
			if tt.wantNil && got != nil {
				t.Errorf("UnmarshalStringSlice(%q) = %v, want nil", tt.input, got)
			}
			if !tt.wantNil && len(got) != tt.wantLen {
				t.Errorf("UnmarshalStringSlice(%q) len = %d, want %d", tt.input, len(got), tt.wantLen)
			}
		})
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	original := []string{"Rock", "Alternative", "Indie"}
	marshaled := MarshalStringSlice(original)
	result := UnmarshalStringSlice(marshaled)

	if len(result) != len(original) {
		t.Fatalf("round trip length mismatch: got %d, want %d", len(result), len(original))
	}
	for i, v := range result {
		if v != original[i] {
			t.Errorf("round trip mismatch at index %d: got %q, want %q", i, v, original[i])
		}
	}
}
