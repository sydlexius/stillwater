package templates

import "testing"

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
