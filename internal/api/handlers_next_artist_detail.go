package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextArtistDetailPage serves the next/ channel artist-detail page
// (M55 #1336). When the resolved UX channel is not "next" (the lane is off or a
// sw_ux=stable cookie opted the user back) it delegates to the stable
// handleArtistDetailPage so /next/artists/{id} never dead-ends (decision 12),
// mirroring handleNextArtistsPage. Otherwise it assembles the shared
// ArtistDetailData, resolves prev/next-artist neighbor ids (for the h/l
// shortcuts) from the filter-aware ListIDs ordering, reads the section
// order/hidden prefs, and renders next.ArtistDetailPage.
func (r *Router) handleNextArtistDetailPage(w http.ResponseWriter, req *http.Request) {
	if middleware.UXChannelFromContext(req.Context()) != middleware.UXNext {
		r.handleArtistDetailPage(w, req)
		return
	}

	data, a, ok := r.buildArtistDetailData(w, req)
	if !ok {
		return
	}

	prevID, nextID := r.resolveArtistNeighbors(req, a.ID)

	order := parseSectionList(r.getUserStringPreference(req.Context(), PrefArtistDetailSectionOrder, ""))
	hidden := parseSectionList(r.getUserStringPreference(req.Context(), PrefArtistDetailHiddenSections, ""))

	pageData := next.ArtistDetailPageData{
		Detail:       data,
		PrevArtistID: prevID,
		NextArtistID: nextID,
		SectionOrder: order,
		Hidden:       hidden,
	}

	// Inject the field -> finding chips map so the metadata rows render an inline
	// chip on each field a live violation touches (field-tag-on-rule; #1336). The
	// stable channel never sets this, so its FieldDisplay rows stay chip-free.
	ctx := templates.WithFieldFindings(req.Context(), r.buildFieldFindings(req.Context(), a.ID))
	renderTempl(w, req.WithContext(ctx), next.ArtistDetailPage(r.assetsFor(req), pageData))
}

// artworkKindToType maps a next/ Manage-artwork modal kind (the plain-language
// switcher label) to the API image-type segment the editor uses.
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

// handleNextArtworkModal renders the reusable image editor (ArtworkManageEditor)
// as a fragment for the next/ in-page Manage-artwork modal, scoped to the
// requested kind. It reuses the same ImageSearchData the stable image page
// builds (handleArtistImagesPage). The modal supersedes the stable
// /artists/{id}/images page for the next/ channel; that route remains
// registered. Capability deltas vs the stable page: this handler hardcodes
// AutoCrop:false and SelectedIndex:-1 (the modal does not pre-select a slot).
// The modal shell lazy-loads this fragment per active kind.
func (r *Router) handleNextArtworkModal(w http.ResponseWriter, req *http.Request) {
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
		r.logger.Error("handleNextArtworkModal: GetByID", "artist_id", id, "error", err)
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

	var webSearchEnabled bool
	if r.providerSettings != nil {
		var wsErr error
		webSearchEnabled, wsErr = r.providerSettings.AnyWebSearchEnabled(req.Context())
		if wsErr != nil {
			r.logger.Warn("handleNextArtworkModal: AnyWebSearchEnabled", "error", wsErr)
			// webSearchEnabled stays false: degraded but non-fatal
		}
	}
	autoFetch := r.getUserBoolPreference(req.Context(), PrefAutoFetchImages, r.getBoolSetting(req.Context(), "auto_fetch_images", false))

	data := templates.ImageSearchData{
		Artist:           *a,
		WebSearchEnabled: webSearchEnabled,
		AutoFetchImages:  autoFetch,
		SelectedType:     selectedType,
		SelectedIndex:    -1,
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
// surface only in the Open Findings list). Returns nil when the rule service is
// absent or the artist has no field-tagged violations.
func (r *Router) buildFieldFindings(ctx context.Context, artistID string) map[string][]templates.FieldFinding {
	if r.ruleService == nil {
		return nil
	}
	byArtist, err := r.ruleService.GetViolationsForArtists(ctx, []string{artistID})
	if err != nil {
		r.logger.Warn("loading violations for field chips", "artist_id", artistID, "error", err)
		return nil
	}
	violations := byArtist[artistID]
	if len(violations) == 0 {
		return nil
	}
	out := map[string][]templates.FieldFinding{}
	// Index rather than value-range: rule.Violation is a large struct and copying
	// it each iteration trips gocritic's rangeValCopy.
	for i := range violations {
		v := &violations[i]
		fields := rule.RuleFields(v.RuleID)
		if len(fields) == 0 {
			continue
		}
		chip := templates.FieldFinding{
			Severity: v.Severity,
			Message:  v.Message,
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
