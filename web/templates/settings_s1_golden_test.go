package templates

// settings_s1_golden_test.go -- byte-identical pre/post extraction gate for
// M55 issue #1809 (S1 Essentials: extract stable General+Libraries cards into
// shared Section* templ funcs).
//
// Phase 1 (pre-extraction): render SettingsPage for each fixture and write the
// HTML to /tmp/m55-1809-s1/before_<n>.html.
// Phase 2 (post-extraction): run the same fixtures, write after_<n>.html.
// The diff between before and after MUST be empty for every fixture.
//
// The committed per-section golden tests at the bottom of this file (those
// whose names start with TestSection*) render each extracted Section* func in
// isolation and compare against web/templates/testdata/section_*.golden.html.
// Golden files are generated from the first post-extraction run via -update flag
// (or by running go generate on the test).  Once committed they lock future
// regressions.
//
// NOTE: the extraction itself was byte-identical, but this PR additionally
// applied a11y and i18n hardening to some touched cards (switch label
// association via aria-labelledby on SectionActiveProfile/SectionBehavior, and
// localized Image Cache size labels on SectionImageCache). The committed
// per-section goldens therefore intentionally differ from main's original
// inline render for those controls; they capture the hardened output.

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
)

// updateGolden controls whether TestSection* tests write new golden files
// instead of comparing against existing ones.  Run with -update to
// regenerate after intentional template changes.
var updateGolden = flag.Bool("update", false, "regenerate golden files")

// s1TempDir is where the before/after full-page renders live.  These are
// ephemeral artifacts used only for the extraction byte-diff and are NOT
// committed to git.
const s1TempDir = "/tmp/m55-1809-s1"

// s1GoldenDir is the committed directory for per-section golden HTML.
const s1GoldenDir = "testdata"

// imageCacheTestAssets supplies a non-empty SettingsImageCacheJS path so the
// SectionImageCache golden fixtures render a valid <script src="..."> element.
// Production always populates this path (see internal/api/handlers.go); an
// empty AssetPaths would emit <script src=""></script>, which is invalid HTML.
var imageCacheTestAssets = AssetPaths{SettingsImageCacheJS: "/static/js/settings/image-cache.js"}

// editableCustomProfile is a user-created custom profile (editable).
var editableCustomProfile = platform.Profile{
	ID:         "custom",
	Name:       "Custom",
	IsBuiltin:  true, // seeded builtin but editable (ID == "custom")
	IsActive:   true,
	NFOEnabled: true,
	NFOFormat:  "kodi",
	ImageNaming: platform.ImageNaming{
		Thumb:  []string{"folder.jpg", "artist.jpg"},
		Fanart: []string{"fanart.jpg"},
		Logo:   []string{"logo.png"},
		Banner: []string{"banner.jpg"},
	},
	UseSymlinks: true,
}

// readonlyBuiltinProfile is a built-in emby profile (not editable).
var readonlyBuiltinProfile = platform.Profile{
	ID:         "emby",
	Name:       "Emby",
	IsBuiltin:  true,
	IsActive:   false,
	NFOEnabled: true,
	NFOFormat:  "emby",
	ImageNaming: platform.ImageNaming{
		Thumb:  []string{"folder.jpg"},
		Fanart: []string{"fanart.jpg"},
		Logo:   []string{"logo.png"},
		Banner: []string{"banner.jpg"},
	},
	UseSymlinks: false,
}

// secondProfile is a second profile for populating data.Profiles.
var secondProfile = platform.Profile{
	ID:         "jellyfin",
	Name:       "Jellyfin",
	IsBuiltin:  true,
	IsActive:   false,
	NFOEnabled: true,
	NFOFormat:  "jellyfin",
	ImageNaming: platform.ImageNaming{
		Thumb:  []string{"folder.jpg"},
		Fanart: []string{"backdrop.jpg"},
		Logo:   []string{"logo.png"},
		Banner: []string{"banner.jpg"},
	},
	UseSymlinks: false,
}

// twoLibraries provides populated Libraries data.
var twoLibraries = []library.Library{
	{ID: "lib1", Name: "Music", Path: "/music", Type: "regular", Source: "manual"},
	{ID: "lib2", Name: "Classical", Path: "/classical", Type: "regular", Source: "jellyfin"},
}

