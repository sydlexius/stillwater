package templates

import (
	"bytes"
	"strings"
	"testing"
)

func TestInboundWebhookURL(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		scheme   string
		basePath string
		platform string
		want     string
	}{
		{
			"plain HTTP no base path",
			"localhost:1973", "http", "", "lidarr",
			"http://localhost:1973/api/v1/webhooks/inbound/lidarr?apikey=YOUR_TOKEN",
		},
		{
			"HTTPS with base path",
			"sw.example.com", "https", "/app",
			"emby",
			"https://sw.example.com/app/api/v1/webhooks/inbound/emby?apikey=YOUR_TOKEN",
		},
		{
			"fallback placeholder",
			"", "http", "", "jellyfin",
			"http://YOUR_HOST:1973/api/v1/webhooks/inbound/jellyfin?apikey=YOUR_TOKEN",
		},
		{
			"root base path treated as empty",
			"myhost:1973", "http", "/", "lidarr",
			"http://myhost:1973/api/v1/webhooks/inbound/lidarr?apikey=YOUR_TOKEN",
		},
		{
			"empty scheme defaults to http",
			"myhost:1973", "", "", "emby",
			"http://myhost:1973/api/v1/webhooks/inbound/emby?apikey=YOUR_TOKEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := SettingsData{
				Host:     tt.host,
				Scheme:   tt.scheme,
				BasePath: tt.basePath,
			}
			got := d.inboundWebhookURL(tt.platform)
			if got != tt.want {
				t.Errorf("inboundWebhookURL(%q) = %q, want %q", tt.platform, got, tt.want)
			}
		})
	}
}

// TestMetadataLanguagesJSON_PreservesEmpty locks in the invariant the
// Providers tab JS depends on: when the user has no stored preference (or
// has explicitly cleared it via the Clear UI), the template renders
// `data-languages="[]"` rather than coercing back to the default `["en"]`.
// Coercion would silently re-render an English pill after a reset and
// contradict what the user just did. See issue #1138.
func TestMetadataLanguagesJSON_PreservesEmpty(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil slice preserves empty", nil, `[]`},
		{"empty slice preserves empty", []string{}, `[]`},
		{"single language serializes normally", []string{"en"}, `["en"]`},
		{"multiple languages preserve order", []string{"en-US", "en-GB", "en"}, `["en-US","en-GB","en"]`},
		{"non-Latin tag marshals unchanged", []string{"ja"}, `["ja"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := SettingsData{MetadataLanguages: tt.input}
			got := d.metadataLanguagesJSON()
			if got != tt.want {
				t.Errorf("metadataLanguagesJSON() with %v = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestSettingsUpdatesTab_RestartRequiredVisible asserts that when the
// server has marked the binary apply complete, the Updates tab renders
// the persistent "restart to finish" banner and the Apply button is
// disabled. This is the post-Apply UI contract for issue #1169: without
// these markers the user has no signal that Apply succeeded and that a
// restart is the only remaining step.
func TestSettingsUpdatesTab_RestartRequiredVisible(t *testing.T) {
	data := UpdatesTabData{
		CurrentVersion:  "v0.9.0",
		Channel:         "stable",
		LatestVersion:   "v0.9.5",
		UpdateAvailable: true,
		RestartRequired: true,
		PendingVersion:  "v0.9.5",
	}

	var buf bytes.Buffer
	if err := settingsUpdatesTab(data, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// The restart-required banner DOM is always present (so the JS can
	// later flip its visibility), but when RestartRequired is true the
	// `hidden` class must NOT be applied. Asserting the absence of
	// `class="...hidden..."` on the banner element is the most direct
	// way to lock the visibility behavior.
	bannerIdx := strings.Index(html, `id="updates-restart-required-row"`)
	if bannerIdx == -1 {
		t.Fatal("missing restart-required banner element in rendered HTML")
	}
	// Look at the surrounding tag to verify the hidden class is not on it.
	openTagStart := strings.LastIndex(html[:bannerIdx], "<")
	openTagEnd := strings.Index(html[bannerIdx:], ">") + bannerIdx
	if openTagStart < 0 || openTagEnd <= openTagStart {
		t.Fatal("malformed banner markup; could not find surrounding tag")
	}
	bannerTag := html[openTagStart : openTagEnd+1]
	if strings.Contains(bannerTag, "hidden") {
		t.Errorf("banner is hidden when RestartRequired is true; tag = %q", bannerTag)
	}

	// The pending version tag must appear so the user knows which release
	// will load on restart.
	if !strings.Contains(html, "v0.9.5") {
		t.Error("pending version v0.9.5 missing from rendered banner")
	}

	// The Apply button must be disabled even though UpdateAvailable=true,
	// because applying again would overwrite the staged binary.
	applyIdx := strings.Index(html, `id="updates-apply-btn"`)
	if applyIdx == -1 {
		t.Fatal("missing Apply button in rendered HTML")
	}
	applyTagEnd := strings.Index(html[applyIdx:], ">") + applyIdx
	applyTag := html[applyIdx : applyTagEnd+1]
	if !strings.Contains(applyTag, "disabled") {
		t.Errorf("Apply button not disabled when RestartRequired is true; tag = %q", applyTag)
	}
}

// TestSettingsUpdatesTab_RestartRequiredHidden asserts the banner stays
// hidden in the normal pre-Apply state. This is the negative complement
// to the visible test above: a regression that always-renders the banner
// would silently confuse users into restarting before any apply ran.
func TestSettingsUpdatesTab_RestartRequiredHidden(t *testing.T) {
	data := UpdatesTabData{
		CurrentVersion: "v0.9.0",
		Channel:        "stable",
	}

	var buf bytes.Buffer
	if err := settingsUpdatesTab(data, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	bannerIdx := strings.Index(html, `id="updates-restart-required-row"`)
	if bannerIdx == -1 {
		t.Fatal("missing restart-required banner element in rendered HTML")
	}
	openTagStart := strings.LastIndex(html[:bannerIdx], "<")
	openTagEnd := strings.Index(html[bannerIdx:], ">") + bannerIdx
	bannerTag := html[openTagStart : openTagEnd+1]
	if !strings.Contains(bannerTag, "hidden") {
		t.Errorf("banner not hidden in default state; tag = %q", bannerTag)
	}
}
