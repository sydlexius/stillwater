package api

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/web/templates"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// TestLayoutNext_MountsSharedChrome verifies that the next/ channel layout
// composes the same shared chrome the stable Layout does (M55 #1340): the
// ProgressPill status bar, base-path + CSRF injection, the toast container, and
// the SSE client, plus the navigation sidebar. LayoutNext must not fork that
// infrastructure, so these markers must appear in its rendered output.
func TestLayoutNext_MountsSharedChrome(t *testing.T) {
	t.Parallel()

	assets := templates.AssetPaths{
		BasePath:    "/stillwater",
		CSS:         "/stillwater/static/css/styles.css?v=abc123",
		HTMX:        "/stillwater/static/js/htmx.min.js",
		SSEJS:       "/stillwater/static/js/sse.js",
		SidebarJS:   "/stillwater/static/js/sidebar.js",
		LoginBG:     "/stillwater/static/img/login-bg.jpg",
		IsAdmin:     true,
		CurrentPath: "/dashboard",
		Username:    "testuser",
		Role:        "administrator",
	}

	var buf bytes.Buffer
	if err := next.LayoutNext("Next Dashboard", assets).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
		t.Fatalf("rendering LayoutNext: %v", err)
	}
	out := buf.String()

	markers := map[string]string{
		"ProgressPill status bar":  `id="sw-progress-pill-stack"`,
		"base-path injection meta": `name="htmx-base-path"`,
		"app-version meta":         `name="app-version"`,
		"toast container":          `id="error-toast-container"`,
		"main content landmark":    `id="sw-main-content"`,
		"SSE client script":        "/stillwater/static/js/sse.js",
		"cache-busted CSS":         "/stillwater/static/css/styles.css?v=abc123",
		"page title":               "<title>Next Dashboard - Stillwater</title>",
	}
	for name, want := range markers {
		if !strings.Contains(out, want) {
			t.Errorf("LayoutNext output missing %s (substring %q)", name, want)
		}
	}

	// The next/ sidebar delegates to the stable nav, so the sidebar's primary
	// navigation landmark must be present.
	if !strings.Contains(out, "<nav") {
		t.Error("LayoutNext output missing sidebar <nav> landmark")
	}
}