// s1Fixtures returns the full set of SettingsData fixtures exercising every
// conditional branch in the 7 extracted cards (SectionPlatformProfile,
// SectionActiveProfile, SectionTLSStatus, SectionBasePath,
// SectionImageCache, SectionLibraries).
func s1Fixtures() []struct {
	name string
	data SettingsData
} {
	twoProfiles := []platform.Profile{editableCustomProfile, secondProfile}

	return []struct {
		name string
		data SettingsData
	}{
		{
			// Fixture 0: ActiveProfile non-nil + editable + symlinks supported +
			// UseSymlinks true + TLS byo + BasePath env override +
			// CacheMaxSizeMB custom + libraries populated.
			name: "active-profile-editable-symlinks-byo-envoverride-debug-custom-cache-libs",
			data: SettingsData{
				ActiveTab:     TabGeneral,
				Profiles:      twoProfiles,
				ActiveProfile: &editableCustomProfile,
				Libraries:     twoLibraries,
				TLS: TLSStatusData{
					Mode:             "byo",
					HTTPSPort:        443,
					HTTPRedirectPort: 80,
					HTTP3Port:        443,
				},
				SymlinkSupported:    true,
				BasePath:            "/sw",
				BasePathEnvOverride: true,
				CacheMaxSizeMB:      "777",
			},
		},
		{
			// Fixture 1: ActiveProfile nil + TLS off + no env override + debug
			// false + CacheMaxSizeMB "0" + no libraries.
			name: "no-active-profile-tls-off-no-env-no-debug-unlimited-cache-no-libs",
			data: SettingsData{
				ActiveTab:           TabGeneral,
				Profiles:            twoProfiles,
				ActiveProfile:       nil,
				Libraries:           nil,
				TLS:                 TLSStatusData{Mode: "off", HTTPPort: 1973},
				SymlinkSupported:    false,
				BasePath:            "",
				BasePathEnvOverride: false,
				CacheMaxSizeMB:      "0",
			},
		},
		{
			// Fixture 2: ActiveProfile non-nil + editable + symlinks NOT supported +
			// UseSymlinks false + TLS acme with domain + BasePath set no env override +
			// CacheMaxSizeMB "1024".
			name: "active-profile-editable-nosymlink-support-acme-domain-no-envoverride-1gb-cache",
			data: SettingsData{
				ActiveTab: TabGeneral,
				Profiles:  twoProfiles,
				ActiveProfile: &platform.Profile{
					ID:         "custom",
					Name:       "Custom",
					IsBuiltin:  true,
					IsActive:   true,
					NFOEnabled: true,
					NFOFormat:  "kodi",
					ImageNaming: platform.ImageNaming{
						Thumb:  []string{"folder.jpg"},
						Fanart: []string{"fanart.jpg"},
						Logo:   []string{"logo.png"},
						Banner: []string{"banner.jpg"},
					},
					UseSymlinks: false,
				},
				Libraries: twoLibraries,
				TLS: TLSStatusData{
					Mode:       "acme",
					AcmeDomain: "stillwater.example.com",
					HTTPSPort:  443,
				},
				SymlinkSupported:    false,
				BasePath:            "/stillwater",
				BasePathEnvOverride: false,
				CacheMaxSizeMB:      "1024",
			},
		},
		{
			// Fixture 3: ActiveProfile non-nil + readonly builtin (not editable) +
			// TLS acme without domain + BasePath set + no env override +
			// CacheMaxSizeMB "" (empty).
			name: "readonly-profile-acme-no-domain-basepath-empty-cache",
			data: SettingsData{
				ActiveTab:     TabGeneral,
				Profiles:      twoProfiles,
				ActiveProfile: &readonlyBuiltinProfile,
				Libraries:     twoLibraries,
				TLS: TLSStatusData{
					Mode:       "acme",
					AcmeDomain: "",
					HTTPSPort:  443,
				},
				SymlinkSupported:    true,
				BasePath:            "/",
				BasePathEnvOverride: false,
				CacheMaxSizeMB:      "",
			},
		},
		{
			// Fixture 4: Libraries tab active + two libraries.
			// This also exercises the Libraries panel in isolation.
			name: "libraries-tab-two-libs",
			data: SettingsData{
				ActiveTab:           TabLibraries,
				Profiles:            twoProfiles,
				ActiveProfile:       nil,
				Libraries:           twoLibraries,
				TLS:                 TLSStatusData{Mode: "off", HTTPPort: 1973},
				SymlinkSupported:    false,
				BasePathEnvOverride: false,
				CacheMaxSizeMB:      "256",
			},
		},
		{
			// Fixture 5: Libraries tab active + empty Libraries slice.
			name: "libraries-tab-empty-libs",
			data: SettingsData{
				ActiveTab:           TabLibraries,
				Profiles:            twoProfiles,
				ActiveProfile:       nil,
				Libraries:           []library.Library{},
				TLS:                 TLSStatusData{Mode: "off", HTTPPort: 1973},
				SymlinkSupported:    false,
				BasePathEnvOverride: false,
				CacheMaxSizeMB:      "512",
			},
		},
		{
			// Fixture 6: TLS HTTP3Port non-zero + HTTPRedirectPort non-zero.
			name: "tls-byo-http3-redirect",
			data: SettingsData{
				ActiveTab:     TabGeneral,
				Profiles:      twoProfiles,
				ActiveProfile: nil,
				TLS: TLSStatusData{
					Mode:             "byo",
					HTTPSPort:        443,
					HTTPRedirectPort: 80,
					HTTP3Port:        443,
				},
				CacheMaxSizeMB: "2048",
			},
		},
	}
}

