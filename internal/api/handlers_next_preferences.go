package api

import (
	"net/http"
	"strconv"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/templates"
	next "github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextPreferencesPage renders the next/ preferences flyout drawer content
// fragment. This is the HTMX target loaded when the drawer is first opened:
// the drawer shell lives in LayoutNext; this handler returns the drawer body.
//
// It can also be loaded as a standalone page at /next/preferences for
// direct-URL access (accessibility: users can bookmark/share the path).
//
// GET /next/preferences
// GET /next/preferences-drawer (drawer fragment; same handler, no chrome)
func (r *Router) handleNextPreferencesPage(w http.ResponseWriter, req *http.Request) {
	if middleware.UXChannelFromContext(req.Context()) != middleware.UXNext {
		http.NotFound(w, req)
		return
	}
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}
	prefs, err := r.loadNextPrefsData(w, req, userID)
	if err != nil {
		// loadNextPrefsData already wrote the error response.
		return
	}
	renderTempl(w, req, next.PreferencesPageNext(r.assetsFor(req), prefs))
}

// handleNextPreferencesDrawer returns only the drawer body fragment for
// HTMX lazy loading. The drawer chrome shell is already in the DOM (mounted by
// LayoutNext); this handler returns the body content that fills it in.
//
// GET /next/preferences-drawer
func (r *Router) handleNextPreferencesDrawer(w http.ResponseWriter, req *http.Request) {
	if middleware.UXChannelFromContext(req.Context()) != middleware.UXNext {
		http.NotFound(w, req)
		return
	}
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	prefs, err := r.loadNextPrefsData(w, req, userID)
	if err != nil {
		return
	}
	renderTempl(w, req, next.PrefsDrawer(r.assetsFor(req), prefs))
}

// loadNextPrefsData reads all stored preferences for userID and builds a
// PreferencesData struct, falling back to compiled defaults for missing keys.
// On error it writes the HTTP response and returns a non-nil error.
func (r *Router) loadNextPrefsData(w http.ResponseWriter, req *http.Request, userID string) (templates.PreferencesData, error) {
	ctx := req.Context()

	rows, err := r.db.QueryContext(ctx,
		`SELECT key, value FROM user_preferences WHERE user_id = ?`, userID)
	if err != nil {
		r.logger.Error("querying user preferences for next/ drawer", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.PreferencesData{}, err
	}
	defer rows.Close() //nolint:errcheck // rows.Close error is not actionable here; SQL error already checked via rows.Err

	stored := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			r.logger.Error("scanning user preference for next/ drawer", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return templates.PreferencesData{}, err
		}
		stored[k] = v
	}
	if err := rows.Err(); err != nil {
		r.logger.Error("iterating user preferences for next/ drawer", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.PreferencesData{}, err
	}

	pref := func(key string) string {
		if v, ok := stored[key]; ok {
			return v
		}
		if def, ok := preferenceDefaults[key]; ok {
			return def.defaultValue
		}
		return ""
	}

	pageSize := PageSizeDefault
	if v, ok := stored[PrefPageSize]; ok {
		if n, err2 := strconv.Atoi(v); err2 == nil && n >= PageSizeMin && n <= PageSizeMax {
			pageSize = n
		}
	}

	bgOpacity := strconv.Itoa(BgOpacityDefault)
	if v, ok := stored[PrefBgOpacity]; ok {
		bgOpacity = normalizeBgOpacity(v)
	}

	legacyAutoFetch := strconv.FormatBool(r.getBoolSetting(ctx, "auto_fetch_images", false))
	autoFetchImages := legacyAutoFetch
	if v, ok := stored[PrefAutoFetchImages]; ok {
		autoFetchImages = normalizeBoolPref(v, legacyAutoFetch)
	}

	notifEnabled := preferenceDefaults[PrefNotificationEnabled].defaultValue
	if v, ok := stored[PrefNotificationEnabled]; ok {
		notifEnabled = normalizeBoolPref(v, notifEnabled)
	}

	return templates.PreferencesData{
		Theme:                      pref(PrefTheme),
		ThumbnailSize:              pref(PrefThumbnailSize),
		SidebarState:               pref(PrefSidebarState),
		ContentWidth:               pref(PrefContentWidth),
		ReducedMotion:              pref(PrefReducedMotion),
		Language:                   pref(PrefLanguage),
		FontFamily:                 pref(PrefFontFamily),
		LetterSpacing:              pref(PrefLetterSpacing),
		FontSize:                   pref(PrefFontSize),
		LiteMode:                   pref(PrefLiteMode),
		PageSize:                   pageSize,
		AutoFetchImages:            autoFetchImages,
		BackgroundOpacity:          bgOpacity,
		Density:                    pref(PrefDensity),
		MonoFont:                   pref(PrefMonoFont),
		KbdHints:                   pref(PrefKbdHints),
		NotificationEnabled:        notifEnabled,
		ArtistDetailSectionOrder:   parseSectionList(stored[PrefArtistDetailSectionOrder]),
		ArtistDetailHiddenSections: parseSectionList(stored[PrefArtistDetailHiddenSections]),
	}, nil
}
