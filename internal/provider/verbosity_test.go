package provider

import (
	"testing"
)

// TestFieldVerbosityOptions_Wikipedia verifies that the Wikipedia provider
// returns a biography verbosity control with intro as the default option
// and both intro and full options present.
func TestFieldVerbosityOptions_Wikipedia(t *testing.T) {
	t.Parallel()

	opts := FieldVerbosityOptions(NameWikipedia)
	if len(opts) == 0 {
		t.Fatal("expected verbosity options for Wikipedia, got none")
	}

	var bioOpt *FieldVerbosity
	for i := range opts {
		if opts[i].Field == "biography" {
			bioOpt = &opts[i]
			break
		}
	}
	if bioOpt == nil {
		t.Fatal("expected biography field verbosity option for Wikipedia")
	}

	if len(bioOpt.Options) < 2 {
		t.Fatalf("expected at least 2 verbosity options for biography, got %d", len(bioOpt.Options))
	}

	// First option is the default.
	if bioOpt.Options[0].Value != VerbosityIntro {
		t.Errorf("default verbosity = %q, want %q", bioOpt.Options[0].Value, VerbosityIntro)
	}

	// Both intro and full must be present.
	hasIntro, hasFull := false, false
	for _, o := range bioOpt.Options {
		switch o.Value {
		case VerbosityIntro:
			hasIntro = true
		case VerbosityFull:
			hasFull = true
		}
	}
	if !hasIntro {
		t.Error("intro option not found in biography verbosity options")
	}
	if !hasFull {
		t.Error("full option not found in biography verbosity options")
	}

	// LabelKey must be non-empty for all options.
	if bioOpt.LabelKey == "" {
		t.Error("biography verbosity LabelKey must not be empty")
	}
	for _, o := range bioOpt.Options {
		if o.LabelKey == "" {
			t.Errorf("option %q has empty LabelKey", o.Value)
		}
	}
}

// TestFieldVerbosityOptions_NoOptions verifies that providers without
// verbosity controls return nil.
func TestFieldVerbosityOptions_NoOptions(t *testing.T) {
	t.Parallel()

	// MusicBrainz has no verbosity controls in v1.
	opts := FieldVerbosityOptions(NameMusicBrainz)
	if opts != nil {
		t.Errorf("expected nil verbosity options for MusicBrainz, got %v", opts)
	}
}

// TestDefaultVerbosity verifies that DefaultVerbosity returns the first option
// value and returns empty string for an empty slice.
func TestDefaultVerbosity(t *testing.T) {
	t.Parallel()

	opts := []FieldVerbosityOption{
		{Value: VerbosityIntro, LabelKey: "intro_key"},
		{Value: VerbosityFull, LabelKey: "full_key"},
	}
	if got := DefaultVerbosity(opts); got != VerbosityIntro {
		t.Errorf("DefaultVerbosity = %q, want %q", got, VerbosityIntro)
	}

	if got := DefaultVerbosity(nil); got != "" {
		t.Errorf("DefaultVerbosity(nil) = %q, want empty string", got)
	}
}

// TestIsValidVerbosity checks acceptance and rejection of verbosity values.
func TestIsValidVerbosity(t *testing.T) {
	t.Parallel()

	opts := []FieldVerbosityOption{
		{Value: VerbosityIntro},
		{Value: VerbosityFull},
	}

	if !IsValidVerbosity(opts, VerbosityIntro) {
		t.Errorf("IsValidVerbosity(%q) = false, want true", VerbosityIntro)
	}
	if !IsValidVerbosity(opts, VerbosityFull) {
		t.Errorf("IsValidVerbosity(%q) = false, want true", VerbosityFull)
	}
	if IsValidVerbosity(opts, "medium") {
		t.Error("IsValidVerbosity(\"medium\") = true, want false for unknown value")
	}
	if IsValidVerbosity(opts, "") {
		t.Error("IsValidVerbosity(\"\") = true, want false for empty value")
	}
}

// TestFieldVerbosityCatalogue_SelfConsistent guards the invariants the
// FieldVerbosity structs cannot express at the type level: every catalogue
// entry must have a non-empty Field and LabelKey, at least one option, and
// unique non-empty option Values and LabelKeys.
func TestFieldVerbosityCatalogue_SelfConsistent(t *testing.T) {
	t.Parallel()
	for _, name := range AllProviderNames() {
		for _, fv := range FieldVerbosityOptions(name) {
			if fv.Field == "" {
				t.Errorf("%s: FieldVerbosity with an empty Field", name)
			}
			if fv.LabelKey == "" {
				t.Errorf("%s/%s: empty LabelKey", name, fv.Field)
			}
			if len(fv.Options) == 0 {
				t.Errorf("%s/%s: no options", name, fv.Field)
			}
			seen := make(map[string]bool)
			for _, o := range fv.Options {
				if o.Value == "" {
					t.Errorf("%s/%s: option with an empty Value", name, fv.Field)
				}
				if o.LabelKey == "" {
					t.Errorf("%s/%s: option %q with an empty LabelKey", name, fv.Field, o.Value)
				}
				if seen[o.Value] {
					t.Errorf("%s/%s: duplicate option Value %q", name, fv.Field, o.Value)
				}
				seen[o.Value] = true
			}
		}
	}
}
