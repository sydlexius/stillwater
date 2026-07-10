package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// artworkKindToType maps a Manage-artwork modal kind (the plain-language
// switcher label) to the API image-type segment the editor uses.
// NOTE: tests/unit/artwork-modal.test.js mirrors these constants in GO_KIND_TO_TYPE;
// keep both in sync when adding or changing cases.
func artworkKindToType(kind string) string {
	switch kind {
	case "logo":
		return "logo"
	case "banner":
		return "banner"
	case "backdrops":
		return "fanart"
	default:
		return "thumb" // primary / unknown
	}
}

// handleArtworkModal renders the reusable image editor (ArtworkManageEditor)
// as a fragment for the artist-detail page's in-page Manage-artwork modal,
// scoped to the requested kind (promoted from the next/ channel in #1757
// PR-3b). It reuses the same ImageSearchData the /artists/{id}/images page
// builds (handleArtistImagesPage); that route remains registered. Capability
// deltas vs the images page: this handler hardcodes AutoCrop:false.
// SelectedIndex defaults to -1 (no slot pre-selected) but is set from an
// optional ?slot= query param for kind=backdrops (#2323/#2281 item 4): the
// backdrop tile carousel (artist_artwork.templ's data-artwork-slot) threads
// which specific tile was clicked through artwork-modal.js's loadBody() to
// here, so the "Current Backdrop" hero + Actions menu scope to that slot
// instead of always showing the primary (slot 0) regardless of which tile
// was clicked. The modal shell lazy-loads this fragment per active kind.
func (r *Router) handleArtworkModal(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	id := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		r.logger.Error("handleArtworkModal: GetByID", "artist_id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Validate the kind param: only the four modal kinds are legal. An unknown
	// kind defaults to "primary" rather than silently mapping to "thumb" through
	// artworkKindToType's default branch (defensive against future kind-set growth).
	kind := req.URL.Query().Get("kind")
	switch kind {
	case "primary", "logo", "banner", "backdrops":
		// valid
	default:
		kind = "primary"
	}
	selectedType := artworkKindToType(kind)

	// #2323/#2281 item 4: only meaningful for kind=backdrops. Validate against
	// the artist's actual fanart count so an out-of-range or garbage slot
	// value (stale/racing tile click, tampered query string) falls back to
	// the generic unscoped view rather than a 404-ing or wrongly-scoped hero.
	selectedIndex := -1
	if kind == "backdrops" {
		if slotStr := req.URL.Query().Get("slot"); slotStr != "" {
			if slot, slotErr := strconv.Atoi(slotStr); slotErr == nil && slot >= 0 && slot < a.FanartCount {
				selectedIndex = slot
			}
		}
	}

	var webSearchEnabled bool
	if r.providerSettings != nil {
		var wsErr error
		webSearchEnabled, wsErr = r.providerSettings.AnyWebSearchEnabled(req.Context())
		if wsErr != nil {
			r.logger.Warn("handleArtworkModal: AnyWebSearchEnabled", "error", wsErr)
			// webSearchEnabled stays false: degraded but non-fatal
		}
	}
	autoFetch := r.getUserBoolPreference(req.Context(), PrefAutoFetchImages, r.getBoolSetting(req.Context(), "auto_fetch_images", false))

	data := templates.ImageSearchData{
		Artist:           *a,
		WebSearchEnabled: webSearchEnabled,
		AutoFetchImages:  autoFetch,
		SelectedType:     selectedType,
		SelectedIndex:    selectedIndex,
		ProfileName:      r.getActiveProfileName(req.Context()),
		AutoCrop:         false,
		BasePath:         r.basePath,
	}
	renderTempl(w, req, templates.ArtworkManageEditor(data))
}

// buildFieldFindings maps the artist's active rule violations to the metadata
// field(s) each rule inspects (rule.RuleFields), producing the field -> chips
// map the next/ artist-detail page renders. Image rules and whole-record /
// cross-field rules carry no field tag and are intentionally omitted here (they
// surface only in the non-field "Other findings" list). Returns nil when the
// rule service is absent or the artist has no field-tagged violations.
//
// It sources violations via ListViolationsFiltered (not GetViolationsForArtists)
// because the inline chip's click-popover now offers Fix/Dismiss actions that
// POST to /notifications/<id>/{fix,dismiss}: GetViolationsForArtists returns a
// summary projection with no violation ID, whereas ListViolationsFiltered
// returns the full RuleViolation rows. The "active" status filter matches the
// "Other findings" list's own query so the two surfaces stay consistent.
func (r *Router) buildFieldFindings(ctx context.Context, artistID string) map[string][]templates.FieldFinding {
	if r.ruleService == nil {
		return nil
	}
	violations, err := r.ruleService.ListViolationsFiltered(ctx, rule.ViolationListParams{
		Status:   "active",
		ArtistID: artistID,
	})
	if err != nil {
		r.logger.Warn("loading violations for field chips", "artist_id", artistID, "error", err)
		return nil
	}
	if len(violations) == 0 {
		return nil
	}
	// Friendly rule names for the chip popover header. The persisted
	// RuleViolation row carries only the rule id, so map id -> Name from the
	// built-in catalogue (DefaultRules is an in-memory slice, no DB round trip).
	// A custom or unknown rule id simply yields an empty name and the popover
	// falls back to a generic "Finding" label (fieldFindingTitle).
	ruleNames := map[string]string{}
	// Index rather than value-range: rule.Rule is a large struct and copying it
	// each iteration trips gocritic's rangeValCopy.
	defRules := rule.DefaultRules()
	for i := range defRules {
		ruleNames[defRules[i].ID] = defRules[i].Name
	}
	out := map[string][]templates.FieldFinding{}
	// Index rather than value-range: rule.RuleViolation is a large struct and
	// copying it each iteration trips gocritic's rangeValCopy.
	for i := range violations {
		v := &violations[i]
		fields := rule.RuleFields(v.RuleID)
		if len(fields) == 0 {
			continue
		}
		chip := templates.FieldFinding{
			ID:       v.ID,
			ArtistID: artistID,
			RuleID:   v.RuleID,
			Name:     ruleNames[v.RuleID],
			Severity: v.Severity,
			Message:  v.Message,
			// Fixable gates the popover's Fix button: only an open, fixable
			// violation can be auto-fixed (mirrors artistFindingListItem's gate).
			Fixable: v.Fixable && v.Status == rule.ViolationStatusOpen,
		}
		for _, f := range fields {
			out[f] = append(out[f], chip)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveArtistNeighbors returns the ids of the artists immediately before and
// after id in the current list ordering. It reuses the filter params the
// artists list carries (forwarded as query params on the detail link) so prev/
// next respects the user's active view; absent params it falls back to the
// default name-ascending order. Either id is "" when there is no neighbor or
// when id is outside the capped ListIDs window (large libraries) -- the hero's
// h/l shortcuts no-op cleanly in that case.
func (r *Router) resolveArtistNeighbors(req *http.Request, id string) (prevID, nextID string) {
	params := artist.CountParams{
		Search:    req.URL.Query().Get("search"),
		Filter:    req.URL.Query().Get("filter"),
		LibraryID: req.URL.Query().Get("library_id"),
		Filters:   parseFlyoutFilters(req),
		// CountParams has no Sort/Order fields; ListIDs always returns the
		// canonical sort_name-ascending order (see sqlite_artist.go ListIDs).
	}
	// ListIDs signature: (ids []string, total int, capped bool, err error).
	ids, _, _, err := r.artistService.ListIDs(req.Context(), params)
	if err != nil {
		// Log (don't silently swallow) before degrading: a real ListIDs failure
		// drops the prev/next neighbor links, and without this there'd be no
		// trail. Mirrors buildFieldFindings' warn-and-degrade pattern.
		r.logger.Warn("resolving artist neighbors", "artist_id", id, "error", err)
		return "", ""
	}
	if len(ids) == 0 {
		return "", ""
	}
	for i, cur := range ids {
		if cur != id {
			continue
		}
		if i > 0 {
			prevID = ids[i-1]
		}
		if i+1 < len(ids) {
			nextID = ids[i+1]
		}
		break
	}
	return prevID, nextID
}
