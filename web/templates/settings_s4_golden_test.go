package templates

// settings_s4_golden_test.go -- regression gate for M55 issue #1809 (S4
// System: the Maintenance + Logs tab cards -- Confirm dialogs, Database
// maintenance, Backup, Settings export/import, Log settings, Log viewer --
// extracted into shared Section* templ funcs).
//
// Two complementary gates, both fully self-contained (no temp files, never
// skipped -- so CI always exercises them):
//
//  1. TestS4_PageComposesSections renders SettingsPage and asserts each
//     extracted Section* func's output appears verbatim in the page. This is
//     the durable invariant the extraction guaranteed: the stable chrome
//     composes the SAME section bodies the shared funcs emit, so the page and
//     the future next/ chrome cannot silently diverge.
//
//  2. The per-section golden tests (TestSection*_S4_Golden) render each Section*
//     func in isolation and compare against
//     web/templates/testdata/section_*.golden.html. Regenerate with -update.
//
// Shared test helpers (updateGolden flag, goldenPath, checkOrUpdateGolden,
// testCtx) are defined in settings_s1_golden_test.go / helpers_test.go and
// reused here.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// s4MaintenanceData populates the value-bearing fields the Maintenance cards
// render: the auto-maintenance schedule selector and the two backup-retention
// controls.  The chosen values exercise a non-default selected <option> in each
// selector so the goldens lock the selected-state markup.
var s4MaintenanceData = SettingsData{
	MaintIntervalHours: 24,
	BackupRetention:    7,
	BackupMaxAgeDays:   30,
	// Operational settings surfaced from env-only (#1746, #1753); non-default
	// values lock the rendered initial state of the new System-group cards.
	ArtistWorkers:        2,
	ScannerExclusions:    "Various Artists, Soundtrack",
	ScannerMtimeFastPath: true,
	BackupIntervalHours:  24,
}

// s4OpsEnvPinnedData renders the ops cards in their env-pinned state (env-wins,
// AC4): every control is read-only, its Save button suppressed, and the
// backup restart banner replaced by the "managed by environment" hint. The
// goldens lock that read-only markup so a regression that re-enables a save on
// an env-pinned control is caught.
var s4OpsEnvPinnedData = SettingsData{
	ArtistWorkers:              9,
	ScannerExclusions:          "Live, Bootlegs",
	ScannerMtimeFastPath:       false,
	BackupIntervalHours:        12,
	ArtistWorkersEnvPinned:     true,
	ScannerExclusionsEnvPinned: true,
	ScannerMtimeEnvPinned:      true,
	BackupIntervalEnvPinned:    true,
}

// TestS4_PageComposesSections asserts that SettingsPage embeds each extracted
// Section* func's rendered output verbatim.  If a future edit stops the page
// from composing a shared section (or forks its markup inline), the substring
// check fails.
func TestS4_PageComposesSections(t *testing.T) {
	ctx := testCtx(t)
	// One fixture populating the Maintenance-tab value fields.  The Logs cards
	// render no data-bound values, so the same fixture covers them.  Every
	// panel renders even when hidden (it is only visually hidden via a class),
	// so all six section bodies are present in the page output.
	data := s4MaintenanceData
	data.ActiveTab = TabMaintenance

	var page bytes.Buffer
	if err := SettingsPage(AssetPaths{}, data).Render(ctx, &page); err != nil {
		t.Fatalf("render SettingsPage: %v", err)
	}
	pageStr := page.String()

	check := func(name string, render func(context.Context, io.Writer) error) {
		t.Helper()
		var b bytes.Buffer
		if err := render(ctx, &b); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if !strings.Contains(pageStr, b.String()) {
			t.Errorf("SettingsPage output does not contain %s render verbatim -- the page no longer composes the shared section", name)
		}
	}

	check("SectionConfirmDialogs", SectionConfirmDialogs(data).Render)
	check("SectionDatabaseMaintenance", SectionDatabaseMaintenance(data).Render)
	check("SectionBackup", SectionBackup(data).Render)
	check("SectionExportImport", SectionExportImport(data).Render)
	check("SectionLogSettings", SectionLogSettings(data).Render)
	check("SectionLogViewer", SectionLogViewer(data).Render)
}

// TestSettingsSections_S4_Golden renders each extracted System Section* func in
// isolation and compares it against its committed golden file.  The cases are
// table-driven (one t.Run subtest per section) so each section gets isolated
// failure output and adding a future section is a one-line change.  Regenerate
// the goldens with -update after an intentional markup change.
func TestSettingsSections_S4_Golden(t *testing.T) {
	tests := []struct {
		name   string
		render func(context.Context, io.Writer) error
	}{
		{name: "confirm_dialogs", render: SectionConfirmDialogs(s4MaintenanceData).Render},
		{name: "db_maintenance", render: SectionDatabaseMaintenance(s4MaintenanceData).Render},
		{name: "backup", render: SectionBackup(s4MaintenanceData).Render},
		{name: "export_import", render: SectionExportImport(s4MaintenanceData).Render},
		{name: "log_settings", render: SectionLogSettings(s4MaintenanceData).Render},
		{name: "log_viewer", render: SectionLogViewer(s4MaintenanceData).Render},
		{name: "scanner_ops", render: SectionScannerOps(s4MaintenanceData).Render},
		{name: "backup_schedule", render: SectionBackupSchedule(s4MaintenanceData).Render},
		{name: "scanner_ops_env_pinned", render: SectionScannerOps(s4OpsEnvPinnedData).Render},
		{name: "backup_schedule_env_pinned", render: SectionBackupSchedule(s4OpsEnvPinnedData).Render},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testCtx(t)
			var buf bytes.Buffer
			if err := tt.render(ctx, &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			checkOrUpdateGolden(t, tt.name, buf.String())
		})
	}
}
