package templates

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// staticBasePath is the URL prefix for all static assets (e.g. "/app").
// Set once via SetBasePath at server startup so that logoSrc and logoSrcSet
// produce correct URLs in sub-path deployments.
// Protected by staticBasePathMu because net/http serves requests concurrently.
var (
	staticBasePathMu sync.RWMutex
	staticBasePath   string
)

// SetBasePath configures the URL prefix used by logoSrc / logoSrcSet
// and basePath(). Call once during server initialization.
// This value must match AssetPaths.BasePath passed to full-page templates.
func SetBasePath(bp string) {
	staticBasePathMu.Lock()
	staticBasePath = strings.TrimRight(bp, "/")
	staticBasePathMu.Unlock()
}

// basePath returns the configured URL prefix (e.g. "/app") for use in
// templates that build href/src attributes outside of HTMX (which has
// its own configRequest hook).
func basePath() string {
	staticBasePathMu.RLock()
	bp := staticBasePath
	staticBasePathMu.RUnlock()
	return bp
}

// logoSrc returns the static path to a logo file for the given key.
// Most logos are SVG; audiodb and emby use PNG (128px variant).
func logoSrc(key string) string {
	bp := basePath()
	switch key {
	case "audiodb", "emby":
		return bp + "/static/img/logos/" + key + "-128.png"
	case "custom":
		return bp + "/static/img/favicon.svg"
	default:
		return bp + "/static/img/logos/" + key + ".svg"
	}
}

// logoSrcSet returns an srcset attribute value for PNG logos with multi-DPI
// variants. Returns an empty string for SVG logos (they scale natively).
func logoSrcSet(key string) string {
	bp := basePath()
	switch key {
	case "audiodb", "emby":
		return bp + "/static/img/logos/" + key + "-32.png 1x, " +
			bp + "/static/img/logos/" + key + "-64.png 2x, " +
			bp + "/static/img/logos/" + key + "-128.png 4x"
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
	case "genius":
		return "Genius"
	case "wikipedia":
		return "Wikipedia"
	default:
		return key
	}
}

