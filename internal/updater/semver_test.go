package updater

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		input   string
		want    Version
		wantErr bool
	}{
		{"1.2.3", Version{1, 2, 3, ""}, false},
		{"v1.2.3", Version{1, 2, 3, ""}, false},
		{"0.25.0", Version{0, 25, 0, ""}, false},
		{"1.0.0-beta.1", Version{1, 0, 0, "beta.1"}, false},
		{"1.2.3-rc.2", Version{1, 2, 3, "rc.2"}, false},
		{"1.2.3-dev", Version{1, 2, 3, "dev"}, false},
		{"", Version{}, true},
		{"1.2", Version{}, true},
		{"a.b.c", Version{}, true},
		{"-1.2.3", Version{}, true},
		{"1.-2.3", Version{}, true},
		{"1.2.-3", Version{}, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := Parse(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Parse(%q) = %v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		v    Version
		want string
	}{
		{Version{1, 2, 3, ""}, "1.2.3"},
		{Version{0, 25, 0, ""}, "0.25.0"},
		{Version{1, 0, 0, "beta.1"}, "1.0.0-beta.1"},
	}
	for _, tc := range tests {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestVersionGT(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.2.4", "1.2.3", true},
		{"1.3.0", "1.2.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.2.3", "1.2.3", false},
		{"1.2.3", "1.2.4", false},
		// Pre-release < release of same version.
		{"1.0.0", "1.0.0-beta.1", true},
		{"1.0.0-beta.1", "1.0.0", false},
		// Two pre-releases: lexicographic for alpha identifiers.
		{"1.0.0-rc.2", "1.0.0-beta.1", true},
		{"1.0.0-beta.1", "1.0.0-rc.2", false},
		// Numeric pre-release segments compared as integers.
		{"1.0.0-beta.10", "1.0.0-beta.2", true},
		{"1.0.0-beta.2", "1.0.0-beta.10", false},
		{"1.0.0-alpha.10", "1.0.0-alpha.9", true},
		// Numeric identifier has lower precedence than alphanumeric.
		{"1.0.0-alpha", "1.0.0-1", true},
		{"1.0.0-1", "1.0.0-alpha", false},
		// Longer pre-release wins when all segments equal.
		{"1.0.0-beta.1.extra", "1.0.0-beta.1", true},
		{"1.0.0-beta.1", "1.0.0-beta.1.extra", false},
	}
	for _, tc := range tests {
		a, err := Parse(tc.a)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.a, err)
		}
		b, err := Parse(tc.b)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.b, err)
		}
		if got := a.GT(b); got != tc.want {
			t.Errorf("%v.GT(%v) = %v, want %v", a, b, got, tc.want)
		}
	}
}

func TestIsPreRelease(t *testing.T) {
	stable, _ := Parse("1.0.0")
	beta, _ := Parse("1.0.0-beta.1")
	if stable.IsPreRelease() {
		t.Errorf("1.0.0 should not be a pre-release")
	}
	if !beta.IsPreRelease() {
		t.Errorf("1.0.0-beta.1 should be a pre-release")
	}
}
