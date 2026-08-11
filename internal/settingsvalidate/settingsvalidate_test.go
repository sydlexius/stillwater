package settingsvalidate

import (
	"testing"
)

// -- Unit tests for validator functions --

func TestValidatePositiveInt(t *testing.T) {
	t.Parallel()
	fn := validatePositiveInt("my_key")
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"1", false},
		{"100", false},
		{"0", true},
		{"-1", true},
		{"abc", true},
		{"", true},
	}
	for _, c := range cases {
		canon, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error, got canonical %q", c.input, canon)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
		if !c.wantErr && canon != c.input {
			t.Errorf("input %q: canonical = %q, want %q", c.input, canon, c.input)
		}
	}
}

func TestValidateNonNegativeInt(t *testing.T) {
	t.Parallel()
	fn := validateNonNegativeInt("my_key")
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"0", false},
		{"1", false},
		{"999", false},
		{"-1", true},
		{"abc", true},
		{"", true},
	}
	for _, c := range cases {
		_, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

func TestValidateIntRange(t *testing.T) {
	t.Parallel()
	fn := validateIntRange("my_key", 5, 10)
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"5", false},
		{"7", false},
		{"10", false},
		{"4", true},
		{"11", true},
		{"abc", true},
	}
	for _, c := range cases {
		_, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

// TestOpsSettingsValidators covers the four operational settings surfaced from
// env-only into the UI (#1746, #1753): the registry wiring plus each
// validator's canonicalisation and bounds.
func TestOpsSettingsValidators(t *testing.T) {
	t.Parallel()

	// rule_engine.artist_workers: bounded int 1..64.
	workers, ok := registry["rule_engine.artist_workers"]
	if !ok {
		t.Fatal("rule_engine.artist_workers not registered in the validator registry")
	}
	workerCases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"1", "1", false},
		{"2", "2", false},
		{"64", "64", false},
		{"0", "", true},
		{"65", "", true},
		{"-1", "", true},
		{"abc", "", true},
	}
	for _, c := range workerCases {
		got, err := workers(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("artist_workers(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("artist_workers(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("artist_workers(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// scanner.exclusions: CSV normaliser -- trim tokens, drop empties, rejoin.
	excl, ok := registry["scanner.exclusions"]
	if !ok {
		t.Fatal("scanner.exclusions not registered in the validator registry")
	}
	exclCases := []struct{ in, want string }{
		{"Various Artists, Soundtrack", "Various Artists, Soundtrack"},
		{" VA ,  , OST ", "VA, OST"},
		{"", ""},
		{"  ,  ", ""},
		{"Single", "Single"},
	}
	for _, c := range exclCases {
		got, err := excl(c.in)
		if err != nil {
			t.Errorf("exclusions(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("exclusions(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// scanner.mtime_fast_path: bool, canonicalised to true/false.
	mtime, ok := registry["scanner.mtime_fast_path"]
	if !ok {
		t.Fatal("scanner.mtime_fast_path not registered in the validator registry")
	}
	mtimeCases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"true", "true", false},
		{"TRUE", "true", false},
		{" 1 ", "true", false},
		{"false", "false", false},
		{"0", "false", false},
		{"yes", "", true},
		{"", "", true},
	}
	for _, c := range mtimeCases {
		got, err := mtime(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("mtime(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("mtime(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("mtime(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// backup.interval_hours: positive int (>= 1).
	interval, ok := registry["backup.interval_hours"]
	if !ok {
		t.Fatal("backup.interval_hours not registered in the validator registry")
	}
	for _, c := range []struct {
		in      string
		wantErr bool
	}{
		{"1", false},
		{"24", false},
		{"0", true},
		{"-3", true},
		{"abc", true},
	} {
		_, err := interval(c.in)
		if c.wantErr && err == nil {
			t.Errorf("interval(%q): expected error", c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("interval(%q): unexpected error: %v", c.in, err)
		}
	}
}

func TestValidateEnum(t *testing.T) {
	t.Parallel()
	fn := validateEnum("my_key", "alpha", "beta", "gamma")
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"alpha", false},
		{"beta", false},
		{"gamma", false},
		{"delta", true},
		{"", true},
		{"Alpha", true}, // case-sensitive
	}
	for _, c := range cases {
		_, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

func TestValidateRuleScheduleMinutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"0", false}, // disabled
		{"5", false}, // minimum non-zero
		{"60", false},
		{"1", true}, // 1-4 rejected
		{"4", true},
		{"-1", true},
		{"abc", true},
	}
	for _, c := range cases {
		_, err := validateRuleScheduleMinutes(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

func TestValidateBasePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input     string
		wantErr   bool
		canonical string
	}{
		{"/", false, "/"},
		{"", false, "/"},    // normalised to /
		{"   ", false, "/"}, // whitespace normalised to /
		{"/app", false, "/app"},
		{"/my-app", false, "/my-app"},
		{"/apps/stillwater", false, "/apps/stillwater"},
		{"app", true, ""},       // missing leading /
		{"/app/", true, ""},     // trailing /
		{"//app", true, ""},     // double leading /
		{"/app name", true, ""}, // space not allowed
		{"/v1.0", true, ""},     // dot not allowed
	}
	for _, c := range cases {
		got, err := validateBasePath(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("input %q: expected error, got canonical %q", c.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
			continue
		}
		if got != c.canonical {
			t.Errorf("input %q: canonical = %q, want %q", c.input, got, c.canonical)
		}
	}
}

func TestValidateLocalAuthEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input     string
		wantErr   bool
		canonical string
	}{
		{"true", false, "true"},
		{"1", false, "true"},
		{"TRUE", false, "true"},   // normalised
		{" true ", false, "true"}, // trimmed
		{"false", true, ""},
		{"0", true, ""},
		{"False", true, ""},
		{" false ", true, ""},
		{"yes", true, ""},
		{"", true, ""},
	}
	for _, c := range cases {
		got, err := validateLocalAuthEnabled(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("input %q: expected error, got canonical %q", c.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
			continue
		}
		if got != c.canonical {
			t.Errorf("input %q: canonical = %q, want %q", c.input, got, c.canonical)
		}
	}
}
