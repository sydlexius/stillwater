package rule

import (
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// ErrNotFound is returned when a rule record does not exist.
var ErrNotFound = errors.New("rule not found")

// ErrJobNotFound is returned when a bulk job record does not exist.
var ErrJobNotFound = errors.New("bulk job not found")

// ErrViolationNotFound is returned when a violation record does not exist
// or is not in the expected state for the requested operation.
var ErrViolationNotFound = errors.New("violation not found")

const (
	// AutomationModeAuto indicates the rule runs automatically during evaluation.
	AutomationModeAuto = "auto"
	// AutomationModeManual indicates the rule requires manual triggering.
	AutomationModeManual = "manual"
)

// RuleCategory is the functional grouping of a rule.
type RuleCategory string

const (
	// RuleCategoryNFO groups rules that validate NFO file presence and content.
	RuleCategoryNFO RuleCategory = "nfo"
	// RuleCategoryImage groups rules that validate artwork dimensions and format.
	RuleCategoryImage RuleCategory = "image"
	// RuleCategoryMetadata groups rules that validate artist metadata fields.
	RuleCategoryMetadata RuleCategory = "metadata"
)

// Rule represents a validation rule stored in the database.
type Rule struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	Category            RuleCategory `json:"category"`
	Enabled             bool         `json:"enabled"`
	AutomationMode      string       `json:"automation_mode"` // "auto", "manual"
	Config              RuleConfig   `json:"config"`
	FilesystemDependent bool         `json:"filesystem_dependent"` // true if rule requires a local library with filesystem path
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
}

// RuleConfig holds configurable acceptance criteria for a rule.
// Stored as JSON in the config column.
type RuleConfig struct {
	MinWidth            int     `json:"min_width,omitempty"`
	MinHeight           int     `json:"min_height,omitempty"`
	AspectRatio         float64 `json:"aspect_ratio,omitempty"`
	Tolerance           float64 `json:"tolerance,omitempty"`
	MinLength           int     `json:"min_length,omitempty"`
	Severity            string  `json:"severity,omitempty"`              // "error", "warning", "info"
	SelectBestCandidate bool    `json:"select_best_candidate,omitempty"` // auto-pick highest-res when multiple candidates
	ThresholdPercent    float64 `json:"threshold_percent,omitempty"`     // total padding area % threshold (logo_padding)
	TrimMargin          int     `json:"trim_margin,omitempty"`           // pixels of padding to keep after trimming (logo_padding)
	MinCount            int     `json:"min_count,omitempty"`             // minimum number of images (backdrop_min_count)
	ArticleMode         string  `json:"article_mode,omitempty"`          // "prefix" (default), "suffix", "strip"
	CoverageThreshold   float64 `json:"coverage_threshold,omitempty"`    // discography_populated: min % of MB release groups the NFO must cover (0-100)
	ReleaseTypes        string  `json:"release_types,omitempty"`         // discography_populated: comma-separated MB primary types to include (e.g. "Album,EP")
	DiscoveryOnly       bool    `json:"-"`                               // transient: set by pipeline in manual mode, never persisted
}

// Violation represents a single rule failure for an artist.
type Violation struct {
	RuleID   string     `json:"rule_id"`
	RuleName string     `json:"rule_name"`
	Category string     `json:"category"`
	Severity string     `json:"severity"`
	Message  string     `json:"message"`
	Fixable  bool       `json:"fixable"`
	Config   RuleConfig `json:"-"` // populated at evaluation time; not serialized
}

