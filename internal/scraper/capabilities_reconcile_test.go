package scraper

import (
	"sort"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// This file guards the #2897 invariant on the scraper side: the capability
// table the scraper serves must agree, field for field, with the provider
// table the settings UI and the docs matrix read.
//
// ProviderCapabilities() now DERIVES its rows from provider.ProviderCapabilities(),
// so these tests are not proving two hand-written tables happen to match.
// They pin the vocabulary translation between them -- which provider-side
// fields and image types have a scraper counterpart -- because that mapping is
// the only place the two vocabularies can still drift.

// scraperFieldVocabulary is the provider-side field vocabulary the scraper
// models as routable, restated independently of scraperFieldFor so a change to
// the mapping has to be made deliberately in two places rather than silently in
// one.
var scraperFieldVocabulary = map[string]FieldName{
	"biography": FieldBiography, "genres": FieldGenres, "styles": FieldStyles,
	"moods": FieldMoods, "members": FieldMembers, "formed": FieldFormed,
	"born": FieldBorn, "died": FieldDied, "disbanded": FieldDisbanded,
	"years_active": FieldYearsActive, "origin": FieldOrigin,
	"type": FieldType, "gender": FieldGender,
}

// providerOnlyFields are provider-side capability names with no scraper
// counterpart: none has a FieldConfig row or a fieldAppliers entry, so nothing
// could route them even if they were listed.
var providerOnlyFields = map[string]bool{
	"name": true, "sort_name": true, "disambiguation": true,
	"aliases": true, "similar_artists": true,
}

// scraperImageVocabulary and providerOnlyImages split provider ImageTypes the
// same way.
var scraperImageVocabulary = map[provider.ImageType]FieldName{
	provider.ImageThumb: FieldThumb, provider.ImageFanart: FieldFanart,
	provider.ImageLogo: FieldLogo, provider.ImageBanner: FieldBanner,
}

var providerOnlyImages = map[provider.ImageType]bool{
	provider.ImageHDLogo: true, provider.ImageWideThumb: true,
	provider.ImageBackground: true,
}

func sortedFieldNames(fields []FieldName) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, string(f))
	}
	sort.Strings(out)
	return out
}

