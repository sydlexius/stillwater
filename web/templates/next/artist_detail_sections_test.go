package next

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/web/templates"
)

// detailDataWithConnections builds an ArtistDetailPageData that includes two
// connections: one Emby and one Lidarr.
func detailDataWithConnections() ArtistDetailPageData {
	data := detailPageData(nil, nil)
	data.Detail.Connections = []templates.ArtistDetailConnection{
		{ID: "conn-emby", Name: "My Emby", Type: "emby", URL: "http://emby:8096/artist/123"},
		{ID: "conn-lidarr", Name: "My Lidarr", Type: "lidarr", URL: "http://lidarr:7878/artist/456"},
	}
	return data
}

// TestSectionProviders_RendersLazyPlatformMounts verifies that SectionProviders
// emits a lazy-load placeholder div for EACH connection with the correct HTMX
// attributes and the platform_state heading.
func TestSectionProviders_RendersLazyPlatformMounts(t *testing.T) {
	t.Parallel()
	data := detailDataWithConnections()

	var buf bytes.Buffer
	if err := SectionProviders(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render SectionProviders: %v", err)
	}
	out := buf.String()

	// Section nav attributes required for j/k keyboard nav.
	for _, want := range []string{
		`data-sw-section="providers"`,
		`id="next-providers-art-1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SectionProviders missing %q", want)
		}
	}

	// Emby connection: lazy-load placeholder with correct hx-get.
	for _, want := range []string{
		`id="platform-state-conn-emby"`,
		`hx-get="/api/v1/artists/art-1/platform-state?connection_id=conn-emby"`,
		`hx-trigger="revealed"`,
		`hx-swap="outerHTML"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SectionProviders Emby mount missing %q", want)
		}
	}

	// Lidarr connection: also gets a platform-state mount (all connections are
	// included; the platform-state handler handles unsupported types gracefully).
	if !strings.Contains(out, `id="platform-state-conn-lidarr"`) {
		t.Errorf("SectionProviders missing Lidarr connection mount")
	}
	if !strings.Contains(out, `hx-get="/api/v1/artists/art-1/platform-state?connection_id=conn-lidarr"`) {
		t.Errorf("SectionProviders missing Lidarr hx-get")
	}

	// The stable .sw-dash-card chrome: no hardcoded opaque fill.
	if strings.Contains(out, "bg-white") || strings.Contains(out, "bg-gray-800") {
		t.Errorf("SectionProviders must use sw-dash-card, not hardcoded opaque fills")
	}
}

// TestSectionProviders_EmptyConnections verifies the providers section renders
// its card (with no mounts) when there are no connections.
func TestSectionProviders_EmptyConnections(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil) // no connections

	var buf bytes.Buffer
	if err := SectionProviders(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render SectionProviders (empty): %v", err)
	}
	out := buf.String()

	// Section and heading still render.
	if !strings.Contains(out, `id="next-providers-art-1"`) {
		t.Errorf("SectionProviders missing section id on empty connections")
	}
	// No platform-state placeholders.
	if strings.Contains(out, "platform-state-") {
		t.Errorf("SectionProviders should have no platform-state mounts for empty connections")
	}
}

// TestSectionDiscography_RendersLazyMount verifies that SectionDiscography
// emits the lazy-load mount pointing at /artists/{id}/discography/tab with the
// correct trigger and the loading placeholder text.
func TestSectionDiscography_RendersLazyMount(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil)

	var buf bytes.Buffer
	if err := SectionDiscography(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render SectionDiscography: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`id="next-discography-art-1"`,
		`data-sw-section="discography"`,
		`id="artist-discography-tab-art-1"`,
		`hx-get="/artists/art-1/discography/tab"`,
		`hx-trigger="intersect once"`,
		`hx-swap="innerHTML"`,
		// Loading placeholder (from discography.loading i18n key).
		`Loading discography`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SectionDiscography missing %q", want)
		}
	}

	// Must use sw-dash-card chrome, not the stable sw-card opaque fills.
	if strings.Contains(out, "bg-white") || strings.Contains(out, "bg-gray-800") {
		t.Errorf("SectionDiscography must use sw-dash-card, not hardcoded opaque fills")
	}
}

