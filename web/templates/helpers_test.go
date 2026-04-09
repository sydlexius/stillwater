package templates

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/provider"
)

// testCtx returns a context with the embedded English translator loaded,
// so i18n lookups in helper functions return real translations during tests.
func testCtx(tb testing.TB) context.Context {
	tb.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		tb.Fatalf("loading i18n bundle: %v", err)
	}
	return i18n.WithTranslator(context.Background(), bundle.Translator("en"))
}

func TestLogoSrc_BasePath(t *testing.T) {
	tests := []struct {
		name string
		bp   string
		key  string
		want string
	}{
		{"root base path, svg logo", "", "discogs", "/static/img/logos/discogs.svg"},
		{"sub-path, svg logo", "/stillwater", "discogs", "/stillwater/static/img/logos/discogs.svg"},
		{"nested sub-path, svg logo", "/foo/bar", "musicbrainz", "/foo/bar/static/img/logos/musicbrainz.svg"},
		{"root base path, png logo", "", "emby", "/static/img/logos/emby-128.png"},
		{"sub-path, png logo", "/stillwater", "audiodb", "/stillwater/static/img/logos/audiodb-128.png"},
		{"root base path, custom logo", "", "custom", "/static/img/favicon.svg"},
		{"sub-path, custom logo", "/app", "custom", "/app/static/img/favicon.svg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetBasePath(tt.bp)
			t.Cleanup(func() { SetBasePath("") })
			got := logoSrc(tt.key)
			if got != tt.want {
				t.Errorf("logoSrc(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestLogoSrcSet_BasePath(t *testing.T) {
	tests := []struct {
		name string
		bp   string
		key  string
		want string
	}{
		{"svg logo returns empty", "", "discogs", ""},
		{"root base path, audiodb", "", "audiodb", "/static/img/logos/audiodb-32.png 1x, /static/img/logos/audiodb-64.png 2x, /static/img/logos/audiodb-128.png 4x"},
		{"sub-path, emby", "/stillwater", "emby", "/stillwater/static/img/logos/emby-32.png 1x, /stillwater/static/img/logos/emby-64.png 2x, /stillwater/static/img/logos/emby-128.png 4x"},
		{"nested sub-path, audiodb", "/foo/bar", "audiodb", "/foo/bar/static/img/logos/audiodb-32.png 1x, /foo/bar/static/img/logos/audiodb-64.png 2x, /foo/bar/static/img/logos/audiodb-128.png 4x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetBasePath(tt.bp)
			t.Cleanup(func() { SetBasePath("") })
			got := logoSrcSet(tt.key)
			if got != tt.want {
				t.Errorf("logoSrcSet(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMirrorServerType(t *testing.T) {
	tests := []struct {
		name   string
		mirror *provider.MirrorConfig
		want   string
	}{
		{"nil mirror is official", nil, "official"},
		{"beta URL is beta", &provider.MirrorConfig{BaseURL: betaMirrorURL, RateLimit: 1}, "beta"},
		{"custom URL is custom", &provider.MirrorConfig{BaseURL: "http://192.168.1.126:5000/ws/2", RateLimit: 10}, "custom"},
		{"official URL in config is official", &provider.MirrorConfig{BaseURL: "https://musicbrainz.org/ws/2", RateLimit: 1}, "official"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mirrorServerType(tt.mirror)
			if got != tt.want {
				t.Errorf("mirrorServerType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMirrorStatusLabel(t *testing.T) {
	ctx := testCtx(t)
	tests := []struct {
		name   string
		mirror *provider.MirrorConfig
		want   string
	}{
		{"nil mirror has no label", nil, ""},
		{"official URL has no label", &provider.MirrorConfig{BaseURL: "https://musicbrainz.org/ws/2", RateLimit: 1}, ""},
		{"beta shows label", &provider.MirrorConfig{BaseURL: betaMirrorURL, RateLimit: 1}, "Beta server"},
		{"custom shows label", &provider.MirrorConfig{BaseURL: "http://10.0.0.1:5000/ws/2", RateLimit: 10}, "Custom mirror"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mirrorStatusLabel(ctx, tt.mirror)
			if got != tt.want {
				t.Errorf("mirrorStatusLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
