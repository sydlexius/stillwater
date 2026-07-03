package templates

// artist_detail_field_locks_golden_test.go -- golden test for
// artistFieldLocksPanel (M55 #1754 parity-gate: port the v1 field-locks
// summary panel to next/). Renders the panel in isolation for the two
// interactive states (has locked fields / no locked fields) and compares
// against committed golden HTML, following the settings golden-test pattern
// (web/templates/settings_s1_golden_test.go).
//
// Run with -update to regenerate the golden files after an intentional
// template change.

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

var updateFieldLocksGolden = flag.Bool("update-field-locks-golden", false, "regenerate field-locks golden files")

const fieldLocksGoldenDir = "testdata"

func fieldLocksGoldenPath(name string) string {
	return filepath.Join(fieldLocksGoldenDir, "field_locks_"+name+".golden.html")
}

func checkOrUpdateFieldLocksGolden(t *testing.T, name, rendered string) {
	t.Helper()
	path := fieldLocksGoldenPath(name)
	if *updateFieldLocksGolden {
		if err := os.MkdirAll(fieldLocksGoldenDir, 0755); err != nil {
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
		t.Fatalf("golden file %s missing; run with -update-field-locks-golden to generate it", path)
	}
	if string(golden) != rendered {
		diffPos := -1
		for i := 0; i < len(golden) && i < len(rendered); i++ {
			if golden[i] != rendered[i] {
				diffPos = i
				break
			}
		}
		if diffPos == -1 {
			diffPos = min(len(golden), len(rendered))
		}
		t.Errorf("%q does not match golden %s (first diff at byte %d, golden len=%d, rendered len=%d)",
			name, path, diffPos, len(golden), len(rendered))
	}
}

// fieldLocksPageData builds an ArtistDetailPageData with the given locked
// fields for the panel's fixtures.
func fieldLocksPageData(lockedFields []string) ArtistDetailPageData {
	data := detailPageData(nil, nil)
	data.Detail.Artist = artist.Artist{
		ID:           "art-1",
		Name:         "Render Test Artist",
		Type:         "Group",
		Path:         "/music/Render Test Artist",
		LockedFields: lockedFields,
	}
	return data
}

// TestArtistFieldLocksPanel_HasLockedFields_Golden covers the populated state:
// one chip per locked field, each with a working unlock affordance.
func TestArtistFieldLocksPanel_HasLockedFields_Golden(t *testing.T) {
	data := fieldLocksPageData([]string{"biography", "genres"})

	var buf bytes.Buffer
	if err := artistFieldLocksPanel(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`id="next-field-locks-art-1"`,
		"biography",
		"genres",
		`hx-delete="/api/v1/artists/art-1/field-locks/biography"`,
		`hx-delete="/api/v1/artists/art-1/field-locks/genres"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered panel missing %q", want)
		}
	}

	checkOrUpdateFieldLocksGolden(t, "present", out)
}

// TestArtistFieldLocksPanel_NoLockedFields_Golden covers the empty state: the
// panel must render nothing at all (v1 parity -- no empty amber card).
func TestArtistFieldLocksPanel_NoLockedFields_Golden(t *testing.T) {
	data := fieldLocksPageData(nil)

	var buf bytes.Buffer
	if err := artistFieldLocksPanel(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if strings.TrimSpace(out) != "" {
		t.Errorf("empty-state panel should render nothing, got %q", out)
	}
}

// TestArtistDetailPage_FieldLocksPanelMounted verifies the panel is wired into
// the full page render (fixed after the hero, not inside the reorderable
// section list) when the artist has locked fields.
func TestArtistDetailPage_FieldLocksPanelMounted(t *testing.T) {
	t.Parallel()
	data := fieldLocksPageData([]string{"name"})

	var buf bytes.Buffer
	if err := ArtistDetailPage(AssetPaths{}, data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	heroIdx := strings.Index(out, `id="next-hero-art-1"`)
	sortableIdx := strings.Index(out, "data-sw-sortable-section")
	locksIdx := strings.Index(out, `id="next-field-locks-art-1"`)
	if heroIdx == -1 || sortableIdx == -1 || locksIdx == -1 {
		t.Fatalf("expected hero, field-locks panel, and sortable region all present: hero=%d locks=%d sortable=%d", heroIdx, locksIdx, sortableIdx)
	}
	if heroIdx >= locksIdx || locksIdx >= sortableIdx {
		t.Errorf("field-locks panel must render after the hero and before the reorderable region, got hero=%d locks=%d sortable=%d", heroIdx, locksIdx, sortableIdx)
	}
}
