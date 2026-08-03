//go:build unix

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
)

// This file is the F1 regression from the #2689 fix-round: a hostile review
// found that the FIRST round bounded the internal/image seam but not the
// phash / fanart-duplicate call chains that hold the SHARED
// backdropRepairRunning singleton. reverifySlotPHash in particular took no
// ctx at all. A bare os.Open on any of those chains meant the handler's
// context.WithTimeout could not guarantee its deferred release() ever ran --
// the exact wedge #2689 exists to eliminate, one seam further out than the
// previous round covered.
//
// Unlike handlers_remediate_stall_test.go (which injects the stall at the
// pipeline seam with a fake PipelineRunner), THIS test drives the REAL
// *rule.Pipeline through a real FIFO planted at the exact slot
// reverifySlotPHash reads. That is deliberate: the defect this proves is
// specifically about the production read path reaching the shared
// singleton-holding os.Open unbounded, so a fake pipeline that never touches
// disk cannot exercise it. A FIFO with no writer blocks open(2)/read(2) in
// the kernel exactly as a stalled network mount does -- see
// internal/image/readio_stall_test.go for the same repro at the primitive
// layer.

// wedgeJPEG encodes a small distinguishable JPEG so the pipeline's real
// PerceptualHash / re-verification path has genuine bytes to work with for
// every slot except the wedged one.
func wedgeJPEG(t *testing.T, variant int) []byte {
	t.Helper()
	const (
		blocks = 8
		w      = 640
		h      = 360
	)
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := blockVal(x*blocks/w, y*blocks/h, variant)
			m.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

// seedWedgeArtist inserts an artist row with a real on-disk path.
func seedWedgeArtist(t *testing.T, db *sql.DB, id, name, path string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, ?)`,
		id, name, name, path)
}

// seedWedgeHashedImage records a fanart slot's stored perceptual hash, as the
// phash detector's index reads it. Always "fanart" here -- this file's whole
// scenario is the fanart-slot re-verification wedge -- so the image type is
// not parameterized.
func seedWedgeHashedImage(t *testing.T, db *sql.DB, artistID string, slot int, phashHex string) {
	t.Helper()
	id := fmt.Sprintf("%s-fanart-wedge-%d", artistID, slot)
	mustExec(t, db,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
		 VALUES (?, ?, 'fanart', ?, 1, ?, '')`,
		id, artistID, slot, phashHex)
}

// newWedgePipeline builds a real *rule.Pipeline over a real migrated SQLite
// DB and wires it into a real Router exactly as production does -- no fake
// PipelineRunner. That is the point: the defect under test is in the
// production read path itself.
func newWedgePipeline(t *testing.T) (*Router, *sql.DB) {
	t.Helper()
	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	ruleEngine := rule.NewEngine(ruleSvc, db, nil, nil, logger)
	pipeline := rule.NewPipeline(ruleEngine, artistSvc, ruleSvc, nil, nil, logger)

	r := NewRouter(RouterDeps{
		SessionSecret:     testSessionSecret,
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		RuleEngine:        ruleEngine,
		Pipeline:          pipeline,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
	})
	return r, db
}

// wedgeFifo plants a FIFO with no writer at path, standing in for a stalled
// mount. Skips (rather than fails) on a platform without mkfifo. A writer is
// opened at cleanup so the abandoned read can drain and the goroutine exit
// rather than outlive the test binary.
func wedgeFifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
	})
}

