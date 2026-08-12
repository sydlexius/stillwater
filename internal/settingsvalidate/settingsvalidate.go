// Package settingsvalidate holds the validation rules for the application
// settings key-value table.
//
// It exists as a leaf package (standard library only) because TWO paths write
// settings and both must apply the same rules: PUT /api/v1/settings in
// internal/api, and envelope import in internal/settingsio. internal/api
// already imports internal/settingsio, so the rules cannot live in either one
// without a cycle -- and a second hand-maintained copy is exactly the drift
// that #3004 was (five keys read at boot that the registry did not carry).
//
// Every rule and error message here is preserved verbatim from the original
// internal/api implementation: the text is surfaced directly to API callers,
// and handler tests assert that a rejection names the offending key.
package settingsvalidate

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// validator validates a setting value and returns the canonical form to persist.
// If the value is invalid the returned error message is surfaced directly to the
// caller; keep it user-readable and free of internal package details.
type validator func(v string) (canonical string, err error)

// registry maps setting keys to their validation functions.
// To add a new validated setting: add one entry here.
// Keys absent from the map are accepted without validation (pass-through) --
// see Validate's `ok` return, and #3005 for whether that default should change.
var registry = map[string]validator{
	"backup_retention_count":             validatePositiveInt("backup_retention_count"),
	"backup_max_age_days":                validateNonNegativeInt("backup_max_age_days"),
	"cache.image.max_size_mb":            validateNonNegativeInt("cache.image.max_size_mb"),
	"images.backdrop.target_count":       validateIntRange("images.backdrop.target_count", 1, 10),
	"provider.name_similarity_threshold": validateIntRange("provider.name_similarity_threshold", 0, 100),
	"rule_schedule.interval_minutes":     validateRuleScheduleMinutes,
	"musicbrainz.contributions":          validateEnum("musicbrainz.contributions", "disabled", "web_form", "api"),
	"auth.method":                        validateEnum("auth.method", "local", "emby", "jellyfin"),
	"server.base_path":                   validateBasePath,
	"auth.providers.local.enabled":       validateLocalAuthEnabled,
	// Operational settings surfaced from env-only into the UI (#1746, #1753).
	"rule_engine.artist_workers": validateIntRange("rule_engine.artist_workers", 1, 64),
	"scanner.exclusions":         validateCSV,
	"scanner.mtime_fast_path":    validateBool("scanner.mtime_fast_path"),
	"backup.interval_hours":      validatePositiveInt("backup.interval_hours"),
	// MBID re-validation sweep (#2810, wired in #3003). These are read at boot
	// by getDBIntSetting, which parses with fmt.Sscanf("%d") -- a parse that
	// stops at the first non-digit and reports success. Without an entry here
	// "0.5" is stored raw, read back as a valid, in-range 0, and honored: a
	// name-similarity threshold of 0 matches every name, so the check reports
	// success while verifying nothing. Validating at the write boundary is what
	// keeps that value from ever reaching the reader (#3004).
	"mbid_revalidate.enabled":                   validateBool("mbid_revalidate.enabled"),
	"mbid_revalidate.interval_hours":            validatePositiveInt("mbid_revalidate.interval_hours"),
	"mbid_revalidate.max_per_pass":              validatePositiveInt("mbid_revalidate.max_per_pass"),
	"mbid_revalidate.name_similarity_threshold": validateIntRange("mbid_revalidate.name_similarity_threshold", 0, 100),
	"mbid_revalidate.catalogue_match_percent":   validateIntRange("mbid_revalidate.catalogue_match_percent", 0, 100),
}

// validateCSV normalises a comma-separated value the same way config.setCSV
// (the SW_SCANNER_EXCLUSIONS loader) does: split on commas, trim whitespace
// from each token, drop empty tokens, and rejoin with ", " so the persisted
// form is canonical and round-trips cleanly. An all-empty input canonicalises
// to "" (no exclusions), which is valid. Never returns an error.
func validateCSV(v string) (string, error) {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ", "), nil
}

// validateBool returns a validator that accepts the common boolean literals and
// canonicalises them to "true"/"false". Input is trimmed and lowercased so
// "TRUE", " 1 ", and similar variants are accepted.
func validateBool(key string) validator {
	return func(v string) (string, error) {
		switch strings.TrimSpace(strings.ToLower(v)) {
		case "true", "1":
			return "true", nil
		case "false", "0":
			return "false", nil
		default:
			return "", fmt.Errorf("%s must be true or false", key)
		}
	}
}

// validatePositiveInt returns a validator that accepts integers >= 1.
func validatePositiveInt(key string) validator {
	return func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", fmt.Errorf("%s must be a positive integer", key)
		}
		return v, nil
	}
}

// validateNonNegativeInt returns a validator that accepts integers >= 0.
func validateNonNegativeInt(key string) validator {
	return func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return "", fmt.Errorf("%s must be zero or a positive integer", key)
		}
		return v, nil
	}
}

