package rule

import (
	"encoding/json"
	"errors"
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

// ViolationListParams controls filtering, sorting, and grouping of violations.
type ViolationListParams struct {
	Status    string // "active", "open", "resolved", "dismissed", "pending_choice", "" (all)
	Sort      string // "artist_name", "severity", "rule_id", "created_at"
	Order     string // "asc", "desc"
	Severity  string // filter: "error", "warning", "info"
	Category  string // filter: "nfo", "image", "metadata"
	RuleID    string // filter by specific rule
	ArtistID  string // filter by specific artist
	GroupBy   string // "artist", "rule", "severity", "category", ""
	Limit     int    // pagination limit; 0 = no limit (backward compatible)
	Offset    int    // pagination offset
	Search    string // free-text search across artist_name, message, rule_id
	LibraryID string // filter by library (via artist join)
	Fixable   string // filter by fixable: "yes", "no", "" (all)
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
