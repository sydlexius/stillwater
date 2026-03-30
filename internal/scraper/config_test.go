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

	// Default config must not pre-set any verbosity; providers choose the
	// conservative default when the field is empty.
	for _, f := range cfg.Fields {
		if f.Verbosity != "" {
			t.Errorf("Field %q has non-empty default verbosity %q; default should be empty", f.Field, f.Verbosity)
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

func TestScraperConfig_VerbosityFor(t *testing.T) {
	cfg := DefaultConfig()

	// Default config has no verbosity set for any field.
	if got := cfg.VerbosityFor(FieldBiography); got != "" {
		t.Errorf("VerbosityFor(biography) on default config = %q, want empty", got)
	}

	// After setting verbosity on a field it should be returned.
	for i := range cfg.Fields {
		if cfg.Fields[i].Field == FieldBiography {
			cfg.Fields[i].Verbosity = string(VerbosityFull)
		}
	}
	if got := cfg.VerbosityFor(FieldBiography); got != string(VerbosityFull) {
		t.Errorf("VerbosityFor(biography) = %q, want %q", got, VerbosityFull)
	}

	// Unknown field returns empty string.
	if got := cfg.VerbosityFor("nonexistent"); got != "" {
		t.Errorf("VerbosityFor(nonexistent) = %q, want empty", got)
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

func TestValidBiographyVerbosity(t *testing.T) {
	tests := []struct {
		v    BiographyVerbosity
		want bool
	}{
		{VerbosityIntro, true},
		{VerbosityFull, true},
		{"", false},
		{"summary", false},
		{"INTRO", false},
	}
	for _, tt := range tests {
		if got := ValidBiographyVerbosity(tt.v); got != tt.want {
			t.Errorf("ValidBiographyVerbosity(%q) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestVerbosityOptionsFor(t *testing.T) {
	// Wikipedia + biography should return two options with intro as default.
	opts := VerbosityOptionsFor(FieldBiography, provider.NameWikipedia)
	if len(opts) != 2 {
		t.Fatalf("VerbosityOptionsFor(biography, wikipedia) len = %d, want 2", len(opts))
	}
	foundDefault := false
	for _, o := range opts {
		if o.Default {
			if o.Value != string(VerbosityIntro) {
				t.Errorf("default verbosity option value = %q, want %q", o.Value, VerbosityIntro)
			}
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Error("no default verbosity option set for Wikipedia biography")
	}

	// Other provider-field combinations should return nil.
	if got := VerbosityOptionsFor(FieldBiography, provider.NameLastFM); got != nil {
		t.Errorf("VerbosityOptionsFor(biography, lastfm) = %v, want nil", got)
	}
	if got := VerbosityOptionsFor(FieldGenres, provider.NameWikipedia); got != nil {
		t.Errorf("VerbosityOptionsFor(genres, wikipedia) = %v, want nil", got)
	}
}

func TestValidateConfig_InvalidVerbosity(t *testing.T) {
	cfg := DefaultConfig()
	// Set an invalid verbosity on the biography field (value not in allowed list).
	for i := range cfg.Fields {
		if cfg.Fields[i].Field == FieldBiography {
			cfg.Fields[i].Verbosity = "invalid"
		}
	}
	if err := ValidateConfig(cfg); err == nil {
		t.Error("expected validation error for invalid biography verbosity, got nil")
	}
}

func TestValidateConfig_VerbosityOnUnsupportedField(t *testing.T) {
	cfg := DefaultConfig()
	// Setting verbosity on a field/provider that doesn't support it should fail.
	for i := range cfg.Fields {
		if cfg.Fields[i].Field == FieldGenres {
			cfg.Fields[i].Verbosity = string(VerbosityFull)
		}
	}
	if err := ValidateConfig(cfg); err == nil {
		t.Error("expected validation error for verbosity on non-supporting field, got nil")
	}
}

func TestValidateConfig_ValidVerbosity(t *testing.T) {
	for _, v := range []string{string(VerbosityIntro), string(VerbosityFull)} {
		cfg := DefaultConfig()
		for i := range cfg.Fields {
			if cfg.Fields[i].Field == FieldBiography {
				cfg.Fields[i].Primary = provider.NameWikipedia
				cfg.Fields[i].Verbosity = v
			}
		}
		if err := ValidateConfig(cfg); err != nil {
			t.Errorf("ValidateConfig with verbosity %q returned unexpected error: %v", v, err)
		}
	}
}
