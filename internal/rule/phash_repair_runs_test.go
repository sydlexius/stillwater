package rule

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/image"
)

// quarantineForTest writes one entry into op opID of dir through the REAL
// quarantine writer (not hand-written JSON), so the manifest has exactly the
// shape production produces. The source file is a real fixture image.
func quarantineForTest(t *testing.T, dir, opID, fileName string, slot int) {
	t.Helper()
	writePollutionFanart(t, dir, fileName, slot)
	err := image.QuarantineImage(context.Background(), dir, opID, filepath.Join(dir, fileName), image.RepairEntry{
		ArtistID:          "x",
		ImageType:         "fanart",
		SlotIndex:         slot,
		FileName:          fileName,
		MatchedArtistID:   "art-b",
		MatchedArtistName: "Artist B",
		Similarity:        0.97,
	})
	if err != nil {
		t.Fatalf("QuarantineImage(%s): %v", opID, err)
	}
}

// TestListPHashRepairRuns_NewestFirstScopedAndTolerant is the pipeline-level
// contract. The fixture makes creation order and lexical order DISAGREE:
// "aaa-first" is created first but sorts first lexically, "zzz-second" is
// created later but sorts last. Newest-first therefore means zzz before aaa,
// which is the opposite of image.ListRepairOps order. A test where the two
// orders agreed would pass even if the sort were deleted.
func TestListPHashRepairRuns_NewestFirstScopedAndTolerant(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dirA)
	seedRepairArtist(t, db, "art-b", "Artist B", dirB)

	quarantineForTest(t, dirA, "aaa-first", "fanart2.jpg", 1)
	// Manifest timestamps come from the wall clock; make the gap unambiguous.
	time.Sleep(20 * time.Millisecond)
	quarantineForTest(t, dirA, "zzz-second", "fanart3.jpg", 2)
	quarantineForTest(t, dirB, "bbb-other", "fanart2.jpg", 1) // another artist's run

	// Entries the listing must skip without failing: a foreign directory name,
	// a malformed manifest, and a valid-looking op dir with no manifest.
	repairDir := filepath.Join(dirA, image.RepairDirName)
	for _, name := range []string{"Not_An_Op", "mmm-broken", "nnn-empty"} {
		if err := os.MkdirAll(filepath.Join(repairDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repairDir, "mmm-broken", "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Precondition: the two orders really disagree, or the test proves nothing.
	lexical, err := image.ListRepairOps(dirA)
	if err != nil {
		t.Fatal(err)
	}
	if len(lexical) < 2 || lexical[0] != "aaa-first" {
		t.Fatalf("precondition: ListRepairOps must put aaa-first first, got %v", lexical)
	}

	runs, err := p.ListPHashRepairRuns(context.Background(), "art-a")
	if err != nil {
		t.Fatalf("ListPHashRepairRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("want exactly the 2 valid runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].OpID != "zzz-second" || runs[1].OpID != "aaa-first" {
		t.Errorf("want newest first [zzz-second aaa-first], got [%s %s]", runs[0].OpID, runs[1].OpID)
	}
	if !runs[0].CreatedAt.After(runs[1].CreatedAt) {
		t.Errorf("CreatedAt must be descending: %v then %v", runs[0].CreatedAt, runs[1].CreatedAt)
	}
	for _, r := range runs {
		if r.OpID == "bbb-other" {
			t.Errorf("another artist's run leaked into the list: %+v", r)
		}
	}
	e := runs[1].Entries
	if len(e) != 1 || e[0].FileName != "fanart2.jpg" || e[0].SlotIndex != 1 ||
		e[0].StoredName == "" || e[0].MatchedArtistID != "art-b" || e[0].Similarity != 0.97 {
		t.Errorf("entry fields not carried from the manifest: %+v", e)
	}

	// Artist B sees only its own run.
	runsB, err := p.ListPHashRepairRuns(context.Background(), "art-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(runsB) != 1 || runsB[0].OpID != "bbb-other" {
		t.Errorf("art-b must see only bbb-other, got %+v", runsB)
	}
}

// TestListPHashRepairRuns_MissingQuarantineDirIsEmptyNotNil: an artist that was
// never backed out has no .sw-repair directory; that is an empty (non-nil, so
// it encodes as []) list, not an error.
func TestListPHashRepairRuns_MissingQuarantineDirIsEmptyNotNil(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedRepairArtist(t, db, "art-a", "Artist A", t.TempDir())

	runs, err := p.ListPHashRepairRuns(context.Background(), "art-a")
	if err != nil {
		t.Fatalf("missing .sw-repair must not be an error: %v", err)
	}
	if runs == nil || len(runs) != 0 {
		t.Errorf("want empty non-nil slice, got %#v", runs)
	}
}

// TestListPHashRepairRuns_UnknownArtistIsNotFound pins the 404 mapping input.
func TestListPHashRepairRuns_UnknownArtistIsNotFound(t *testing.T) {
	p, _ := newPHashRepairPipeline(t)
	_, err := p.ListPHashRepairRuns(context.Background(), "nope")
	if err == nil || !IsArtistNotFound(err) {
		t.Fatalf("want an artist-not-found error, got %v", err)
	}
}

// TestListPHashRepairRuns_UnreadableQuarantineDirIsAnError: a real I/O failure
// on .sw-repair itself (here, it is a regular file) must not be disguised as an
// empty list.
func TestListPHashRepairRuns_UnreadableQuarantineDirIsAnError(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dir := t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dir)
	if err := os.WriteFile(filepath.Join(dir, image.RepairDirName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListPHashRepairRuns(context.Background(), "art-a"); err == nil {
		t.Fatal("a non-directory .sw-repair must surface an error")
	}
}

// TestListPHashRepairRuns_FindsTheRunARealBackOutCreated drives the real
// remediate pipeline and checks the op id it returns is the one listed, so the
// list and the restore agree on ids end to end.
func TestListPHashRepairRuns_FindsTheRunARealBackOutCreated(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil || res.OpID == "" {
		t.Fatalf("remediate: %v, %+v", err, res)
	}
	runs, err := p.ListPHashRepairRuns(context.Background(), "art-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].OpID != res.OpID || len(runs[0].Entries) != 1 {
		t.Fatalf("want the one real run %s with 1 entry, got %+v", res.OpID, runs)
	}
	if runs[0].Entries[0].MatchedArtistID != "art-b" {
		t.Errorf("entry must name the colliding artist, got %+v", runs[0].Entries[0])
	}
}
