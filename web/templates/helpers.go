package templates

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/a-h/templ"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// artistDirBasename returns the leaf component of the artist's filesystem
// path. Used by the rename-directory action in the artist detail page to
// pre-fill the prompt with the current directory name. Returns the empty
// string when the artist has no path (e.g. virtual library entries).
func artistDirBasename(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

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

// BasePath is the exported accessor for the configured URL prefix, so the
// separate next/ template package can build the same base-path-aware hrefs as
// the stable templates without duplicating the SetBasePath global.
func BasePath() string { return basePath() }

// artistDetailBaseKeyType is the unexported context key under which a handler
// stashes the channel-aware artist-detail URL base (e.g. "/next/artists").
type artistDetailBaseKeyType struct{}

// WithArtistDetailBase returns a context carrying the channel-aware artist-detail
// URL base. The next/ channel sets it to "/next/artists" before rendering shared
// fragments (the dashboard action queue) so the shared DashboardActionCard links
// stay inside the next/ channel instead of leaking to the stable /artists/<id>
// screen (M55 #1852). The stable channel never sets it, so artistDetailHref
// falls back to "/artists". Mirrors the WithFieldFindings ctx-injection pattern.
func WithArtistDetailBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, artistDetailBaseKeyType{}, base)
}

// artistDetailHref builds the channel-aware artist-detail href:
// basePath + the ctx-injected base (default "/artists") + "/" + artistID.
// Shared templates rendered in both channels (e.g. DashboardActionCard) call
// this so the next/ channel keeps artist links on /next/artists/<id>.
func artistDetailHref(ctx context.Context, basePath, artistID string) templ.SafeURL {
	base := "/artists"
	if v, ok := ctx.Value(artistDetailBaseKeyType{}).(string); ok && v != "" {
		base = v
	}
	return templ.SafeURL(basePath + base + "/" + artistID)
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
			if p != "" {
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
func mergeSliceValues(current, providerVals []string) string {
	seen := make(map[string]bool, len(current)+len(providerVals))
	var merged []string
	for _, v := range current {
		trimmed := strings.TrimSpace(v)
		lower := strings.ToLower(trimmed)
		if lower != "" && !seen[lower] {
			seen[lower] = true
			merged = append(merged, trimmed)
		}
	}
	for _, v := range providerVals {
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

// recommendedReasonLabel returns the localized tooltip text for the
// "Recommended" badge on the duplicates page. The reason values come from
// artist.ChooseSurvivor's second return.
func recommendedReasonLabel(ctx context.Context, reason string) string {
	switch reason {
	case "canonical_basename":
		return t(ctx, "artist_duplicates.recommended_reason.canonical_basename")
	case "most_content":
		return t(ctx, "artist_duplicates.recommended_reason.most_content")
	case "fallback":
		return t(ctx, "artist_duplicates.recommended_reason.fallback")
	}
	return ""
}

// mergeI18nJSON builds the JSON blob the merge modal's JS reads at runtime
// for all user-facing strings. Embedded once in artist_duplicates.templ via
// the hidden #merge-i18n div, mirroring the layoutI18nJSON pattern.
func mergeI18nJSON(ctx context.Context) string {
	m := map[string]string{
		"preview_loading":           t(ctx, "artist_duplicates.merge_modal.preview_loading"),
		"preview_empty":             t(ctx, "artist_duplicates.merge_modal.preview_empty"),
		"preview_network_error":     t(ctx, "artist_duplicates.merge_modal.preview_network_error"),
		"moves_heading":             t(ctx, "artist_duplicates.merge_modal.moves_heading"),
		"warnings_heading":          t(ctx, "artist_duplicates.merge_modal.warnings_heading"),
		"warning_override":          t(ctx, "artist_duplicates.merge_modal.warning_override"),
		"platform_rescan_note":      t(ctx, "artist_duplicates.merge_modal.platform_rescan_note"),
		"conflicts_heading":         t(ctx, "artist_duplicates.merge_modal.conflicts_heading"),
		"conflicts_help":            t(ctx, "artist_duplicates.merge_modal.conflicts_help"),
		"recommended_badge":         t(ctx, "artist_duplicates.recommended_badge"),
		"reason_canonical_basename": t(ctx, "artist_duplicates.recommended_reason.canonical_basename"),
		"reason_most_content":       t(ctx, "artist_duplicates.recommended_reason.most_content"),
		"reason_fallback":           t(ctx, "artist_duplicates.recommended_reason.fallback"),
		"error_merge_in_progress":   t(ctx, "artist_duplicates.merge_modal.error_merge_in_progress"),
		"error_locked":              t(ctx, "artist_duplicates.merge_modal.error_locked"),
		"error_stale_group":         t(ctx, "artist_duplicates.merge_modal.error_stale_group"),
		"error_survivor_missing":    t(ctx, "artist_duplicates.merge_modal.error_survivor_missing"),
		"error_unknown":             t(ctx, "artist_duplicates.merge_modal.error_unknown"),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// duplicateGroupMembersJSON serializes a near-duplicate group's members to a
// JSON array for embedding as a data attribute on the group card. The merge
// modal's JS reads the blob when the user clicks "Merge..." so it can render
// survivor radios without an extra round-trip to the server.
//
// Field names are lowercased and match what the JS expects; only the subset
// the modal needs is serialized (locked state is not surfaced here -- the
// 423 error response handles that case at submit time).
func duplicateGroupMembersJSON(members []ArtistDuplicateMember) string {
	type wire struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Path              string `json:"path"`
		MBID              string `json:"mbid"`
		Recommended       bool   `json:"recommended"`
		RecommendedReason string `json:"recommended_reason"`
	}
	out := make([]wire, 0, len(members))
	for _, m := range members {
		out = append(out, wire(m))
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// layoutI18nJSON builds a JSON object of translated strings used by the
// global toast, modal, and undo infrastructure in layout.templ. The JS
// reads the blob at startup so no English literals remain in the script block.
// Interpolated strings use Go %s/%d format verbs; JS substitutes values via
// str.replace('%s', value) or str.replace('%d', value).
func layoutI18nJSON(ctx context.Context) string {
	m := map[string]string{
		"grouped_aria":    t(ctx, "notifications.grouped_aria"),
		"repeated_aria":   t(ctx, "notifications.repeated_aria"),
		"dismiss_aria":    t(ctx, "notifications.dismiss_aria"),
		"undo":            t(ctx, "actions.undo"),
		"undo_fix_aria":   t(ctx, "notifications.undo_fix_aria"),
		"close_aria":      t(ctx, "notifications.close_aria"),
		"undoing":         t(ctx, "actions.undoing"),
		"http_status":     t(ctx, "errors.http_status"),
		"fix_reverted":    t(ctx, "notifications.fix_reverted"),
		"undo_failed":     t(ctx, "notifications.undo_failed"),
		"request_failed":  t(ctx, "errors.request_failed"),
		"request_timeout": t(ctx, "errors.request_timeout"),
		"unknown":         t(ctx, "errors.unknown"),
		"confirm":         t(ctx, "common.confirm"),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// tourStepsJSON builds a JSON object of translated tour step titles and
// descriptions. Called by tourI18nJSON in layout.templ so tour.js can
// display localized popover text.
func tourStepsJSON(ctx context.Context) string {
	m := map[string]string{
		"nav_title":         t(ctx, "guide.tour_step_nav_title"),
		"nav_desc":          t(ctx, "guide.tour_step_nav_desc"),
		"scan_title":        t(ctx, "guide.tour_step_scan_title"),
		"scan_desc":         t(ctx, "guide.tour_step_scan_desc"),
		"search_title":      t(ctx, "guide.tour_step_search_title"),
		"search_desc":       t(ctx, "guide.tour_step_search_desc"),
		"filter_title":      t(ctx, "guide.tour_step_filter_title"),
		"filter_desc":       t(ctx, "guide.tour_step_filter_desc"),
		"sort_title":        t(ctx, "guide.tour_step_sort_title"),
		"sort_desc":         t(ctx, "guide.tour_step_sort_desc"),
		"view_title":        t(ctx, "guide.tour_step_view_title"),
		"view_desc":         t(ctx, "guide.tour_step_view_desc"),
		"artist_list_title": t(ctx, "guide.tour_step_artist_list_title"),
		"artist_list_desc":  t(ctx, "guide.tour_step_artist_list_desc"),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// cleanBannerTitle returns the title for the green (clean) banner.
// When Stillwater is actively managing one or more connections it returns
// a "Managed by Stillwater." heading so the user can tell the green state
// means active protection, not merely the absence of a problem. The
// generic "All clear." title is used when no connections are managed
// (write-back savers were never on, or no connections are configured).
func cleanBannerTitle(ctx context.Context, v ConflictBannerView) string {
	if len(v.ManagedConnections) > 0 {
		return t(ctx, "banner.clean.title_managed")
	}
	return t(ctx, "banner.clean.title")
}

// cleanBannerBody returns the descriptive body text for the green banner.
// When managed connections are present it names them (using the
// connection Name, falling back to Type when Name is blank) so the user
// can see exactly which platforms Stillwater is protecting. For the
// no-managed-connections case it returns the existing "no conflict
// gating active" copy.
func cleanBannerBody(ctx context.Context, v ConflictBannerView) string {
	if len(v.ManagedConnections) == 0 {
		return t(ctx, "banner.clean.body")
	}
	names := make([]string, 0, len(v.ManagedConnections))
	for _, c := range v.ManagedConnections {
		label := c.Name
		if label == "" {
			label = c.Type
		}
		if label == "" {
			continue
		}
		names = append(names, label)
	}
	if len(names) == 0 {
		return t(ctx, "banner.clean.body")
	}
	return tf(ctx, "banner.clean.body_managed", strings.Join(names, ", "))
}

// warnTitle returns the state-specific headline used by the amber banner
// variants. Copy mirrors the approved mockup so the three axes (image,
// nfo, both) each read as a concrete action rather than a generic warning.
func warnTitle(ctx context.Context, axis string) string {
	switch axis {
	case "image":
		return t(ctx, "banner.warn.title.image")
	case "nfo":
		return t(ctx, "banner.warn.title.nfo")
	case "both":
		return t(ctx, "banner.warn.title.both")
	}
	return t(ctx, "banner.warn.title.default")
}

// warnSubtitle builds the body-text clause that explains which server is
// causing the pause and, where available, which library.
func warnSubtitle(ctx context.Context, axis string, v ConflictBannerView) string {
	if len(v.Connections) == 0 {
		switch axis {
		case "image":
			return t(ctx, "banner.warn.subtitle.image_no_conn")
		case "nfo":
			return t(ctx, "banner.warn.subtitle.nfo_no_conn")
		}
		return t(ctx, "banner.warn.subtitle.default_no_conn")
	}
	c := v.Connections[0]
	name := c.Name
	if name == "" {
		name = c.Type
	}
	if strings.TrimSpace(name) == "" {
		// Last-resort label when both Name and Type are blank so the
		// rendered subtitle never starts with " is saving...".
		name = t(ctx, "banner.warn.subtitle.fallback_name")
	}
	switch axis {
	case "image":
		if c.LibraryName != "" {
			return tf(ctx, "banner.warn.subtitle.image_with_library", name, c.LibraryName)
		}
		return tf(ctx, "banner.warn.subtitle.image", name)
	case "nfo":
		if c.LibraryName != "" {
			return tf(ctx, "banner.warn.subtitle.nfo_with_library", name, c.LibraryName)
		}
		return tf(ctx, "banner.warn.subtitle.nfo", name)
	case "both":
		return tf(ctx, "banner.warn.subtitle.both", name)
	}
	return tf(ctx, "banner.warn.subtitle.default", name)
}

// warnAffected returns the "Affected: ..." sub-line matching the mockup's
// per-axis guidance. The image-axis copy intentionally matches every gated
// endpoint (upload, fetch, crop, fanart assign + batch fetch, rules,
// maintenance) so the banner does not promise actions the API will reject
// with 409. The earlier copy claimed "edits to existing artwork are still
// allowed", which contradicted the gate -- gateImageWrite blocks crop and
// fanart assign too.
func warnAffected(ctx context.Context, axis string) string {
	switch axis {
	case "image":
		return t(ctx, "banner.warn.affected.image")
	case "nfo":
		return t(ctx, "banner.warn.affected.nfo")
	case "both":
		return t(ctx, "banner.warn.affected.both")
	}
	return ""
}

// saverAxisLabel returns the localized pill text for an offender's enabled
// savers ("image + NFO saver", "image saver", "NFO saver"). Returns the
// empty string when neither axis is on, which lets templ-level switches
// skip emitting an empty span.
func saverAxisLabel(ctx context.Context, image, nfo bool) string {
	switch {
	case image && nfo:
		return t(ctx, "banner.saver.image_and_nfo")
	case image:
		return t(ctx, "banner.saver.image")
	case nfo:
		return t(ctx, "banner.saver.nfo")
	}
	return ""
}

// conflictImageGated reports whether the banner state should cause
// image-writing UI to grey out. Composite "both" and "round_trip" always
// gate; "image_only" gates; everything else allows writes.
func conflictImageGated(v ConflictBannerView) bool {
	return v.State == "image_only" || v.State == "both" || v.State == "round_trip"
}

// conflictNFOGated mirrors conflictImageGated for the NFO axis.
func conflictNFOGated(v ConflictBannerView) bool {
	return v.State == "nfo_only" || v.State == "both" || v.State == "round_trip"
}

// conflictConnectionHref builds the in-app deep link the banner uses to
// jump from an offender row to its settings card. The tab query param
// activates the Connections tab before the fragment scrolls the browser
// to the specific card, so the user lands directly on the "Detected on
// this server" panel regardless of which tab was previously active.
func conflictConnectionHref(connID string) templ.SafeURL {
	return templ.SafeURL(basePath() + "/settings?tab=connections#connection-" + connID)
}

// conflictSettingsConnectionsHref targets the Connections tab without a
// specific card, used by the "Review each connection" CTA in banner
// state C.
func conflictSettingsConnectionsHref() templ.SafeURL {
	return templ.SafeURL(basePath() + "/settings?tab=connections")
}

// manageServerFilesPayload returns the JSON body for the hx-vals attribute
// used by the "Let Stillwater manage" toggle. enable=true means the user is
// flipping the toggle on (so the POST enables remediation); false reverses.
func manageServerFilesPayload(enable bool) string {
	return hxValsJSONAny(map[string]any{"enabled": enable})
}

// humanBytes formats a file size in IEC binary units (KiB, MiB, GiB).
// Used by the foreign-files page so per-row sizes are scannable.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB"}
	div, exp := int64(unit), 0
	// Cap exp at the last suffix index so values larger than 1 TiB still
	// scale against TiB (e.g. 1 PiB renders as "1024.0 TiB") rather than
	// inheriting the next-tier divisor and underreporting.
	for x := n / unit; x >= unit && exp < len(suffixes)-1; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}

// boolAttr returns "true" or "false" for use in HTML attributes like aria-checked.
func boolAttr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// obConflictBlockValue returns "1" when the OOBE conflict step should
// disable the Continue button (round-trip) and "" otherwise. The hidden
// input value is read by updateConflictGate() in onboarding.templ.
func obConflictBlockValue(blocking bool) string {
	if blocking {
		return "1"
	}
	return ""
}

// obConflictWarnTitle returns the OOBE-step heading for amber states.
// Mirrors warnTitle but with first-time-setup phrasing per the issue spec.
func obConflictWarnTitle(ctx context.Context, axis string) string {
	switch axis {
	case "image":
		return t(ctx, "banner.onboarding.warn_title_image")
	case "nfo":
		return t(ctx, "banner.onboarding.warn_title_nfo")
	default:
		return t(ctx, "banner.onboarding.warn_title_both")
	}
}

// obConflictWarnBody returns the body copy for amber pre-flight states.
// References fanart-style duplicate file names so the user understands the
// failure mode without having to read the post-OOBE banner first.
func obConflictWarnBody(ctx context.Context, axis string) string {
	switch axis {
	case "image":
		return t(ctx, "banner.onboarding.warn_body_image")
	case "nfo":
		return t(ctx, "banner.onboarding.warn_body_nfo")
	default:
		return t(ctx, "banner.onboarding.warn_body_both")
	}
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
// from the comparison, limited to the first 5 entries. The ctx parameter
// is used to look up the translated " (+%d more)" suffix via the i18n system.
func joinMatchedNames(ctx context.Context, comp *artist.AlbumComparison) string {
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
		result += tf(ctx, "common.plus_more", len(comp.Matches)-5)
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

// roundTripOverlapHTML renders the round-trip overlap sentence as a single
// HTML string where the connection names and path carry inline color styling.
// Using a single translated key (banner.round_trip.overlap) gives translators
// control over word order -- the three %s placeholders receive the pre-styled
// HTML spans for nameA, nameB, and path. Each value is HTML-escaped before
// embedding in the span so user-supplied strings cannot inject markup.
// The returned string is intended for use with @templ.Raw() inside a <li>.
//
// Option A choice (M50 localization milestone): a single key with three %s
// placeholders lets translators reorder the names and path for languages
// where the English "X and Y share Z" structure is grammatically incorrect.
func roundTripOverlapHTML(ctx context.Context, aName, bName, path, nameColorClass, pathColorClass string) string {
	// Escape each user-supplied value before embedding in HTML attributes/content.
	safeA := html.EscapeString(aName)
	safeB := html.EscapeString(bName)
	safePath := html.EscapeString(path)

	spanA := fmt.Sprintf(`<span class="font-medium %s">%s</span>`, nameColorClass, safeA)
	spanB := fmt.Sprintf(`<span class="font-medium %s">%s</span>`, nameColorClass, safeB)
	codePath := fmt.Sprintf(`<code class="%s px-1 rounded">%s</code>`, pathColorClass, safePath)

	tpl := t(ctx, "banner.round_trip.overlap")
	if tpl == "banner.round_trip.overlap" {
		// Missing-key fallback: t() returns the raw key when the active
		// locale lacks the entry. Without this guard fmt.Sprintf would
		// emit %!(EXTRA ...) noise. Mirrors the guard in tf().
		tpl = "%s and %s share %s"
	}
	return fmt.Sprintf(tpl, spanA, spanB, codePath)
}

// ArtistTypeLabel normalizes + localizes a raw artist Type (the DB/MusicBrainz
// value, e.g. "group", "solo act") into the display label used across the app
// ("Group", "Person", ...). Shared so the artist-detail Type row, the hero type
// tag, and the artists list all render the same label (M55 #1336; the broader
// case/normalization sweep is #1843). Unknown/empty falls back to "Other",
// matching the artists list.
func ArtistTypeLabel(ctx context.Context, rawType string) string {
	switch strings.ToLower(strings.TrimSpace(rawType)) {
	case "person", "solo", "solo act":
		return t(ctx, "artists.filter.person")
	case "group":
		return t(ctx, "artists.filter.group")
	case "orchestra", "choir":
		return t(ctx, "artists.filter.orchestra")
	default:
		return t(ctx, "artists.filter.other")
	}
}

// artistTypeRowValue is the display value for the metadata Type row: the
// normalized label for a set type, but "" for an unset type so the row renders
// the standard "Not set" placeholder rather than the "Other" bucket label.
func artistTypeRowValue(ctx context.Context, rawType string) string {
	if strings.TrimSpace(rawType) == "" {
		return ""
	}
	return ArtistTypeLabel(ctx, rawType)
}

// --- Field finding chips (M55 #1336, field-tag-on-rule) ---

// FieldFinding is one active rule violation surfaced as an inline chip on the
// metadata field it touches. It is a presentation-only projection of a
// rule.Violation (kept here so the templates package does not import the rule
// engine): Severity drives the chip color AND its label (the severity word,
// matching the Open Findings list's severity badge), and Message is the hover
// tooltip carrying the specific problem.
type FieldFinding struct {
	Severity string
	Message  string
}

// fieldFindingsKeyType is the unexported context key under which the
// artist-detail handler stashes the field -> findings map.
type fieldFindingsKeyType struct{}

// WithFieldFindings returns a context carrying the field -> findings map the
// next/ artist-detail page renders as inline chips. The stable channel never
// sets it, so its FieldDisplay rows render no chips (fieldFindingsFor returns
// nil). The map is keyed by the same field names FieldDisplay switches on.
func WithFieldFindings(ctx context.Context, m map[string][]FieldFinding) context.Context {
	return context.WithValue(ctx, fieldFindingsKeyType{}, m)
}

// fieldFindingsFor returns the active findings tagged to the given field, or nil
// when none are present or no map was injected into the context.
func fieldFindingsFor(ctx context.Context, field string) []FieldFinding {
	m, ok := ctx.Value(fieldFindingsKeyType{}).(map[string][]FieldFinding)
	if !ok {
		return nil
	}
	return m[field]
}

// normalizeGenderDisplay returns the gender value in canonical Title case for
// display (UAT 4A item 7). Providers often store "female"/"male" lowercase,
// which read inconsistently next to the other Title/Sentence-cased field
// values. It upper-cases the first rune of each hyphen- or space-separated word
// ("non-binary" -> "Non-Binary" stays readable; "female" -> "Female") and
// leaves an empty value untouched so the "Not set" path still fires.
func normalizeGenderDisplay(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	capWord := func(w string) string {
		if w == "" {
			return ""
		}
		r := []rune(w)
		return strings.ToUpper(string(r[0])) + strings.ToLower(string(r[1:]))
	}
	// Capitalize across both spaces and hyphens (e.g. "non-binary").
	out := make([]byte, 0, len(v))
	word := strings.Builder{}
	flush := func() {
		if word.Len() > 0 {
			out = append(out, capWord(word.String())...)
			word.Reset()
		}
	}
	for _, r := range v {
		if r == '-' || r == ' ' {
			flush()
			out = append(out, byte(r))
			continue
		}
		word.WriteRune(r)
	}
	flush()
	return string(out)
}
