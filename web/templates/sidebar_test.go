package templates

// sidebar_test.go -- render-level coverage for the promoted Reports section
// (M55 #1757 PR-1; the #1778/#1715 nav promoted over the v1 sidebar). The full
// Sidebar component is large, but a few specific markers can be pinned without
// snapshotting the whole nav. The Reports section renders for ALL roles, but its
// contents differ by role: admins get the HTMX-hydrated count placeholders
// (compliance/duplicates/foreign) that poll every 60s via ?ch=next hx-get URLs;
// non-admins get the Reports workspace link plus a PLAIN Compliance link (no
// count pill, so no 60s poll-and-403) and NONE of the admin-only count
// placeholders or the Foreign Files item. These tests pin the admin count URLs
// (?ch=next) and the presence/absence of each item per role.

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
	if !strings.Contains(html, `hx-get="/api/v1/reports/compliance/count?ch=next"`) {
		t.Error("admin sidebar missing compliance count hx-get URL (?ch=next)")
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
	if !strings.Contains(html, `class="sw-sidebar-link sw-sidebar-subnav-link"`) {
		t.Error("admin sidebar missing sw-sidebar-subnav-link class on a Reports child")
	}
	// Backdrop Duplicates, Platform Backdrop Duplicates and Unmatched Images
	// MOVED to the Images section (#2608). They are no longer server-rendered
	// anywhere in the sidebar -- the Images section hydrates them, and hides
	// entirely when every count is zero. Assert their absence so a revert that
	// re-adds a static link here is caught.
	for _, moved := range []string{
		`data-path="/reports/backdrop-duplicates"`,
		`data-path="/reports/platform-backdrop-duplicates"`,
		`data-path="/reports/foreign-files"`,
	} {
		if strings.Contains(html, moved) {
			t.Errorf("sidebar still server-renders %s; it moved into the hydrated Images section (#2608)", moved)
		}
	}
}

// TestSidebar_ImagesSection_AdminContainerOnly pins the #2608 structure: the
// admin sidebar ships an EMPTY hydration container for the Images section and
// nothing else. The header, the Unmatched row and the duplicate rows all come
// from the endpoint, because the section HIDES when every count is zero and
// that decision needs all three counts at once -- a server-rendered header
// here could not be taken back.
func TestSidebar_ImagesSection_AdminContainerOnly(t *testing.T) {
	html := renderSidebar(t, true)

	if !strings.Contains(html, `id="sidebar-images-section"`) {
		t.Fatal("admin sidebar missing the Images section hydration container")
	}
	if !strings.Contains(html, `hx-get="/api/v1/reports/duplicate-images/nav?ch=next"`) {
		t.Error("Images container missing its hx-get URL (?ch=next)")
	}
	if !strings.Contains(html, `hx-on::after-swap="swSidebar.swImagesNavSwap()"`) {
		t.Error("Images container missing the after-swap hook that drives the unmatched pulse")
	}
	// The container must be EMPTY and must NOT carry .sw-sidebar-section: that
	// class has vertical padding, so an all-zero (empty) response would leave a
	// phantom gap where the hidden section used to be. The fragment puts the
	// class on its own wrapper instead.
	if strings.Contains(html, `class="sw-sidebar-section" id="sidebar-images-section"`) ||
		strings.Contains(html, `id="sidebar-images-section" class="sw-sidebar-section"`) {
		t.Error("Images container must not carry .sw-sidebar-section (padding would leave a gap when hidden)")
	}
	if !strings.Contains(html, `hx-on::after-swap="swSidebar.swImagesNavSwap()"></div>`) {
		t.Error("Images container must be server-rendered EMPTY; the section hydrates as a whole")
	}
}

// Non-admins get no Images container at all -- not an empty one that would
// poll and 403.
func TestSidebar_ImagesSection_AbsentForNonAdmin(t *testing.T) {
	html := renderSidebar(t, false)

	for _, forbidden := range []string{
		`id="sidebar-images-section"`,
		`/api/v1/reports/duplicate-images/nav`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("non-admin sidebar must omit %s (the endpoint 403s; no poll-and-403)", forbidden)
		}
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
	// Backdrop Duplicates is admin-only (requireForeignAdmin); non-admins must
	// not see it. It now lives in the Images section, which non-admins do not
	// get at all.
	if strings.Contains(html, `data-path="/reports/backdrop-duplicates"`) {
		t.Error("non-admin sidebar must omit the admin-only Backdrop Duplicates item")
	}
}
