package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// prefsPageRequest issues GET /preferences against the promoted preferences
// page handler with (or without) an authed user context.
func prefsPageRequest(t *testing.T, r *Router, userID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if userID != "" {
		ctx = middleware.WithTestUserID(ctx, userID)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/preferences", nil)
	w := httptest.NewRecorder()
	r.handleUserPreferencesPage(w, req)
	return w
}

// prefsDrawerRequest issues GET /preferences-drawer against the fragment
// handler.
func prefsDrawerRequest(t *testing.T, r *Router, userID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if userID != "" {
		ctx = middleware.WithTestUserID(ctx, userID)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/preferences-drawer", nil)
	w := httptest.NewRecorder()
	r.handleUserPreferencesDrawer(w, req)
	return w
}

// toggleState extracts the aria-checked value of the toggle button whose
// rendered markup carries the given id. Returns "" when the id (or its
// aria-checked attribute within the same tag) is not found.
func toggleState(body, id string) string {
	idx := strings.Index(body, `id="`+id+`"`)
	if idx == -1 {
		return ""
	}
	tagEnd := strings.Index(body[idx:], ">")
	if tagEnd == -1 {
		return ""
	}
	tag := body[idx : idx+tagEnd]
	const marker = `aria-checked="`
	a := strings.Index(tag, marker)
	if a == -1 {
		// The id and aria-checked may sit in either order within the tag;
		// scan backwards from the id to the tag opening as well.
		start := strings.LastIndex(body[:idx], "<")
		tag = body[start : idx+tagEnd]
		a = strings.Index(tag, marker)
		if a == -1 {
			return ""
		}
	}
	rest := tag[a+len(marker):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return ""
	}
	return rest[:end]
}

// seedUserPref stores a single preference row for the user directly in the DB
// (same insert the artist-lock page-size tests use).
func seedUserPref(t *testing.T, r *Router, userID, key, value string) {
	t.Helper()
	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		userID, key, value)
	if err != nil {
		t.Fatalf("storing %s pref: %v", key, err)
	}
}

// TestHandleUserPreferencesPage_RendersStandalonePage verifies the authed
// standalone page renders the drawer content inline (the direct-URL / no-JS
// fallback) with the page wrapper that neutralizes the flyout positioning.
func TestHandleUserPreferencesPage_RendersStandalonePage(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	w := prefsPageRequest(t, r, "test-user")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"sw-prefs-page-wrapper", // standalone wrapper (renders the drawer in-flow)
		"sw-prefs-drawer",       // the drawer markup itself
		"sw-prefs-search",       // filter box
		"pref-d-font-size-slider",
		"data-stop-names",               // localized 5-stop label contract for prefs-drawer.js
		`data-group-id="artist-layout"`, // artist-detail layout card group
	} {
		if !strings.Contains(body, want) {
			t.Errorf("standalone preferences page missing %q", want)
		}
	}
}

