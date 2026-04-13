package scraper

import (
	"strings"
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

	if len(caps) != 9 {
		t.Errorf("ProviderCapabilities count = %d, want 9", len(caps))
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
	if len(names) != 16 {
		t.Errorf("AllFieldNames count = %d, want 16", len(names))
	}

	// Check uniqueness
	seen := make(map[FieldName]bool)
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate field name: %q", n)
		}
		seen[n] = true
	}

	// Explicitly verify that the newly-added detail fields are present.
	// Length alone is not sufficient: a swap could satisfy the count check
	// while dropping one of the required field names.
	required := map[FieldName]bool{
		FieldYearsActive: false,
		FieldType:        false,
		FieldGender:      false,
	}
	for _, n := range names {
		if _, ok := required[n]; ok {
			required[n] = true
		}
	}
	for f, found := range required {
		if !found {
			t.Errorf("missing field name: %s", f)
		}
	}
}

// TestIsValidFieldName verifies that every known field is accepted and that
// unknown values are rejected.
func TestIsValidFieldName(t *testing.T) {
	for _, f := range AllFieldNames() {
		if !IsValidFieldName(f) {
			t.Errorf("IsValidFieldName(%q) = false, want true", f)
		}
	}
	for _, bad := range []FieldName{"", "unknown", "BIOGRAPHY", "thumbnail"} {
		if IsValidFieldName(bad) {
			t.Errorf("IsValidFieldName(%q) = true, want false", bad)
		}
	}
}

// TestValidateConfig exercises the success path and each failure branch of
// ValidateConfig (unknown field, unknown primary provider, unknown provider
// in fallback chain).
func TestValidateConfig(t *testing.T) {
	t.Run("default config is valid", func(t *testing.T) {
		if err := ValidateConfig(DefaultConfig()); err != nil {
			t.Errorf("ValidateConfig(DefaultConfig()) = %v, want nil", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		cfg := &ScraperConfig{
			Fields: []FieldConfig{{Field: "bogus", Primary: provider.NameMusicBrainz}},
		}
		err := ValidateConfig(cfg)
		if err == nil {
			t.Fatal("ValidateConfig with unknown field returned nil error")
		}
		if !strings.Contains(err.Error(), "unknown field name") {
			t.Errorf("err = %v, want substring %q", err, "unknown field name")
		}
	})

	t.Run("unknown primary provider", func(t *testing.T) {
		cfg := &ScraperConfig{
			Fields: []FieldConfig{{Field: FieldBiography, Primary: "not-a-provider"}},
		}
		err := ValidateConfig(cfg)
		if err == nil {
			t.Fatal("ValidateConfig with unknown provider returned nil error")
		}
		if !strings.Contains(err.Error(), "unknown provider name") {
			t.Errorf("err = %v, want substring %q", err, "unknown provider name")
		}
	})

	t.Run("empty primary is allowed", func(t *testing.T) {
		cfg := &ScraperConfig{
			Fields: []FieldConfig{{Field: FieldBiography, Primary: ""}},
		}
		if err := ValidateConfig(cfg); err != nil {
			t.Errorf("ValidateConfig with empty primary = %v, want nil", err)
		}
	})

	t.Run("unknown provider in fallback chain", func(t *testing.T) {
		cfg := &ScraperConfig{
			FallbackChains: []FallbackChain{
				{Category: CategoryMetadata, Providers: []provider.ProviderName{"nope"}},
			},
		}
		err := ValidateConfig(cfg)
		if err == nil {
			t.Fatal("ValidateConfig with unknown fallback provider returned nil error")
		}
		if !strings.Contains(err.Error(), "unknown provider name in fallback chain") {
			t.Errorf("err = %v, want substring %q", err, "unknown provider name in fallback chain")
		}
	})
}
