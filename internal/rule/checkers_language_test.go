package rule

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// stubMetadataProvider is a fake MetadataProvider used by checker and fixer
// tests. It returns the configured metadata regardless of input, and records
// the number of FetchMetadata calls so tests can assert no-op behavior.
type stubMetadataProvider struct {
	metadata *provider.ArtistMetadata
	err      error
	calls    int
}

func (s *stubMetadataProvider) FetchMetadata(_ context.Context, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &provider.FetchResult{Metadata: s.metadata}, nil
}

// engineWithMetadataStub builds a minimal Engine wired with the given
// metadata-provider stub. Used by name_language_pref checker and fixer tests.
func engineWithMetadataStub(stub MetadataProvider) *Engine {
	e := newTestEngine()
	e.metadataProvider = stub
	return e
}

func TestNameLanguagePrefChecker(t *testing.T) {
	tests := []struct {
		name        string
		artist      artist.Artist
		stub        *stubMetadataProvider
		ctx         context.Context
		wantViol    bool
		wantFixable bool
		wantSubstr  string
		wantNoCalls bool
	}{
		{
			name:   "skips when artist is locked",
			artist: artist.Artist{Name: "BTS", MusicBrainzID: "mbid-1", Locked: true},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Bangtan Sonyeondan"},
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"ko", "en"}),
			wantViol:    false,
			wantNoCalls: true,
		},
		{
			name:   "skips when no language preferences in context",
			artist: artist.Artist{Name: "Rammstein", MusicBrainzID: "mbid-1"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Rammstein"},
			},
			ctx:         context.Background(),
			wantViol:    false,
			wantNoCalls: true,
		},
		{
			name:   "passes when script matches preferred locale",
			artist: artist.Artist{Name: "Rammstein", SortName: "Rammstein", MusicBrainzID: "mbid-1"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Rammstein", SortName: "Rammstein"},
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"de", "en"}),
			wantViol:    false,
			wantNoCalls: true,
		},
		{
			name:   "flags kanji name with english prefs and MB alias available",
			artist: artist.Artist{Name: "\u5c3e\u5d0e\u8c4a", SortName: "\u5c3e\u5d0e\u8c4a", MusicBrainzID: "mbid-2"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Ozaki Yutaka", SortName: "Ozaki, Yutaka"},
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    true,
			wantFixable: true,
			wantSubstr:  "Ozaki Yutaka",
		},
		{
			name:   "flags kanji name with english prefs and no MB alias",
			artist: artist.Artist{Name: "\u5c3e\u5d0e\u8c4a", MusicBrainzID: "mbid-3"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "\u5c3e\u5d0e\u8c4a"},
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    true,
			wantFixable: false,
			wantSubstr:  "no localized alias available",
		},
		{
			name:        "flags kanji name with no MBID at all",
			artist:      artist.Artist{Name: "\u5c3e\u5d0e\u8c4a", MusicBrainzID: ""},
			stub:        &stubMetadataProvider{},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    true,
			wantFixable: false,
			wantSubstr:  "no localized alias available",
			wantNoCalls: true,
		},
		{
			name:   "flags kanji name when MB fetch fails",
			artist: artist.Artist{Name: "\u5c3e\u5d0e\u8c4a", MusicBrainzID: "mbid-err"},
			stub: &stubMetadataProvider{
				err: errors.New("upstream 503"),
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    true,
			wantFixable: false,
			wantSubstr:  "no localized alias available",
		},
		{
			name:   "flags cyrillic name with english-only prefs",
			artist: artist.Artist{Name: "\u0420\u0430\u043c\u043c\u0448\u0442\u0430\u0439\u043d", MusicBrainzID: "mbid-4"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Rammstein"},
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    true,
			wantFixable: true,
			wantSubstr:  "Rammstein",
		},
		{
			name:        "cyrillic name passes with russian in prefs",
			artist:      artist.Artist{Name: "\u0420\u0430\u043c\u043c\u0448\u0442\u0430\u0439\u043d", MusicBrainzID: "mbid-5"},
			stub:        &stubMetadataProvider{},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"ru", "en"}),
			wantViol:    false,
			wantNoCalls: true,
		},
		{
			name:        "hangul name passes with korean in prefs",
			artist:      artist.Artist{Name: "\ubc29\ud0c4\uc18c\ub144\ub2e8", MusicBrainzID: "mbid-6"},
			stub:        &stubMetadataProvider{},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"ko"}),
			wantViol:    false,
			wantNoCalls: true,
		},
		{
			name:        "latin name with japanese prefs flags unfixable",
			artist:      artist.Artist{Name: "Rammstein", MusicBrainzID: "mbid-7"},
			stub:        &stubMetadataProvider{},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"ja"}),
			wantViol:    true,
			wantFixable: false,
			wantSubstr:  "latin script",
		},
		{
			name:   "flags only sort name when alias sort differs",
			artist: artist.Artist{Name: "\u5c3e\u5d0e\u8c4a", SortName: "\u5c3e\u5d0e\u8c4a", MusicBrainzID: "mbid-8"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "\u5c3e\u5d0e\u8c4a", SortName: "Ozaki, Yutaka"},
			},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    true,
			wantFixable: true,
			wantSubstr:  "sort",
		},
		{
			name:        "skips empty name",
			artist:      artist.Artist{Name: "", MusicBrainzID: "mbid-9"},
			stub:        &stubMetadataProvider{},
			ctx:         provider.WithMetadataLanguages(context.Background(), []string{"en"}),
			wantViol:    false,
			wantNoCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := engineWithMetadataStub(tt.stub)
			checker := e.makeNameLanguagePrefChecker()
			v := checker(tt.ctx, &tt.artist, RuleConfig{})

			if tt.wantViol && v == nil {
				t.Fatalf("expected violation, got nil")
			}
			if !tt.wantViol && v != nil {
				t.Fatalf("expected no violation, got: %s", v.Message)
			}
			if v != nil {
				if v.RuleID != RuleNameLanguagePref {
					t.Errorf("RuleID = %q, want %q", v.RuleID, RuleNameLanguagePref)
				}
				if v.Fixable != tt.wantFixable {
					t.Errorf("Fixable = %v, want %v (msg: %s)", v.Fixable, tt.wantFixable, v.Message)
				}
				if tt.wantSubstr != "" && !strings.Contains(v.Message, tt.wantSubstr) {
					t.Errorf("message %q does not contain %q", v.Message, tt.wantSubstr)
				}
			}
			if tt.wantNoCalls && tt.stub.calls != 0 {
				t.Errorf("expected no FetchMetadata calls, got %d", tt.stub.calls)
			}
		})
	}
}

func TestNameLanguagePrefChecker_NilProvider(t *testing.T) {
	e := newTestEngine()
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"en"})

	checker := e.makeNameLanguagePrefChecker()
	v := checker(ctx, &artist.Artist{Name: "\u5c3e\u5d0e\u8c4a", MusicBrainzID: "mbid"}, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for script mismatch even without provider")
	}
	if v.Fixable {
		t.Error("expected Fixable=false when metadataProvider is nil")
	}
}