// fieldLabel returns a human-readable label for a field name via i18n lookup.
// Keys use the pattern "field.<name>" (e.g. "field.biography" -> "Biography").
// Unknown fields fall back to converting snake_case to Title Case so raw
// database column names are never shown to the user.
func fieldLabel(ctx context.Context, field string) string {
	result := t(ctx, "field."+field)
	// If the key was not found, t() returns the key itself. In that case
	// humanize the raw field name (e.g. "spotify_artist_id" -> "Spotify Artist Id").
	if result == "field."+field {
		parts := strings.Split(field, "_")
		for i, p := range parts {
			if len(p) > 0 {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
		}
		return strings.Join(parts, " ")
	}
	return result
}

// providerIDURL returns the canonical external URL for a provider ID field.
// Returns an empty string when no URL pattern exists for the given field.
func providerIDURL(field, value string) string {
	if value == "" {
		return ""
	}
	escaped := url.PathEscape(value)
	switch field {
	case "musicbrainz_id":
		return "https://musicbrainz.org/artist/" + escaped
	case "audiodb_id":
		return "https://www.theaudiodb.com/artist/" + escaped
	case "discogs_id":
		return "https://www.discogs.com/artist/" + escaped
	case "wikidata_id":
		return "https://www.wikidata.org/wiki/" + escaped
	case "deezer_id":
		return "https://www.deezer.com/artist/" + escaped
	default:
		return ""
	}
}

// truncateText truncates a string to maxLen characters, appending "..." if truncated.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// hxValsJSON builds a JSON object string from key-value pairs for use in
// hx-vals attributes. Using json.Marshal for the entire object avoids
// unsafe quoting from manual string interpolation.
func hxValsJSON(pairs map[string]string) string {
	b, err := json.Marshal(pairs)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// hxValsJSONAny is like hxValsJSON but accepts mixed-type values (strings,
// ints, bools) for use in hx-vals attributes that need non-string JSON values.
func hxValsJSONAny(pairs map[string]any) string {
	b, err := json.Marshal(pairs)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// mergeSliceValues combines current and provider slices, deduplicating
// case-insensitively while preserving original casing. Returns a raw
// comma-separated string suitable for passing to hxValsJSON.
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
	return strings.Join(merged, ", ")
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
	m := map[string]string{"source": r.Source}
	if r.MusicBrainzID != "" {
		m["mbid"] = r.MusicBrainzID
	}
	if r.Source == "discogs" && r.ProviderID != "" {
		m["discogs_id"] = r.ProviderID
	}
	return hxValsJSON(m)
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

// tierBadgeLabel returns the display label for an access tier via i18n.
func tierBadgeLabel(ctx context.Context, tier provider.AccessTier) string {
	key := "tier." + string(tier)
	result := t(ctx, key)
	if result == key {
		return string(tier)
	}
	return result
}

// tierTooltip returns a tooltip description for an access tier via i18n.
func tierTooltip(ctx context.Context, tier provider.AccessTier) string {
	key := "tier_tooltip." + string(tier)
	result := t(ctx, key)
	if result == key {
		return ""
	}
	return result
}

// getKeyLinkText returns the link label for obtaining a provider API key via i18n.
func getKeyLinkText(ctx context.Context, tier provider.AccessTier) string {
	key := "tier_link." + string(tier)
	result := t(ctx, key)
	if result == key {
		return ""
	}
	return result
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

const (
	officialMirrorURL = "https://musicbrainz.org/ws/2"
	betaMirrorURL     = "https://beta.musicbrainz.org/ws/2"
)

// mirrorServerType returns "official", "beta", or "custom" based on the
// current mirror configuration.
func mirrorServerType(m *provider.MirrorConfig) string {
	if m == nil || m.BaseURL == officialMirrorURL {
		return "official"
	}
	if m.BaseURL == betaMirrorURL {
		return "beta"
	}
	return "custom"
}

// mirrorStatusLabel returns a short label for the active server config,
// shown as a badge on the provider card header.
func mirrorStatusLabel(ctx context.Context, m *provider.MirrorConfig) string {
	serverType := mirrorServerType(m)
	if serverType == "official" {
		return ""
	}
	key := "mirror." + serverType
	result := t(ctx, key)
	if result == key {
		return serverType
	}
	return result
}

// albumMatchClasses returns Tailwind CSS classes for the album match badge
// based on the match percentage: green (70%+), amber (30-69%), red (<30%).
func albumMatchClasses(percent int) string {
	switch {
	case percent >= 70:
		return "bg-green-100 dark:bg-green-900/30 text-green-700 dark:text-green-300"
	case percent >= 30:
		return "bg-amber-100 dark:bg-amber-900/30 text-amber-700 dark:text-amber-300"
	default:
		return "bg-red-100 dark:bg-red-900/30 text-red-700 dark:text-red-300"
	}
}

// hasMatchedAlbums reports whether the comparison has any matched albums.
func hasMatchedAlbums(comp *artist.AlbumComparison) bool {
	return comp != nil && len(comp.Matches) > 0
}

// joinMatchedNames returns a comma-separated string of matched album names
// from the comparison, limited to the first 5 entries.
func joinMatchedNames(comp *artist.AlbumComparison) string {
	if comp == nil || len(comp.Matches) == 0 {
		return ""
	}
	limit := 5
	if len(comp.Matches) < limit {
		limit = len(comp.Matches)
	}
	names := make([]string, limit)
	for i := 0; i < limit; i++ {
		names[i] = comp.Matches[i].RemoteName
	}
	result := strings.Join(names, ", ")
	if len(comp.Matches) > 5 {
		result += fmt.Sprintf(" (+%d more)", len(comp.Matches)-5)
	}
	return result
}

// sourceDisplayName returns a human-readable name for a library source key.
func sourceDisplayName(source string) string {
	switch source {
	case "emby":
		return "Emby"
	case "jellyfin":
		return "Jellyfin"
	case "lidarr":
		return "Lidarr"
	default:
		return ""
	}
}

// localizedImageType returns a translated display name for an internal image
// type ID ("thumb", "fanart", "logo", "banner"). Falls back to the raw ID when
// no translation key exists.
func localizedImageType(ctx context.Context, typeID string) string {
	key := "image.type." + typeID
	result := t(ctx, key)
	if result == key {
		return typeID
	}
	return result
}

// translationBefore returns the portion of s before the first occurrence of
// placeholder. Returns the entire string when placeholder is not found.
func translationBefore(s, placeholder string) string {
	idx := strings.Index(s, placeholder)
	if idx < 0 {
		return s
	}
	return s[:idx]
}

// translationAfter returns the portion of s after the first occurrence of
// placeholder. Returns an empty string when placeholder is not found.
func translationAfter(s, placeholder string) string {
	idx := strings.Index(s, placeholder)
	if idx < 0 {
		return ""
	}
	return s[idx+len(placeholder):]
}