// TestS1_WriteBeforeHTML renders SettingsPage for every s1Fixture and writes
// before_<n>.html to s1TempDir.  This test must pass BEFORE the extraction
// so that the post-extraction TestS1_DiffBeforeAfter can compare.
func TestS1_WriteBeforeHTML(t *testing.T) {
	if err := os.MkdirAll(s1TempDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", s1TempDir, err)
	}
	ctx := testCtx(t)
	for i, fx := range s1Fixtures() {
		var buf bytes.Buffer
		if err := SettingsPage(AssetPaths{}, fx.data).Render(ctx, &buf); err != nil {
			t.Fatalf("fixture %d %q render: %v", i, fx.name, err)
		}
		out := filepath.Join(s1TempDir, fmt.Sprintf("before_%d.html", i))
		if err := os.WriteFile(out, buf.Bytes(), 0644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
		t.Logf("wrote %s (%d bytes)", out, buf.Len())
	}
}

// TestS1_WriteAfterHTML renders SettingsPage for every s1Fixture and writes
// after_<n>.html to s1TempDir.  Run after extraction.  The diff is in
// TestS1_DiffBeforeAfter.
func TestS1_WriteAfterHTML(t *testing.T) {
	if err := os.MkdirAll(s1TempDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", s1TempDir, err)
	}
	ctx := testCtx(t)
	for i, fx := range s1Fixtures() {
		var buf bytes.Buffer
		if err := SettingsPage(AssetPaths{}, fx.data).Render(ctx, &buf); err != nil {
			t.Fatalf("fixture %d %q render: %v", i, fx.name, err)
		}
		out := filepath.Join(s1TempDir, fmt.Sprintf("after_%d.html", i))
		if err := os.WriteFile(out, buf.Bytes(), 0644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
		t.Logf("wrote %s (%d bytes)", out, buf.Len())
	}
}

// TestS1_DiffBeforeAfter compares every before/after pair and fails if any
// differ.  This is the byte-identical acceptance gate for the extraction.
func TestS1_DiffBeforeAfter(t *testing.T) {
	for i := range s1Fixtures() {
		before := filepath.Join(s1TempDir, fmt.Sprintf("before_%d.html", i))
		after := filepath.Join(s1TempDir, fmt.Sprintf("after_%d.html", i))

		bData, err := os.ReadFile(before)
		if err != nil {
			t.Skipf("fixture %d: before file missing (%v) -- run TestS1_WriteBeforeHTML first", i, err)
			continue
		}
		aData, err := os.ReadFile(after)
		if err != nil {
			t.Skipf("fixture %d: after file missing (%v) -- run TestS1_WriteAfterHTML first", i, err)
			continue
		}
		if !bytes.Equal(bData, aData) {
			// Show first differing byte position for fast debugging.
			diffPos := -1
			for j := 0; j < len(bData) && j < len(aData); j++ {
				if bData[j] != aData[j] {
					diffPos = j
					break
				}
			}
			if diffPos == -1 {
				diffPos = len(bData)
				if len(aData) < diffPos {
					diffPos = len(aData)
				}
			}
			start := diffPos - 80
			if start < 0 {
				start = 0
			}
			end := diffPos + 80
			if end > len(bData) {
				end = len(bData)
			}
			t.Errorf("fixture %d: before/after differ at byte %d (before len=%d, after len=%d)\nbefore context: %q\n", i, diffPos, len(bData), len(aData), string(bData[start:end]))
		}
	}
}

// -- Per-section golden regression tests ----------------------------------
//
// Each TestSection* renders the corresponding Section* func in isolation for a
// representative fixture and compares the output against a committed golden
// file in web/templates/testdata/section_<name>.golden.html.
//
// Run with -update to regenerate golden files after intentional changes.
// Golden files are generated from the first post-extraction run.

// goldenPath returns the absolute path to the committed golden file for the
// given section name.
func goldenPath(name string) string {
	return filepath.Join(s1GoldenDir, "section_"+name+".golden.html")
}

// checkOrUpdateGolden compares rendered output against the committed golden
// file, or writes the file when -update is given.
func checkOrUpdateGolden(t *testing.T, sectionName, rendered string) {
	t.Helper()
	path := goldenPath(sectionName)
	if *updateGolden {
		if err := os.MkdirAll(s1GoldenDir, 0755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(rendered), 0644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("updated golden %s", path)
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden file %s missing; run with -update to generate it", path)
	}
	if string(golden) != rendered {
		// Find first difference for fast debugging.
		diffPos := -1
		for i := 0; i < len(golden) && i < len(rendered); i++ {
			if golden[i] != rendered[i] {
				diffPos = i
				break
			}
		}
		if diffPos == -1 {
			diffPos = len(golden)
			if len(rendered) < diffPos {
				diffPos = len(rendered)
			}
		}
		t.Errorf("section %q does not match golden %s (first diff at byte %d, golden len=%d, rendered len=%d)",
			sectionName, path, diffPos, len(golden), len(rendered))
	}
}

func TestSectionPlatformProfile_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		Profiles: []platform.Profile{editableCustomProfile, secondProfile, readonlyBuiltinProfile},
	}
	var buf bytes.Buffer
	if err := SectionPlatformProfile(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "platform_profile", buf.String())
}

func TestSectionActiveProfile_ActiveEditable_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		ActiveProfile:    &editableCustomProfile,
		SymlinkSupported: true,
	}
	var buf bytes.Buffer
	if err := SectionActiveProfile(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "active_profile_editable", buf.String())
}

