package updater

import "testing"

func TestParseSemver(t *testing.T) {
	cases := []struct {
		input string
		want  semver
		err   bool
	}{
		{"v1.2.3", semver{1, 2, 3, ""}, false},
		{"1.2.3", semver{1, 2, 3, ""}, false},
		{"v0.9.6-rc.2", semver{0, 9, 6, "rc.2"}, false},
		{"v1.0.0-beta.1", semver{1, 0, 0, "beta.1"}, false},
		{"invalid", semver{}, true},
		{"1.2", semver{}, true},
	}

	for _, tc := range cases {
		got, err := parseSemver(tc.input)
		if tc.err {
			if err == nil {
				t.Errorf("parseSemver(%q): expected error, got %+v", tc.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSemver(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSemver(%q) = %+v, want %+v", tc.input, got, tc.want)
		}
	}
}

func TestSemverCompare(t *testing.T) {
	cases := []struct {
		a    semver
		b    semver
		want int
	}{
		{semver{1, 0, 0, ""}, semver{0, 9, 9, ""}, 1},
		{semver{0, 9, 9, ""}, semver{1, 0, 0, ""}, -1},
		{semver{1, 0, 0, ""}, semver{1, 0, 0, ""}, 0},
		{semver{1, 0, 0, ""}, semver{1, 0, 0, "rc.1"}, 1},  // stable > prerelease
		{semver{1, 0, 0, "rc.1"}, semver{1, 0, 0, ""}, -1}, // prerelease < stable
		{semver{1, 0, 0, "rc.2"}, semver{1, 0, 0, "rc.1"}, 1},
		{semver{0, 0, 1, ""}, semver{0, 0, 2, ""}, -1},
		{semver{0, 2, 0, ""}, semver{0, 1, 9, ""}, 1},
	}

	for _, tc := range cases {
		got := semverCompare(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("semverCompare(%+v, %+v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
