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

// TestComparePrerelease_UndottedRcN covers issue #2732: this project tags
// releases with a single undotted prerelease identifier ("rc11", not
// "rc.11"), which the plain spec-11.4 lexicographic path misranks
// ("rc11" < "rc9", since '1' < '9') even though the tag means rc11 is
// numerically after rc9. comparePrerelease special-cases identical
// alpha-prefix + trailing-digit-run identifiers to compare the digit run
// numerically instead. See the deviation note on comparePrerelease.
func TestComparePrerelease_UndottedRcN(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		// The reported bug: rc11 must be newer than rc9.
		{"rc11 > rc9", "rc11", "rc9", 1},
		{"rc9 < rc11", "rc9", "rc11", -1},

		// Numeric ordering holds across the digit-count boundary.
		{"rc9 < rc10", "rc9", "rc10", -1},
		{"rc10 > rc9", "rc10", "rc9", 1},
		{"rc2 < rc10", "rc2", "rc10", -1},
		{"rc10 > rc2", "rc10", "rc2", 1},

		// Equality.
		{"rc9 == rc9", "rc9", "rc9", 0},

		// Dotted identifiers still use the existing spec-correct path:
		// "11" and "9" are themselves numeric identifiers (aNum && bNum),
		// so they compare numerically regardless of this fix.
		{"rc.11 > rc.9 (dotted)", "rc.11", "rc.9", 1},
		{"rc.9 < rc.11 (dotted)", "rc.9", "rc.11", -1},

		// No trailing digits at all: plain lexicographic per spec.
		{"alpha < beta", "alpha", "beta", -1},
		{"beta > alpha", "beta", "alpha", 1},

		// Same alpha prefix, one identifier has no trailing digit run at
		// all ("rc") and the other does ("rc9"). Decision: this does NOT
		// qualify for numeric comparison (both sides must have a digit
		// run), so it falls back to plain lexicographic string comparison,
		// where "rc" < "rc9" because "rc" is a proper prefix of "rc9".
		{"rc < rc9 (bare prefix)", "rc", "rc9", -1},
		{"rc9 > rc (bare prefix)", "rc9", "rc", 1},

		// Different alpha prefixes with digits: the prefix decides, not
		// the number ("alpha2" has a lexicographically smaller prefix
		// than "beta1" regardless of 2 vs 1).
		{"alpha2 < beta1 (prefix decides)", "alpha2", "beta1", -1},
		{"beta1 > alpha2 (prefix decides)", "beta1", "alpha2", 1},

		// Purely numeric identifiers still follow the existing numeric
		// path (aNum && bNum in comparePrerelease), unaffected by this fix.
		{"1 < 9 (pure numeric)", "1", "9", -1},
		{"9 > 1 (pure numeric)", "9", "1", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := comparePrerelease(tc.a, tc.b); got != tc.want {
				t.Errorf("comparePrerelease(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestComparePrerelease_RcSequenceSorts verifies the full real tag sequence
// sorts correctly end to end: rc1 < rc2 < rc9 < rc10 < rc11.
func TestComparePrerelease_RcSequenceSorts(t *testing.T) {
	seq := []string{"rc1", "rc2", "rc9", "rc10", "rc11"}
	for i := 0; i < len(seq)-1; i++ {
		lo, hi := seq[i], seq[i+1]
		if got := comparePrerelease(lo, hi); got != -1 {
			t.Errorf("comparePrerelease(%q, %q) = %d, want -1", lo, hi, got)
		}
		if got := comparePrerelease(hi, lo); got != 1 {
			t.Errorf("comparePrerelease(%q, %q) = %d, want 1", hi, lo, got)
		}
	}
}

// TestSemverCompare_ReleaseOutranksPrereleaseOfSameVersion is a focused
// regression for issue #2732's context: a release version must still
// outrank any prerelease of that same version, independent of the rc11-vs-
// rc9 fix (this is the a.PreRelease == "" branch in semverCompare, not
// comparePrerelease).
func TestSemverCompare_ReleaseOutranksPrereleaseOfSameVersion(t *testing.T) {
	release := semver{1, 6, 0, ""}
	rc11 := semver{1, 6, 0, "rc11"}
	if got := semverCompare(release, rc11); got != 1 {
		t.Errorf("semverCompare(1.6.0, 1.6.0-rc11) = %d, want 1", got)
	}
	if got := semverCompare(rc11, release); got != -1 {
		t.Errorf("semverCompare(1.6.0-rc11, 1.6.0) = %d, want -1", got)
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
