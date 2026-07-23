package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestScan_PerFieldLockSurvivesRescan reproduces the issue #2749 production
// failure end to end, at the layer where an operator actually experienced it.
//
// Sequence: the library is scanned, the operator hand-edits a field and pins it
// with a per-field lock, then the library is scanned again. The artist is NOT
// whole-artist locked (artists.locked stays false), so the rescan re-parses the
// NFO -- which is exactly the path that used to overwrite the pinned value,
// because it called ApplyMetadata with a zero MergeOptions and the lock never
// reached the merge engine.
//
// Two NFO shapes are exercised: one that omits the element entirely (an
// unconditional merge of "" clears the field) and one that carries a competing
// value.
func TestScan_PerFieldLockSurvivesRescan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		nfo  string
	}{
		{
			name: "nfo omits the element",
			nfo: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <biography>Bristol group.</biography>
</artist>`,
		},
		{
			name: "nfo carries a different value",
			nfo: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <biography>Bristol group.</biography>
  <yearsactive>1991-2010</yearsactive>
</artist>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			libDir := t.TempDir()
			artistDir := filepath.Join(libDir, "Massive Attack")
			createArtistDirWithNFO(t, libDir, "Massive Attack", tc.nfo)

			svc, artistSvc := setupScanner(t, libDir)
			ctx := context.Background()

			if _, err := svc.Run(ctx); err != nil {
				t.Fatalf("initial Run: %v", err)
			}
			waitForScan(t, svc, 5*time.Second)

			a, err := artistSvc.GetByPath(ctx, artistDir)
			if err != nil || a == nil {
				t.Fatalf("artist not found after initial scan: %v", err)
			}
			if a.Locked {
				t.Fatal("precondition failed: artist must not be whole-artist locked, " +
					"or the rescan would skip NFO parsing and the test would pass vacuously")
			}

			// The operator hand-edits the field and pins it.
			a.YearsActive = "1988-present"
			if err := artistSvc.Update(ctx, a); err != nil {
				t.Fatalf("updating artist: %v", err)
			}
			if err := artistSvc.SetLockedFields(ctx, a.ID, []string{string(artist.FieldYearsActive)}); err != nil {
				t.Fatalf("setting locked fields: %v", err)
			}

			// Precondition: the lock and the value are both persisted, so a
			// failure below is the merge path and not a bad setup.
			pinned, err := artistSvc.GetByID(ctx, a.ID)
			if err != nil || pinned == nil {
				t.Fatalf("re-reading artist: %v", err)
			}
			if pinned.YearsActive != "1988-present" {
				t.Fatalf("precondition failed: hand-edited value not persisted, got %q", pinned.YearsActive)
			}
			if len(pinned.LockedFields) != 1 || pinned.LockedFields[0] != "years_active" {
				t.Fatalf("precondition failed: lock not persisted, got %v", pinned.LockedFields)
			}

			// Touch the NFO so the rescan definitely reprocesses the directory.
			nfoPath := filepath.Join(artistDir, "artist.nfo")
			if err := os.WriteFile(nfoPath, []byte(tc.nfo), 0o644); err != nil {
				t.Fatalf("rewriting nfo: %v", err)
			}

			if _, err := svc.Run(ctx); err != nil {
				t.Fatalf("rescan Run: %v", err)
			}
			waitForScan(t, svc, 5*time.Second)

			after, err := artistSvc.GetByID(ctx, a.ID)
			if err != nil || after == nil {
				t.Fatalf("re-reading artist after rescan: %v", err)
			}
			if after.YearsActive != "1988-present" {
				t.Errorf("locked years_active was overwritten by the rescan: got %q, want %q",
					after.YearsActive, "1988-present")
			}
			// Anti-regression: the lock must not have disabled NFO merging
			// wholesale -- the unlocked biography still comes from the NFO.
			if after.Biography != "Bristol group." {
				t.Errorf("unlocked biography did not merge from the NFO: got %q", after.Biography)
			}
		})
	}
}
