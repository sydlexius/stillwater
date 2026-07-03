package templates

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestArtistsTable_TypeLabelNormalized verifies that the promoted table row
// renders the type cell via ArtistTypeLabel, not the raw database value
// (#1843; retargeted from the deleted stable ArtistRow in #1757 PR-3a).
// "solo act" is the canonical MusicBrainz string for a person-type artist and
// must display as the localized "Person" label, not the raw "solo act" string.
func TestArtistsTable_TypeLabelNormalized(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	data := ArtistListData{
		Artists: []artist.Artist{{
			ID:   "test-id",
			Name: "Test Artist",
			Type: "solo act",
		}},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsTable(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// The raw "solo act" string must not appear in the type cell output.
	if strings.Contains(out, "solo act") {
		t.Errorf("ArtistsTable rendered raw type %q; want normalized label via ArtistTypeLabel", "solo act")
	}
	// The normalized label ("Person") must be present.
	personLabel := ArtistTypeLabel(ctx, "solo act")
	if !strings.Contains(out, personLabel) {
		t.Errorf("ArtistsTable missing normalized type label %q (raw: %q)", personLabel, "solo act")
	}
}

// TestArtistsTable_TableHeaderNoUppercase verifies the promoted ArtistsTable
// renders column headers in Title Case (no CSS uppercase class on <th>
// elements) so that i18n strings render naturally (#1843).
func TestArtistsTable_TableHeaderNoUppercase(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	var buf bytes.Buffer
	if err := ArtistsTable(ArtistListData{}).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// No <th> may carry the uppercase token in its class list, regardless of
	// where it sits in the class string (exact-sequence matching let class
	// reordering hide a regression).
	uppercaseTH := regexp.MustCompile(`<th[^>]*class="[^"]*\buppercase\b[^"]*"`)
	if uppercaseTH.MatchString(out) {
		t.Errorf("ArtistsTable <th> still carries CSS uppercase class; Title Case i18n strings will render ALL CAPS")
	}
}
