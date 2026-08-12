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

// -- Tests for the package's exported API --

// TestValidate covers the three-value contract every consumer depends on.
//
// This exists because the package's exported surface was, briefly, guarded
// only by internal/api's handler tests -- one package away. A hostile review
// proved it: gutting Validate so it returned the raw value without calling the
// validator passed every test in this package and failed only in internal/api.
// That is fine while internal/api is the sole consumer and worthless the
// moment there is another, which is exactly what #3008 adds (settingsio, whose
// entire point is that it does NOT route through internal/api).
//
// The ok/err split is the part worth pinning: "no rule for this key" and "this
// value is invalid" are different facts with opposite correct responses, and a
// consumer that conflates them either rejects every unknown key or accepts
// every bad value.
func TestValidate(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		key       string
		value     string
		canonical string
		wantOK    bool
		// wantErrMsg is the EXACT user-visible message, empty when no error is
		// expected. Exact rather than a substring: this text reaches the
		// operator through PUT /api/v1/settings, so a validator quietly
		// rewording its rejection is a user-visible change that a
		// "contains the key" assertion would wave through.
		wantErrMsg string
	}{
		// A registered key with a valid value: canonical form, ok, no error.
		{"valid int in range", "provider.name_similarity_threshold", "60", "60", true, ""},
		{"valid bool canonicalised", "scanner.mtime_fast_path", "TRUE", "true", true, ""},
		{"valid bool from 1", "scanner.mtime_fast_path", "1", "true", true, ""},
		{"valid csv normalised", "scanner.exclusions", " VA ,  , OST ", "VA, OST", true, ""},
		{"valid base path coerced", "server.base_path", "", "/", true, ""},
		// The backdrop count's 1-10 bound had no test anywhere in the repo
		// before this package existed -- a hostile review found that mutating
		// its lower bound to 0 survived every suite. Both ends pinned here.
		{"backdrop count lower bound", "images.backdrop.target_count", "1", "1", true, ""},
		{"backdrop count upper bound", "images.backdrop.target_count", "10", "10", true, ""},

		// A registered key with an invalid value: ok is TRUE (a rule exists)
		// and err is non-nil. A caller must not read ok=true as "accepted".
		{"int out of range", "provider.name_similarity_threshold", "101", "", true,
			"provider.name_similarity_threshold must be between 0 and 100"},
		{"int fractional", "provider.name_similarity_threshold", "0.5", "", true,
			"provider.name_similarity_threshold must be between 0 and 100"},
		{"bool unparsable", "scanner.mtime_fast_path", "sometimes", "", true,
			"scanner.mtime_fast_path must be true or false"},
		{"positive int zero", "backup.interval_hours", "0", "", true,
			"backup.interval_hours must be a positive integer"},
		{"backdrop count below range", "images.backdrop.target_count", "0", "", true,
			"images.backdrop.target_count must be between 1 and 10"},
		{"backdrop count above range", "images.backdrop.target_count", "11", "", true,
			"images.backdrop.target_count must be between 1 and 10"},

		// An UNREGISTERED key: the value passes through unchanged, ok is
		// false, and there is no error. Losing the raw value here would make
		// every unvalidated setting store an empty string.
		{"unknown key passes through", "definitely.not.registered", "anything", "anything", false, ""},
		{"unknown key keeps empty", "definitely.not.registered", "", "", false, ""},
		{"pre-rename key is unknown", "mbid_revalidate.name_similarity", "60", "60", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			canonical, ok, err := Validate(c.key, c.value)
			if ok != c.wantOK {
				t.Errorf("Validate(%q, %q) ok = %v, want %v", c.key, c.value, ok, c.wantOK)
			}
			if canonical != c.canonical {
				t.Errorf("Validate(%q, %q) canonical = %q, want %q",
					c.key, c.value, canonical, c.canonical)
			}
			// FATAL, not Errorf: everything below dereferences err, and
			// t.Errorf continues. A mutation that wrongly returns a nil error
			// on a wantErrMsg case would otherwise panic on err.Error()
			// instead of reporting a legible failure -- in a test whose whole
			// job is to be mutated.
			if c.wantErrMsg == "" {
				if err != nil {
					t.Fatalf("Validate(%q, %q) unexpected error: %v", c.key, c.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%q, %q) expected error %q, got nil", c.key, c.value, c.wantErrMsg)
			}
			// Exact, not a substring: the message reaches the operator through
			// PUT /api/v1/settings, and it must name the offending key so a
			// multi-key save says which one was refused.
			if err.Error() != c.wantErrMsg {
				t.Errorf("Validate(%q, %q) error = %q, want %q",
					c.key, c.value, err.Error(), c.wantErrMsg)
			}
		})
	}
}