// TestPHashRemediate_WedgedReverifyRead_ReleasesSharedSingleton is the F1
// regression. Artist A holds three fanart slots; slot 1 (fanart2.jpg) is
// flagged as cross-artist pollution matching artist B's slot 0, and is
// REPLACED WITH A FIFO before the remediate call. reverifySlotPHash's
// re-read of that exact file is where the previous round's bound stopped:
// with ctx threaded through img.ReadImageFileBounded the handler's own work
// deadline aborts the read, the handler returns, and backdropRepairRunning
// is released. Revert the ctx threading (F1) and this test goes RED: the
// handler blocks past its 10s test timeout because the bare os.Open never
// unblocks, and the singleton stays claimed forever.
func TestPHashRemediate_WedgedReverifyRead_ReleasesSharedSingleton(t *testing.T) {
	// Deliberately NOT t.Parallel(): withRemediationWorkTimeout below writes
	// a package-level var.

	r, db := newWedgePipeline(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	seedWedgeArtist(t, db, "wedge-a", "Wedge Artist A", dirA)
	seedWedgeArtist(t, db, "wedge-b", "Wedge Artist B", dirB)

	v0 := wedgeJPEG(t, 0)
	v1 := wedgeJPEG(t, 1) // the pollution; this is the hash slot 1 is FLAGGED with
	v2 := wedgeJPEG(t, 2)
	if err := os.WriteFile(filepath.Join(dirA, "fanart.jpg"), v0, 0o644); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "fanart3.jpg"), v2, 0o644); err != nil {
		t.Fatalf("writing fanart3.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "fanart.jpg"), v1, 0o644); err != nil {
		t.Fatalf("writing B's fanart.jpg: %v", err)
	}

	// Slot 1 (fanart2.jpg) is a FIFO, not a real file. DiscoverFanart matches
	// it by name via os.DirEntry (no open), so it is enumerated as a normal
	// candidate slot; only reverifySlotPHash's read of its bytes blocks.
	fifoPath := filepath.Join(dirA, "fanart2.jpg")
	wedgeFifo(t, fifoPath)

	h0, err := img.PerceptualHash(bytes.NewReader(v0))
	if err != nil {
		t.Fatalf("hashing v0: %v", err)
	}
	h1, err := img.PerceptualHash(bytes.NewReader(v1))
	if err != nil {
		t.Fatalf("hashing v1: %v", err)
	}
	h2, err := img.PerceptualHash(bytes.NewReader(v2))
	if err != nil {
		t.Fatalf("hashing v2: %v", err)
	}
	seedWedgeHashedImage(t, db, "wedge-a", 0, img.HashHex(h0))
	// Slot 1 is stored with v1's hash -- the flagged pollution -- even though
	// the FILE on disk is now a FIFO. This is exactly the state the detector
	// scores from: ScanPHashMismatches is DB-only and never touches disk, so
	// the scan flags this slot as a suspect regardless of what is actually on
	// disk. The wedge is reached only once remediation tries to RE-VERIFY the
	// flagged slot against its on-disk bytes, in reverifySlotPHash.
	seedWedgeHashedImage(t, db, "wedge-a", 1, img.HashHex(h1))
	seedWedgeHashedImage(t, db, "wedge-a", 2, img.HashHex(h2))
	seedWedgeHashedImage(t, db, "wedge-b", 0, img.HashHex(h1))

	// Shorten the handler's OWN work deadline so the wedge resolves inside
	// the test's patience. The request context is left un-canceled on
	// purpose -- that is what a real wedged operator request looks like, and
	// it forces the handler's own bound (the thing #2689 added) to be what
	// ends the run.
	withRemediationWorkTimeout(t, 300*time.Millisecond)

	body, err := json.Marshal(map[string]any{"artist_id": "wedge-a"})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/phash-mismatch/remediate", bytes.NewReader(body))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		r.handlePHashMismatchRemediate(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the phash remediate handler never returned once its work deadline elapsed; " +
			"reverifySlotPHash's read is wedged on the FIFO with no ctx bound, the SHARED " +
			"backdropRepairRunning singleton is pinned, and every later remediate/restore/bulk " +
			"action will 409 forever -- this is #2689 on the seam the previous round missed")
	}

	r.bulkActionMu.Lock()
	running := r.backdropRepairRunning
	r.bulkActionMu.Unlock()
	if running {
		t.Fatal("backdropRepairRunning is still claimed after a wedged phash re-verify read; " +
			"the shared destructive-fanart slot is now permanently unavailable")
	}

	// The operator-visible property: the slot is re-claimable. A second POST
	// against a clean artist must not 409.
	seedWedgeArtist(t, db, "wedge-c", "Wedge Artist C", t.TempDir())
	second := httptest.NewRecorder()
	secondBody, err := json.Marshal(map[string]any{"artist_id": "wedge-c"})
	if err != nil {
		t.Fatalf("marshaling second body: %v", err)
	}
	secondReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/phash-mismatch/remediate", bytes.NewReader(secondBody))
	r.handlePHashMismatchRemediate(second, secondReq)

	if second.Code == http.StatusConflict {
		t.Fatal("a later remediate POST returned 409 after the wedged run ended; " +
			"this is the permanent-409 #2689 reports")
	}
	if second.Code != http.StatusOK {
		t.Fatalf("a later remediate POST returned %d, want 200; body: %s", second.Code, second.Body.String())
	}

	// Sanity: the fixtures used above are genuinely distinct, so a "no
	// suspect found" false negative could not masquerade as the wedge never
	// being reached.
	if strings.EqualFold(img.HashHex(h0), img.HashHex(h1)) || strings.EqualFold(img.HashHex(h1), img.HashHex(h2)) {
		t.Fatalf("wedge fixtures must be perceptually distinct: h0=%s h1=%s h2=%s",
			img.HashHex(h0), img.HashHex(h1), img.HashHex(h2))
	}
}
