package langpref

import (
	"reflect"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
		ok    bool
	}{
		{
			name:  "single language",
			input: []string{"en"},
			want:  []string{"en"},
			ok:    true,
		},
		{
			name:  "multiple tags preserve order",
			input: []string{"ja", "en-GB", "fr"},
			want:  []string{"ja", "en-GB", "fr"},
			ok:    true,
		},
		{
			name:  "canonicalize casing",
			input: []string{"EN-gb", "ZH-hant-tw", "JA"},
			want:  []string{"en-GB", "zh-Hant-TW", "ja"},
			ok:    true,
		},
		{
			name:  "nil slice rejected",
			input: nil,
			want:  nil,
			ok:    false,
		},
		{
			name:  "empty slice rejected",
			input: []string{},
			want:  nil,
			ok:    false,
		},
		{
			name:  "over max rejected",
			input: tooManyTags(),
			want:  nil,
			ok:    false,
		},
		{
			name:  "empty tag rejected",
			input: []string{"en", ""},
			want:  nil,
			ok:    false,
		},
		{
			name:  "invalid character rejected",
			input: []string{"en@GB"},
			want:  nil,
			ok:    false,
		},
		{
			name:  "duplicate exact rejected",
			input: []string{"en", "en"},
			want:  nil,
			ok:    false,
		},
		{
			name:  "duplicate case-insensitive rejected",
			input: []string{"en-GB", "EN-gb"},
			want:  nil,
			ok:    false,
		},
		{
			name:  "primary subtag too short rejected",
			input: []string{"e"},
			want:  nil,
			ok:    false,
		},
		{
			name:  "primary subtag too long rejected",
			input: []string{"engl"},
			want:  nil,
			ok:    false,
		},
		{
			name:  "subtag over 8 chars rejected",
			input: []string{"en-verylongregion"},
			want:  nil,
			ok:    false,
		},
		{
			name:  "overall length cap",
			input: []string{"aaaaaaaaa-bbbbbbbbb-ccccccccc-ddddddddd"},
			want:  nil,
			ok:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Validate(tc.input)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (input=%v)", ok, tc.ok, tc.input)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Validate(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestValidate_DoesNotMutateInput(t *testing.T) {
	input := []string{"EN-gb", "JA"}
	snapshot := append([]string(nil), input...)
	if _, ok := Validate(input); !ok {
		t.Fatal("expected valid input")
	}
	if !reflect.DeepEqual(input, snapshot) {
		t.Errorf("Validate mutated its input: got %v, want %v", input, snapshot)
	}
}

func TestValidateJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantJSON  string
		wantSlice []string
		ok        bool
	}{
		{
			name:      "canonical single",
			input:     `["en"]`,
			wantJSON:  `["en"]`,
			wantSlice: []string{"en"},
			ok:        true,
		},
		{
			name:      "normalizes casing",
			input:     `["EN-gb","JA"]`,
			wantJSON:  `["en-GB","ja"]`,
			wantSlice: []string{"en-GB", "ja"},
			ok:        true,
		},
		{
			name:  "invalid JSON",
			input: `not json`,
			ok:    false,
		},
		{
			name:  "empty array",
			input: `[]`,
			ok:    false,
		},
		{
			name:  "duplicate",
			input: `["en","en"]`,
			ok:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotJSON, gotSlice, ok := ValidateJSON(tc.input)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (input=%q)", ok, tc.ok, tc.input)
			}
			if !ok {
				return
			}
			if gotJSON != tc.wantJSON {
				t.Errorf("JSON = %q, want %q", gotJSON, tc.wantJSON)
			}
			if !reflect.DeepEqual(gotSlice, tc.wantSlice) {
				t.Errorf("slice = %v, want %v", gotSlice, tc.wantSlice)
			}
		})
	}
}

func TestNormalizeJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid returns canonical", `["EN-gb","JA"]`, `["en-GB","ja"]`},
		{"already canonical unchanged", `["en"]`, `["en"]`},
		{"invalid falls back to default", `not json`, DefaultJSON},
		{"empty falls back to default", `[]`, DefaultJSON},
		{"duplicates fall back to default", `["en","EN"]`, DefaultJSON},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeJSON(tc.input); got != tc.want {
				t.Errorf("NormalizeJSON(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"valid", `["en","ja"]`, []string{"en", "ja"}},
		{"empty string returns nil", "", nil},
		{"invalid JSON returns nil", `not json`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseJSON(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseJSON(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestDefaultTags_FreshCopy(t *testing.T) {
	a := DefaultTags()
	b := DefaultTags()
	if &a[0] == &b[0] {
		t.Fatal("DefaultTags must return a fresh slice on each call")
	}
	a[0] = "mutated"
	if DefaultTags()[0] != "en" {
		t.Error("mutating the returned slice leaked back into the default")
	}
}

// tooManyTags returns MaxEntries+1 distinct valid tags to exercise the
// upper bound of Validate.
func tooManyTags() []string {
	// Generate unique tags: "aa", "ab", ... past MaxEntries.
	out := make([]string, 0, MaxEntries+1)
	for i := 0; i <= MaxEntries; i++ {
		// Two-letter tag: first letter 'a'..'j', second 'a'..
		first := byte('a' + (i / 10))
		second := byte('a' + (i % 10))
		out = append(out, string([]byte{first, second}))
	}
	return out
}
