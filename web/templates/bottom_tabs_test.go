package templates

// bottom_tabs_test.go -- render-level coverage for the mobile "More" sheet
// (#2382). At mobile widths the desktop sidebar is display:none with no
// replacement affordance; BottomTabs' 5th "More" tab plus its BottomSheet is
// that replacement. The issue's acceptance criteria requires 1:1 coverage
// against the sidebar's destination list -- these tests pin the full set of
// sidebar hrefs (both the static ones in sidebar.templ and the hydrated
// Images-section ones in duplicate_images_nav.templ) and assert every one is
// reachable from the rendered BottomTabs output, so a sidebar destination
// added later without a matching More-sheet entry fails here instead of
// silently going unreachable on mobile again.

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

func renderBottomTabs(t *testing.T, isAdmin bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := BottomTabs("", "/", isAdmin).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("rendering BottomTabs: %v", err)
	}
	return buf.String()
}

// TestBottomTabs_MoreSheet_AdminCoverage pins 1:1 coverage of every sidebar
// destination reachable by an administrator. The set mirrors sidebar.templ
// (Dashboard, Artists, Reports workspace, Compliance, Activity, Logs,
// Settings, Preferences) plus the hydrated Images section in
// duplicate_images_nav.templ (Unmatched, Library Duplicates, Platform
// Duplicates) and Duplicates (admin-only, sidebar.templ). Dashboard,
// Artists, Compliance and Settings are covered by the four primary tabs, not
// the sheet; everything else must appear in the sheet.
func TestBottomTabs_MoreSheet_AdminCoverage(t *testing.T) {
	html := renderBottomTabs(t, true)

	// The four primary tabs.
	for _, primary := range []string{`href="/"`, `href="/artists"`, `href="/reports/compliance"`, `href="/settings"`} {
		if !strings.Contains(html, primary) {
			t.Errorf("admin BottomTabs missing primary tab %s", primary)
		}
	}

	// Every remaining sidebar destination must be reachable from the More
	// sheet as a static href.
	for _, dest := range []string{
		"/reports",                              // Reports workspace link
		"/reports/duplicates",                   // Duplicates (admin-only)
		"/reports/foreign-files",                // Images: Unmatched
		"/reports/backdrop-duplicates",          // Images: Library Duplicates
		"/reports/platform-backdrop-duplicates", // Images: Platform Duplicates
		"/activity",                             // Activity
		"/logs",                                 // Logs (admin-only)
	} {
		if !strings.Contains(html, `href="`+dest+`"`) {
			t.Errorf("admin More sheet missing sidebar destination %q; the mobile nav has drifted from the sidebar (#2382)", dest)
		}
	}

	// Non-link actions the sidebar's bottom bar exposes: theme toggle, help,
	// log out. These aren't hrefs (they're onclick/hx-post actions), so pin
	// their distinguishing markers instead.
	if !strings.Contains(html, `hx-post="/api/v1/auth/logout"`) {
		t.Error("admin More sheet missing the Log Out action")
	}
	// English fixture strings (testCtx uses the default/English locale); these
	// pin presence of the label text, not a translation lookup.
	for _, label := range []string{"Cycle theme", "Help shortcuts", "Preferences"} {
		if !strings.Contains(html, label) {
			t.Errorf("admin More sheet missing expected label %q", label)
		}
	}
}

// TestBottomTabs_MoreSheet_NonAdminOmitsAdminOnly mirrors sidebar_test.go's
// non-admin assertions: the admin-only destinations (Duplicates, the whole
// Images section, Logs) must NOT appear in a non-admin's More sheet, the same
// way they don't appear in a non-admin's sidebar (their count endpoints 403
// for non-admins).
func TestBottomTabs_MoreSheet_NonAdminOmitsAdminOnly(t *testing.T) {
	html := renderBottomTabs(t, false)

	if strings.Contains(html, `href="/settings"`) {
		t.Error("non-admin BottomTabs must omit the Settings primary tab")
	}
	for _, forbidden := range []string{
		"/reports/duplicates",
		"/reports/foreign-files",
		"/reports/backdrop-duplicates",
		"/reports/platform-backdrop-duplicates",
		"/logs",
	} {
		if strings.Contains(html, `href="`+forbidden+`"`) {
			t.Errorf("non-admin More sheet must omit admin-only destination %q", forbidden)
		}
	}
	// Reports workspace and Activity remain reachable by everyone.
	for _, dest := range []string{"/reports", "/activity"} {
		if !strings.Contains(html, `href="`+dest+`"`) {
			t.Errorf("non-admin More sheet missing universally reachable destination %q", dest)
		}
	}
	if !strings.Contains(html, `hx-post="/api/v1/auth/logout"`) {
		t.Error("non-admin More sheet missing the Log Out action")
	}
}

