package next

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// renderArtworkSection renders ArtworkSection for the given detail data and
// returns the HTML, failing the test on a render error.
func renderArtworkSection(t *testing.T, data *templates.ArtistDetailData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ArtworkSection(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render artwork section: %v", err)
	}
	return buf.String()
}

// TestArtworkSection_SectionChrome verifies the section renders as a
// keyboard-navigable next/ card with the artwork section markers and the single
// "Manage artwork" trigger (default kind = primary).
func TestArtworkSection_SectionChrome(t *testing.T) {
	t.Parallel()
	data := &templates.ArtistDetailData{Artist: artist.Artist{ID: "art-1", Name: "Chrome Test"}}
	out := renderArtworkSection(t, data)

	for label, want := range map[string]string{
		"section card":    "sw-dash-card",
		"section id":      `id="next-artwork-art-1"`,
		"section nav key": `data-sw-section="artwork"`,
		"heading":         `id="next-artwork-heading"`,
		"manage trigger":  `data-sw-artwork-open`,
		"default kind":    `data-artwork-kind="primary"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("section missing %s (%q)", label, want)
		}
	}
}

// TestArtworkSection_IdentityTiles verifies aspect-true tiles for the three
// single-slot kinds: a present image opens the lightbox; a missing image shows
// an add-state that opens the modal (never a link to the retired image pages).
func TestArtworkSection_IdentityTiles(t *testing.T) {
	t.Parallel()

	// All three single-slot kinds present so the logo checker + per-tile
	// affordances render.
	data := &templates.ArtistDetailData{Artist: artist.Artist{
		ID:           "art-1",
		Name:         "Tiles",
		ThumbExists:  true,
		ThumbWidth:   600,
		ThumbHeight:  600,
		BannerExists: true,
		LogoExists:   true,
	}}
	out := renderArtworkSection(t, data)

	for label, want := range map[string]string{
		// Present images render at native aspect, height-normalized (no fixed box).
		"native-aspect image":   "sw-artwork-native-img",
		"logo checker bg":       "sw-artwork-checker",
		"present-tile lightbox": "swLightbox.open(this.dataset.lightboxSrc",
		"primary file url":      "/api/v1/artists/art-1/images/thumb/file",
		"banner file url":       "/api/v1/artists/art-1/images/banner/file",
		"primary dimensions":    "600x600",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("identity tiles missing %s (%q)", label, want)
		}
	}
	// Present tiles must NOT impose a fixed aspect box (that would crop/squash).
	if strings.Contains(out, "aspect-square") {
		t.Error("present identity image should not be forced into a fixed aspect box")
	}

	// A missing kind must render an add-state that opens the modal (not a link to
	// the retired image page).
	missing := &templates.ArtistDetailData{Artist: artist.Artist{ID: "art-1", Name: "Tiles", LogoExists: false}}
	missingOut := renderArtworkSection(t, missing)
	if !strings.Contains(missingOut, `data-artwork-kind="logo"`) {
		t.Error("missing-logo tile should open the modal pre-selected to logo")
	}
	// The retired page is /artists/{id}/images (anchor href, with an optional
	// ?type= query). The live API URLs are /api/v1/artists/{id}/images/{type}/...
	// so guard against the retired-page markers specifically, not the API paths.
	for _, retired := range []string{"images?type=", `/artists/art-1/images"`} {
		if strings.Contains(out, retired) || strings.Contains(missingOut, retired) {
			t.Errorf("artwork section must never link to the retired image page (found %q)", retired)
		}
	}
}

