package templates

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// severityClass returns Tailwind color classes for a severity badge.
func severityClass(severity string) string {
	switch severity {
	case "error":
		return "bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300"
	case "warning":
		return "bg-yellow-100 dark:bg-yellow-900/50 text-yellow-800 dark:text-yellow-300"
	case "info":
		return "bg-blue-100 dark:bg-blue-900/50 text-blue-800 dark:text-blue-300"
	default:
		return "bg-gray-100 dark:bg-gray-700 text-gray-800 dark:text-gray-300"
	}
}

// formatAge renders a compact relative age suffix (e.g. "3m", "5h", "2d").
func formatAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	ago := time.Since(t)
	if ago.Hours() < 1 {
		return fmt.Sprintf("%dm", int(ago.Minutes()))
	}
	if ago.Hours() < 24 {
		return fmt.Sprintf("%dh", int(ago.Hours()))
	}
	return fmt.Sprintf("%dd", int(ago.Hours()/24))
}

// fixButtonLabel maps a rule ID to a short action label for a Fix button.
// The ctx parameter is used to look up the translated label via the i18n system.
func fixButtonLabel(ctx context.Context, ruleID string) string {
	switch ruleID {
	case rule.RuleNFOExists:
		return t(ctx, "rule.fix.generate_nfo")
	case rule.RuleNFOHasMBID:
		return t(ctx, "rule.fix.fetch_mbid")
	case rule.RuleBioExists:
		return t(ctx, "rule.fix.fetch_biography")
	case rule.RuleExtraneousImages:
		return t(ctx, "rule.fix.delete_extraneous")
	case rule.RuleLogoPadding:
		return t(ctx, "rule.fix.trim_logo")
	case rule.RuleDirectoryNameMismatch:
		return t(ctx, "rule.fix.rename_directory")
	case rule.RuleDiscographyPopulated:
		return t(ctx, "rule.fix.populate_discography")
	default:
		if strings.Contains(ruleID, "thumb") || strings.Contains(ruleID, "fanart") ||
			strings.Contains(ruleID, "logo") || strings.Contains(ruleID, "banner") {
			return t(ctx, "rule.fix.fetch_best_image")
		}
		return t(ctx, "rule.fix.default")
	}
}
