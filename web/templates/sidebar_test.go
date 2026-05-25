package templates

// sidebar_test.go -- render-level coverage for the Reports sub-nav (#1665).
// The full Sidebar component is large, but a few specific markers can be
// pinned without snapshotting the whole nav: the compliance child link must
// always render, and the duplicates placeholder must render with the right
// hx-get URL for admin users (and be omitted entirely for non-admins so a
// non-admin tab never spawns the 60s poll-and-403).

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

func TestSidebar_ReportsSubnav_ComplianceAlwaysRenders(t *testing.T) {
	for _, isAdmin := range []bool{true, false} {
		html := renderSidebar(t, isAdmin)
		if !strings.Contains(html, `data-path="/reports/compliance"`) {
			t.Errorf("isAdmin=%v: sidebar missing compliance sub-nav child", isAdmin)
		}
		if !strings.Contains(html, `class="sw-sidebar-link sw-sidebar-subnav-link"`) {
			t.Errorf("isAdmin=%v: sidebar missing sw-sidebar-subnav-link class on child", isAdmin)
		}
	}
}

func TestSidebar_ReportsSubnav_DuplicatesAdminOnly(t *testing.T) {
	adminHTML := renderSidebar(t, true)
	if !strings.Contains(adminHTML, `id="sidebar-duplicates-nav"`) {
		t.Error("admin sidebar missing duplicates placeholder element")
	}
	if !strings.Contains(adminHTML, `hx-get="/api/v1/reports/duplicates/count"`) {
		t.Error("admin sidebar missing duplicates count hx-get URL")
	}
	if !strings.Contains(adminHTML, `hx-trigger="load, every 60s"`) {
		t.Error("admin sidebar missing duplicates hx-trigger (load + 60s poll)")
	}

	operatorHTML := renderSidebar(t, false)
	if strings.Contains(operatorHTML, `id="sidebar-duplicates-nav"`) {
		t.Error("non-admin sidebar should omit duplicates placeholder; would spawn a 60s poll-and-403")
	}
	if strings.Contains(operatorHTML, `hx-get="/api/v1/reports/duplicates/count"`) {
		t.Error("non-admin sidebar should omit duplicates count hx-get attribute entirely")
	}
}