// TestBottomTabs_MoreTab_TouchTarget44px pins the presence of the CSS class
// carrying the 44px min-width/min-height rule (UX0). The rule itself lives in
// input.css and is exercised by rendered evidence separately; this guards
// only that the More tab shares the same class as the other bottom tabs
// rather than drifting onto an unstyled element.
func TestBottomTabs_MoreTab_TouchTarget44px(t *testing.T) {
	html := renderBottomTabs(t, true)
	if !strings.Contains(html, `class="sw-bottom-tab"`) {
		t.Fatal("More tab missing the shared sw-bottom-tab class (44px touch target rule)")
	}
	if !strings.Contains(html, `aria-controls="bs-more-nav"`) {
		t.Error("More tab missing aria-controls pointing at the sheet")
	}
	if !strings.Contains(html, `id="bs-more-nav"`) {
		t.Error("More sheet missing its id (bs-more-nav)")
	}
}

// sidebarDestinations extracts every navigable destination from the sidebar's
// TEMPL SOURCE at test time.
//
// This is the drift guard the issue's first acceptance criterion actually
// asks for, and it is deliberately not a second hand-maintained list. The
// hardcoded slices in the tests above catch a destination removed from the
// SHEET, but they cannot catch one ADDED to the SIDEBAR -- and that is the
// direction the regression travels: someone adds a sidebar entry, never
// touches the mobile nav, and the destination is silently unreachable on a
// phone again. Verified by mutation: renaming a sidebar href to a value the
// sheet does not carry left every other test in this file passing.
//
// Reading source rather than rendering is deliberate. The sidebar's
// admin-gated and count-hydrated entries only appear in rendered output under
// the right role and non-zero counts, so a render-based enumeration would
// quietly under-report exactly the items most likely to drift.
func sidebarDestinations(t *testing.T) []string {
	t.Helper()
	// duplicate_images_nav.templ holds the Images section the sidebar
	// hydrates in via HTMX, so it is part of the sidebar's surface even
	// though it lives in its own file.
	var out []string
	seen := map[string]bool{}
	for _, f := range []string{"sidebar.templ", "duplicate_images_nav.templ"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		for _, m := range basePathHref.FindAllStringSubmatch(string(src), -1) {
			d := m[1]
			// "/" is the Dashboard primary tab, not a sheet item.
			if d == "" || d == "/" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		t.Fatal("no destinations parsed from the sidebar sources; the extraction " +
			"regex has drifted and every assertion built on it would pass vacuously")
	}
	return out
}

// basePathHref matches the sidebar's `templ.SafeURL(data.BasePath + "/path")`
// link form. Pinned as a package-level var so the vacuity check above has a
// single thing to guard.
var basePathHref = regexp.MustCompile(`BasePath \+ "([^"]*)"`)

// TestBottomTabs_MoreSheet_NoSidebarDestinationIsUnreachable is the real 1:1
// coverage assertion: EVERY destination the sidebar offers must be reachable
// at mobile width, either as one of the four primary tabs or as a More-sheet
// entry. It derives the expected set from the sidebar source, so a sidebar
// entry added without a matching mobile affordance fails here rather than
// shipping as an unreachable page on a phone -- which is the whole of #2382.
func TestBottomTabs_MoreSheet_NoSidebarDestinationIsUnreachable(t *testing.T) {
	// Admin render: the widest surface, so it must cover every destination.
	// A non-admin's sidebar is a strict subset (see the non-admin test above).
	html := renderBottomTabs(t, true)

	// A destination counts as reachable if the mobile nav offers ANY
	// affordance for it, not only a plain href. Preferences is the case that
	// forces this: the sidebar renders it as a drawer trigger whose href is a
	// no-JS fallback, and the More sheet opens the same drawer via OnClick. An
	// href-only assertion would call that unreachable and be wrong.
	reachable := func(dest string) bool {
		if strings.Contains(html, `href="`+dest+`"`) {
			return true
		}
		// The sheet's non-href affordances are templ scripts named for what
		// they open. Map the destination to the handler that reaches it.
		for _, marker := range sheetActionMarkers[dest] {
			if strings.Contains(html, marker) {
				return true
			}
		}
		return false
	}

	for _, dest := range sidebarDestinations(t) {
		if !reachable(dest) {
			t.Errorf("sidebar destination %q is NOT reachable from the mobile nav.\n"+
				"Add it to moreTabItems (or make it a primary tab). A sidebar entry "+
				"with no mobile affordance is unreachable on a phone, which is the "+
				"defect #2382 fixes.", dest)
		}
	}
}

// sheetActionMarkers maps a sidebar destination to the rendered markers that
// prove the More sheet reaches it by some means OTHER than a plain link.
//
// Deliberately a tiny explicit map rather than a loose "any mention of the
// path" search: the point of the drift test is to fail when a destination
// gains no mobile affordance, and a fuzzy match would let an unrelated
// occurrence of the string satisfy it. Adding an entry here is a conscious
// statement that this destination is reached a different way.
var sheetActionMarkers = map[string][]string{
	// Preferences opens the drawer in-place; openPreferencesFromSheet is the
	// sheet's handler, matching the sidebar's own data-sw-prefs-trigger.
	"/preferences": {"openPreferencesFromSheet"},
}
