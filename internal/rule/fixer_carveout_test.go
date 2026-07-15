package rule

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// TestImageSlotProtected_EdgeCases covers the non-protecting branches of the
// #2533 guard helper: an un-wired pipeline, an empty image type, and a slot
// with no artist_images row must all report NOT protected (so the fix
// proceeds), distinct from the locked/user-provenance cases that DO protect.
func TestImageSlotProtected_EdgeCases(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	ctx := context.Background()

	a := &artist.Artist{Name: "Edge", SortName: "Edge", MusicBrainzID: "mbid-edge", LibraryID: "lib-edge"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())
	if p.imageSlotProtected(ctx, a.ID, "thumb") {
		t.Error("a slot with no artist_images row must not be protected")
	}
	if p.imageSlotProtected(ctx, a.ID, "") {
		t.Error("an empty image type must not be protected")
	}

	pNil := NewPipeline(engine, nil, ruleSvc, nil, nil, testLogger())
	if pNil.imageSlotProtected(ctx, a.ID, "thumb") {
		t.Error("a pipeline with no artist service must not report protection")
	}

	// Fail-closed: when the lock-state read errors (closed DB), the slot is
	// treated as PROTECTED so a data-loss carve-out never fails open and lets a
	// fetch-replace fixer overwrite a possibly-operator-set image.
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	if !p.imageSlotProtected(ctx, a.ID, "thumb") {
		t.Error("on a lock-state read error the slot must be treated as protected (fail closed)")
	}
}

// TestProtectedFanartSlots_QueryError covers the duplicate fixer's fail-toward-
// preservation path: when the lock-state query errors (here, a closed DB), the
// helper returns an error so Fix skips the delete rather than risk destroying a
// protected image.
func TestProtectedFanartSlots_QueryError(t *testing.T) {
	db := setupTestDB(t)
	// protectedFanartSlots only touches f.db; the hash-recorder arg is unused
	// here. Close the DB so the SELECT errors.
	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	if _, err := f.protectedFanartSlots(context.Background(), "any-artist"); err == nil {
		t.Error("protectedFanartSlots must return an error when the lock-state query fails")
	}
}

