package templates

import (
	"strings"
	"testing"
	"time"
)

// The as-of line and the Refresh button follow the state of the preview, and a
// swapped-in fragment has exactly one element that takes focus.
func TestExtraFanartMigrationBody_AsOfRefreshAndFocus(t *testing.T) {
	t.Parallel()
	at := time.Now().Add(-3*time.Minute - 10*time.Second)
	rows := capRows(1)
	cases := []struct {
		name      string
		view      ExtraFanartMigrationView
		asOf      bool
		refresh   bool
		autofocus int
	}{
		{"page load", ExtraFanartMigrationView{Status: "planned", Moves: 1, Rows: rows, AsOf: at}, true, true, 0},
		{"fragment", ExtraFanartMigrationView{Status: "planned", Moves: 1, Rows: rows, AsOf: at, Fragment: true}, true, true, 1},
		{"zero stamp", ExtraFanartMigrationView{Status: "planned", Moves: 1, Rows: rows}, false, true, 0},
		{"empty state", ExtraFanartMigrationView{Status: "nothing_to_do", AsOf: at, Fragment: true}, true, true, 1},
		{"stopped with rows", ExtraFanartMigrationView{Status: "planned", Rows: rows, Aborted: true, AsOf: at, Fragment: true}, false, true, 1},
		{"stopped, no rows", ExtraFanartMigrationView{Aborted: true, AsOf: at, Fragment: true}, false, true, 1},
		{"receipt", ExtraFanartMigrationView{Status: "migrated", Receipt: true, Moved: 1, Rows: rows, AsOf: at, Fragment: true}, false, false, 1},
		{"running", ExtraFanartMigrationView{Running: true, AsOf: at, Fragment: true}, false, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := renderCapBody(t, tc.view)
			if got := strings.Contains(body, `id="extrafanart-migration-as-of"`); got != tc.asOf {
				t.Errorf("as-of line present = %v, want %v", got, tc.asOf)
			}
			if tc.asOf && !strings.Contains(body, "Preview as of 3m ago.") {
				t.Error("the as-of line must show the relative time")
			}
			if got := strings.Contains(body, `id="extrafanart-migration-refresh-button"`); got != tc.refresh {
				t.Errorf("refresh button present = %v, want %v", got, tc.refresh)
			}
			if n := strings.Count(body, "autofocus"); n != tc.autofocus {
				t.Errorf("autofocus count = %d, want %d", n, tc.autofocus)
			}
		})
	}
}

// Run and Refresh swap the same body, so each must disable the other while its
// request is out (a Refresh during a run drops the run's receipt), and the stopped
// preview's live region must be the body paragraph, never the card holding the
// focused title (it would be announced twice).
func TestExtraFanartMigrationBody_ControlsDisableEachOtherAndErrorAlertIsOnTheBody(t *testing.T) {
	t.Parallel()
	body := renderCapBody(t, ExtraFanartMigrationView{Status: "planned", Moves: 1, Rows: capRows(1), AsOf: time.Now()})
	both := `hx-disabled-elt="#extrafanart-migration-run-button, #extrafanart-migration-refresh-button"`
	if n := strings.Count(body, both); n != 2 {
		t.Errorf("both buttons must disable BOTH controls while in flight, found %d of 2", n)
	}
	stopped := renderCapBody(t, ExtraFanartMigrationView{Aborted: true, Fragment: true})
	card := stopped[strings.Index(stopped, `id="extrafanart-migration-error"`):]
	card = card[:strings.Index(card, "</div>")]
	if strings.Contains(strings.SplitN(card, ">", 2)[0], `role="alert"`) {
		t.Error("role=alert must not sit on the card that holds the focused title")
	}
	if !strings.Contains(card, `role="alert">`) {
		t.Error("the error body paragraph must carry role=alert")
	}
}
