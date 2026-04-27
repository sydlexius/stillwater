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
		{"trailing slash normalized", "/stillwater/", "discogs", "/stillwater/static/img/logos/discogs.svg"},
		{"root slash normalized to empty", "/", "discogs", "/static/img/logos/discogs.svg"},
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
		{"trailing slash normalized, emby", "/stillwater/", "emby", "/stillwater/static/img/logos/emby-32.png 1x, /stillwater/static/img/logos/emby-64.png 2x, /stillwater/static/img/logos/emby-128.png 4x"},
		{"root slash normalized to empty", "/", "audiodb", "/static/img/logos/audiodb-32.png 1x, /static/img/logos/audiodb-64.png 2x, /static/img/logos/audiodb-128.png 4x"},
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

// TestManageServerFilesPayload pins the JSON the settings-page uses for
// the hx-vals toggle so escaping drift does not silently break it. The
// payload is produced by json.Marshal via hxValsJSONAny, so the compact
// form without whitespace around the colon is what HTMX actually sees.
func TestManageServerFilesPayload(t *testing.T) {
	for _, enable := range []bool{true, false} {
		got := manageServerFilesPayload(enable)
		wantSubstr := `"enabled":true`
		if !enable {
			wantSubstr = `"enabled":false`
		}
		if len(got) == 0 || got[0] != '{' || got[len(got)-1] != '}' {
			t.Errorf("payload not JSON-shaped: %q", got)
		}
		if !contains(got, wantSubstr) {
			t.Errorf("enable=%v payload=%q want substring %q", enable, got, wantSubstr)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestWarnTitle(t *testing.T) {
	cases := map[string]string{
		"image": "Image file writes paused",
		"nfo":   "NFO file writes paused",
		"both":  "Image and NFO file writes paused",
		"":      "Write-back conflict",
	}
	for axis, want := range cases {
		if got := warnTitle(axis); got != want {
			t.Errorf("warnTitle(%q) = %q, want %q", axis, got, want)
		}
	}
}

func TestWarnSubtitle_UsesConnectionName(t *testing.T) {
	v := ConflictBannerView{
		Connections: []ConflictBannerConn{{Name: "Emby UAT", LibraryName: "Music"}},
	}
	got := warnSubtitle("image", v)
	if got == "" || !contains(got, "Emby UAT") {
		t.Errorf("subtitle should mention Emby UAT: %q", got)
	}
	if !contains(got, `"Music"`) {
		t.Errorf("subtitle should mention library Music: %q", got)
	}
}

func TestWarnSubtitle_FallsBackWhenNoConnections(t *testing.T) {
	got := warnSubtitle("nfo", ConflictBannerView{})
	if got == "" {
		t.Error("empty fallback")
	}
}

// TestWarnSubtitle_FallsBackWhenConnectionIdentityBlank pins the "A connected
// server" fallback branch: the ledger may carry a connection with neither
// Name nor Type populated (e.g. a peer whose identity probe failed before
// the conflict was detected). Without this test the branch could silently
// regress to the " is saving artwork..." rendering that started the
// warnSubtitle fix in round 1.
func TestWarnSubtitle_FallsBackWhenConnectionIdentityBlank(t *testing.T) {
	v := ConflictBannerView{
		Connections: []ConflictBannerConn{{Name: "", Type: "", LibraryName: "Music"}},
	}
	got := warnSubtitle("image", v)
	if !contains(got, "A connected server") {
		t.Errorf("expected generic actor fallback, got %q", got)
	}
}

func TestWarnAffected_PerAxis(t *testing.T) {
	if warnAffected("image") == "" || warnAffected("nfo") == "" || warnAffected("both") == "" {
		t.Error("affected text should be populated for all axes")
	}
	if warnAffected("other") != "" {
		t.Error("unknown axis should return empty")
	}
}

func TestConflictGates(t *testing.T) {
	cases := []struct {
		state     string
		wantImage bool
		wantNFO   bool
	}{
		{"clean", false, false},
		{"image_only", true, false},
		{"nfo_only", false, true},
		{"both", true, true},
		{"round_trip", true, true},
	}
	for _, c := range cases {
		v := ConflictBannerView{State: c.state}
		if got := conflictImageGated(v); got != c.wantImage {
			t.Errorf("conflictImageGated(%q) = %v, want %v", c.state, got, c.wantImage)
		}
		if got := conflictNFOGated(v); got != c.wantNFO {
			t.Errorf("conflictNFOGated(%q) = %v, want %v", c.state, got, c.wantNFO)
		}
	}
}

// TestArtistDirBasename covers the helper that pre-fills the
// rename-directory prompt with the leaf component of the artist's
// filesystem path.
func TestArtistDirBasename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty path", "", ""},
		{"whitespace only", "   ", ""},
		{"absolute path", "/music/Some Artist", "Some Artist"},
		{"trailing slash", "/music/Some Artist/", "Some Artist"},
		{"single segment", "OnlyName", "OnlyName"},
		{"unicode segment", "/music/上原ひろみ", "上原ひろみ"},
		{"with leading whitespace", "  /music/Whitespace", "Whitespace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := artistDirBasename(c.in)
			if got != c.want {
				t.Errorf("artistDirBasename(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