// validateIntRange returns a validator that accepts integers in [lo, hi].
func validateIntRange(key string, lo, hi int) validator {
	return func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return "", fmt.Errorf("%s must be between %d and %d", key, lo, hi)
		}
		return v, nil
	}
}

// validateEnum returns a validator that accepts only the listed literal values.
func validateEnum(key string, allowed ...string) validator {
	return func(v string) (string, error) {
		for _, a := range allowed {
			if v == a {
				return v, nil
			}
		}
		return "", fmt.Errorf("%s must be %s", key, strings.Join(allowed, ", "))
	}
}

// validateRuleScheduleMinutes accepts 0 (disabled) or any value >= 5.
func validateRuleScheduleMinutes(v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil || (n != 0 && n < 5) {
		return "", errors.New("rule_schedule.interval_minutes must be 0 (disabled) or >= 5")
	}
	return v, nil
}

// validateBasePath validates the server.base_path setting.
//
// Rules:
//   - "/" (root) is the canonical "no prefix" value and is always valid.
//   - An empty string is normalised to "/".
//   - Any other value must start with "/" and must NOT end with "/".
//   - The value must not start with "//" or "/\".
//   - Allowed characters: letters, digits, hyphen, underscore, slash.
//
// We do NOT enforce here that the env override is unset: an admin who edits
// the YAML config out-of-band still expects the saved override to take effect
// on the next process restart that lacks SW_BASE_PATH. The UI already hides
// the editable input when the env override is active, so the only way to reach
// this validator with the env set is a direct API call, which we treat as
// "save the override anyway; env still wins at runtime."
func validateBasePath(v string) (string, error) {
	bp := strings.TrimSpace(v)
	if bp == "" {
		return "/", nil
	}
	if bp == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(bp, "/") {
		return "", errors.New("server.base_path must start with \"/\"")
	}
	// Mirror the loader (cmd/stillwater/main.go isValidPersistedBasePath) and
	// the client (web/templates/settings.templ saveBasePath): a second character
	// of "/" or "\" is rejected. The charset check below would already reject
	// backslash, but "//foo" passes that check and would otherwise persist a
	// value the loader then refuses to apply on next restart, leaving the user
	// with a successful save and a restart banner for a base path that is
	// silently ignored.
	if len(bp) >= 2 && (bp[1] == '/' || bp[1] == '\\') {
		return "", errors.New("server.base_path must not start with \"//\" or \"/\\\\\"")
	}
	if strings.HasSuffix(bp, "/") {
		return "", errors.New("server.base_path must not end with \"/\"")
	}
	for _, c := range bp {
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '/'
		if !ok {
			return "", errors.New("server.base_path may only contain letters, digits, hyphens, underscores, and slashes")
		}
	}
	return bp, nil
}

// validateLocalAuthEnabled rejects any attempt to disable local authentication.
// Local auth provides break-glass access when all federated providers are
// misconfigured. The value is normalised (trimmed, lowercased) before the check
// to guard against "FALSE", " false ", and similar variants.
func validateLocalAuthEnabled(v string) (string, error) {
	normalized := strings.TrimSpace(strings.ToLower(v))
	switch normalized {
	case "true", "1":
		return "true", nil
	case "false", "0", "":
		return "", errors.New("local authentication cannot be disabled; it provides break-glass access if all other providers are misconfigured")
	default:
		return "", errors.New("auth.providers.local.enabled must be \"true\"")
	}
}

// policyKeys are validators that encode a WRITE-TIME POLICY rather than a
// data-validity rule: the value they refuse is well-formed and may legitimately
// already exist in the database, but an operator is not permitted to SET it
// through the API.
//
// The distinction matters because the two answer different questions. A
// data-validity rule asks "is this a usable value?" and its answer is the same
// wherever the value comes from. A policy rule asks "may this actor make this
// change here?" -- and a RESTORE is not an actor making a change: it
// re-establishes a state that already existed, from a backup the operator
// chose to take.
//
// Applying a policy rule at restore time drops the row, which either silently
// changes a security posture or makes a legitimate backup unrestorable (#2534).
// So IsPolicy lets a caller that is re-establishing state skip these while
// still enforcing every data-validity rule.
//
// Keep this set SMALL and justify each entry. A rule belongs here only if its
// rejection is about WHO IS WRITING rather than WHAT IS WRITTEN.
var policyKeys = map[string]string{
	// Refuses any falsy value so local auth cannot be switched off through the
	// API -- it is the break-glass path when every other provider is
	// misconfigured. A backup that legitimately recorded it as disabled is a
	// state to restore, not an operator disabling it now.
	//
	// SAFE TO EXEMPT TODAY FOR A SECOND, LOAD-BEARING REASON: nothing reads
	// this value at runtime. Its only non-test consumer is
	// handlers_platform.go, which feeds templates.AuthProvidersData.LocalEnabled,
	// and that field appears nowhere in the rendered template -- the toggle is
	// hardcoded disabled. handleLogin selects the provider from auth.method,
	// never from this key. So restoring it as false disables nothing.
	//
	// RE-ASSESS BEFORE WIRING IT INTO THE LOGIN PATH. One of the two import
	// entry points (POST /api/v1/setup/restore) is UNAUTHENTICATED -- gated on
	// a zero-user instance and the envelope passphrase, which is why this is
	// not a bypass today. If this key ever gains a real runtime effect, that
	// combination becomes an auth-relevant change arriving over an
	// unauthenticated endpoint, and this entry must be reconsidered rather
	// than inherited.
	"auth.providers.local.enabled": "break-glass guard: local auth may not be disabled via the API",
}

