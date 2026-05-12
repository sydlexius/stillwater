package scraper

import (
	"encoding/json"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// FuzzScraperConfigRoundTrip feeds arbitrary byte slices to the JSON decoder
// for ScraperConfig and then validates the result. When both unmarshal and
// validation succeed the test asserts that re-marshaling and re-unmarshaling
// the struct produces an equivalent value (same Fields, FallbackChains, ID,
// Scope, and timestamps). This catches enum/discriminator drift on FieldName
// and ProviderName as well as any marshal/unmarshal asymmetry.
func FuzzScraperConfigRoundTrip(f *testing.F) {
	// Seed 1: DefaultConfig() serialized -- the canonical happy path.
	defaultJSON, err := json.Marshal(DefaultConfig())
	if err != nil {
		f.Fatalf("serializing DefaultConfig: %v", err)
	}
	f.Add(defaultJSON)

	// Seed 2: Config with an unknown provider name.
	f.Add([]byte(`{"fields":[{"field":"biography","primary":"unknown-provider","enabled":true,"category":"metadata"}],"fallback_chains":[]}`))

	// Seed 3: Config with an empty fallback chain providers list.
	f.Add([]byte(`{"fields":[],"fallback_chains":[{"category":"metadata","providers":[]}]}`))

	// Seed 4: Provider name containing JSON metacharacters.
	f.Add([]byte(`{"fields":[{"field":"biography","primary":"prov\"}{:[]\\","enabled":true}],"fallback_chains":[]}`))

	// Seed 5: Duplicate FieldConfig entries (same field name, different primary).
	f.Add([]byte(`{"fields":[` +
		`{"field":"biography","primary":"lastfm","enabled":true,"category":"metadata"},` +
		`{"field":"biography","primary":"musicbrainz","enabled":false,"category":"metadata"}` +
		`],"fallback_chains":[]}`))

	// Seed 6: FallbackChain with a nil-like gap -- empty string provider in the slice.
	f.Add([]byte(`{"fields":[],"fallback_chains":[{"category":"metadata","providers":["musicbrainz","","lastfm"]}]}`))

	// Seed 7: Very large fields array -- stress the slice allocation path.
	largeCfg := &ScraperConfig{Scope: ScopeGlobal}
	for i := 0; i < 500; i++ {
		largeCfg.Fields = append(largeCfg.Fields, FieldConfig{
			Field:    FieldBiography,
			Primary:  provider.NameMusicBrainz,
			Enabled:  true,
			Category: CategoryMetadata,
		})
	}
	largeJSON, err := json.Marshal(largeCfg)
	if err != nil {
		f.Fatalf("serializing large config: %v", err)
	}
	f.Add(largeJSON)

	// Seed 8: Completely empty JSON object.
	f.Add([]byte(`{}`))

	// Seed 9: Empty byte slice -- must not panic.
	f.Add([]byte(``))

	// Seed 10: Config with all valid fields and no providers set (empty primary).
	f.Add([]byte(`{"fields":[{"field":"biography","primary":"","enabled":false,"category":"metadata"}],"fallback_chains":[]}`))

	// Seed 11: FallbackChains slice that is JSON null.
	f.Add([]byte(`{"fields":[],"fallback_chains":null}`))

	// Seed 12: Known-valid provider names in fallback chains.
	f.Add([]byte(`{"fields":[],"fallback_chains":[{"category":"images","providers":["fanarttv","audiodb"]}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Step 1: unmarshal -- errors are expected for garbage input.
		var cfg1 ScraperConfig
		if err := json.Unmarshal(data, &cfg1); err != nil {
			// Invalid JSON: acceptable, no panic is the invariant here.
			return
		}

		// Step 2: validate -- typed errors are acceptable; panics are not.
		if err := ValidateConfig(&cfg1); err != nil {
			// Typed validation error: the fuzz engine has found an invalid but
			// parseable config. This is expected and correct behavior.
			return
		}

		// Step 3: cfg1 is valid -- marshal must succeed.
		out, err := json.Marshal(&cfg1)
		if err != nil {
			t.Fatalf("Marshal of validated ScraperConfig failed: %v", err)
		}

		// Step 4: re-unmarshal the marshaled output.
		var cfg2 ScraperConfig
		if err := json.Unmarshal(out, &cfg2); err != nil {
			t.Fatalf("Unmarshal of re-marshaled ScraperConfig failed: %v", err)
		}

		// Step 5: assert equivalence on the semantically significant fields.
		// Timestamps (CreatedAt, UpdatedAt) are compared via .Equal() to avoid
		// false failures from time.Location pointer differences introduced by the
		// JSON codec (both represent UTC but may differ in internal representation).

		if cfg1.ID != cfg2.ID {
			t.Errorf("ID mismatch after round-trip: %q vs %q", cfg1.ID, cfg2.ID)
		}
		if cfg1.Scope != cfg2.Scope {
			t.Errorf("Scope mismatch after round-trip: %q vs %q", cfg1.Scope, cfg2.Scope)
		}
		if !cfg1.CreatedAt.Equal(cfg2.CreatedAt) {
			t.Errorf("CreatedAt mismatch after round-trip: %v vs %v", cfg1.CreatedAt, cfg2.CreatedAt)
		}
		if !cfg1.UpdatedAt.Equal(cfg2.UpdatedAt) {
			t.Errorf("UpdatedAt mismatch after round-trip: %v vs %v", cfg1.UpdatedAt, cfg2.UpdatedAt)
		}

		// Fields: compare count and each entry.
		if len(cfg1.Fields) != len(cfg2.Fields) {
			t.Errorf("Fields length mismatch after round-trip: %d vs %d",
				len(cfg1.Fields), len(cfg2.Fields))
		} else {
			for i := range cfg1.Fields {
				f1, f2 := cfg1.Fields[i], cfg2.Fields[i]
				if f1.Field != f2.Field {
					t.Errorf("Fields[%d].Field mismatch: %q vs %q", i, f1.Field, f2.Field)
				}
				if f1.Primary != f2.Primary {
					t.Errorf("Fields[%d].Primary mismatch: %q vs %q", i, f1.Primary, f2.Primary)
				}
				if f1.Enabled != f2.Enabled {
					t.Errorf("Fields[%d].Enabled mismatch: %v vs %v", i, f1.Enabled, f2.Enabled)
				}
				if f1.Category != f2.Category {
					t.Errorf("Fields[%d].Category mismatch: %q vs %q", i, f1.Category, f2.Category)
				}
			}
		}

		// FallbackChains: compare count and each entry.
		if len(cfg1.FallbackChains) != len(cfg2.FallbackChains) {
			t.Errorf("FallbackChains length mismatch after round-trip: %d vs %d",
				len(cfg1.FallbackChains), len(cfg2.FallbackChains))
		} else {
			for i := range cfg1.FallbackChains {
				ch1, ch2 := cfg1.FallbackChains[i], cfg2.FallbackChains[i]
				if ch1.Category != ch2.Category {
					t.Errorf("FallbackChains[%d].Category mismatch: %q vs %q",
						i, ch1.Category, ch2.Category)
				}
				if len(ch1.Providers) != len(ch2.Providers) {
					t.Errorf("FallbackChains[%d].Providers length mismatch: %d vs %d",
						i, len(ch1.Providers), len(ch2.Providers))
				} else {
					for j := range ch1.Providers {
						if ch1.Providers[j] != ch2.Providers[j] {
							t.Errorf("FallbackChains[%d].Providers[%d] mismatch: %q vs %q",
								i, j, ch1.Providers[j], ch2.Providers[j])
						}
					}
				}
			}
		}
	})
}
