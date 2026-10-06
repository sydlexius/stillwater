package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// scanRows returns the artist's history rows for field recorded by a scan.
func scanRows(t *testing.T, h *artist.HistoryService, artistID, field string) []artist.MetadataChange {
	t.Helper()
	rows, _, err := h.List(context.Background(), artistID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	var out []artist.MetadataChange
	for _, row := range rows {
		if row.Source == "scan" && row.Field == field {
			out = append(out, row)
		}
	}
	return out
}

// TestNFOSuppliedFields_SkipsBlankedAndUnmovedFields pins the two exclusions
// in the overlay (#3078). A field the NFO merge left alone is not stamped, and
// neither is one that ended up EMPTY: the merge never empties a field, so a
// blank (a gender cleared because it does not apply to a group) was not read
// out of the NFO and must not be recorded as if it were.
func TestNFOSuppliedFields_SkipsBlankedAndUnmovedFields(t *testing.T) {
	t.Parallel()
	before := trackedFieldValues(&artist.Artist{Biography: "old bio", Gender: "female", Origin: "Bristol"})
	after := &artist.Artist{Biography: "new bio", Gender: "", Origin: "Bristol"}

	got := nfoSuppliedFields(before, after)
	if len(got) != 1 || got["biography"] != artist.ProducerNFO {
		t.Errorf("overlay = %v, want only biography stamped nfo", got)
	}
}

func producerNFO(bio string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <biography>` + bio + `</biography>
</artist>`
}

// TestScan_RescanStampsNFOProducerPerField pins what a re-scan records as the
// history producer (#3078), in two phases.
//
// Phase 1 is the ordinary case: the NFO on disk changed, so the re-scan moves
// the biography and the row must say the NFO supplied it.
//
// Phase 2 is why the stamp is per field rather than one value for the whole
// write. The scanner loads every artist once at the start of a library pass
// and writes the whole row back later, so a field can move for a reason the
// NFO had nothing to do with: here an edit lands after the scanner's load, and
// the scanner's stale copy overwrites it. That row is recorded as unrecorded
// ("") -- stamping it "nfo" would be a false record.
func TestScan_RescanStampsNFOProducerPerField(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	artistDir := filepath.Join(libDir, "Massive Attack")
	nfoPath := filepath.Join(artistDir, "artist.nfo")
	createArtistDirWithNFO(t, libDir, "Massive Attack", producerNFO("first bio"))

	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	historySvc := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(historySvc)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)
	a, err := artistSvc.GetByPath(ctx, artistDir)
	if err != nil || a == nil {
		t.Fatalf("artist not found after initial scan: %v", err)
	}
	// PRECONDITIONS: the first scan imported the NFO and recorded no scan
	// history, and the artist is not locked (a locked artist skips the NFO).
	if a.Biography != "first bio" || a.Locked {
		t.Fatalf("fixture: biography = %q, locked = %v; want the NFO text on an unlocked artist", a.Biography, a.Locked)
	}
	if n := len(scanRows(t, historySvc, a.ID, "biography")); n != 0 {
		t.Fatalf("fixture: %d scan rows before the re-scan, want 0", n)
	}

	// Phase 1: the NFO changes, a full re-scan picks it up.
	if err := os.WriteFile(nfoPath, []byte(producerNFO("second bio")), 0o644); err != nil {
		t.Fatalf("rewriting nfo: %v", err)
	}
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("rescan Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)
	bioRows := scanRows(t, historySvc, a.ID, "biography")
	if len(bioRows) != 1 {
		t.Fatalf("biography scan rows = %d, want exactly 1", len(bioRows))
	}
	if bioRows[0].NewValue != "second bio" || bioRows[0].Producer != artist.ProducerNFO {
		t.Errorf("biography row new value/producer = %q/%q, want the NFO text stamped nfo",
			bioRows[0].NewValue, bioRows[0].Producer)
	}

	// Phase 2: the scanner holds a copy loaded BEFORE an edit to a field the
	// NFO does not carry, then re-scans with that stale copy.
	stale, err := artistSvc.GetByPath(ctx, artistDir)
	if err != nil || stale == nil {
		t.Fatalf("loading the scanner's copy: %v", err)
	}
	if _, err := artistSvc.UpdateField(ctx, a.ID, "origin", "Bristol"); err != nil {
		t.Fatalf("editing origin after the load: %v", err)
	}
	// PRECONDITION: the edit is stored and the scanner's copy does not have it.
	if stored, err := artistSvc.GetByID(ctx, a.ID); err != nil || stored.Origin != "Bristol" || stale.Origin != "" {
		t.Fatalf("fixture: stored origin / stale origin not as expected (err %v)", err)
	}
	if err := os.WriteFile(nfoPath, []byte(producerNFO("third bio")), 0o644); err != nil {
		t.Fatalf("rewriting nfo: %v", err)
	}
	detected, err := svc.detectFilesWithFastPath(artistDir, stale)
	if err != nil {
		t.Fatalf("detecting files: %v", err)
	}
	if err := svc.processExistingArtist(ctx, artistDir, "", stale, false, detected, &ScanResult{}); err != nil {
		t.Fatalf("processExistingArtist: %v", err)
	}

	// The NFO-supplied field is still stamped nfo in the same write. The row
	// is found by its value, not its position: history timestamps have
	// one-second precision, so two rows written this fast have no stable order.
	var third []artist.MetadataChange
	for _, row := range scanRows(t, historySvc, a.ID, "biography") {
		if row.NewValue == "third bio" {
			third = append(third, row)
		}
	}
	if len(third) != 1 || third[0].Producer != artist.ProducerNFO {
		t.Errorf("rows for the third NFO text = %+v, want exactly one stamped nfo", third)
	}
	// ...and the field the NFO did not supply is not.
	originRows := scanRows(t, historySvc, a.ID, "origin")
	if len(originRows) != 1 {
		t.Fatalf("origin scan rows = %d, want exactly 1 (the stale copy overwriting the edit)", len(originRows))
	}
	if originRows[0].Producer != artist.ProducerUnrecorded {
		t.Errorf("origin row producer = %q, want unrecorded: the NFO did not supply this value",
			originRows[0].Producer)
	}
}
