package provider_test

import (
	"go/ast"
	"go/constant"
	"go/types"
	"reflect"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/sydlexius/stillwater/internal/provider"
)

// This file holds the conformance guard behind #2897: a field is advertised
// for a provider if and only if that provider's adapter can actually populate
// it.
//
// Every other capability declaration in the repo is hand-maintained, so a
// test comparing two hand-maintained tables would pass whenever both are wrong
// in the same way -- which is how the divergences in #2897 survived. This
// guard derives the ground truth from the adapter SOURCE instead: it type-checks
// each adapter package and records every assignment to a field of
// provider.ArtistMetadata, plus every provider.Image* constant the package
// references. That derived set is what the declarations are measured against.
//
// The derivation is deliberately an over-approximation -- it counts an
// assignment anywhere in the package, not only inside GetArtist. That direction
// is the safe one: it can only fail a declaration that omits a field the
// adapter does touch, never bless one that the adapter never touches. Hostile
// review confirmed that by construction: an assignment in dead, uncalled
// non-test code makes the guard DEMAND a declaration rather than bless one, and
// assignments in _test.go files are not seen at all (packages.Load runs without
// NeedForTest).

// adapterDirs lists the package directory under internal/provider/ for every
// provider in AllProviderNames(). TestAdapterDirsCoverAllProviders keeps it
// complete, so a newly added provider cannot skip this guard by being absent.
var adapterDirs = map[provider.ProviderName]string{
	provider.NameMusicBrainz: "musicbrainz",
	provider.NameWikipedia:   "wikipedia",
	provider.NameFanartTV:    "fanarttv",
	provider.NameAudioDB:     "audiodb",
	provider.NameDiscogs:     "discogs",
	provider.NameLastFM:      "lastfm",
	provider.NameWikidata:    "wikidata",
	provider.NameDeezer:      "deezer",
	provider.NameGenius:      "genius",
	provider.NameSpotify:     "spotify",
}

// fieldVocabulary maps an ArtistMetadata struct field to the capability name
// used in ProviderCapability.SupportedFields (the struct's own JSON tag).
// A struct field absent from this map is plumbing rather than a declarable
// capability; TestFieldVocabularyIsTotal asserts the two sets together cover
// ArtistMetadata exactly, so a field added later cannot silently escape.
var fieldVocabulary = map[string]string{
	"Name":           "name",
	"SortName":       "sort_name",
	"Type":           "type",
	"Gender":         "gender",
	"Disambiguation": "disambiguation",
	"Origin":         "origin",
	"Biography":      "biography",
	"Genres":         "genres",
	"Styles":         "styles",
	"Moods":          "moods",
	"YearsActive":    "years_active",
	"Born":           "born",
	"Formed":         "formed",
	"Died":           "died",
	"Disbanded":      "disbanded",
	"Members":        "members",
	"SimilarArtists": "similar_artists",
	"Aliases":        "aliases",
}

// nonCapabilityFields are ArtistMetadata fields that carry identity or
// plumbing rather than artist data an operator can route to a provider. They
// are never declared as capabilities.
var nonCapabilityFields = map[string]bool{
	"ProviderID": true, "MusicBrainzID": true, "AudioDBID": true,
	"DiscogsID": true, "WikidataID": true, "DeezerID": true,
	"AllMusicID": true, "SpotifyID": true, "URLs": true,
	"MembersAuthoritative": true,
}

// declarationExceptions records a capability an adapter assigns in source but
// that is deliberately NOT declared, with the reason. Each entry is a claim
// about a live API, so it needs evidence outside the type checker -- keep this
// list at zero entries wherever possible.
var declarationExceptions = map[provider.ProviderName]map[string]string{
	provider.NameSpotify: {
		// Spotify's artist endpoint returns an empty genres array in
		// practice, so the assignment never yields data. Locked separately
		// by TestSpotifyCapabilitiesExcludeGenres.
		"genres": "the Spotify artist endpoint returns genres as an empty array",
	},
}

// adapterCapabilities is the set of capability names an adapter package can
// populate, derived from its source.
type adapterCapabilities struct {
	fields map[string]bool
	images map[provider.ImageType]bool
}

