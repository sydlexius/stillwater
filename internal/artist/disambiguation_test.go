package artist

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func TestSplitNameDisambiguation(t *testing.T) {
	cases := []struct {
		name       string
		artist     *Artist
		meta       *provider.ArtistMetadata
		wantSplit  bool
		wantName   string
		wantDisamb string
	}{
		{
			name:       "promotes when exact concat match",
			artist:     &Artist{Name: "Nirvana (Seattle grunge)"},
			meta:       &provider.ArtistMetadata{Name: "Nirvana", Disambiguation: "Seattle grunge"},
			wantSplit:  true,
			wantName:   "Nirvana",
			wantDisamb: "Seattle grunge",
		},
		{
			name:       "case insensitive match still splits",
			artist:     &Artist{Name: "nirvana (seattle grunge)"},
			meta:       &provider.ArtistMetadata{Name: "Nirvana", Disambiguation: "Seattle grunge"},
			wantSplit:  true,
			wantName:   "Nirvana",
			wantDisamb: "Seattle grunge",
		},
		{
			name:       "skips when name does not match concat shape",
			artist:     &Artist{Name: "Nirvana"},
			meta:       &provider.ArtistMetadata{Name: "Nirvana", Disambiguation: "Seattle grunge"},
			wantSplit:  false,
			wantName:   "Nirvana",
			wantDisamb: "",
		},
		{
			name:       "skips when disambiguation already set",
			artist:     &Artist{Name: "Nirvana (Seattle grunge)", Disambiguation: "UK band"},
			meta:       &provider.ArtistMetadata{Name: "Nirvana", Disambiguation: "Seattle grunge"},
			wantSplit:  false,
			wantName:   "Nirvana (Seattle grunge)",
			wantDisamb: "UK band",
		},
		{
			name:       "skips when provider has no disambiguation",
			artist:     &Artist{Name: "Some (name)"},
			meta:       &provider.ArtistMetadata{Name: "Some", Disambiguation: ""},
			wantSplit:  false,
			wantName:   "Some (name)",
			wantDisamb: "",
		},
		{
			name:       "nil meta is a noop",
			artist:     &Artist{Name: "Anything (x)"},
			meta:       nil,
			wantSplit:  false,
			wantName:   "Anything (x)",
			wantDisamb: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitNameDisambiguation(tc.artist, tc.meta)
			if got != tc.wantSplit {
				t.Errorf("SplitNameDisambiguation = %v, want %v", got, tc.wantSplit)
			}
			if tc.artist.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", tc.artist.Name, tc.wantName)
			}
			if tc.artist.Disambiguation != tc.wantDisamb {
				t.Errorf("Disambiguation = %q, want %q", tc.artist.Disambiguation, tc.wantDisamb)
			}
		})
	}
}