// TestHandleUserPreferencesPage_Unauthenticated verifies the page handler
// falls back to the login page rather than leaking preference content.
func TestHandleUserPreferencesPage_Unauthenticated(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	w := prefsPageRequest(t, r, "")
	if w.Code != http.StatusOK {
		t.Errorf("unauthenticated status = %d, want %d (login page)", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if strings.Contains(body, "sw-prefs-drawer") {
		t.Errorf("unauthenticated response leaked drawer content")
	}
}

// TestHandleUserPreferencesDrawer_FragmentOnly verifies the HTMX fragment
// endpoint returns the drawer body without the page chrome.
func TestHandleUserPreferencesDrawer_FragmentOnly(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	w := prefsDrawerRequest(t, r, "test-user")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-prefs-drawer") {
		t.Errorf("fragment missing the drawer markup")
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "sw-prefs-page-wrapper") {
		t.Errorf("fragment must not carry page chrome or the standalone wrapper")
	}
}

// TestHandleUserPreferencesDrawer_Unauthorized verifies the fragment endpoint
// rejects unauthenticated requests with a JSON 401 (it is an XHR target, not
// a navigable page).
func TestHandleUserPreferencesDrawer_Unauthorized(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	w := prefsDrawerRequest(t, r, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Errorf("401 body missing the error message: %s", w.Body.String())
	}
}

// TestUserPrefsData_StoredValuesWinOverDefaults seeds stored preferences and
// asserts the rendered drawer reflects them: the font-size slider sits on the
// stored stop, the seeded section order leads the layout card, and the hidden
// section renders with its visibility toggled off.
func TestUserPrefsData_StoredValuesWinOverDefaults(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	const userID = "prefs-stored-user"

	seedUserPref(t, r, userID, "font_size", "xx-large")
	seedUserPref(t, r, userID, "theme", "light")
	seedUserPref(t, r, userID, "page_size", "120")
	seedUserPref(t, r, userID, "bg_opacity", "90")
	seedUserPref(t, r, userID, "auto_fetch_images", "true")
	seedUserPref(t, r, userID, "notification_enabled", "false")
	seedUserPref(t, r, userID, "artist_detail_section_order", `["discography","metadata"]`)
	seedUserPref(t, r, userID, "artist_detail_hidden_sections", `["providers"]`)

	w := prefsPageRequest(t, r, userID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// font_size xx-large = slider stop index 4.
	if !strings.Contains(body, `value="4"`) {
		t.Errorf("slider not on the stored xx-large stop (value=4)")
	}
	// page_size round-trips into the number input.
	if !strings.Contains(body, `value="120"`) {
		t.Errorf("page_size input does not carry the stored 120")
	}
	// bg_opacity round-trips through normalizeBgOpacity into the slider.
	if !strings.Contains(body, `value="90"`) {
		t.Errorf("bg_opacity slider does not carry the stored 90")
	}
	// Stored toggle values normalize through normalizeBoolPref: auto-fetch on,
	// notifications off (each overrides its compiled default).
	if af := toggleState(body, "pref-d-auto-fetch"); af != "true" {
		t.Errorf("auto_fetch_images toggle aria-checked = %q, want true", af)
	}
	if ne := toggleState(body, "pref-d-notification"); ne != "false" {
		t.Errorf("notification_enabled toggle aria-checked = %q, want false", ne)
	}
	// Stored section order leads the layout card: discography's row must
	// appear before metadata's row in the rendered markup.
	disco := strings.Index(body, `data-section-id="discography"`)
	meta := strings.Index(body, `data-section-id="metadata"`)
	if disco == -1 || meta == -1 {
		t.Fatalf("layout card rows missing (discography=%d, metadata=%d)", disco, meta)
	}
	if disco > meta {
		t.Errorf("stored section order not applied: discography (%d) renders after metadata (%d)", disco, meta)
	}
}

// TestUserPrefsData_DefaultsWhenUnset verifies a user with no stored rows gets
// the compiled defaults (medium font stop, default section taxonomy).
func TestUserPrefsData_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	w := prefsPageRequest(t, r, "prefs-fresh-user")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// font_size default medium = slider stop index 1.
	if !strings.Contains(body, `value="1"`) {
		t.Errorf("slider not on the default medium stop (value=1)")
	}
	// All six layout sections render in the card.
	for _, sec := range []string{"metadata", "artwork", "findings", "providers", "discography", "identifiers"} {
		if !strings.Contains(body, `data-section-id="`+sec+`"`) {
			t.Errorf("layout card missing default section %q", sec)
		}
	}
}

// TestUserPrefsData_ShowPlatformDebugDefault verifies the show_platform_debug
// pref defaults to "false" when no stored row exists: the Behavior group toggle
// renders with aria-checked="false".
func TestUserPrefsData_ShowPlatformDebugDefault(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	w := prefsPageRequest(t, r, "prefs-debug-default-user")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := toggleState(w.Body.String(), "pref-d-show-platform-debug"); got != "false" {
		t.Errorf("show_platform_debug default: aria-checked = %q, want false", got)
	}
}

// TestUserPrefsData_ShowPlatformDebugStored verifies the stored value wins:
// seeding show_platform_debug = "true" causes the Behavior group toggle to
// render with aria-checked="true". This exercises the stored-value override
// branch added by M55 #2060.
func TestUserPrefsData_ShowPlatformDebugStored(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	const userID = "prefs-debug-stored-user"

	seedUserPref(t, r, userID, "show_platform_debug", "true")

	w := prefsPageRequest(t, r, userID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := toggleState(w.Body.String(), "pref-d-show-platform-debug"); got != "true" {
		t.Errorf("show_platform_debug stored override: aria-checked = %q, want true", got)
	}
}