// loadAdapterCapabilities type-checks every adapter package and derives what
// each one assigns. It loads all packages in a single call because
// packages.Load dominates the runtime of this file.
func loadAdapterCapabilities(t *testing.T) map[provider.ProviderName]adapterCapabilities {
	t.Helper()

	names := make([]provider.ProviderName, 0, len(adapterDirs))
	for name := range adapterDirs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	pathToProvider := make(map[string]provider.ProviderName, len(names))
	patterns := make([]string, 0, len(names))
	for _, name := range names {
		pkgPath := "github.com/sydlexius/stillwater/internal/provider/" + adapterDirs[name]
		pathToProvider[pkgPath] = name
		patterns = append(patterns, pkgPath)
	}

	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedSyntax |
		packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("loading adapter packages: %v", err)
	}
	if len(pkgs) != len(patterns) {
		t.Fatalf("packages.Load returned %d packages, want %d -- the derivation would be measuring an incomplete set", len(pkgs), len(patterns))
	}

	out := make(map[provider.ProviderName]adapterCapabilities, len(pkgs))
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			t.Fatalf("type-checking %s: %v", pkg.PkgPath, pkg.Errors)
		}
		name, ok := pathToProvider[pkg.PkgPath]
		if !ok {
			t.Fatalf("packages.Load returned unrequested package %q", pkg.PkgPath)
		}
		out[name] = deriveCapabilities(t, pkg, nil)
	}

	if len(out) != len(adapterDirs) {
		t.Fatalf("derived capabilities for %d providers, want %d", len(out), len(adapterDirs))
	}
	return out
}

// deriveCapabilities walks one type-checked package and records the
// ArtistMetadata fields it assigns and the ImageType constants it references.
// When skipFuncs is non-nil, top-level funcs whose name it contains are not
// walked; only TestAdapterFieldsAreNotSearchOnly uses that.
func deriveCapabilities(t *testing.T, pkg *packages.Package, skipFuncs map[string]bool) adapterCapabilities {
	t.Helper()

	caps := adapterCapabilities{
		fields: make(map[string]bool),
		images: make(map[provider.ImageType]bool),
	}

	isArtistMetadata := func(tp types.Type) bool {
		if ptr, ok := tp.(*types.Pointer); ok {
			tp = ptr.Elem()
		}
		named, ok := tp.(*types.Named)
		return ok && named.Obj().Name() == "ArtistMetadata" &&
			named.Obj().Pkg() != nil &&
			named.Obj().Pkg().Path() == "github.com/sydlexius/stillwater/internal/provider"
	}

	// recordAssign handles `meta.Genres = ...` and `meta.Genres = append(...)`.
	recordAssign := func(lhs ast.Expr) {
		sel, ok := lhs.(*ast.SelectorExpr)
		if !ok {
			return
		}
		tv, ok := pkg.TypesInfo.Types[sel.X]
		if !ok || !isArtistMetadata(tv.Type) {
			return
		}
		caps.fields[sel.Sel.Name] = true
	}

	// recordImage resolves a provider.ImageXxx reference to the ImageType
	// VALUE the constant holds, rather than mangling the identifier name.
	recordImage := func(sel *ast.SelectorExpr) {
		obj, ok := pkg.TypesInfo.Uses[sel.Sel]
		if !ok {
			return
		}
		konst, ok := obj.(*types.Const)
		if !ok || konst.Pkg() == nil ||
			konst.Pkg().Path() != "github.com/sydlexius/stillwater/internal/provider" {
			return
		}
		named, ok := konst.Type().(*types.Named)
		if !ok || named.Obj().Name() != "ImageType" {
			return
		}
		caps.images[provider.ImageType(constant.StringVal(konst.Val()))] = true
	}

	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && skipFuncs != nil && skipFuncs[fn.Name.Name] {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						recordAssign(lhs)
					}
				case *ast.SelectorExpr:
					recordImage(node)
				case *ast.CompositeLit:
					tv, ok := pkg.TypesInfo.Types[node.Type]
					if !ok || !isArtistMetadata(tv.Type) {
						return true
					}
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if ident, ok := kv.Key.(*ast.Ident); ok {
							caps.fields[ident.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	return caps
}

// declarableFields converts derived struct-field names into capability names,
// dropping plumbing fields and the documented exceptions.
func declarableFields(t *testing.T, name provider.ProviderName, caps adapterCapabilities) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(caps.fields))
	for structField := range caps.fields {
		vocab, ok := fieldVocabulary[structField]
		if !ok {
			if nonCapabilityFields[structField] {
				continue
			}
			t.Fatalf("adapter %s assigns ArtistMetadata.%s, which is in neither fieldVocabulary nor nonCapabilityFields -- classify it before this guard can measure %s", name, structField, name)
		}
		if _, excepted := declarationExceptions[name][vocab]; excepted {
			continue
		}
		out[vocab] = true
	}
	return out
}

