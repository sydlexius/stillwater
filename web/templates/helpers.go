package templates

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// logoSrc returns the static path to a logo file for the given key.
// Most logos are SVG; audiodb and emby use PNG (128px variant).
func logoSrc(key string) string {
	switch key {
	case "audiodb", "emby":
		return "/static/img/logos/" + key + "-128.png"
	case "custom":
		return "/static/img/favicon.svg"
	default:
		return "/static/img/logos/" + key + ".svg"
	}
}

// logoSrcSet returns an srcset attribute value for PNG logos with multi-DPI
// variants. Returns an empty string for SVG logos (they scale natively).
func logoSrcSet(key string) string {
	switch key {
	case "audiodb", "emby":
		return "/static/img/logos/" + key + "-32.png 1x, " +
			"/static/img/logos/" + key + "-64.png 2x, " +
			"/static/img/logos/" + key + "-128.png 4x"
	default:
		return ""
	}
}

// providerDisplayName returns a human-readable name for a provider key.
func providerDisplayName(key string) string {
	switch key {
	case "musicbrainz":
		return "MusicBrainz"
	case "fanarttv":
		return "Fanart.tv"
	case "audiodb":
		return "TheAudioDB"
	case "discogs":
		return "Discogs"
	case "lastfm":
		return "Last.fm"
	case "wikidata":
		return "Wikidata"
	case "duckduckgo":
		return "DuckDuckGo"
	case "deezer":
		return "Deezer"
	default:
		return key
	}
}

// fieldLabel returns a human-readable label for a field name.
func fieldLabel(field string) string {
	switch field {
	case "biography":
		return "Biography"
	case "genres":
		return "Genres"
	case "styles":
		return "Styles"
	case "moods":
		return "Moods"
	case "formed":
		return "Formed"
	case "born":
		return "Born"
	case "disbanded":
		return "Disbanded"
	case "died":
		return "Died"
	case "years_active":
		return "Years Active"
	case "type":
		return "Type"
	case "gender":
		return "Gender"
	case "members":
		return "Members"
	default:
		return field
	}
}

// truncateText truncates a string to maxLen characters, appending "..." if truncated.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// escapeJSONValue escapes special characters in a string for safe embedding
// in a JSON value within an HTML attribute.
func escapeJSONValue(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	// json.Marshal wraps the string in quotes; strip them for embedding in hx-vals.
	return string(b[1 : len(b)-1])
}

// mergeSliceValues combines current and provider slices, deduplicating
// case-insensitively while preserving original casing. Returns a
// comma-separated string escaped for safe hx-vals JSON embedding.
func mergeSliceValues(current, provider []string) string {
	seen := make(map[string]bool, len(current)+len(provider))
	var merged []string
	for _, v := range current {
		trimmed := strings.TrimSpace(v)
		lower := strings.ToLower(trimmed)
		if lower != "" && !seen[lower] {
			seen[lower] = true
			merged = append(merged, trimmed)
		}
	}
	for _, v := range provider {
		trimmed := strings.TrimSpace(v)
		lower := strings.ToLower(trimmed)
		if lower != "" && !seen[lower] {
			seen[lower] = true
			merged = append(merged, trimmed)
		}
	}
	return escapeJSONValue(strings.Join(merged, ", "))
}

// membersJSON serializes a slice of MemberInfo to a JSON string for
// embedding as a data attribute on the "Use this" button in the provider modal.
func membersJSON(members []provider.MemberInfo) string {
	b, err := json.Marshal(members)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// boolAttr returns "true" or "false" for use in HTML attributes like aria-checked.
func boolAttr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// disambiguationHxVals builds the hx-vals JSON string for a disambiguation result card.
func disambiguationHxVals(r provider.ArtistSearchResult) string {
	var parts []string
	if r.MusicBrainzID != "" {
		parts = append(parts, `"mbid":"`+escapeJSONValue(r.MusicBrainzID)+`"`)
	}
	if r.Source == "discogs" && r.ProviderID != "" {
		parts = append(parts, `"discogs_id":"`+escapeJSONValue(r.ProviderID)+`"`)
	}
	parts = append(parts, `"source":"`+escapeJSONValue(r.Source)+`"`)
	return "{" + strings.Join(parts, ",") + "}"
}

// tierBadgeClasses returns Tailwind CSS classes for an access tier badge chip.
func tierBadgeClasses(tier provider.AccessTier) string {
	switch tier {
	case provider.TierFree:
		return "bg-green-100 dark:bg-green-900/30 text-green-700 dark:text-green-300 border border-green-200 dark:border-green-800"
	case provider.TierFreeKey:
		return "bg-blue-100 dark:bg-blue-900/30 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800"
	case provider.TierFreemium:
		return "bg-amber-100 dark:bg-amber-900/30 text-amber-700 dark:text-amber-300 border border-amber-200 dark:border-amber-800"
	case provider.TierPaid:
		return "bg-purple-100 dark:bg-purple-900/30 text-purple-700 dark:text-purple-300 border border-purple-200 dark:border-purple-800"
	default:
		return "bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400 border border-gray-200 dark:border-gray-700"
	}
}

// tierBadgeLabel returns the display label for an access tier.
func tierBadgeLabel(tier provider.AccessTier) string {
	switch tier {
	case provider.TierFree:
		return "Free"
	case provider.TierFreeKey:
		return "Free Key"
	case provider.TierFreemium:
		return "Freemium"
	case provider.TierPaid:
		return "Paid"
	default:
		return string(tier)
	}
}

// tierTooltip returns a tooltip description for an access tier.
func tierTooltip(tier provider.AccessTier) string {
	switch tier {
	case provider.TierFree:
		return "No account or API key required"
	case provider.TierFreeKey:
		return "Free account required to obtain an API key"
	case provider.TierFreemium:
		return "Free tier available with limits; paid tier unlocks more"
	case provider.TierPaid:
		return "Paid access required (no free tier)"
	default:
		return ""
	}
}

// getKeyLinkText returns the link label for obtaining a provider API key.
func getKeyLinkText(tier provider.AccessTier) string {
	if tier == provider.TierPaid {
		return "Purchase access"
	}
	return "Get free key"
}

// rateLimitText formats a RateLimitInfo into a short human-readable string.
func rateLimitText(rl *provider.RateLimitInfo) string {
	if rl == nil {
		return ""
	}
	var parts []string
	if rl.RequestsPerSecond > 0 {
		if rl.RequestsPerSecond == float64(int(rl.RequestsPerSecond)) {
			parts = append(parts, fmt.Sprintf("%d req/s", int(rl.RequestsPerSecond)))
		} else {
			parts = append(parts, fmt.Sprintf("%.1f req/s", rl.RequestsPerSecond))
		}
	}
	if rl.RequestsPerDay > 0 {
		parts = append(parts, fmt.Sprintf("%d/day", rl.RequestsPerDay))
	}
	if rl.RequestsPerMonth > 0 {
		parts = append(parts, fmt.Sprintf("%d/month", rl.RequestsPerMonth))
	}
	return strings.Join(parts, " / ")
}