// TestScraperCapabilitiesReconcileWithProviderTable asserts the cross-table
// invariant for EVERY provider and EVERY field, in both directions: a field is
// advertised in the scraper table if and only if it is advertised in the
// provider table (after dropping the provider-only vocabulary).
//
// Mutation-proven in both directions -- see #2897. Introducing an
// advertised-but-unsupplied divergence (adding a field to one table only) and a
// supplied-but-unadvertised one (removing a field from one table only) each
// fail this test with the provider and field named.
func TestScraperCapabilitiesReconcileWithProviderTable(t *testing.T) {
	scraperCaps := ProviderCapabilities()
	providerCaps := provider.ProviderCapabilities()

	all := provider.AllProviderNames()
	if len(all) == 0 {
		t.Fatal("AllProviderNames() is empty -- every assertion below would pass vacuously")
	}
	if len(scraperCaps) == 0 {
		t.Fatal("scraper.ProviderCapabilities() is empty -- every assertion below would pass vacuously")
	}
	if len(providerCaps) == 0 {
		t.Fatal("provider.ProviderCapabilities() is empty -- every assertion below would pass vacuously")
	}

	byProvider := make(map[provider.ProviderName]ProviderCapability, len(scraperCaps))
	for _, c := range scraperCaps {
		byProvider[c.Provider] = c
	}

	// Guard against a vacuous pass in the other direction: at least one
	// provider must actually declare metadata fields and at least one must
	// declare image fields, or an all-empty table would satisfy every
	// iff-check below.
	var totalMetadata, totalImages int
	for _, c := range scraperCaps {
		totalMetadata += len(c.MetadataFields)
		totalImages += len(c.ImageFields)
	}
	if totalMetadata == 0 {
		t.Fatal("no provider declares any metadata field -- the reconciliation would pass vacuously")
	}
	if totalImages == 0 {
		t.Fatal("no provider declares any image field -- the reconciliation would pass vacuously")
	}

	for _, name := range all {
		t.Run(string(name), func(t *testing.T) {
			scraperCap, ok := byProvider[name]
			if !ok {
				t.Fatalf("scraper.ProviderCapabilities() has no entry for %q", name)
			}
			providerCap, ok := providerCaps[name]
			if !ok {
				t.Fatalf("provider.ProviderCapabilities() has no entry for %q", name)
			}

			// Provider table -> scraper table.
			wantMetadata := make(map[FieldName]bool)
			for _, field := range providerCap.SupportedFields {
				scraperField, routable := scraperFieldVocabulary[field]
				if !routable {
					if !providerOnlyFields[field] {
						t.Errorf("provider table advertises %q for %s, which is in neither scraperFieldVocabulary nor providerOnlyFields -- classify it before this guard can measure %s", field, name, name)
					}
					continue
				}
				wantMetadata[scraperField] = true
			}
			wantImages := make(map[FieldName]bool)
			for _, img := range providerCap.SupportedImages {
				scraperField, routable := scraperImageVocabulary[img]
				if !routable {
					if !providerOnlyImages[img] {
						t.Errorf("provider table advertises image %q for %s, which is in neither scraperImageVocabulary nor providerOnlyImages", img, name)
					}
					continue
				}
				wantImages[scraperField] = true
			}

			gotMetadata := make(map[FieldName]bool, len(scraperCap.MetadataFields))
			for _, f := range scraperCap.MetadataFields {
				gotMetadata[f] = true
			}
			gotImages := make(map[FieldName]bool, len(scraperCap.ImageFields))
			for _, f := range scraperCap.ImageFields {
				gotImages[f] = true
			}

			for f := range wantMetadata {
				if !gotMetadata[f] {
					t.Errorf("%s: provider table advertises %q but the scraper table omits it -- the config layer will never offer %s for %q (scraper has %v, provider implies %v)",
						name, f, name, f, sortedFieldNames(scraperCap.MetadataFields), sortedFieldNames(mapKeys(wantMetadata)))
				}
			}
			for f := range gotMetadata {
				if !wantMetadata[f] {
					t.Errorf("%s: the scraper table advertises %q but the provider table does not -- %s cannot supply it (scraper has %v, provider implies %v)",
						name, f, name, sortedFieldNames(scraperCap.MetadataFields), sortedFieldNames(mapKeys(wantMetadata)))
				}
			}
			for f := range wantImages {
				if !gotImages[f] {
					t.Errorf("%s: provider table advertises image %q but the scraper table omits it -- every %s fetch for %q is dead (scraper has %v, provider implies %v)",
						name, f, name, f, sortedFieldNames(scraperCap.ImageFields), sortedFieldNames(mapKeys(wantImages)))
				}
			}
			for f := range gotImages {
				if !wantImages[f] {
					t.Errorf("%s: the scraper table advertises image %q but the provider table does not (scraper has %v, provider implies %v)",
						name, f, sortedFieldNames(scraperCap.ImageFields), sortedFieldNames(mapKeys(wantImages)))
				}
			}
		})
	}
}

