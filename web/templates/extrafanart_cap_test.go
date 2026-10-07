package templates

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

func renderCapBody(t *testing.T, view ExtraFanartMigrationView) string {
	t.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator("en"))
	var buf bytes.Buffer
	if err := ExtraFanartMigrationBody(view).Render(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func capRows(n int) []ExtraFanartMigrationRow {
	rows := make([]ExtraFanartMigrationRow, n)
	for i := range rows {
		rows[i] = ExtraFanartMigrationRow{ArtistID: "a", Artist: "Alpha", File: fmt.Sprintf("img%04d.jpg", i), Dest: "fanart.jpg", Outcome: "planned"}
	}
	return rows
}

// More rows than the cap render exactly the cap, with one line naming both
// numbers (thousands separated); the summary tiles still count everything.
func TestExtraFanartMigrationBody_CapsRowsAndSaysSo(t *testing.T) {
	t.Parallel()
	view := ExtraFanartMigrationView{Status: "planned", Moves: 1201, Rows: capRows(1201), RowCap: 1000}
	body := renderCapBody(t, view)
	if n := strings.Count(body, "border-t border-gray-100"); n != 1000 {
		t.Errorf("want exactly 1000 rows, got %d", n)
	}
	if !strings.Contains(body, `id="extrafanart-migration-capped"`) || !strings.Contains(body, "Showing the first 1,000 of 1,201 rows") {
		t.Error("want the line with both numbers")
	}
	if strings.Contains(body, "img1000.jpg") || !strings.Contains(body, "img0999.jpg") {
		t.Error("the cap must keep the first rows and drop the rest")
	}
	if !strings.Contains(body, `id="extrafanart-migration-moves">1201<`) {
		t.Error("the summary tile must keep counting all files")
	}
}

// At the cap, and under it, nothing is left out and no line is shown. The default
// cap is the named constant.
func TestExtraFanartMigrationBody_NoNoteAtOrUnderTheCap(t *testing.T) {
	t.Parallel()
	for _, view := range []ExtraFanartMigrationView{
		{Status: "planned", Rows: capRows(3), RowCap: 3},
		{Status: "planned", Rows: capRows(2), RowCap: 3},
		{Status: "planned", Rows: capRows(ExtraFanartMaxRows)},
	} {
		body := renderCapBody(t, view)
		if strings.Contains(body, "extrafanart-migration-capped") {
			t.Errorf("%d rows must not show the capped line", len(view.Rows))
		}
		if n := strings.Count(body, "border-t border-gray-100"); n != len(view.Rows) {
			t.Errorf("want all %d rows, got %d", len(view.Rows), n)
		}
	}
	if ExtraFanartMaxRows != 500 {
		t.Errorf("the default cap is documented as 500, got %d", ExtraFanartMaxRows)
	}
}