// TestHas covers the predicate the cross-package drift guards use (#3007,
// #3009). A stub returning a constant fails here in both directions: `true`
// fails the unregistered cases, `false` the registered ones.
func TestHas(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		key  string
		want bool
	}{
		{"provider.name_similarity_threshold", true},
		{"mbid_revalidate.enabled", true},
		{"server.base_path", true},
		{"scanner.exclusions", true},
		// Read at boot but deliberately unvalidated today, tracked in #3005.
		// If one gains a validator, flip the expectation -- do not delete it.
		{"logging.level", false},
		{"db_maintenance.enabled", false},
		// Renamed away in #3004; must not come back.
		{"mbid_revalidate.name_similarity", false},
		{"definitely.not.a.real.setting.key", false},
	} {
		if got := Has(c.key); got != c.want {
			t.Errorf("Has(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestKeys asserts the enumeration matches the registry and, more importantly,
// that a caller cannot reach the registry through the returned slice. Keys is
// consumed by the #3009 gate, which walks every validated key.
func TestKeys(t *testing.T) {
	t.Parallel()
	keys := Keys()
	if len(keys) != len(registry) {
		t.Fatalf("Keys() returned %d keys, registry holds %d", len(keys), len(registry))
	}
	for _, k := range keys {
		if !Has(k) {
			t.Errorf("Keys() returned %q, which Has() does not recognize", k)
		}
	}
	// Mutating the returned slice must not affect the registry. Without the
	// defensive copy this would corrupt the rules for every later caller in
	// the process.
	if len(keys) > 0 {
		victim := keys[0]
		keys[0] = "clobbered"
		if !Has(victim) {
			t.Errorf("mutating the slice Keys() returned removed %q from the registry", victim)
		}
		if Has("clobbered") {
			t.Error("mutating the slice Keys() returned inserted a key into the registry")
		}
	}
}

// TestValidateStoredState covers the restore-path validator directly, in the
// package that owns it.
//
// It exists in this form because the previous PR's review found the same shape:
// an exported function whose only caller lives in another package is guarded by
// that caller's tests, which is an accident of who happens to call it rather
// than a property of this package. The pre-push gate caught the repeat --
// these 19 lines were at 0% coverage here while passing every test in
// internal/settingsio.
//
// The contract has four outcomes and each is a different fact:
//   - no validator            -> pass through, ok=false, relaxed=false
//   - valid                   -> canonical, ok=true,  relaxed=false
//   - policy-refused but sane -> canonical, ok=true,  relaxed=TRUE
//   - malformed               -> error even for a policy key
func TestValidateStoredState(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		key         string
		value       string
		canonical   string
		wantOK      bool
		wantRelaxed bool
		wantErr     bool
	}{
		// Ordinary data rules are unchanged from Validate: a restore does not
		// get to store a malformed value.
		{"valid int", "provider.name_similarity_threshold", "60", "60", true, false, false},
		{"invalid int still rejected", "provider.name_similarity_threshold", "101", "", true, false, true},
		{"fractional still rejected", "provider.name_similarity_threshold", "0.5", "", true, false, true},
		{"unknown key passes through", "not.a.registered.key", "anything", "anything", false, false, false},

		// The policy key. "false" is REFUSED by Validate (the break-glass
		// guard) but is a legitimate stored state to re-establish.
		{"policy-refused value is relaxed", "auth.providers.local.enabled", "false", "false", true, true, false},
		{"policy-refused, canonicalised", "auth.providers.local.enabled", "  FALSE  ", "false", true, true, false},
		{"policy value that is also VALID is not relaxed", "auth.providers.local.enabled", "true", "true", true, false, false},
		{"policy value canonicalised without relaxing", "auth.providers.local.enabled", "1", "true", true, false, false},

		// The half that a previous revision got wrong: relaxing the POLICY
		// must never relax the DATA rule.
		{"malformed rejected even for a policy key", "auth.providers.local.enabled", "banana", "", true, false, true},
		{"empty rejected even for a policy key", "auth.providers.local.enabled", "", "", true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			canonical, ok, relaxed, err := ValidateStoredState(c.key, c.value)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if relaxed != c.wantRelaxed {
				t.Errorf("relaxed = %v, want %v", relaxed, c.wantRelaxed)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if canonical != c.canonical {
				t.Errorf("canonical = %q, want %q", canonical, c.canonical)
			}
		})
	}
}

// TestValidateStoredState_MatchesValidateOffPolicyPath asserts the two entry
// points agree everywhere except the policy keys. Without this, a future edit
// could quietly loosen the restore path for ordinary settings -- the exemption
// is meant to be a narrow carve-out, not a second, weaker validator.
func TestValidateStoredState_MatchesValidateOffPolicyPath(t *testing.T) {
	t.Parallel()
	// Values chosen to exercise both an accept and a reject per key shape.
	for _, v := range []string{"true", "false", "1", "0", "", "60", "101", "0.5", "banana", "/sw"} {
		for _, key := range Keys() {
			if _, isPolicy := IsPolicy(key); isPolicy {
				continue // the carve-out; covered explicitly above
			}
			wantCanonical, wantOK, wantErr := Validate(key, v)
			gotCanonical, gotOK, relaxed, gotErr := ValidateStoredState(key, v)
			if relaxed {
				t.Errorf("key %q value %q: relaxed=true for a NON-policy key", key, v)
			}
			if gotOK != wantOK || (gotErr != nil) != (wantErr != nil) || gotCanonical != wantCanonical {
				t.Errorf("key %q value %q: ValidateStoredState = (%q,%v,%v), Validate = (%q,%v,%v)",
					key, v, gotCanonical, gotOK, gotErr, wantCanonical, wantOK, wantErr)
			}
		}
	}
}

// TestPolicyKeysHaveDataValidators pins the fail-closed rule: every policy key
// must declare the data half of its validator, or ValidateStoredState refuses
// the value rather than guessing. Adding an entry to policyKeys without one is
// the mistake this catches, and it would otherwise surface as a restore that
// silently drops a setting.
func TestPolicyKeysHaveDataValidators(t *testing.T) {
	t.Parallel()
	for key := range policyKeys {
		if _, ok := policyDataValidators[key]; !ok {
			t.Errorf("policy key %q has no entry in policyDataValidators: "+
				"ValidateStoredState will refuse every value for it", key)
		}
		if !Has(key) {
			t.Errorf("policy key %q is not in the validator registry at all", key)
		}
	}
	// And the set stays deliberately small. A second entry is a decision that
	// warrants review -- one import entry point is unauthenticated -- not a
	// one-line addition that slips through.
	if len(policyKeys) != 1 {
		t.Errorf("policyKeys has %d entries; it had 1 when this guard was written. "+
			"Adding one is fine, but confirm the new key has no security-relevant "+
			"runtime reader before updating this count", len(policyKeys))
	}
}