func mapKeys(m map[FieldName]bool) []FieldName {
	out := make([]FieldName, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestEveryAdvertisedFieldIsRoutable asserts that every field any provider
// advertises in the scraper table is a field the scraper can actually route:
// it is a known FieldName and it has a row in DefaultConfig. Advertising a
// field with no config row is the #2895 shape -- the provider is offered for
// something no refresh ever fetches.
func TestEveryAdvertisedFieldIsRoutable(t *testing.T) {
	caps := ProviderCapabilities()
	if len(caps) == 0 {
		t.Fatal("ProviderCapabilities() is empty -- every assertion below would pass vacuously")
	}

	configured := make(map[FieldName]bool)
	for _, f := range DefaultConfig().Fields {
		configured[f.Field] = true
	}
	if len(configured) == 0 {
		t.Fatal("DefaultConfig() has no fields -- every assertion below would pass vacuously")
	}

	var checked int
	for _, c := range caps {
		for _, f := range append(append([]FieldName{}, c.MetadataFields...), c.ImageFields...) {
			checked++
			if !IsValidFieldName(f) {
				t.Errorf("%s advertises %q, which is not a known FieldName", c.Provider, f)
				continue
			}
			if !configured[f] {
				t.Errorf("%s advertises %q, which has no DefaultConfig row -- nothing would ever fetch it", c.Provider, f)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no advertised fields were checked -- the assertion passed vacuously")
	}
}

// TestProviderCapabilitiesCategoriesAreConsistent asserts each table row keeps
// metadata fields and image fields in their own slice. A metadata field landing
// in ImageFields would be routed through the image path and silently dropped.
func TestProviderCapabilitiesCategoriesAreConsistent(t *testing.T) {
	caps := ProviderCapabilities()
	if len(caps) == 0 {
		t.Fatal("ProviderCapabilities() is empty -- every assertion below would pass vacuously")
	}
	for _, c := range caps {
		for _, f := range c.MetadataFields {
			if CategoryFor(f) != CategoryMetadata {
				t.Errorf("%s lists %q under MetadataFields, but CategoryFor says it is %q", c.Provider, f, CategoryFor(f))
			}
		}
		for _, f := range c.ImageFields {
			if CategoryFor(f) != CategoryImages {
				t.Errorf("%s lists %q under ImageFields, but CategoryFor says it is %q", c.Provider, f, CategoryFor(f))
			}
		}
	}
}

// TestProviderCapabilitiesRequiresAuthMatchesAdapters pins the RequiresAuth
// value this table reports against provider.ProviderRequiresKey, the single
// declaration providerRequiresAuth (config.go) defers to instead of
// restating. This test does NOT independently prove ProviderRequiresKey
// itself matches each adapter's real RequiresAuth() method -- it cannot,
// without constructing every adapter, which would introduce an import cycle
// from internal/scraper. That proof lives in
// internal/provider/requires_auth_conformance_test.go
// (TestAdapterRequiresAuthMatchesProviderRequiresKey), which constructs every
// adapter and asserts it against the same provider.ProviderRequiresKey this
// test reads. Together the two tests close the loop: flipping an adapter's
// RequiresAuth() reddens the provider-side test by name; flipping
// providerRequiresAuth's derivation (or ProviderCapabilities().RequiresAuth)
// out of step with provider.ProviderRequiresKey reddens this one.
//
// #2897 changed AudioDB's reported value from true to false. That is a
// deliberate correction, not a side effect: audiodb.Adapter.RequiresAuth()
// returns false (its key is OPTIONAL -- it unlocks a higher rate tier but the
// free endpoint works without one), so the old hard-coded true was telling
// GET /api/v1/scraper/providers that AudioDB could not be used unconfigured.
func TestProviderCapabilitiesRequiresAuthMatchesAdapters(t *testing.T) {
	caps := ProviderCapabilities()
	if len(caps) == 0 {
		t.Fatal("ProviderCapabilities() is empty -- every assertion below would pass vacuously")
	}

	var checked int
	for _, c := range caps {
		checked++
		want := provider.ProviderRequiresKey(c.Provider)
		if c.RequiresAuth != want {
			t.Errorf("%s RequiresAuth = %v, want %v (must match provider.ProviderRequiresKey; GET /api/v1/scraper/providers reports this value)",
				c.Provider, c.RequiresAuth, want)
		}
	}
	if checked == 0 {
		t.Fatal("no providers were checked -- the assertion passed vacuously")
	}
}
