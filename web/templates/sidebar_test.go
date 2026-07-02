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
	// Admins get the HTMX-hydrated count placeholders (not static links).
	if !strings.Contains(html, `id="sidebar-compliance-nav"`) {
		t.Error("admin sidebar missing compliance count placeholder")
	}
	if !strings.Contains(html, `id="sidebar-duplicates-nav"`) {
		t.Error("admin sidebar missing duplicates count placeholder")
	}
	if !strings.Contains(html, `hx-get="/api/v1/reports/duplicates/count?ch=next"`) {
		t.Error("admin sidebar missing duplicates count hx-get URL (?ch=next)")
	}
	if !strings.Contains(html, `hx-trigger="load, every 60s"`) {
		t.Error("admin sidebar missing duplicates hx-trigger (load + 60s poll)")
	}
	// Foreign Files child is always present for admins and uses the sub-nav class.
	if !strings.Contains(html, `data-path="/reports/foreign-files"`) {
		t.Error("admin sidebar missing foreign-files sub-nav child")
	}
	if !strings.Contains(html, `class="sw-sidebar-link sw-sidebar-subnav-link"`) {
		t.Error("admin sidebar missing sw-sidebar-subnav-link class on a Reports child")
	}
}

// TestSidebar_ReportsSection_NonAdmin pins the #1757 fix-round restoration:
// non-admins see the Reports section again -- the workspace link plus a PLAIN
// Compliance link (both wrapOptionalAuth pages) -- but NOT the admin-only count
// pills (compliance/duplicates) or the Foreign Files item, whose count
// endpoints return 403 for non-admins (omitted to avoid a poll-and-403 and
// markup that lies about a reachable route).
func TestSidebar_ReportsSection_NonAdmin(t *testing.T) {
	html := renderSidebar(t, false)
	// Visible to non-admins.
	if !strings.Contains(html, `data-path="/reports"`) {
		t.Error("non-admin sidebar missing the Reports workspace link")
	}
	if !strings.Contains(html, `data-path="/reports/compliance"`) {
		t.Error("non-admin sidebar missing the plain Compliance link")
	}
	// Omitted for non-admins (admin-only count endpoints 403).
	if strings.Contains(html, `id="sidebar-compliance-nav"`) {
		t.Error("non-admin sidebar must omit the admin-only compliance count pill (poll-and-403)")
	}
	if strings.Contains(html, `id="sidebar-duplicates-nav"`) {
		t.Error("non-admin sidebar must omit the admin-only duplicates count pill (poll-and-403)")
	}
	if strings.Contains(html, `data-path="/reports/foreign-files"`) {
		t.Error("non-admin sidebar must omit the admin-only Foreign Files item")
	}
}
