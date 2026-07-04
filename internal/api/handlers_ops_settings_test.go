package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
)

// opsTestRouter builds a settings-capable Router with the scanner service and
// rule pipeline wired (buildSettingsData reads their live getters for the
// operational settings) so the ops-display path under test is exercised end to
// end. testRouter/settingsPageTestRouter leave both nil.
func opsTestRouter(t *testing.T) (*Router, *scanner.Service, *rule.Pipeline) {
	t.Helper()
	r := settingsPageTestRouter(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	sc := scanner.NewService(nil, nil, nil, logger, "/music", nil)
	r.scannerService = sc

	artistSvc := artist.NewService(r.db)
	ruleSvc := rule.NewService(r.db)
	engine := rule.NewEngine(ruleSvc, r.db, nil, nil, logger)
	pipeline := rule.NewPipeline(engine, artistSvc, ruleSvc, nil, nil, logger)
	r.pipeline = pipeline

	return r, sc, pipeline
}

func opsRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/next/settings", nil)
}

// TestBuildSettingsData_OpsDisplay_LiveValues: with no SW_* env set, the four
// operational fields reflect the live service values and every EnvPinned flag
// is false (nothing is pinned, so the controls stay editable).
func TestBuildSettingsData_OpsDisplay_LiveValues(t *testing.T) {
	// No t.Parallel: the sibling tests mutate process env via t.Setenv.
	for _, k := range []string{
		"SW_RULE_ENGINE_ARTIST_WORKERS", "SW_SCANNER_EXCLUSIONS",
		"SW_SCANNER_MTIME_FAST_PATH", "SW_BACKUP_INTERVAL",
	} {
		t.Setenv(k, "") // t.Setenv restores the prior value at cleanup.
	}
	if err := os.Unsetenv("SW_RULE_ENGINE_ARTIST_WORKERS"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("SW_SCANNER_EXCLUSIONS"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("SW_SCANNER_MTIME_FAST_PATH"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("SW_BACKUP_INTERVAL"); err != nil {
		t.Fatal(err)
	}

	r, sc, pipeline := opsTestRouter(t)
	pipeline.SetArtistWorkers(7)
	sc.SetExclusions([]string{"Various Artists", "Soundtrack"})
	sc.SetMtimeFastPath(false)

	data, ok := r.buildSettingsData(opsRequest(), "", false)
	if !ok {
		t.Fatal("buildSettingsData returned ok=false")
	}

	if data.ArtistWorkers != 7 {
		t.Errorf("ArtistWorkers = %d, want 7 (live pipeline value)", data.ArtistWorkers)
	}
	if data.ScannerExclusions != "Various Artists, Soundtrack" {
		t.Errorf("ScannerExclusions = %q, want %q (original-case, joined)", data.ScannerExclusions, "Various Artists, Soundtrack")
	}
	if data.ScannerMtimeFastPath {
		t.Errorf("ScannerMtimeFastPath = true, want false (live value)")
	}
	if data.ArtistWorkersEnvPinned || data.ScannerExclusionsEnvPinned ||
		data.ScannerMtimeEnvPinned || data.BackupIntervalEnvPinned {
		t.Errorf("no SW_* set, but an EnvPinned flag is true: %+v", []bool{
			data.ArtistWorkersEnvPinned, data.ScannerExclusionsEnvPinned,
			data.ScannerMtimeEnvPinned, data.BackupIntervalEnvPinned,
		})
	}
}

// TestBuildSettingsData_OpsDisplay_EnvPinned: when the SW_* vars are set, every
// EnvPinned flag is true and the displayed value equals the effective env value
// (the boot overlay applies env to the live services, simulated here, and the
// backup display reads the env var directly).
func TestBuildSettingsData_OpsDisplay_EnvPinned(t *testing.T) {
	t.Setenv("SW_RULE_ENGINE_ARTIST_WORKERS", "9")
	t.Setenv("SW_SCANNER_EXCLUSIONS", "Live, Bootlegs")
	t.Setenv("SW_SCANNER_MTIME_FAST_PATH", "false")
	t.Setenv("SW_BACKUP_INTERVAL", "12")

	r, sc, pipeline := opsTestRouter(t)
	// The boot overlay (applyPersistedOpsSettings) applies env to the live
	// services; replicate that so the effective display equals the env value.
	pipeline.SetArtistWorkers(9)
	sc.SetExclusions([]string{"Live", "Bootlegs"})
	sc.SetMtimeFastPath(false)

	data, ok := r.buildSettingsData(opsRequest(), "", false)
	if !ok {
		t.Fatal("buildSettingsData returned ok=false")
	}

	if !data.ArtistWorkersEnvPinned || !data.ScannerExclusionsEnvPinned ||
		!data.ScannerMtimeEnvPinned || !data.BackupIntervalEnvPinned {
		t.Errorf("all SW_* set, but an EnvPinned flag is false: %+v", []bool{
			data.ArtistWorkersEnvPinned, data.ScannerExclusionsEnvPinned,
			data.ScannerMtimeEnvPinned, data.BackupIntervalEnvPinned,
		})
	}
	if data.ArtistWorkers != 9 {
		t.Errorf("ArtistWorkers = %d, want 9 (env-effective)", data.ArtistWorkers)
	}
	if data.ScannerExclusions != "Live, Bootlegs" {
		t.Errorf("ScannerExclusions = %q, want %q (env-effective)", data.ScannerExclusions, "Live, Bootlegs")
	}
	if data.BackupIntervalHours != 12 {
		t.Errorf("BackupIntervalHours = %d, want 12 (env value shown directly)", data.BackupIntervalHours)
	}
}

// TestBuildSettingsData_BackupInterval_InvalidEnv: a present-but-invalid
// SW_BACKUP_INTERVAL is ignored by the loader and blocks the boot overlay, so
// the value in force is the config default. The display must show 24, not the
// misleading persisted value, and the pin flag stays true.
func TestBuildSettingsData_BackupInterval_InvalidEnv(t *testing.T) {
	for _, bad := range []string{"bogus", "0", "-5"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("SW_BACKUP_INTERVAL", bad)
			r, _, _ := opsTestRouter(t)
			// Persist a distinct value that must NOT be shown when env is bad.
			if _, err := r.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				"backup.interval_hours", "48", "2026-01-01T00:00:00Z"); err != nil {
				t.Fatalf("seeding persisted backup.interval_hours: %v", err)
			}

			data, ok := r.buildSettingsData(opsRequest(), "", false)
			if !ok {
				t.Fatal("buildSettingsData returned ok=false")
			}
			if data.BackupIntervalHours != 24 {
				t.Errorf("BackupIntervalHours = %d, want 24 (default; not the persisted 48)", data.BackupIntervalHours)
			}
			if !data.BackupIntervalEnvPinned {
				t.Errorf("BackupIntervalEnvPinned = false, want true (env var is set)")
			}
		})
	}
}