// TestArtworkSection_BackdropsCarousel verifies the carousel renders the reused
// fanart tile grid when slots exist (without the stable channel's sync-state
// badges), and shows an add-state opening the Backdrops kind when there are none.
func TestArtworkSection_BackdropsCarousel(t *testing.T) {
	t.Parallel()

	withFanart := &templates.ArtistDetailData{Artist: artist.Artist{ID: "art-1", Name: "BD", FanartCount: 3}}
	out := renderArtworkSection(t, withFanart)
	for label, want := range map[string]string{
		"tile grid":           "sw-artwork-bd-grid",
		"fanart-manage hook":  "data-sw-fanart-gallery",
		"slot 0 file":         "/api/v1/artists/art-1/images/fanart/0/file",
		"slot 2 file":         "/api/v1/artists/art-1/images/fanart/2/file",
		"set-primary star":    `data-set-primary-index="1"`,
		"inline add-tile":     "sw-artwork-bd-add",
		"add opens backdrops": `data-artwork-kind="backdrops"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("backdrops grid missing %s (%q)", label, want)
		}
	}
	// The next/ surface intentionally omits the stable channel's per-slot
	// fanart sync-state badges (maintainer: not part of the next/ UX), so the
	// carousel must NOT lazy-load /fanart-sync-state nor emit badge placeholders.
	for label, unwanted := range map[string]string{
		"sync-state lazy load":   "/api/v1/artists/art-1/fanart-sync-state",
		"sync-badge placeholder": "data-sync-badge",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("backdrops grid should not contain %s (%q)", label, unwanted)
		}
	}
	// The [+] add-tile must come AFTER the last backdrop tile (inline last cell).
	if lastBackdropIdx, addTileIdx := strings.LastIndex(out, "/images/fanart/2/file"), strings.Index(out, "sw-artwork-bd-add"); lastBackdropIdx < 0 || addTileIdx < 0 || addTileIdx < lastBackdropIdx {
		t.Errorf("the [+] add-tile must render immediately after the last backdrop (idx add=%d last=%d)", addTileIdx, lastBackdropIdx)
	}

	noFanart := &templates.ArtistDetailData{Artist: artist.Artist{ID: "art-1", Name: "BD", FanartCount: 0}}
	emptyOut := renderArtworkSection(t, noFanart)
	if !strings.Contains(emptyOut, `data-artwork-kind="backdrops"`) {
		t.Error("empty backdrops state should open the modal's Backdrops kind")
	}
}

// TestArtworkSection_ReconciliationStatus verifies the per-connection
// reconciliation line: managed vs mirror vs out-of-folder note, computed from
// existing data (no content compare).
func TestArtworkSection_ReconciliationStatus(t *testing.T) {
	t.Parallel()

	data := &templates.ArtistDetailData{
		Artist: artist.Artist{ID: "art-1", Name: "Recon"},
		Connections: []templates.ArtistDetailConnection{
			{ID: "c1", Name: "Managed Emby", Type: "emby", Managed: true},
			{ID: "c2", Name: "Unmanaged Jellyfin", Type: "jellyfin", Managed: false},
			{ID: "c3", Name: "Lidarr", Type: "lidarr", Managed: false},
		},
	}
	out := renderArtworkSection(t, data)

	for label, want := range map[string]string{
		"recon heading":      "Reconciliation status",
		"managed name":       "Managed Emby",
		"managed status":     "Managed by Stillwater",
		"out-of-folder note": "may also keep its own internal copy",
		"plain mirror":       "Mirror of the shared library folder",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reconciliation status missing %s (%q)", label, want)
		}
	}
}

// TestArtworkSection_ReconciliationLocalOnly verifies that with no connections
// the section states the local folder is the only source of truth.
func TestArtworkSection_ReconciliationLocalOnly(t *testing.T) {
	t.Parallel()
	data := &templates.ArtistDetailData{Artist: artist.Artist{ID: "art-1", Name: "Local"}}
	out := renderArtworkSection(t, data)
	if !strings.Contains(out, "only source of truth") {
		t.Error("with no connections the section should state local-only source of truth")
	}
}

// TestArtistDetailPage_RendersLightboxOverlay is a regression guard: the
// Artwork tiles + carousel call window.swLightbox.open, which needs the
// #sw-lightbox overlay DOM present on the page. It lives only in the stable
// Images tab, so the next/ page must render the shared LightboxOverlay itself
// (else the full-size view dead-ends).
func TestArtistDetailPage_RendersLightboxOverlay(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, detailPageData(nil, nil)).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render page: %v", err)
	}
	if !strings.Contains(buf.String(), `id="sw-lightbox"`) {
		t.Error("next/ artist-detail page must render the #sw-lightbox overlay for the Artwork tiles")
	}
}

// TestArtworkSection_MaintainerFeedback covers the 2026-06-05 mop-up items:
// no "Identity" subhead, mono numeric dims, a low-res severity dot, a single
// hero-styled Manage button (no per-tile Manage buttons), and a persistent
// [+] add in a populated Backdrops carousel.
func TestArtworkSection_MaintainerFeedback(t *testing.T) {
	t.Parallel()
	data := &templates.ArtistDetailData{Artist: artist.Artist{
		ID: "art-1", Name: "FB",
		ThumbExists: true, ThumbWidth: 600, ThumbHeight: 600, ThumbLowRes: true,
		FanartCount: 3,
	}}
	out := renderArtworkSection(t, data)

	// #15: no "Identity" subhead.
	if strings.Contains(out, "Identity") {
		t.Error("the Identity subhead should be removed")
	}
	// #13: numeric dims use a mono class.
	if !strings.Contains(out, `class="font-mono text-xs`) {
		t.Error("numeric dims should use the font-mono class")
	}
	// #16: a low-res image renders a severity dot + tooltip.
	if !strings.Contains(out, "Low resolution") {
		t.Error("a low-res image should render the low-res finding dot/tooltip")
	}
	// #12: exactly one Manage button (header), styled like the hero buttons; no
	// per-tile "Manage" buttons.
	if got := strings.Count(out, "Manage artwork"); got != 1 {
		t.Errorf("want a single Manage artwork button, got %d", got)
	}
	if !strings.Contains(out, "var(--swd-line)") {
		t.Error("the Manage button should use the hero toolbar styling (--swd-line border)")
	}
	// #14: a populated carousel still exposes a persistent [+] add.
	if !strings.Contains(out, `data-artwork-kind="backdrops"`) {
		t.Error("populated Backdrops carousel should still expose a [+] add (data-artwork-kind=backdrops)")
	}
}