// IsPolicy reports whether key's validator encodes a write-time policy rather
// than a data-validity rule, along with the reason. Callers re-establishing
// stored state (settings import) relax these via ValidateStoredState; callers
// accepting an operator action (PUT /api/v1/settings) must NOT.
func IsPolicy(key string) (reason string, ok bool) {
	reason, ok = policyKeys[key]
	return reason, ok
}

// policyDataValidators holds the DATA-VALIDITY half of a policy key's rule, for
// callers that relax the policy half. A policy validator typically refuses two
// different things at once -- a well-formed value the caller may not set, AND
// input that is simply malformed -- and only the first is a policy.
//
// Without this split, relaxing the policy discards the data rule too, and a
// boolean setting accepts "banana". That is not a hypothetical: it was the
// state of this code until a review probed it.
var policyDataValidators = map[string]validator{
	// The value is a boolean whatever the policy says. Canonicalises the same
	// way validateBool does, so a restored value reads back correctly through
	// getDBBoolSetting (which tests v == "true" || v == "1").
	"auth.providers.local.enabled": validateBool("auth.providers.local.enabled"),
}

// ValidateStoredState validates a value that is being RE-ESTABLISHED from
// stored state (a settings import) rather than SET by an operator.
//
// It applies every data-validity rule and relaxes only the write-time policy
// half: a restore is not an actor making a change, so refusing a value the
// operator already had either silently alters their configuration or makes a
// legitimate backup unrestorable (#2534, #3008). It is NOT a way to store
// anything -- a malformed value is still rejected, and the returned value is
// still canonical.
//
// relaxed reports whether a policy was set aside, so the caller can log it.
// Never use this for an operator-initiated write; that is Validate.
func ValidateStoredState(key, value string) (canonical string, ok bool, relaxed bool, err error) {
	canonical, ok, err = Validate(key, value)
	if err == nil || !ok {
		return canonical, ok, false, err
	}
	if _, isPolicy := policyKeys[key]; !isPolicy {
		return canonical, ok, false, err
	}
	// Policy key: drop the policy refusal, but hold the value to the data rule.
	if dataFn, hasData := policyDataValidators[key]; hasData {
		canonical, err = dataFn(value)
		if err != nil {
			// Genuinely malformed, not merely policy-refused.
			return "", true, false, err
		}
		return canonical, true, true, nil
	}
	// A policy key with no declared data validator: refuse rather than guess.
	// Adding an entry to policyKeys without one is the bug this catches.
	return "", true, false, err
}

// Validate applies the registered validator for key, returning the canonical
// form to persist.
//
// The three-value return is what lets a caller preserve the established
// pass-through behavior instead of inventing a policy of its own:
//
//	canonical -- the value to store (the input itself when no validator exists)
//	ok        -- whether a validator EXISTS for this key
//	err       -- non-nil only when a validator exists AND rejected the value
//
// A caller that wants to reject unknown keys checks ok; a caller matching
// today's semantics ignores it and stores canonical. Collapsing ok into err
// would make "no rule for this key" indistinguishable from "this value is
// invalid", which are different facts with opposite correct responses.
func Validate(key, value string) (canonical string, ok bool, err error) {
	fn, ok := registry[key]
	if !ok {
		return value, false, nil
	}
	canonical, err = fn(value)
	if err != nil {
		return "", true, err
	}
	return canonical, true, nil
}

// Has reports whether key has a registered validator.
//
// Used by tests in other packages to assert that a settings key they read is
// validated at the write boundary -- the guard added in #3007 after five keys
// shipped readable-but-unvalidated.
func Has(key string) bool {
	_, ok := registry[key]
	return ok
}

// Keys returns every registered settings key, unordered.
//
// For a caller that needs to enumerate the validated set rather than probe it
// one key at a time (see #3009, the gate asserting every boot-read key is
// validated). Returns a fresh slice; the registry itself is never exposed, so
// no caller can mutate the rules.
func Keys() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
