package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderLayoutFor(t *testing.T, isAdmin bool) string {
	t.Helper()
	var buf bytes.Buffer
	child := templ.Raw("<p>page</p>")
	ctx := templ.WithChildren(testCtx(t), child)
	if err := Layout("t", AssetPaths{IsAdmin: isAdmin}).Render(ctx, &buf); err != nil {
		t.Fatalf("render layout: %v", err)
	}
	return buf.String()
}

// The mount is admin-only on the template side; the endpoint is admin-gated
// independently. A non-admin page must not carry the banner element or script.
func TestLayout_RegistryRepairBannerIsAdminOnly(t *testing.T) {
	if html := renderLayoutFor(t, true); !strings.Contains(html, `id="sw-registry-repair-banner"`) {
		t.Error("admin layout is missing the registry repair banner mount")
	}
	html := renderLayoutFor(t, false)
	if strings.Contains(html, "sw-registry-repair-banner") || strings.Contains(html, "registry-repair/banner") {
		t.Error("non-admin layout carries the registry repair banner")
	}
}

// Slice 1 is read-only: the banner starts hidden, links to the docs, offers a
// dismissal, and renders NO run/repair control (slice 2 owns that).
func TestRegistryRepairBanner_ReadOnlyAndHiddenByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := RegistryRepairBanner().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`class="hidden `, `role="status"`, `how-to/repair-image-registry/#background-check`,
		`id="sw-registry-repair-dismiss"`, `data-count-other="{count} registry rows`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("banner markup missing %q", want)
		}
	}
	if n := strings.Count(html, "<button"); n != 1 {
		t.Errorf("banner renders %d buttons, want only the dismiss button", n)
	}
	if strings.Contains(html, "remediate") {
		t.Error("slice 1 banner must not reference the remediate endpoint")
	}
}