func sortedKeys[T ~string](m map[T]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// TestFieldVocabularyIsTotal asserts that fieldVocabulary and
// nonCapabilityFields together classify every ArtistMetadata field, and that
// each vocabulary entry matches the struct's own JSON tag. Without this, a
// field added to ArtistMetadata later is silently unclassifiable and the
// conformance guard would either skip it or fail with no explanation.
func TestFieldVocabularyIsTotal(t *testing.T) {
	typ := reflect.TypeOf(provider.ArtistMetadata{})
	if typ.NumField() == 0 {
		t.Fatal("ArtistMetadata has no fields -- the reflection precondition is broken")
	}

	for i := range typ.NumField() {
		f := typ.Field(i)
		vocab, declarable := fieldVocabulary[f.Name]
		plumbing := nonCapabilityFields[f.Name]
		switch {
		case declarable && plumbing:
			t.Errorf("ArtistMetadata.%s is in both fieldVocabulary and nonCapabilityFields; it must be in exactly one", f.Name)
		case !declarable && !plumbing:
			t.Errorf("ArtistMetadata.%s is in neither fieldVocabulary nor nonCapabilityFields; classify it so the capability guard can measure it", f.Name)
		case declarable:
			jsonTag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if jsonTag != vocab {
				t.Errorf("fieldVocabulary[%q] = %q, want %q (the field's JSON tag, which is the capability vocabulary)", f.Name, vocab, jsonTag)
			}
		}
	}

	for name := range fieldVocabulary {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("fieldVocabulary has entry %q, which is not a field of ArtistMetadata", name)
		}
	}
	for name := range nonCapabilityFields {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("nonCapabilityFields has entry %q, which is not a field of ArtistMetadata", name)
		}
	}
}

// TestAdapterDirsCoverAllProviders keeps the adapter map complete, so a
// provider added to AllProviderNames() cannot skip the conformance guard
// merely by being absent from adapterDirs.
func TestAdapterDirsCoverAllProviders(t *testing.T) {
	all := provider.AllProviderNames()
	if len(all) == 0 {
		t.Fatal("AllProviderNames() is empty -- every assertion below would pass vacuously")
	}
	for _, name := range all {
		if _, ok := adapterDirs[name]; !ok {
			t.Errorf("AllProviderNames() includes %q with no adapterDirs entry; add its package directory so its capabilities are checked", name)
		}
	}
	for name := range adapterDirs {
		found := false
		for _, known := range all {
			if known == name {
				found = true
			}
		}
		if !found {
			t.Errorf("adapterDirs has entry %q, which AllProviderNames() does not list", name)
		}
	}
}

// TestProviderCapabilitiesMatchAdapterSource is the #2897 guard. For every
// provider it asserts both directions: nothing is advertised that the adapter
// never assigns, and nothing the adapter assigns goes unadvertised.
func TestProviderCapabilitiesMatchAdapterSource(t *testing.T) {
	derived := loadAdapterCapabilities(t)
	declared := provider.ProviderCapabilities()

	all := provider.AllProviderNames()
	if len(all) == 0 {
		t.Fatal("AllProviderNames() is empty -- every assertion below would pass vacuously")
	}

	for _, name := range all {
		t.Run(string(name), func(t *testing.T) {
			cap, ok := declared[name]
			if !ok {
				t.Fatalf("ProviderCapabilities() has no entry for %q", name)
			}
			adapter, ok := derived[name]
			if !ok {
				t.Fatalf("no derived capabilities for %q", name)
			}

			wantFields := declarableFields(t, name, adapter)
			gotFields := make(map[string]bool, len(cap.SupportedFields))
			for _, f := range cap.SupportedFields {
				gotFields[f] = true
			}
			for f := range gotFields {
				if !wantFields[f] {
					t.Errorf("SupportedFields advertises %q, but the %s adapter never assigns it -- a priority slot for %q can never answer (advertised: %v, adapter supplies: %v)",
						f, name, f, sortedKeys(gotFields), sortedKeys(wantFields))
				}
			}
			for f := range wantFields {
				if !gotFields[f] {
					t.Errorf("the %s adapter assigns %q but SupportedFields omits it -- nothing will ever route %q to %s (advertised: %v, adapter supplies: %v)",
						name, f, f, name, sortedKeys(gotFields), sortedKeys(wantFields))
				}
			}

			gotImages := make(map[provider.ImageType]bool, len(cap.SupportedImages))
			for _, img := range cap.SupportedImages {
				gotImages[img] = true
			}
			for img := range gotImages {
				if !adapter.images[img] {
					t.Errorf("SupportedImages advertises %q, but the %s adapter never emits it (advertised: %v, adapter supplies: %v)",
						img, name, sortedKeys(gotImages), sortedKeys(adapter.images))
				}
			}
			for img := range adapter.images {
				if !gotImages[img] {
					t.Errorf("the %s adapter emits image type %q but SupportedImages omits it (advertised: %v, adapter supplies: %v)",
						name, img, sortedKeys(gotImages), sortedKeys(adapter.images))
				}
			}
		})
	}
}

