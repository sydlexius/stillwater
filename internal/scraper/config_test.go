package scraper

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func TestCategoryFor(t *testing.T) {
	tests := []struct {
		field FieldName
		want  FieldCategory
	}{
		{FieldBiography, CategoryMetadata},
		{FieldGenres, CategoryMetadata},
		{FieldMembers, CategoryMetadata},
		{FieldThumb, CategoryImages},
		{FieldFanart, CategoryImages},
		{FieldLogo, CategoryImages},
		{FieldBanner, CategoryImages},
	}
	for _, tt := range tests {
		if got := CategoryFor(tt.field); got != tt.want {
			t.Errorf("CategoryFor(%q) = %q, want %q", tt.field, got, tt.want)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Scope != ScopeGlobal {
		t.Errorf("Scope = %q, want %q", cfg.Scope, ScopeGlobal)
	}

	if len(cfg.Fields) != len(AllFieldNames()) {
		t.Errorf("Fields count = %d, want %d", len(cfg.Fields), len(AllFieldNames()))
	}

	// Check that all fields are enabled
	for _, f := range cfg.Fields {
		if !f.Enabled {
			t.Errorf("Field %q is disabled in default config", f.Field)
		}
	}

	// Check fallback chains
	if len(cfg.FallbackChains) != 2 {
		t.Fatalf("FallbackChains count = %d, want 2", len(cfg.FallbackChains))
	}

	metaChain := cfg.FallbackChainFor(CategoryMetadata)
	if metaChain == nil {
		t.Fatal("no metadata fallback chain")
	}
	if len(metaChain.Providers) == 0 {
		t.Error("metadata fallback chain is empty")
	}

	imgChain := cfg.FallbackChainFor(CategoryImages)
	if imgChain == nil {
		t.Fatal("no images fallback chain")
	}
	if len(imgChain.Providers) == 0 {
		t.Error("images fallback chain is empty")
	}
}

func TestScraperConfig_PrimaryFor(t *testing.T) {
	cfg := DefaultConfig()

	if got := cfg.PrimaryFor(FieldBiography); got != provider.NameLastFM {
		t.Errorf("PrimaryFor(biography) = %q, want %q", got, provider.NameLastFM)
	}

	if got := cfg.PrimaryFor(FieldThumb); got != provider.NameFanartTV {
		t.Errorf("PrimaryFor(thumb) = %q, want %q", got, provider.NameFanartTV)
	}

	if got := cfg.PrimaryFor("nonexistent"); got != "" {
		t.Errorf("PrimaryFor(nonexistent) = %q, want empty", got)
	}
}

func TestProviderCapabilities(t *testing.T) {
	caps := ProviderCapabilities()

	if len(caps) != 6 {
		t.Errorf("ProviderCapabilities count = %d, want 6", len(caps))
	}

	// Verify MusicBrainz has no image fields
	for _, c := range caps {
		if c.Provider == provider.NameMusicBrainz {
			if len(c.ImageFields) > 0 {
				t.Error("MusicBrainz should have no image fields")
			}
			if len(c.MetadataFields) == 0 {
				t.Error("MusicBrainz should have metadata fields")
			}
		}
		// Verify Fanart.tv has no metadata fields
		if c.Provider == provider.NameFanartTV {
			if len(c.MetadataFields) > 0 {
				t.Error("Fanart.tv should have no metadata fields")
			}
			if len(c.ImageFields) == 0 {
				t.Error("Fanart.tv should have image fields")
			}
		}
	}
}

func TestAllFieldNames(t *testing.T) {
	names := AllFieldNames()
	if len(names) != 13 {
		t.Errorf("AllFieldNames count = %d, want 13", len(names))
	}

	// Check uniqueness
	seen := make(map[FieldName]bool)
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate field name: %q", n)
		}
		seen[n] = true
	}
}
