package rule

import (
	"encoding/json"
	"time"
)

// Automation modes for rules
const (
	AutomationModeAuto     = "auto"
	AutomationModeNotify   = "notify"
	AutomationModeDisabled = "disabled"
)

// Rule represents a validation rule stored in the database.
type Rule struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Category       string     `json:"category"` // "nfo", "image", "metadata"
	Enabled        bool       `json:"enabled"`
	AutomationMode string     `json:"automation_mode"` // "auto", "notify", "disabled"
	Config         RuleConfig `json:"config"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
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
type EvaluationResult struct {
	ArtistID    string      `json:"artist_id"`
	ArtistName  string      `json:"artist_name"`
	Violations  []Violation `json:"violations"`
	RulesPassed int         `json:"rules_passed"`
	RulesTotal  int         `json:"rules_total"`
	HealthScore float64     `json:"health_score"`
}

// HealthSnapshot represents a recorded library health score.
type HealthSnapshot struct {
	ID               string    `json:"id"`
	TotalArtists     int       `json:"total_artists"`
	CompliantArtists int       `json:"compliant_artists"`
	Score            float64   `json:"score"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// Violation status constants
const (
	ViolationStatusOpen          = "open"
	ViolationStatusDismissed     = "dismissed"
	ViolationStatusResolved      = "resolved"
	ViolationStatusPendingChoice = "pending_choice" // multiple image candidates waiting for user selection
)

// RuleViolation represents a persisted rule violation for the notifications view.
type RuleViolation struct {
	ID          string           `json:"id"`
	RuleID      string           `json:"rule_id"`
	ArtistID    string           `json:"artist_id"`
	ArtistName  string           `json:"artist_name"`
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
