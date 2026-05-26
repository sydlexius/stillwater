package foreign

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestScanner_BaselineFirstScanRecordsToAllowlist pins #1584: on the
// first scan of a fresh install (baseline flag unset), every detected
// foreign file lands in the global content-hash allowlist instead of the
// alert ledger. The OOBE summary then surfaces the count as
// informational copy, not as 325 red-banner incidents.
func TestScanner_BaselineFirstScanRecordsToAllowlist(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("legacy-image-bytes"))
	mustWrite(t, filepath.Join(dir, "fanart.jpg"), []byte("another-image-bytes"))

	if _, err := db.Exec(
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`,
		"a1", "Test Artist", dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "Test Artist", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Ledger must be empty: nothing got recorded as an alert.
	ledger, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List ledger: %v", err)
	}
	if len(ledger) != 0 {
		t.Fatalf("baseline scan must NOT write to ledger; got %d rows: %#v", len(ledger), ledger)
	}

	// Allowlist now contains both files (global scope).
	allow, err := repo.ListAllowlist(context.Background())
	if err != nil {
		t.Fatalf("ListAllowlist: %v", err)
	}
	if len(allow) != 2 {
		t.Fatalf("expected 2 allowlist rows; got %d: %#v", len(allow), allow)
	}
	names := map[string]bool{}
	for _, e := range allow {
		if e.Scope != ScopeGlobal {
			t.Errorf("allowlist row %q must be global; got %q", e.FileName, e.Scope)
		}
		names[e.FileName] = true
	}
	for _, want := range []string{"backdrop.jpg", "fanart.jpg"} {
		if !names[want] {
			t.Errorf("expected %q in allowlist; got %#v", want, names)
		}
	}

	// Baseline flag must now be flipped so subsequent scans alert.
	var done string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_completed'`).Scan(&done); err != nil {
		t.Fatalf("scanning baseline flag: %v", err)
	}
	if done != "true" {
		t.Errorf("baseline_completed: got %q, want true", done)
	}
	var count string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_count'`).Scan(&count); err != nil {
		t.Fatalf("scanning baseline count: %v", err)
	}
	if count != "2" {
		t.Errorf("baseline_count: got %q, want 2", count)
	}
}

// TestScanner_SecondScanDetectsNewForeign exercises the post-baseline
// behavior: with the baseline flag set, a NEW file appearing in an
// artist directory must surface as an alert in the ledger. Pre-existing
// files that were baselined remain quiet because their content_hash is
// in the allowlist.
func TestScanner_SecondScanDetectsNewForeign(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)
	ctx := context.Background()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("baseline-bytes"))
	if _, err := db.Exec(
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`,
		"a1", "Test Artist", dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "Test Artist", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// First scan: baselines the pre-existing file.
	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	ledger, _ := repo.List(ctx)
	if len(ledger) != 0 {
		t.Fatalf("ledger after baseline must be empty; got %d", len(ledger))
	}

	// Now a media server appends a new artwork file to the same artist.
	mustWrite(t, filepath.Join(dir, "fanart.jpg"), []byte("intruder-bytes"))

	if err := scanner.Scan(ctx); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	ledger, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List ledger: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("expected one new alert; got %d: %#v", len(ledger), ledger)
	}
	if ledger[0].FileName != "fanart.jpg" {
		t.Errorf("alert file_name: got %q, want fanart.jpg", ledger[0].FileName)
	}
}

// TestScanner_BaselineSurvivesContextCancel verifies the "fail soft"
// path: if the baseline scan is canceled mid-flight, the partial
// allowlist is kept but the baseline_completed flag is NOT flipped, so
// the next scan resumes in baseline mode and admits the remaining
// artists. Without this, a canceled OOBE step would silently fall
// through to alert mode on retry.
func TestScanner_BaselineSurvivesContextCancel(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Scan runs -> no work done

	// One artist directory with a foreign file so a real scan would
	// produce one allowlist row. With the canceled context, we expect
	// zero (the scanner short-circuits before iterating).
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("payload"))
	if _, err := db.Exec(
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`,
		"a1", "A", dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Cancellation propagates from the lister probe or the first
	// QueryRow; either way the function must NOT mark baseline as done.
	_ = scanner.Scan(ctx)

	var v string
	err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_completed'`).Scan(&v)
	if err == nil {
		// Row exists -> flag was set. That would be wrong.
		t.Errorf("baseline_completed must not be set on a canceled scan; got %q", v)
	}
}