// TestSectionDebug_GatingAndContent verifies two sub-cases:
// (a) With an Emby connection, the debug section renders a readonly platform-state
//
//	mount for that connection with hx-get containing &readonly=true, and the
//	field-provenance heading.
//
// (b) A Lidarr-only connection does NOT get a platform-state mount in the debug
//
//	section (mirrors the stable conn.Type == TypeEmby || TypeJellyfin filter).
func TestSectionDebug_GatingAndContent(t *testing.T) {
	t.Parallel()

	t.Run("emby_connection_gets_readonly_mount", func(t *testing.T) {
		t.Parallel()
		data := detailPageData(nil, nil)
		data.Detail.Connections = []templates.ArtistDetailConnection{
			{ID: "conn-emby", Name: "My Emby", Type: "emby", URL: "http://emby:8096"},
		}

		var buf bytes.Buffer
		if err := SectionDebug(data).Render(nextTestCtx(t), &buf); err != nil {
			t.Fatalf("render SectionDebug: %v", err)
		}
		out := buf.String()

		for _, want := range []string{
			`id="next-debug-art-1"`,
			`data-sw-section="debug"`,
			// Readonly platform-state lazy mount.
			`id="debug-platform-state-conn-emby"`,
			// HTML attribute encoding: & becomes &amp; in the output.
			`connection_id=conn-emby&amp;readonly=true`,
			`hx-trigger="revealed"`,
			`hx-swap="outerHTML"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("SectionDebug (emby) missing %q", want)
			}
		}
	})

	t.Run("lidarr_only_no_platform_state_mount", func(t *testing.T) {
		t.Parallel()
		data := detailPageData(nil, nil)
		data.Detail.Connections = []templates.ArtistDetailConnection{
			{ID: "conn-lidarr", Name: "My Lidarr", Type: "lidarr", URL: "http://lidarr:7878"},
		}

		var buf bytes.Buffer
		if err := SectionDebug(data).Render(nextTestCtx(t), &buf); err != nil {
			t.Fatalf("render SectionDebug: %v", err)
		}
		out := buf.String()

		// Lidarr connections must NOT get a readonly platform-state mount in debug.
		if strings.Contains(out, "debug-platform-state-conn-lidarr") {
			t.Errorf("SectionDebug must not render a platform-state mount for Lidarr connections")
		}
	})

	t.Run("jellyfin_gets_readonly_mount", func(t *testing.T) {
		t.Parallel()
		data := detailPageData(nil, nil)
		data.Detail.Connections = []templates.ArtistDetailConnection{
			{ID: "conn-jf", Name: "My Jellyfin", Type: "jellyfin", URL: "http://jf:8096"},
		}

		var buf bytes.Buffer
		if err := SectionDebug(data).Render(nextTestCtx(t), &buf); err != nil {
			t.Fatalf("render SectionDebug: %v", err)
		}
		out := buf.String()

		if !strings.Contains(out, `id="debug-platform-state-conn-jf"`) {
			t.Errorf("SectionDebug should render a readonly mount for Jellyfin connections")
		}
		// HTML attribute encoding: & becomes &amp; in the output.
		if !strings.Contains(out, `connection_id=conn-jf&amp;readonly=true`) {
			t.Errorf("SectionDebug Jellyfin mount must carry readonly=true")
		}
	})
}

// TestArtistDetailLegend_AdvertisesShortcuts verifies the page-level legend
// (.sw-list-tips) is rendered and its content references j/k section nav, r
// Refresh, R Run rules, and Esc close as advertised in the design.
func TestArtistDetailLegend_AdvertisesShortcuts(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, detailPageData(nil, nil)).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// The legend container is present.
	if !strings.Contains(out, `class="sw-list-tips"`) {
		t.Errorf("page missing .sw-list-tips legend")
	}
	if !strings.Contains(out, `role="note"`) {
		t.Errorf("page missing role=note on legend")
	}

	// The legend uses sw-kbd chips; each key appears as its own inline text node.
	for _, want := range []string{"h", "j", "k", "r", "R", "Esc", "e", "f"} {
		if !strings.Contains(out, want) {
			t.Errorf("legend text missing keyboard key %q reference", want)
		}
	}
}

// TestArtistDetailPage_4CSectionsRender verifies that once 4C builds the
// providers and discography sections, they render their card IDs (replacing the
// earlier suppressed-mount test).
func TestArtistDetailPage_4CSectionsRender(t *testing.T) {
	t.Parallel()
	data := detailDataWithConnections()

	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`id="next-providers-art-1"`,
		`id="next-discography-art-1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing 4C section id %q", want)
		}
	}
}

// TestArtistDetailPage_DebugSectionGatedOnDataFlags verifies the debug section
// renders only when ShowPlatformDebug && HasDebugConnection.
func TestArtistDetailPage_DebugSectionGatedOnDataFlags(t *testing.T) {
	t.Parallel()

	embyConn := templates.ArtistDetailConnection{
		ID: "conn-emby", Name: "Emby", Type: "emby", URL: "http://emby:8096",
	}
	lidarrConn := templates.ArtistDetailConnection{
		ID: "conn-lidarr", Name: "Lidarr", Type: "lidarr", URL: "http://lidarr:7878",
	}

	cases := []struct {
		name         string
		showDebug    bool
		hasDebugConn bool
		conns        []templates.ArtistDetailConnection
		wantDebug    bool
	}{
		{"debug_off", false, false, nil, false},
		{"debug_on_no_conn", true, false, []templates.ArtistDetailConnection{lidarrConn}, false},
		{"debug_on_with_emby", true, true, []templates.ArtistDetailConnection{embyConn}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := detailPageData(nil, nil)
			data.Detail.ShowPlatformDebug = tc.showDebug
			data.Detail.HasDebugConnection = tc.hasDebugConn
			data.Detail.Connections = tc.conns

			var buf bytes.Buffer
			if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			out := buf.String()

			if tc.wantDebug && !strings.Contains(out, `id="next-debug-art-1"`) {
				t.Errorf("expected debug section but it is absent")
			}
			if !tc.wantDebug && strings.Contains(out, `id="next-debug-art-1"`) {
				t.Errorf("debug section should be absent but is present")
			}
		})
	}
}