// TestApplyLiveSettingSideEffects_AppliesToServices: with no env pin, a save
// for the three live-applied ops keys reaches the scanner service and rule
// pipeline so the change takes effect without a restart.
func TestApplyLiveSettingSideEffects_AppliesToServices(t *testing.T) {
	t.Setenv("SW_RULE_ENGINE_ARTIST_WORKERS", "")
	t.Setenv("SW_SCANNER_EXCLUSIONS", "")
	t.Setenv("SW_SCANNER_MTIME_FAST_PATH", "")
	for _, k := range []string{"SW_RULE_ENGINE_ARTIST_WORKERS", "SW_SCANNER_EXCLUSIONS", "SW_SCANNER_MTIME_FAST_PATH"} {
		_ = os.Unsetenv(k)
	}

	r, sc, pipeline := opsTestRouter(t)
	sc.SetMtimeFastPath(true)

	r.applyLiveSettingSideEffects(map[string]string{
		"scanner.exclusions":         "Various Artists, VA",
		"scanner.mtime_fast_path":    "false",
		"rule_engine.artist_workers": "5",
	})

	if got := pipeline.ArtistWorkers(); got != 5 {
		t.Errorf("ArtistWorkers = %d, want 5", got)
	}
	if got, want := sc.Exclusions(), []string{"Various Artists", "VA"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Exclusions = %v, want %v", got, want)
	}
	if sc.MtimeFastPath() {
		t.Errorf("MtimeFastPath = true, want false after save")
	}
}

// TestApplyLiveSettingSideEffects_SkipsEnvPinned: when a key is env-pinned, a
// save must NOT mutate the live service (env-wins); the value stays put.
func TestApplyLiveSettingSideEffects_SkipsEnvPinned(t *testing.T) {
	t.Setenv("SW_RULE_ENGINE_ARTIST_WORKERS", "3")
	t.Setenv("SW_SCANNER_EXCLUSIONS", "Pinned")
	t.Setenv("SW_SCANNER_MTIME_FAST_PATH", "true")

	r, sc, pipeline := opsTestRouter(t)
	pipeline.SetArtistWorkers(3)
	sc.SetExclusions([]string{"Pinned"})
	sc.SetMtimeFastPath(true)

	r.applyLiveSettingSideEffects(map[string]string{
		"scanner.exclusions":         "Override",
		"scanner.mtime_fast_path":    "false",
		"rule_engine.artist_workers": "42",
	})

	if got := pipeline.ArtistWorkers(); got != 3 {
		t.Errorf("ArtistWorkers = %d, want 3 (env-pinned, save skipped)", got)
	}
	if got := sc.Exclusions(); len(got) != 1 || got[0] != "Pinned" {
		t.Errorf("Exclusions = %v, want [Pinned] (env-pinned, save skipped)", got)
	}
	if !sc.MtimeFastPath() {
		t.Errorf("MtimeFastPath = false, want true (env-pinned, save skipped)")
	}
}

// TestApplyLiveSettingSideEffects_NilService: a save for the live-applied keys
// must not panic when the backing service is nil; the branch warn-logs and
// skips instead of silently dereferencing.
func TestApplyLiveSettingSideEffects_NilService(t *testing.T) {
	t.Setenv("SW_RULE_ENGINE_ARTIST_WORKERS", "")
	t.Setenv("SW_SCANNER_EXCLUSIONS", "")
	t.Setenv("SW_SCANNER_MTIME_FAST_PATH", "")
	for _, k := range []string{"SW_RULE_ENGINE_ARTIST_WORKERS", "SW_SCANNER_EXCLUSIONS", "SW_SCANNER_MTIME_FAST_PATH"} {
		_ = os.Unsetenv(k)
	}

	r := settingsPageTestRouter(t)
	r.scannerService = nil
	r.pipeline = nil

	// Must not panic.
	r.applyLiveSettingSideEffects(map[string]string{
		"scanner.exclusions":         "X",
		"scanner.mtime_fast_path":    "true",
		"rule_engine.artist_workers": "5",
	})
}