// TestPipeline_RunForArtist_ProtectsUserSetImage is the #2533 carve-out
// regression: a locked or user-provenance thumbnail must survive a full
// rule-evaluation pass byte-for-byte, even when an auto fetch-replace rule
// (thumb_square) would otherwise fetch a provider replacement and clobber it.
//
// The scenario reproduces the reported data loss: the operator crops a
// deliberately non-square thumb; that trips thumb_square (auto), whose fix
// fetches a square provider image and REPLACES the operator's crop. The guard
// in attemptFix must short-circuit before the fixer runs.
//
// Two assertions are mutation-sensitive to the guard:
//   - the fixer's download-and-replace path never runs (the candidate URL,
//     served over loopback, is never hit), and
//   - the on-disk bytes are unchanged.
//
// The per-artist coalesced provider fetch that happens during evaluation is
// deliberately NOT asserted on: it goes through the orchestrator stub (no
// HTTP, no disk write) and fires once for the artist regardless of any single
// slot's protection. The data-loss primitive is the byte replacement, which
// only the fixer's HTTP download path performs -- hence the loopback hit
// counter is the precise signal.
//
// The test also asserts the thumb_square violation actually fired, so a
// future change that stops the rule from triggering cannot make the survival
// assertions pass vacuously.
func TestPipeline_RunForArtist_ProtectsUserSetImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		locked bool   // mark the slot Locked
		source string // artist_images.source
	}{
		{name: "locked slot", locked: true, source: ""},
		{name: "user provenance", locked: false, source: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			artistSvc := artist.NewService(db)
			ruleSvc := NewService(db)
			ctx := context.Background()

			if err := ruleSvc.SeedDefaults(ctx); err != nil {
				t.Fatalf("seeding rules: %v", err)
			}
			// thumb_square is the reported offender: it fetch-replaces a
			// non-square thumb. Enable it in auto mode so the pipeline
			// actually dispatches its fixer.
			sq, err := ruleSvc.GetByID(ctx, RuleThumbSquare)
			if err != nil {
				t.Fatalf("getting %s: %v", RuleThumbSquare, err)
			}
			sq.Enabled = true
			sq.AutomationMode = AutomationModeAuto
			if err := ruleSvc.Update(ctx, sq); err != nil {
				t.Fatalf("updating %s: %v", RuleThumbSquare, err)
			}

			// A deliberately NON-SQUARE existing thumb (600x400) trips
			// thumb_square. Capture its exact bytes for the survival check.
			dir := t.TempDir()
			existing := makeTestJPEG(t, 600, 400)
			thumbPath := filepath.Join(dir, "folder.jpg")
			if err := os.WriteFile(thumbPath, existing, 0o644); err != nil {
				t.Fatalf("writing existing thumb: %v", err)
			}

			a := &artist.Artist{
				Name:          "Carveout Test",
				SortName:      "Carveout Test",
				Path:          dir,
				MusicBrainzID: "mbid-carveout",
				LibraryID:     "lib-carveout",
				ThumbExists:   true, // required for the square checker to run
			}
			if err := artistSvc.Create(ctx, a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}

			// Seed the artist_images row and mark it protected.
			row := &artist.ArtistImage{
				ArtistID:  a.ID,
				ImageType: "thumb",
				SlotIndex: 0,
				Exists:    true,
				Source:    tc.source,
			}
			if err := artistSvc.UpsertImage(ctx, row); err != nil {
				t.Fatalf("UpsertImage: %v", err)
			}
			if tc.locked {
				imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
				if err != nil || len(imgs) == 0 {
					t.Fatalf("GetImagesForArtist: %v (len=%d)", err, len(imgs))
				}
				if err := artistSvc.SetImageLock(ctx, imgs[0].ID, true); err != nil {
					t.Fatalf("SetImageLock: %v", err)
				}
			}

			// A square 1000x1000 provider candidate served over loopback. If
			// the guard fails, the fixer downloads and installs this in place
			// of the operator's crop -- proving the survival assertion is real.
			replacement := makeTestJPEG(t, 1000, 1000)
			var downloadHits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				downloadHits.Add(1)
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(replacement)
			}))
			defer srv.Close()

			count := &countingEvalProvider{
				imagesResult: &provider.FetchResult{
					Images: []provider.ImageResult{
						{URL: srv.URL + "/square.jpg", Type: "thumb", Width: 1000, Height: 1000, Source: "fanarttv"},
					},
				},
			}

			imageFixer := NewImageFixer(&countingImageFacade{c: count}, nil, nonSharedFSCheck(), testLogger())
			// httptest is on 127.0.0.1; allow loopback download.
			imageFixer.httpClient = &http.Client{Timeout: fetchTimeout}
			engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
			pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{imageFixer}, nil, testLogger())
			pipeline.SetOrchestrator(count)

			result, err := pipeline.RunForArtist(ctx, a)
			if err != nil {
				t.Fatalf("RunForArtist: %v", err)
			}

			// Anti-vacuity: thumb_square must actually have fired, else the
			// survival assertions below would pass for the wrong reason.
			var sawSquare bool
			for _, r := range result.Results {
				if r.RuleID == RuleThumbSquare {
					sawSquare = true
				}
			}
			if !sawSquare {
				t.Fatalf("thumb_square violation not present in results; test would be vacuous")
			}

			// The guard's direct effect: the fixer never ran, so its
			// download-and-replace path never reached out for the candidate.
			if got := downloadHits.Load(); got != 0 {
				t.Errorf("candidate download dispatched %d times; want 0 (protected slot must skip the fetch-replace fixer)", got)
			}

			// The operator's image survives byte-for-byte.
			after, err := os.ReadFile(thumbPath)
			if err != nil {
				t.Fatalf("reading thumb after run: %v", err)
			}
			if !bytes.Equal(existing, after) {
				t.Errorf("protected thumb was modified: %d bytes before, %d bytes after", len(existing), len(after))
			}
		})
	}
}
