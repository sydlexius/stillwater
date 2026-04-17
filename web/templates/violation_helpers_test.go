package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

func TestSeverityClass(t *testing.T) {
	tests := []struct {
		in       string
		contains string
	}{
		{"error", "red"},
		{"warning", "yellow"},
		{"info", "blue"},
		{"unknown", "gray"},
		{"", "gray"},
	}
	for _, tt := range tests {
		got := severityClass(tt.in)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("severityClass(%q) = %q, want substring %q", tt.in, got, tt.contains)
		}
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero time", time.Time{}, ""},
		{"30 minutes", now.Add(-30 * time.Minute), "30m"},
		{"3 hours", now.Add(-3 * time.Hour), "3h"},
		{"2 days", now.Add(-2 * 24 * time.Hour), "2d"},
	}
	for _, tt := range tests {
		got := formatAge(tt.t)
		if got != tt.want {
			t.Errorf("formatAge(%s) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFixButtonLabel(t *testing.T) {
	tests := []struct {
		ruleID string
		want   string
	}{
		{rule.RuleNFOExists, "Generate NFO"},
		{rule.RuleNFOHasMBID, "Fetch MBID"},
		{rule.RuleBioExists, "Fetch biography"},
		{rule.RuleExtraneousImages, "Delete extraneous"},
		{rule.RuleLogoPadding, "Trim logo"},
		{rule.RuleDirectoryNameMismatch, "Rename directory"},
		{"thumb_min_res", "Fetch best image"},
		{"fanart_aspect", "Fetch best image"},
		{"logo_min_res", "Fetch best image"},
		{"banner_exists", "Fetch best image"},
		{"unrecognized_rule", "Fix"},
	}
	for _, tt := range tests {
		got := fixButtonLabel(tt.ruleID)
		if got != tt.want {
			t.Errorf("fixButtonLabel(%q) = %q, want %q", tt.ruleID, got, tt.want)
		}
	}
}