// renderArtworkModal renders ArtworkModal and returns the HTML.
func renderArtworkModal(t *testing.T, data *templates.ArtistDetailData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ArtworkModal(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render artwork modal: %v", err)
	}
	return buf.String()
}

// TestArtworkModal_ShellAndKindSwitcher verifies the modal shell: the dialog,
// the four-kind switcher, the close control, the lazy body container, the gate
// banner, the revert affordance, and the reconciliation panel.
func TestArtworkModal_ShellAndKindSwitcher(t *testing.T) {
	t.Parallel()
	data := &templates.ArtistDetailData{Artist: artist.Artist{ID: "art-1", Name: "Modal"}}
	out := renderArtworkModal(t, data)

	for label, want := range map[string]string{
		"modal dialog":       `id="artwork-modal"`,
		"aria modal":         `aria-modal="true"`,
		"close control":      "data-sw-artwork-close",
		"lazy body":          `id="artwork-modal-body"`,
		"gate banner":        `id="artwork-gate-banner"`,
		"revert affordance":  `id="artwork-revert-btn"`,
		"revert gated":       "data-sw-requires-image-write",
		"reconciliation":     "Reconciliation status",
		"legibility surface": "sw-artwork-modal-surface",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modal shell missing %s (%q)", label, want)
		}
	}

	// All four kind tabs present.
	for _, kind := range []string{"primary", "logo", "banner", "backdrops"} {
		if !strings.Contains(out, `data-artwork-kind="`+kind+`"`) {
			t.Errorf("kind switcher missing the %q tab", kind)
		}
	}
}

// TestArtworkSeverityDotClass pins the severity->dot-color mapping. Only the
// "warning" arm is reachable from the templ today (low-res is the sole finding),
// so this locks "error" and the default before richer findings wire them up: a
// regression that mapped "error" to the blue default would otherwise slip by.
func TestArtworkSeverityDotClass(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"error":   "bg-red-500",
		"warning": "bg-amber-500",
		"info":    "bg-blue-500", // unknown severity falls to the default
		"":        "bg-blue-500",
	}
	for severity, want := range cases {
		if got := artworkSeverityDotClass(severity); got != want {
			t.Errorf("artworkSeverityDotClass(%q) = %q, want %q", severity, got, want)
		}
	}
}
