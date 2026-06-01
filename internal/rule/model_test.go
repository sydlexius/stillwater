package rule

import (
	"net/url"
	"testing"
)

// TestTriFilterIsEmpty verifies IsEmpty across the include/exclude state matrix:
// a zero-value filter is empty (adds no SQL clause), and any non-empty include
// OR exclude set makes it non-empty.
func TestTriFilterIsEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    TriFilter
		want bool
	}{
		{name: "zero value is empty", f: TriFilter{}, want: true},
		{name: "include only is not empty", f: TriFilter{Include: []string{"error"}}, want: false},
		{name: "exclude only is not empty", f: TriFilter{Exclude: []string{"info"}}, want: false},
		{name: "both populated is not empty", f: TriFilter{Include: []string{"error"}, Exclude: []string{"info"}}, want: false},
		{name: "empty non-nil slices are empty", f: TriFilter{Include: []string{}, Exclude: []string{}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIncludeOnly verifies the back-compat bridge: a non-empty value becomes a
// single-element Include set (and an empty Exclude), while the empty string
// yields a neutral (zero-value) TriFilter that constrains nothing.
func TestIncludeOnly(t *testing.T) {
	t.Parallel()

	// Empty string is the neutral case: no include, no exclude, IsEmpty true.
	empty := IncludeOnly("")
	if !empty.IsEmpty() {
		t.Errorf("IncludeOnly(\"\") should be empty, got Include=%v Exclude=%v", empty.Include, empty.Exclude)
	}

	// A real value lands in Include with nothing excluded.
	f := IncludeOnly("error")
	if len(f.Include) != 1 || f.Include[0] != "error" {
		t.Errorf("IncludeOnly(\"error\").Include = %v, want [error]", f.Include)
	}
	if len(f.Exclude) != 0 {
		t.Errorf("IncludeOnly(\"error\").Exclude = %v, want empty", f.Exclude)
	}
	if f.IsEmpty() {
		t.Errorf("IncludeOnly(\"error\") should not be empty")
	}
}

// TestTriFilterNormalized verifies the three invariants Normalized applies up
// front so every downstream consumer (URL round-trip, chip rendering, count
// queries) sees the same canonical state:
//
//   - Dedupe: each side keeps only the first occurrence of a value.
//   - Exclude-wins: a value in BOTH Include and Exclude is dropped from Include
//     and kept only in Exclude (matching the SQL, where NOT IN always removes a
//     row even if an IN include would have kept it).
//   - Whitelist: a non-empty Include puts the dimension in whitelist mode, so
//     stale Exclude entries are dropped to neutral.
//
// The empty/neutral case stays empty, and the original filter is not mutated.
func TestTriFilterNormalized(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		in          TriFilter
		wantInclude []string
		wantExclude []string
	}{
		{
			name:        "empty stays empty",
			in:          TriFilter{},
			wantInclude: nil,
			wantExclude: nil,
		},
		{
			name:        "empty non-nil slices normalize to empty",
			in:          TriFilter{Include: []string{}, Exclude: []string{}},
			wantInclude: nil,
			wantExclude: nil,
		},
		{
			name:        "dedupe include side keeps first occurrence",
			in:          TriFilter{Include: []string{"error", "error", "warning", "warning"}},
			wantInclude: []string{"error", "warning"},
			wantExclude: nil,
		},
		{
			name:        "dedupe exclude side keeps first occurrence",
			in:          TriFilter{Exclude: []string{"info", "info", "warning"}},
			wantInclude: nil,
			wantExclude: []string{"info", "warning"},
		},
		{
			// Exclude-wins: a value in both sets is dropped from Include. Here
			// "error" survives in Include (not excluded), and the remaining
			// Include is non-empty, so whitelist mode then drops the leftover
			// exclude too. To isolate exclude-wins from whitelist, the only
			// included value is also excluded so Include empties out.
			name:        "exclude wins drops shared value from include",
			in:          TriFilter{Include: []string{"warning"}, Exclude: []string{"warning"}},
			wantInclude: nil,
			wantExclude: []string{"warning"},
		},
		{
			// Whitelist: a surviving include forces excludes to neutral. "error"
			// is not excluded so it survives Include; "info" is a stale exclude
			// that is dropped because the dimension is now in whitelist mode.
			name:        "whitelist drops stale excludes when include survives",
			in:          TriFilter{Include: []string{"error"}, Exclude: []string{"info"}},
			wantInclude: []string{"error"},
			wantExclude: nil,
		},
		{
			// Exclude-wins removes the shared value from Include, but a different
			// included value still survives, so whitelist mode still drops the
			// excludes (including the one that won exclude-wins) to neutral.
			name:        "exclude wins then whitelist clears excludes",
			in:          TriFilter{Include: []string{"error", "warning"}, Exclude: []string{"warning"}},
			wantInclude: []string{"error"},
			wantExclude: nil,
		},
		{
			name:        "exclude only is preserved and deduped",
			in:          TriFilter{Exclude: []string{"nfo", "image", "nfo"}},
			wantInclude: nil,
			wantExclude: []string{"nfo", "image"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Snapshot the input slices to assert the original is not mutated.
			origInclude := append([]string(nil), tc.in.Include...)
			origExclude := append([]string(nil), tc.in.Exclude...)

			got := tc.in.Normalized()
			if !equalSlices(got.Include, tc.wantInclude) {
				t.Errorf("Normalized().Include = %v, want %v", got.Include, tc.wantInclude)
			}
			if !equalSlices(got.Exclude, tc.wantExclude) {
				t.Errorf("Normalized().Exclude = %v, want %v", got.Exclude, tc.wantExclude)
			}
			// The original must be untouched (Normalized returns a copy).
			if !equalSlices(tc.in.Include, origInclude) {
				t.Errorf("Normalized mutated input Include: %v, want %v", tc.in.Include, origInclude)
			}
			if !equalSlices(tc.in.Exclude, origExclude) {
				t.Errorf("Normalized mutated input Exclude: %v, want %v", tc.in.Exclude, origExclude)
			}
		})
	}
}

// TestTriFilterAppendURLValues verifies the shared wire-form emitter: includes
// carry a "+" prefix and excludes a "-" prefix, both repeated under the same
// key, and a neutral (empty) filter emits nothing. This is the single emitter
// the push-URL handler and the dashboard template helpers both delegate to, so
// the prefixed contract cannot drift between them.
func TestTriFilterAppendURLValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    TriFilter
		key  string
		want []string // expected values under key (order matches include-then-exclude)
	}{
		{
			name: "neutral emits nothing",
			f:    TriFilter{},
			key:  "severity",
			want: nil,
		},
		{
			name: "include-only emits plus prefix",
			f:    TriFilter{Include: []string{"error", "warning"}},
			key:  "severity",
			want: []string{"+error", "+warning"},
		},
		{
			name: "exclude-only emits minus prefix",
			f:    TriFilter{Exclude: []string{"info"}},
			key:  "severity",
			want: []string{"-info"},
		},
		{
			name: "both sides emit include first then exclude",
			f:    TriFilter{Include: []string{"nfo"}, Exclude: []string{"image", "metadata"}},
			key:  "category",
			want: []string{"+nfo", "-image", "-metadata"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := url.Values{}
			tc.f.AppendURLValues(q, tc.key)
			got := q[tc.key]
			if !equalSlices(got, tc.want) {
				t.Errorf("AppendURLValues under %q = %v, want %v", tc.key, got, tc.want)
			}
			// A neutral filter must not register the key at all.
			if len(tc.want) == 0 {
				if _, ok := q[tc.key]; ok {
					t.Errorf("neutral filter registered key %q (values %v)", tc.key, q[tc.key])
				}
			}
		})
	}
}

// equalSlices reports element-wise equality, treating nil and empty as equal so
// the assertions read naturally against the nil-preserving Normalized contract.
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
