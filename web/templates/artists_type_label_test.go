package templates

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestArtistRow_TypeLabelNormalized verifies that the stable ArtistRow renders
// the type cell via ArtistTypeLabel, not the raw database value (#1843).
// "solo act" is the canonical MusicBrainz string for a person-type artist and
// must display as the localized "Person" label, not the raw "solo act" string.
func TestArtistRow_TypeLabelNormalized(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	a := artist.Artist{
		ID:   "test-id",
		Name: "Test Artist",
		Type: "solo act",
	}
	var buf bytes.Buffer
	if err := ArtistRow(a, nil, nil, nil, "", false).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// The raw "solo act" string must not appear in the type cell output.
	if strings.Contains(out, "solo act") {
		t.Errorf("ArtistRow rendered raw type %q; want normalized label via ArtistTypeLabel", "solo act")
	}
	// The normalized label ("Person") must be present.
	personLabel := ArtistTypeLabel(ctx, "solo act")
	if !strings.Contains(out, personLabel) {
		t.Errorf("ArtistRow missing normalized type label %q (raw: %q)", personLabel, "solo act")
	}
}

// TestArtistRow_TableHeaderNoUppercase verifies the stable ArtistTable renders
// column headers in Title Case (no CSS uppercase class on <th> elements) so
// that i18n strings render naturally (#1843).
func TestArtistRow_TableHeaderNoUppercase(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	var buf bytes.Buffer
	if err := ArtistTable(ArtistListData{}).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// No <th> may carry the uppercase token in its class list, regardless of
	// where it sits in the class string (exact-sequence matching let class
	// reordering hide a regression).
	uppercaseTH := regexp.MustCompile(`<th[^>]*class="[^"]*\buppercase\b[^"]*"`)
	if uppercaseTH.MatchString(out) {
		t.Errorf("ArtistTable <th> still carries CSS uppercase class; Title Case i18n strings will render ALL CAPS")
	}
}