func TestSectionActiveProfile_Nil_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		ActiveProfile: nil,
	}
	var buf bytes.Buffer
	if err := SectionActiveProfile(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "active_profile_nil", buf.String())
}

func TestSectionTLSStatus_BYO_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		TLS: TLSStatusData{
			Mode:             "byo",
			HTTPSPort:        443,
			HTTPRedirectPort: 80,
			HTTP3Port:        443,
		},
	}
	var buf bytes.Buffer
	if err := SectionTLSStatus(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "tls_status_byo", buf.String())
}

func TestSectionTLSStatus_ACME_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		TLS: TLSStatusData{
			Mode:       "acme",
			AcmeDomain: "stillwater.example.com",
			HTTPSPort:  443,
		},
	}
	var buf bytes.Buffer
	if err := SectionTLSStatus(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "tls_status_acme", buf.String())
}

func TestSectionTLSStatus_Off_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		TLS: TLSStatusData{Mode: "off", HTTPPort: 1973},
	}
	var buf bytes.Buffer
	if err := SectionTLSStatus(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "tls_status_off", buf.String())
}

func TestSectionBasePath_EnvOverride_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		BasePath:            "/sw",
		BasePathEnvOverride: true,
	}
	var buf bytes.Buffer
	if err := SectionBasePath(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "base_path_env_override", buf.String())
}

func TestSectionBasePath_NoEnvOverride_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		BasePath:            "/stillwater",
		BasePathEnvOverride: false,
	}
	var buf bytes.Buffer
	if err := SectionBasePath(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "base_path_no_env_override", buf.String())
}

func TestSectionImageCache_Custom_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{CacheMaxSizeMB: "777"}
	var buf bytes.Buffer
	if err := SectionImageCache(data, imageCacheTestAssets).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "image_cache_custom", buf.String())
}

func TestSectionImageCache_Zero_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{CacheMaxSizeMB: "0"}
	var buf bytes.Buffer
	if err := SectionImageCache(data, imageCacheTestAssets).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "image_cache_zero", buf.String())
}

func TestSectionImageCache_1024_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{CacheMaxSizeMB: "1024"}
	var buf bytes.Buffer
	if err := SectionImageCache(data, imageCacheTestAssets).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "image_cache_1024", buf.String())
}

func TestSectionLibraries_Populated_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Libraries: twoLibraries}
	var buf bytes.Buffer
	if err := SectionLibraries(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "libraries_populated", buf.String())
}

func TestSectionLibraries_Empty_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Libraries: []library.Library{}}
	var buf bytes.Buffer
	if err := SectionLibraries(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "libraries_empty", buf.String())
}