// TestDefaultPrioritiesHaveNoDeadSlots asserts that every provider in every
// default priority chain can actually answer that field. A dead slot is not
// merely cosmetic: the chain still queries the provider, spending a
// rate-limited request on a guaranteed-empty response.
func TestDefaultPrioritiesHaveNoDeadSlots(t *testing.T) {
	declared := provider.ProviderCapabilities()
	priorities := provider.DefaultPriorities()
	if len(priorities) == 0 {
		t.Fatal("DefaultPriorities() is empty -- every assertion below would pass vacuously")
	}

	imageFields := map[string]provider.ImageType{
		"thumb": provider.ImageThumb, "fanart": provider.ImageFanart,
		"logo": provider.ImageLogo, "banner": provider.ImageBanner,
	}

	// deadSlotExceptions carries pairs that legitimately appear in
	// DefaultPriorities() without a matching ProviderCapabilities() entry,
	// because the field-support declaration is scoped to the full-refresh
	// paths while a priority chain can also serve the per-field comparison
	// path. Each entry needs evidence beyond this test -- see the named test.
	deadSlotExceptions := map[string]map[provider.ProviderName]bool{
		// AudioDB's adapter never assigns YearsActive literally, so
		// ProviderCapabilities() correctly omits it -- both full-refresh
		// paths would find it empty. But FetchFieldFromProviders' per-field
		// comparison path (extractFieldForComparison in orchestrator.go)
		// synthesizes a years_active candidate from AudioDB's Born/Died via
		// SynthesizeYearsActive, so it is a live, user-facing answer on that
		// path. TestFetchFieldFromProviders_AudioDBSynthesizesYearsActive
		// (orchestrator_test.go) guards the synthesis directly.
		"years_active": {provider.NameAudioDB: true},
	}

	for _, fp := range priorities {
		if len(fp.Providers) == 0 {
			t.Errorf("DefaultPriorities() row %q has no providers", fp.Field)
			continue
		}
		for _, name := range fp.Providers {
			if deadSlotExceptions[fp.Field][name] {
				continue
			}
			cap, ok := declared[name]
			if !ok {
				t.Errorf("DefaultPriorities() row %q lists %q, which has no ProviderCapabilities() entry", fp.Field, name)
				continue
			}
			supported := false
			if imgType, isImage := imageFields[fp.Field]; isImage {
				for _, img := range cap.SupportedImages {
					if img == imgType {
						supported = true
					}
				}
			} else {
				for _, f := range cap.SupportedFields {
					if f == fp.Field {
						supported = true
					}
				}
			}
			if !supported {
				t.Errorf("DefaultPriorities() row %q lists %q, which does not support %q -- a dead slot that spends a request on a guaranteed-empty response", fp.Field, name, fp.Field)
			}
		}
	}
}

// TestAdapterFieldsAreNotSearchOnly is a FORWARD-LOOKING pin, and today it is a
// tautology: no adapter assigns ArtistMetadata inside a search function at all,
// so skipping those functions changes no derivation and this currently
// duplicates the main conformance assertion. It is deliberately NOT the
// evidence that the over-approximation is safe -- the dead-code behavior noted
// at the top of this file is. Its value is future tense: if an adapter later
// populates a field only while mapping a SEARCH result, that field would look
// like a GetArtist capability to the main guard, and this is what would catch
// it. If it ever fails, the fix is to scope the derivation, not to widen the
// declaration.
//
// Known limit: the skip list matches top-level function NAMES, so a field set
// in a helper reachable only from a search path slips past both this test and
// the main guard.
func TestAdapterFieldsAreNotSearchOnly(t *testing.T) {
	searchFuncs := map[string]bool{
		"SearchArtist": true, "searchArtist": true, "mapSearchResults": true,
	}

	patterns := make([]string, 0, len(adapterDirs))
	pathToProvider := make(map[string]provider.ProviderName, len(adapterDirs))
	for name, dir := range adapterDirs {
		pkgPath := "github.com/sydlexius/stillwater/internal/provider/" + dir
		pathToProvider[pkgPath] = name
		patterns = append(patterns, pkgPath)
	}
	sort.Strings(patterns)

	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedSyntax |
		packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("loading adapter packages: %v", err)
	}
	if len(pkgs) != len(patterns) {
		t.Fatalf("packages.Load returned %d packages, want %d", len(pkgs), len(patterns))
	}

	declared := provider.ProviderCapabilities()
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			t.Fatalf("type-checking %s: %v", pkg.PkgPath, pkg.Errors)
		}
		name := pathToProvider[pkg.PkgPath]
		withoutSearch := declarableFields(t, name, deriveCapabilities(t, pkg, searchFuncs))
		for _, f := range declared[name].SupportedFields {
			if !withoutSearch[f] {
				t.Errorf("%s advertises %q, but the adapter assigns it only inside a search path -- GetArtist would never populate it", name, f)
			}
		}
	}
}
