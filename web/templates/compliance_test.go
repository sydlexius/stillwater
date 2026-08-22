package templates

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/library"
)

// TestComplianceActiveChips_CarrySelectSel guards #3099: dismissing a
// compliance filter chip must extract just the results shell from the
// response, not swap the response in wholesale.
//
// The compliance pane is reachable at two URLs. /reports/compliance has a
// fragment handler (handleCompliancePage returns ComplianceResults on an
// HTMX request), but the promoted /reports workspace route
// (serveReportsWorkspace) does not -- it always renders the full ReportsPage.
// DismissFilterChip reloads whatever window.location.pathname currently is,
// so a chip dismissed while the browser sits on the bare workspace path gets
// back a full page. Without SelectSel, htmx's makeFragment turns that into a
// fragment of the entire body and the chip's dismiss swaps a second copy of
// the whole page into #compliance-results. Measured at ~80 duplicated DOM
// ids before this fix.
//
// This is a Go-level test of the ARGUMENTS the chip spec carries; it cannot
// see the rendered swap itself (the tests/unit vm-based suite and the a11y
// browser spec cover that). It exists so a future edit to
// complianceActiveChips cannot silently drop SelectSel on one branch while
// leaving the others alone.
func TestComplianceActiveChips_CarrySelectSel(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)

	data := ComplianceData{
		Status:         "non_compliant",
		Filter:         "missing_nfo",
		LibraryID:      "lib-1",
		Libraries:      []library.Library{{ID: "lib-1", Name: "Music"}},
		HealthScoreMin: 10,
		HealthScoreMax: 90,
	}

	chips := complianceActiveChips(ctx, data)
	if len(chips) != 5 {
		t.Fatalf("got %d chips, want 5 (one per active dimension): %+v", len(chips), chips)
	}
	for _, c := range chips {
		if c.SelectSel != "#compliance-results" {
			t.Errorf("chip[%q].SelectSel = %q, want %q; without it, dismissing this chip from the "+
				"reports workspace (which has no fragment handler) swaps a whole second copy of the "+
				"page into the results region", c.Key, c.SelectSel, "#compliance-results")
		}
	}
}

// TestComplianceActiveChips_NoneActive proves the empty case renders no
// chips at all -- SelectSel has nothing to be missing from when nothing is
// active, and this pins that an unfiltered request stays chip-free.
func TestComplianceActiveChips_NoneActive(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)

	chips := complianceActiveChips(ctx, ComplianceData{Status: "all"})
	if len(chips) != 0 {
		t.Fatalf("got %d chips for an unfiltered request, want 0: %+v", len(chips), chips)
	}
}
