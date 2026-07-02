package templates

// sidebar_test.go -- render-level coverage for the promoted Reports section
// (M55 #1757 PR-1; the #1778/#1715 nav promoted over the v1 sidebar). The full
// Sidebar component is large, but a few specific markers can be pinned without
// snapshotting the whole nav: the whole Reports section (Compliance count
// placeholder, Duplicates count placeholder, Foreign Files link) is admin-only
// and must be omitted entirely for non-admins so a non-admin tab never spawns
// the 60s poll-and-403; the Duplicates placeholder must carry the right
// ?ch=next hx-get URL, and the Foreign Files child is always present for admins.

import (
	"bytes"
	"strings"
	"testing"
)

func renderSidebar(t *testing.T, isAdmin bool) string {
	t.Helper()
	var buf bytes.Buffer
	data := SidebarData{
		BasePath:    "",
		IsAdmin:     isAdmin,
		Username:    "test-user",
		DisplayName: "Test User",
		Role:        "operator",
		Version:     "0.0.0-test",
	}
	if isAdmin {
		data.Role = "administrator"
	}
	if err := Sidebar(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("rendering sidebar: %v", err)
	}
	return buf.String()
}

func TestSidebar_ReportsSection_AdminChildrenRender(t *testing.T) {
	html := renderSidebar(t, true)
	// Compliance is now an HTMX-hydrated count placeholder, not a static link.
	if !strings.Contains(html, `id="sidebar-compliance-nav"`) {
		t.Error("admin sidebar missing compliance count placeholder")
	}
	// Foreign Files child is always present for admins and uses the sub-nav class.
	if !strings.Contains(html, `data-path="/reports/foreign-files"`) {
		t.Error("admin sidebar missing foreign-files sub-nav child")
	}
	if !strings.Contains(html, `class="sw-sidebar-link sw-sidebar-subnav-link"`) {
		t.Error("admin sidebar missing sw-sidebar-subnav-link class on a Reports child")
	}
}

func TestSidebar_ReportsSection_AdminOnly(t *testing.T) {
	adminHTML := renderSidebar(t, true)
	if !strings.Contains(adminHTML, `id="sidebar-duplicates-nav"`) {
		t.Error("admin sidebar missing duplicates placeholder element")
	}
	if !strings.Contains(adminHTML, `hx-get="/api/v1/reports/duplicates/count?ch=next"`) {
		t.Error("admin sidebar missing duplicates count hx-get URL (?ch=next)")
	}
	if !strings.Contains(adminHTML, `hx-trigger="load, every 60s"`) {
		t.Error("admin sidebar missing duplicates hx-trigger (load + 60s poll)")
	}

	operatorHTML := renderSidebar(t, false)
	// The entire Reports section is admin-only in the promoted nav.
	if strings.Contains(operatorHTML, `id="sidebar-duplicates-nav"`) {
		t.Error("non-admin sidebar should omit duplicates placeholder; would spawn a 60s poll-and-403")
	}
	if strings.Contains(operatorHTML, `id="sidebar-compliance-nav"`) {
		t.Error("non-admin sidebar should omit the whole Reports section (compliance placeholder present)")
	}
	if strings.Contains(operatorHTML, `data-path="/reports/foreign-files"`) {
		t.Error("non-admin sidebar should omit the foreign-files Reports child")
	}
}