// ImageCandidate represents one provider-sourced image option for a pending violation.
type ImageCandidate struct {
	URL       string `json:"url"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Source    string `json:"source"`     // provider name
	ImageType string `json:"image_type"` // "fanart", "thumb", "logo", "banner"
}

// EvaluationResult holds the outcome of running all rules against an artist.
//
// RulesConsidered is the ordered list of rule IDs the engine actually ran
// (enabled, registered checker, not skipped for a filesystem-missing artist).
// Callers that persist pass/fail results (issue #699) diff this set against
// Violations to learn which rules passed without re-querying the rule list.
type EvaluationResult struct {
	ArtistID        string      `json:"artist_id"`
	ArtistName      string      `json:"artist_name"`
	Violations      []Violation `json:"violations"`
	RulesPassed     int         `json:"rules_passed"`
	RulesTotal      int         `json:"rules_total"`
	HealthScore     float64     `json:"health_score"`
	RulesConsidered []string    `json:"-"` // not serialized; consumed by persistence paths
	// RulesSkipped lists the enabled rules that did NOT run for this artist
	// because they cannot apply to it (no local path, or no comparable stored
	// data), each with a reason. These rules are outside RulesTotal and
	// RulesPassed entirely: a rule that never examined the artist is neither a
	// pass nor a failure, and counting it as a pass is the bug #2509 fixed.
	// Serialized so an API caller can tell "skipped" from "passed".
	RulesSkipped []SkippedRule `json:"rules_skipped,omitempty"`
	// Scoped marks a result produced by EvaluateScoped for a subset of rules.
	// HealthScore is left zero on such a result: health means passed/total across
	// ALL eligible rules, so a subset score is not the artist's score and must
	// never be persisted as one (#2476).
	Scoped bool `json:"-"`
}

// HealthSnapshot represents a recorded library health score.
type HealthSnapshot struct {
	ID               string    `json:"id"`
	TotalArtists     int       `json:"total_artists"`
	CompliantArtists int       `json:"compliant_artists"`
	Score            float64   `json:"score"`
	RecordedAt       time.Time `json:"recorded_at"`
}

const (
	// ViolationStatusOpen indicates an unresolved rule violation.
	ViolationStatusOpen = "open"
	// ViolationStatusDismissed indicates a violation manually dismissed by the user.
	ViolationStatusDismissed = "dismissed"
	// ViolationStatusResolved indicates a violation that has been fixed.
	ViolationStatusResolved = "resolved"
	// ViolationStatusPendingChoice indicates multiple image candidates awaiting user selection.
	ViolationStatusPendingChoice = "pending_choice"
)

// RuleViolation represents a persisted rule violation for the notifications view.
type RuleViolation struct {
	ID          string           `json:"id"`
	RuleID      string           `json:"rule_id"`
	ArtistID    string           `json:"artist_id"`
	ArtistName  string           `json:"artist_name"`
	LibraryName string           `json:"library_name"`
	Severity    string           `json:"severity"`
	Message     string           `json:"message"`
	Fixable     bool             `json:"fixable"`
	Status      string           `json:"status"` // "open", "dismissed", "resolved", "pending_choice"
	Candidates  []ImageCandidate `json:"candidates,omitempty"`
	DismissedAt *time.Time       `json:"dismissed_at,omitempty"`
	ResolvedAt  *time.Time       `json:"resolved_at,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// RuleResult represents a persisted per-artist, per-rule evaluation outcome
// stored in the rule_results table (issue #699). A row is written for every
// (artist, rule) pair that has been evaluated at least once: passed=1 marks a
// rule the artist currently satisfies, passed=0 marks an active violation
// (violation_id links to the corresponding rule_violations row).
//
// FirstFailedAt is preserved across repeated failures (COALESCE on fail) so
// reports can answer "how long has this rule been failing?" without scanning
// rule_violations history. LastPassedAt is stamped each time the row
// transitions to passed=1 so callers can see when the artist most recently
// satisfied the rule.
type RuleResult struct {
	ArtistID         string     `json:"artist_id"`
	RuleID           string     `json:"rule_id"`
	Passed           bool       `json:"passed"`
	ViolationID      string     `json:"violation_id,omitempty"`
	EvaluatedAt      time.Time  `json:"evaluated_at"`
	ViolationMessage string     `json:"violation_message,omitempty"`
	FirstFailedAt    *time.Time `json:"first_failed_at,omitempty"`
	LastPassedAt     *time.Time `json:"last_passed_at,omitempty"`
}

// RuleResultCount aggregates per-artist pass/fail counts for the compliance
// report (issue #699 slice 1). Evaluated is the total number of rule_results
// rows for the artist; Passed is the subset with passed=1. The ratio
// Passed/Evaluated is the coarse "rules satisfied" signal surfaced on the
// /reports/compliance endpoint.
type RuleResultCount struct {
	Passed    int `json:"passed"`
	Evaluated int `json:"evaluated"`
}

// RuleResultWithArtist is a rule_results row joined with the artist's
// display name and (one) library name. When an artist belongs to
// multiple libraries the SQL collapses them with MIN(l.name) so the
// returned value is deterministic but represents a single library, not
// a flattened list. It is the row shape returned by
// GET /api/v1/rules/{id}/results so the front-end can render an
// artist + status grid without a second lookup per row.
type RuleResultWithArtist struct {
	RuleResult
	ArtistName  string `json:"artist_name"`
	IsExcluded  bool   `json:"is_excluded"`
	LibraryName string `json:"library_name,omitempty"`
}

// RuleResultWithRule is a rule_results row joined with the rule's display
// name, severity, and category. It is the row shape returned by GET
// /api/v1/artists/{id}/rule-results so the artist detail page can render
// the per-rule breakdown without re-fetching the rules collection.
type RuleResultWithRule struct {
	RuleResult
	RuleName     string `json:"rule_name"`
	RuleCategory string `json:"rule_category"`
	Severity     string `json:"severity"`
}

// RulePassRate is one row of the dashboard per-rule pass-rate widget.
// PassRate is Passed / Evaluated as a 0.0-1.0 fraction; Evaluated of 0
// is preserved so the widget can render "no data yet" rather than a
// divide-by-zero NaN. Severity is included so the widget can color-code
// failing rules using the same scheme as TopViolations.
type RulePassRate struct {
	RuleID    string  `json:"rule_id"`
	RuleName  string  `json:"rule_name"`
	Severity  string  `json:"severity"`
	Passed    int     `json:"passed"`
	Failed    int     `json:"failed"`
	Evaluated int     `json:"evaluated"`
	PassRate  float64 `json:"pass_rate"`
}

// TriFilter is a tri-state include/exclude filter over a single dimension's
// values (severity, category, rule, library, or fixable). It is the backend
// representation of the filter-flyout's three-state pills:
//
//   - A value in Include means "keep only rows that match one of these"
//     (SQL: <col> IN (...)).
//   - A value in Exclude means "drop rows that match one of these"
//     (SQL: <col> NOT IN (...)).
//   - A value in neither set is neutral: it does not constrain the query.
//
// Both sets may be populated at once (include some values, exclude others).
// A zero-value TriFilter (both sets empty) adds no SQL clause, so an unset
// dimension behaves exactly like the old empty scalar filter did.
//
// The URL contract the flyout JS emits (web/static/js/filter-flyout.js) repeats
// the same query key once per value, each value carrying a state prefix:
// "+value" = include, "-value" = exclude. A bare value with no prefix is
// treated as include for backward compatibility with older links and the
// legacy single-select form.
type TriFilter struct {
	Include []string
	Exclude []string
}

// IsEmpty reports whether the filter constrains nothing (no include and no
// exclude values). Callers use this to skip emitting any SQL clause.
func (f TriFilter) IsEmpty() bool {
	return len(f.Include) == 0 && len(f.Exclude) == 0
}

// Normalized returns a copy of f with the two invariants the SQL builder and
// the flyout JS already assume applied up front, so every downstream consumer
// (URL round-trip, chip rendering, count queries) sees the same canonical
// state:
//
//   - Dedupe: each side keeps only the first occurrence of a value.
//   - Exclude-wins: a value present in BOTH Include and Exclude is dropped
//     from Include and kept only in Exclude, matching the SQL behavior where a
//     NOT IN exclusion always removes a row even if an IN include would have
//     kept it.
//   - Whitelist: when Include is non-empty the dimension is in "whitelist"
//     mode (SQL keeps only included values and ignores explicit excludes), so
//     any leftover Exclude entries are stale and are dropped to neutral. This
//     mirrors the client-side whitelist rule in filter-flyout.js (issue #1217)
//     so a value is never simultaneously included-elsewhere and excluded here.
//
// The original f is not mutated; nil slices are preserved as nil.
func (f TriFilter) Normalized() TriFilter {
	if f.IsEmpty() {
		return TriFilter{}
	}

	// Build the exclude set first so exclude-wins can drop matching includes.
	excludeSet := make(map[string]struct{}, len(f.Exclude))
	var exclude []string
	for _, v := range f.Exclude {
		if _, dup := excludeSet[v]; dup {
			continue
		}
		excludeSet[v] = struct{}{}
		exclude = append(exclude, v)
	}

	includeSet := make(map[string]struct{}, len(f.Include))
	var include []string
	for _, v := range f.Include {
		if _, dup := includeSet[v]; dup {
			continue
		}
		// Exclude-wins: skip a value that is also excluded.
		if _, excluded := excludeSet[v]; excluded {
			continue
		}
		includeSet[v] = struct{}{}
		include = append(include, v)
	}

	// Whitelist: a non-empty Include means explicit excludes are ignored by the
	// SQL, so drop them to neutral to keep the state self-consistent.
	if len(include) > 0 {
		exclude = nil
	}

	return TriFilter{Include: include, Exclude: exclude}
}

// AppendURLValues appends f's tri-state values to q under key, using the
// flyout's URL contract: "+value" for include and "-value" for exclude. The
// flyout's client-side hydration (filter-flyout.js initFromURL) recognizes
// only these prefixed forms, so includes are always emitted with the "+"
// prefix (never bare). This is the single shared emitter used by both the
// push-URL handler and the dashboard template helpers so the wire form cannot
// drift between them.
func (f TriFilter) AppendURLValues(q url.Values, key string) {
	for _, v := range f.Include {
		q.Add(key, "+"+v)
	}
	for _, v := range f.Exclude {
		q.Add(key, "-"+v)
	}
}

// IncludeOnly builds a TriFilter that includes the single given value, or an
// empty (neutral) TriFilter when value is "". It is the back-compat bridge for
// callers that still pass a single bare value (the old scalar contract), such
// as the notifications endpoint and tests: a bare value means "include".
func IncludeOnly(value string) TriFilter {
	if value == "" {
		return TriFilter{}
	}
	return TriFilter{Include: []string{value}}
}

// ViolationListParams controls filtering, sorting, and grouping of violations.
//
// Severity, Category, RuleID, LibraryID, and Fixable are tri-state filters: each
// can independently include and/or exclude a set of values (see TriFilter). An
// empty TriFilter on any dimension adds no SQL clause.
type ViolationListParams struct {
	Status    string    // "active", "open", "resolved", "dismissed", "pending_choice", "" (all)
	Sort      string    // "artist_name", "severity", "rule_id", "created_at"
	Order     string    // "asc", "desc"
	Severity  TriFilter // filter values: "error", "warning", "info"
	Category  TriFilter // filter values: "nfo", "image", "metadata"
	RuleID    TriFilter // filter by specific rule IDs
	ArtistID  string    // filter by specific artist (scalar, programmatic; not user tri-state)
	GroupBy   string    // "artist", "rule", "severity", "category", ""
	Limit     int       // pagination limit; 0 = no limit (backward compatible)
	Offset    int       // pagination offset
	Search    string    // free-text search across artist_name, message, rule_id
	LibraryID TriFilter // filter by library IDs (via artist join)
	Fixable   TriFilter // filter values: "yes", "no"
}

// ViolationGroup holds a group of violations for grouped display.
type ViolationGroup struct {
	Key        string
	Label      string
	Count      int
	Violations []RuleViolation
}

// ViolationTrendPoint holds daily counts of violations created and resolved.
type ViolationTrendPoint struct {
	Date     string `json:"date"`     // YYYY-MM-DD
	Created  int    `json:"created"`  // violations with created_at on this date
	Resolved int    `json:"resolved"` // violations with resolved_at on this date
}

// MarshalConfig serializes a RuleConfig to a JSON string.
func MarshalConfig(cfg RuleConfig) string {
	data, _ := json.Marshal(cfg)
	return string(data)
}

// UnmarshalConfig deserializes a JSON string into a RuleConfig.
func UnmarshalConfig(data string) RuleConfig {
	var cfg RuleConfig
	if data == "" || data == "{}" {
		return cfg
	}
	_ = json.Unmarshal([]byte(data), &cfg)
	return cfg
}
