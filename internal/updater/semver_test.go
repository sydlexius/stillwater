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
		// Negative components must be rejected even though strconv.Atoi
		// accepts them. parseSemver is exported within the package, so
		// callers bypassing the upstream semverRE gate must not produce a
		// negative-valued semver that would then sort before legitimate
		// zero-prefixed versions.
		{"v-1.2.3", semver{}, true},
		{"v1.-2.3", semver{}, true},
		{"v1.2.-3", semver{}, true},
		// Empty prerelease suffix: "v1.2.3-" must be rejected rather than
		// normalized to a stable release with PreRelease == "".
		{"v1.2.3-", semver{}, true},
		// Leading zeros in core components are forbidden by SemVer spec 2.
		// The parser must not silently normalize "v01.2.3" to "1.2.3".
		{"v01.2.3", semver{}, true},
		{"v1.02.3", semver{}, true},
		{"v1.2.03", semver{}, true},
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

func TestIsSemverNumeric(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"0", true},
		{"1", true},
		{"42", true},
		{"", false},
		// Signed integers are valid for strconv.Atoi but NOT valid SemVer
		// numeric identifiers (spec 9 / 11 restrict to [0-9]+). Treating
		// them as numeric would give "-1" higher precedence than "1" via
		// integer comparison; treating them as alphanumeric is correct.
		{"-1", false},
		{"+1", false},
		// Leading zeros are invalid per SemVer spec 9, except for bare "0".
		{"01", false},
		{"001", false},
		// Mixed alphanumeric stays alphanumeric.
		{"rc1", false},
		{"1a", false},
	}
	for _, tc := range cases {
		if got := isSemverNumeric(tc.input); got != tc.want {
			t.Errorf("isSemverNumeric(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestComparePrerelease_SemverSpec(t *testing.T) {
	// Spec-compliant precedence: a numeric identifier sorts lower than an
	// alphanumeric one, independent of the actual characters. Before the
	// digits-only fix, "-1" was parsed numerically by strconv.Atoi and
	// would have sorted lower than "1"; with the fix, "-1" is treated as
	// alphanumeric and sorts higher than the purely numeric "1".
	if got := comparePrerelease("rc.1", "rc.-1"); got != -1 {
		t.Errorf(`comparePrerelease("rc.1", "rc.-1") = %d, want -1 (numeric < alphanumeric)`, got)
	}
	if got := comparePrerelease("rc.-1", "rc.1"); got != 1 {
		t.Errorf(`comparePrerelease("rc.-1", "rc.1") = %d, want 1 (alphanumeric > numeric)`, got)
	}
	// Leading-zero identifier is alphanumeric under the spec.
	if got := comparePrerelease("rc.1", "rc.01"); got != -1 {
		t.Errorf(`comparePrerelease("rc.1", "rc.01") = %d, want -1 (numeric < alphanumeric leading-zero)`, got)
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
		{semver{1, 0, 0, "rc.10"}, semver{1, 0, 0, "rc.2"}, 1},  // numeric: rc.10 > rc.2
		{semver{1, 0, 0, "rc.2"}, semver{1, 0, 0, "rc.10"}, -1}, // numeric: rc.2 < rc.10
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
