package rule

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

func TestNameLanguageFixer_CanFix(t *testing.T) {
	f := &NameLanguageFixer{}
	if !f.CanFix(&Violation{RuleID: RuleNameLanguagePref}) {
		t.Error("NameLanguageFixer should handle name_language_pref")
	}
	if f.CanFix(&Violation{RuleID: RuleBioExists}) {
		t.Error("NameLanguageFixer should not handle bio_exists")
	}
}

func TestNameLanguageFixer_Fix(t *testing.T) {
	tests := []struct {
		name      string
		artist    artist.Artist
		stub      *stubMetadataProvider
		ctx       context.Context
		wantFixed bool
		wantName  string
		wantSort  string
	}{
		{
			name:   "promotes both name and sort name",
			artist: artist.Artist{Name: "Mecano", SortName: "Mecano", MusicBrainzID: "mbid-1"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Mecano (band)", SortName: "Mecano, Band"},
			},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"es", "en"}),
			wantFixed: true,
			wantName:  "Mecano (band)",
			wantSort:  "Mecano, Band",
		},
		{
			name:   "promotes name only when sort matches",
			artist: artist.Artist{Name: "BTS", SortName: "BTS", MusicBrainzID: "mbid-2"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Bangtan Sonyeondan", SortName: "BTS"},
			},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"ko", "en"}),
			wantFixed: true,
			wantName:  "Bangtan Sonyeondan",
			wantSort:  "BTS",
		},
		{
			name:   "no-op when localized name matches",
			artist: artist.Artist{Name: "Rammstein", SortName: "Rammstein", MusicBrainzID: "mbid-3"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Rammstein", SortName: "Rammstein"},
			},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"de", "en"}),
			wantFixed: false,
			wantName:  "Rammstein",
			wantSort:  "Rammstein",
		},
		{
			name:      "skips when MBID is empty",
			artist:    artist.Artist{Name: "X", MusicBrainzID: ""},
			stub:      &stubMetadataProvider{},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"de"}),
			wantFixed: false,
			wantName:  "X",
		},
		{
			name:      "skips when artist is locked",
			artist:    artist.Artist{Name: "X", MusicBrainzID: "mbid", Locked: true},
			stub:      &stubMetadataProvider{},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"de"}),
			wantFixed: false,
			wantName:  "X",
		},
		{
			name:      "skips when no language preferences",
			artist:    artist.Artist{Name: "X", MusicBrainzID: "mbid"},
			stub:      &stubMetadataProvider{},
			ctx:       context.Background(),
			wantFixed: false,
			wantName:  "X",
		},
		{
			name:   "not-fixed when FetchResult has nil Metadata",
			artist: artist.Artist{Name: "X", MusicBrainzID: "mbid-nil"},
			stub: &stubMetadataProvider{
				metadata: nil,
			},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"de"}),
			wantFixed: false,
			wantName:  "X",
		},
		{
			name:   "promotes sort name only when name matches",
			artist: artist.Artist{Name: "Rammstein", SortName: "rammstein", MusicBrainzID: "mbid-sort"},
			stub: &stubMetadataProvider{
				metadata: &provider.ArtistMetadata{Name: "Rammstein", SortName: "Rammstein"},
			},
			ctx:       provider.WithMetadataLanguages(context.Background(), []string{"de"}),
			wantFixed: true,
			wantName:  "Rammstein",
			wantSort:  "Rammstein",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewNameLanguageFixer(tt.stub, testLogger())
			a := tt.artist // copy for mutation
			fr, err := f.Fix(tt.ctx, &a, &Violation{RuleID: RuleNameLanguagePref})
			if err != nil {
				t.Fatalf("Fix returned error: %v", err)
			}
			if fr.Fixed != tt.wantFixed {
				t.Errorf("Fixed = %v, want %v (msg: %s)", fr.Fixed, tt.wantFixed, fr.Message)
			}
			if a.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", a.Name, tt.wantName)
			}
			if tt.wantSort != "" && a.SortName != tt.wantSort {
				t.Errorf("SortName = %q, want %q", a.SortName, tt.wantSort)
			}
		})
	}
}

func TestNameLanguageFixer_Fix_FetchError(t *testing.T) {
	stub := &stubMetadataProvider{err: errors.New("upstream 503")}
	f := NewNameLanguageFixer(stub, testLogger())
	a := &artist.Artist{Name: "X", MusicBrainzID: "mbid"}
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"de"})
	_, err := f.Fix(ctx, a, &Violation{RuleID: RuleNameLanguagePref})
	if err == nil {
		t.Fatal("expected error from Fix when fetch fails")
	}
}

func TestNameLanguageFixer_Fix_NilOrchestrator(t *testing.T) {
	f := &NameLanguageFixer{logger: testLogger()}
	a := &artist.Artist{Name: "X", MusicBrainzID: "mbid"}
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"de"})
	fr, err := f.Fix(ctx, a, &Violation{RuleID: RuleNameLanguagePref})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if fr.Fixed {
		t.Error("expected Fixed=false when orchestrator is nil")
	}
}
