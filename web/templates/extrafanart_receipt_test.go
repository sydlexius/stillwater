package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// Every receipt status takes the right role, accent and body text. An aborted
// run is never quiet, whatever status it carries.
func TestExtraFanartReceipt_RoleAccentAndBodyPerStatus(t *testing.T) {
	t.Parallel()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator("en"))
	rows := []struct {
		name  string
		view  ExtraFanartMigrationView
		alert bool
		body  string
	}{
		{"migrated", ExtraFanartMigrationView{Status: "migrated"}, false, "Every file that was due to move was moved"},
		{"nothing_to_do", ExtraFanartMigrationView{Status: "nothing_to_do"}, false, "No extrafanart files needed moving"},
		{"nothing_checked", ExtraFanartMigrationView{Status: "nothing_checked"}, false, "some artist folders were not found"},
		{"partial", ExtraFanartMigrationView{Status: "partial"}, true, "not cleanly"},
		{"failed", ExtraFanartMigrationView{Status: "failed"}, true, "no file was moved"},
		{"aborted but migrated", ExtraFanartMigrationView{Status: "migrated", Aborted: true}, true, "stopped before it finished"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.view.Receipt = true
			var buf bytes.Buffer
			if err := ExtraFanartMigrationBody(tc.view).Render(ctx, &buf); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.body) {
				t.Errorf("receipt body text %q missing", tc.body)
			}
			if got := strings.Contains(out, `role="alert"`); got != tc.alert {
				t.Errorf("alert role: got %t, want %t", got, tc.alert)
			}
			if got := strings.Contains(out, "sw-card-accent-amber"); got != tc.alert {
				t.Errorf("amber accent: got %t, want %t", got, tc.alert)
			}
			if !tc.alert && !strings.Contains(out, `role="status"`) {
				t.Error("a clean receipt must be role=status")
			}
		})
	}
}
