package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/maintenance"
	"github.com/sydlexius/stillwater/internal/rule"
)

// --- fixture helpers ---------------------------------------------------------

// registryRepairFixture is the shared fixture for the registry-repair endpoint
// tests. Every test in this file needs the SAME set of on-disk conditions,
// because the endpoint's whole contract is about telling them apart.
type registryRepairFixture struct {
	router *Router
	db     *sql.DB
	// presentID has a thumb file on disk with NO registry row (rebuild would
	// insert it) and a fanart/0 row whose file IS on disk but is flagged
	// missing (restore would flip it).
	presentID string
	// absentID's directory is definitively gone (ENOENT) -> counted absent.
	absentID string
	// unreadableID's directory exists but is chmod 000 -> counted unreadable.
	unreadableID string
}

// newRegistryRepairRouter builds a Router wired with a REAL maintenance
// service over the test database, because the endpoint's guarantees (nothing
// is written on a preview; absent and unreadable stay distinct) are properties
// of the actual SQL and the actual filesystem probes. A stubbed service could
// not prove either one.
//
// dbPath is empty: maintenance.Service uses it only for Status/Vacuum, neither
// of which this endpoint touches.
func newRegistryRepairRouter(t *testing.T, db *sql.DB) *Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	return NewRouter(RouterDeps{
		SessionSecret:      testSessionSecret,
		AuthService:        auth.NewService(db),
		ArtistService:      artist.NewService(db),
		RuleService:        ruleSvc,
		Pipeline:           &stubPipeline{},
		MaintenanceService: maintenance.NewService(db, "", "", logger),
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})
}

// writeRepairImage synthesizes a decodable JPEG. The rebuild pass gates every
// insert on a FULL PIXEL DECODE, so a zero-byte placeholder would be skipped
// and the fixture would prove nothing.
func writeRepairImage(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 64)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func seedRepairArtist(t *testing.T, db *sql.DB, id, path string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`,
		id, "artist-"+id[:8], path); err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// seedRepairImageRow seeds one CLEARED slot-0 row: exists_flag 0, unlocked.
// Slot and flag are fixed rather than parameters because that is the only row
// shape these tests need -- a cleared row is what the restore pass looks for,
// and every fixture row this file asserts on is at slot 0 (see slot0Flag).
func seedRepairImageRow(t *testing.T, db *sql.DB, artistID, imageType string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO artist_images
		(id, artist_id, image_type, slot_index, exists_flag, locked)
		VALUES (?, ?, ?, 0, 0, 0)`,
		fmt.Sprintf("%s-%s-0", artistID[:8], imageType), artistID, imageType); err != nil {
		t.Fatalf("seeding image row: %v", err)
	}
}

// newRegistryRepairFixture seeds the three conditions the report must keep
// apart, plus the two things a repair should actually do.
func newRegistryRepairFixture(t *testing.T) *registryRepairFixture {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the unreadable-dir case cannot fire")
	}
	db := newTestDB(t)
	libRoot := t.TempDir()

	const presentID = "11111111-0000-0000-0000-000000000001"
	const absentID = "22222222-0000-0000-0000-000000000001"
	const unreadableID = "33333333-0000-0000-0000-000000000001"

	// present: readable directory holding two real images.
	//   folder.jpg  -> NO registry row at all      -> rebuild would INSERT
	//   backdrop.jpg -> row exists, exists_flag=0  -> restore would FLIP to 1
	presentDir := filepath.Join(libRoot, "present")
	writeRepairImage(t, filepath.Join(presentDir, "folder.jpg"))
	writeRepairImage(t, filepath.Join(presentDir, "backdrop.jpg"))
	seedRepairArtist(t, db, presentID, presentDir)
	seedRepairImageRow(t, db, presentID, "fanart")

	// absent: the directory is definitively gone (ENOENT). A clean, expected
	// no-op for an additive repair -- must be counted absent, never unreadable.
	seedRepairArtist(t, db, absentID, filepath.Join(libRoot, "does-not-exist"))

	// unreadable: the directory exists but cannot be read (EACCES). We cannot
	// tell what is inside, so nothing is touched -- must be counted unreadable,
	// never absent.
	unreadableDir := filepath.Join(libRoot, "unreadable")
	if err := os.MkdirAll(unreadableDir, 0o755); err != nil {
		t.Fatalf("mkdir unreadable: %v", err)
	}
	writeRepairImage(t, filepath.Join(unreadableDir, "folder.jpg"))
	if err := os.Chmod(unreadableDir, 0o000); err != nil {
		t.Fatalf("chmod unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableDir, 0o755) })
	seedRepairArtist(t, db, unreadableID, unreadableDir)

	return &registryRepairFixture{
		router:       newRegistryRepairRouter(t, db),
		db:           db,
		presentID:    presentID,
		absentID:     absentID,
		unreadableID: unreadableID,
	}
}

// postRepair drives the handler directly with an admin context and decodes the
// report.
func postRepair(t *testing.T, r *Router, body string) (int, registryRepairReport, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/registry-repair/remediate", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleRegistryRepairRemediate(w, req)

	var report registryRepairReport
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatalf("decoding report: %v; body: %s", err, w.Body.String())
		}
	}
	return w.Code, report, w.Body.String()
}

// snapshotArtistImages renders EVERY column of EVERY artist_images row, sorted.
// All columns, not just exists_flag: an assertion on a subset would pass
// against a repair that deleted a row and reinserted an equivalent one under a
// fresh primary key, or that quietly rewrote a placeholder or a timestamp.
func snapshotArtistImages(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id, artist_id, image_type, slot_index, exists_flag,
		low_res, placeholder, width, height, phash, content_hash, file_format, source,
		last_written_at, locked FROM artist_images`)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("snapshot columns: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
				continue
			}
			parts[i] = fmt.Sprintf("%v", v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot iterate: %v", err)
	}
	sort.Strings(out)
	return out
}

// slot0Flag reads one slot-0 row's exists_flag, reporting separately whether
// the row exists at all. Both facts matter here: the rebuild pass is what makes
// a row EXIST, and the restore pass is what makes an existing row's flag 1, so
// a helper that conflated "no row" with "flag 0" could not tell the two passes
// apart. Every fixture row this file asserts on is at slot 0.
func slot0Flag(t *testing.T, db *sql.DB, artistID, imageType string) (flag int, found bool) {
	t.Helper()
	err := db.QueryRow(`SELECT exists_flag FROM artist_images
		WHERE artist_id = ? AND image_type = ? AND slot_index = 0`,
		artistID, imageType).Scan(&flag)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("reading slot %s/0: %v", imageType, err)
	}
	return flag, true
}

// --- the load-bearing guarantee ----------------------------------------------

// TestRegistryRepair_DryRunIsByteIdentical is the guarantee the whole
// affirmative-commit convention exists to provide: a preview must leave
// artist_images EXACTLY as it found it.
//
// The assertion is a full-table, all-columns snapshot compared before and
// after, not a count of UPDATE statements and not an exists_flag diff. That is
// deliberate -- it also catches an accidental write introduced ANYWHERE in the
// request path (an incorrectly threaded Commit in either pass, a stray timestamp
// refresh, a delete-and-reinsert), including in code that does not exist yet.
//
// The fixture is not a quiet no-op case: the same request reports work to do in
// both passes (a row to insert and a flag to restore) AND covers both the
// absent and the unreadable condition. A preview over a fixture with nothing to
// find would pass this test while proving nothing.
func TestRegistryRepair_DryRunIsByteIdentical(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	before := snapshotArtistImages(t, f.db)
	if len(before) == 0 {
		t.Fatal("fixture seeded no artist_images rows -- the snapshot would be vacuously equal")
	}

	code, report, body := postRepair(t, f.router, `{"commit":false}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}

	after := snapshotArtistImages(t, f.db)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("preview mutated artist_images\n  before: %v\n  after:  %v", before, after)
	}

	// The preview must have COMPUTED real numbers, not merely refused to run.
	// Without this, a handler that returned an empty report unconditionally
	// would satisfy the byte-identical assertion above.
	if !report.DryRun || report.Commit {
		t.Errorf("report should be marked as a dry run; got commit=%v dry_run=%v", report.Commit, report.DryRun)
	}
	if report.Rebuild.RowsPlanned == 0 {
		t.Error("preview planned no inserts; the fixture has a file with no registry row")
	}
	if report.Restored == 0 {
		t.Error("preview reported no restorable flags; the fixture has a cleared flag whose file is present")
	}
	if report.Rebuilt != 0 {
		t.Errorf("preview reported %d rows rebuilt; a preview writes nothing", report.Rebuilt)
	}
}

// TestRegistryRepair_AbsentIsNotUnreadable pins the issue's explicit
// requirement: a definitively-missing directory (a clean no-op) and a directory
// we could not read (we cannot tell what is there) are reported in SEPARATE
// counters. Collapsing them back into one bucket -- in either direction -- is
// the regression this guards, because "cannot tell" reported as "nothing there"
// is the exact mistake that caused the incident this feature repairs.
func TestRegistryRepair_AbsentIsNotUnreadable(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	code, report, body := postRepair(t, f.router, `{"commit":false}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}

	if report.Absent != 1 {
		t.Errorf("absent = %d, want 1 (the ENOENT directory)", report.Absent)
	}
	if report.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (the EACCES directory)", report.Unreadable)
	}
	// Assert the counters are sourced from DIFFERENT facts, not merely equal by
	// coincidence: the nested rebuild result must attribute one artist to each.
	if report.Rebuild.ArtistsAbsent != 1 || report.Rebuild.ArtistsFailed != 1 {
		t.Errorf("rebuild pass must attribute the two directories separately: absent=%d failed=%d",
			report.Rebuild.ArtistsAbsent, report.Rebuild.ArtistsFailed)
	}
	if report.Scanned != 1 {
		t.Errorf("scanned = %d, want 1 (only the readable directory)", report.Scanned)
	}
}

// TestRegistryRepair_UnreadableIsArtistUnit pins that unreadable is a count of
// ARTISTS and nothing else.
//
// It used to be rebuild.ArtistsFailed + restore.Skipped, which added a count of
// artists to a count of registry rows and produced a number in no unit at all.
// Measured on this exact fixture shape, ONE unreadable directory holding three
// cleared rows reported unreadable: 4 -- one broken folder presented to the
// operator as four unreadable things, which tells them neither how many
// problems they have nor how to size the remediation. The row-unit fact is
// still reported, in its own counter, because the two drive different remedial
// actions: unreadable artists means go fix a mount or a permission, while
// unverifiable rows means that many registry rows stay stale until it is fixed.
func TestRegistryRepair_UnreadableIsArtistUnit(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	// Three cleared rows hanging off the ONE unreadable directory. Under the
	// old summing behavior each of these inflated the artist count by one.
	seedRepairImageRow(t, f.db, f.unreadableID, "fanart")
	seedRepairImageRow(t, f.db, f.unreadableID, "thumb")
	seedRepairImageRow(t, f.db, f.unreadableID, "logo")

	code, report, body := postRepair(t, f.router, `{"commit":false}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}

	if report.Unreadable == 4 {
		t.Fatal("unreadable is summing rows into an artist count: one unreadable " +
			"directory with three cleared rows is ONE unreadable artist, not four")
	}
	if report.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1; one directory is unreadable no matter how "+
			"many rows hang off it", report.Unreadable)
	}
	if report.UnverifiableRows != 3 {
		t.Errorf("unverifiable_rows = %d, want 3; the row-unit fact must still be "+
			"reported, just not added to the artist count", report.UnverifiableRows)
	}
}

// TestRegistryRepair_PathlessArtistIsAccountedFor pins that an artist with no
// resolvable image directory reaches the rollup at all.
//
// The rebuild pass classifies every artist into one of four buckets, but the
// rollup surfaced only three: an artist with an empty path produced
// rebuild.artists_skipped = 1 while contributing 0 to scanned, 0 to absent and
// 0 to unreadable, so it was invisible in every top-level counter and nothing
// in the response said so. That is the silently-swallowed condition this
// endpoint exists to eliminate, one level down.
//
// It also pins that BOTH passes classify the condition the same way. Both call
// artistImageDir and test dir == "", but rebuild counted it as skipped while
// restore folded it into the same bucket as a failed probe -- one fault, two
// different answers depending on which pass observed it.
func TestRegistryRepair_PathlessArtistIsAccountedFor(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	const pathlessID = "99999999-0000-0000-0000-000000000001"
	seedRepairArtist(t, f.db, pathlessID, "")
	seedRepairImageRow(t, f.db, pathlessID, "thumb")

	code, report, body := postRepair(t, f.router, `{"commit":false}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}

	if report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1; the pathless artist must be visible in the rollup",
			report.Skipped)
	}
	// ...and must not have leaked into either filesystem-fact counter. We
	// observed no absence and attempted no read, so it is neither.
	if report.Absent != 1 {
		t.Errorf("absent = %d, want 1 (only the ENOENT artist)", report.Absent)
	}
	if report.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (only the EACCES artist)", report.Unreadable)
	}
	// The restore pass must call it the same thing: no probe was attempted for
	// this row, so it is unresolvable, not a probe that could not answer.
	if report.Restore.Unresolvable != 1 || report.Restore.Skipped != 0 {
		t.Errorf("restore classified the pathless artist's row as unresolvable=%d skipped=%d; "+
			"want 1 and 0, matching how the rebuild pass classifies the same condition",
			report.Restore.Unresolvable, report.Restore.Skipped)
	}
}

// TestRegistryRepair_ArtistCountersAccountForEveryArtist pins the invariant
// that makes the rollup trustworthy: every artist in scope lands in exactly one
// of scanned, absent, unreadable and skipped, so the four sum to the library.
//
// This is the durable guard. Any future branch added to the rebuild pass's
// per-artist classification that forgets to increment a bucket -- or any future
// bucket the rollup forgets to surface, which is exactly how skipped went
// missing -- breaks it immediately, rather than silently shrinking the number
// of artists the report accounts for.
func TestRegistryRepair_ArtistCountersAccountForEveryArtist(t *testing.T) {
	t.Parallel()

	// Both modes: the preview and commit paths differ, and the invariant must
	// hold on each.
	for _, mode := range []struct {
		name string
		body string
	}{
		{name: "preview", body: `{"commit":false}`},
		{name: "commit", body: `{"commit":true}`},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			f := newRegistryRepairFixture(t)

			// A fourth artist covering the skipped bucket, so the invariant is
			// tested against all four and not just the three the fixture seeds.
			const pathlessID = "99999999-0000-0000-0000-000000000002"
			seedRepairArtist(t, f.db, pathlessID, "")

			// Read the true total rather than hardcoding it, so this assertion
			// survives a change to the fixture's artist set.
			var total int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM artists`).Scan(&total); err != nil {
				t.Fatalf("counting artists: %v", err)
			}
			if total == 0 {
				t.Fatal("fixture seeded no artists; the sum would be vacuously equal")
			}

			code, report, body := postRepair(t, f.router, mode.body)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", code, body)
			}

			sum := report.Scanned + report.Absent + report.Unreadable + report.Skipped
			if sum != total {
				t.Errorf("artist counters account for %d of %d artists (scanned=%d absent=%d unreadable=%d skipped=%d); "+
					"every artist in scope must land in exactly one bucket",
					sum, total, report.Scanned, report.Absent, report.Unreadable, report.Skipped)
			}
		})
	}
}

// TestRegistryRepair_WriteFailuresIsZeroOnPreview pins the commit guard on the
// write_failures rollup.
//
// write_failures sums rebuild's planned-but-not-inserted shortfall with
// restore's failed UPDATE statements. On a PREVIEW, rows_planned is the plan rather than
// a shortfall and rows_inserted is 0 by construction, so an unguarded
// subtraction would report every row the repair intends to insert as a write
// failure -- telling the operator their repair is broken when it has not run.
func TestRegistryRepair_WriteFailuresIsZeroOnPreview(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	code, report, body := postRepair(t, f.router, `{"commit":false}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}

	// The plan must be NON-EMPTY, or the assertion below passes vacuously:
	// a zero plan cannot be mistaken for a shortfall in the first place, so it
	// would prove nothing about the guard.
	if report.Rebuild.RowsPlanned == 0 {
		t.Fatal("preview planned no inserts; write_failures would be 0 whether or not " +
			"the commit guard exists, and this test would prove nothing")
	}
	if report.WriteFailures != 0 {
		t.Errorf("write_failures = %d on a preview, want 0; a preview attempts no write, "+
			"and rows_planned (%d) is the plan, not a shortfall",
			report.WriteFailures, report.Rebuild.RowsPlanned)
	}
}

// TestRegistryRepair_CommitWritesBothPasses proves commit:true actually does
// the two things the endpoint composes -- inserts the missing row (rebuild) and
// flips the confirmed-present flag (restore) -- and that the reported counts
// match a RE-READ of the table rather than the handler's own tally.
func TestRegistryRepair_CommitWritesBothPasses(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	if _, found := slot0Flag(t, f.db, f.presentID, "thumb"); found {
		t.Fatal("precondition: thumb/0 must have no row before the repair")
	}
	if flag, found := slot0Flag(t, f.db, f.presentID, "fanart"); !found || flag != 0 {
		t.Fatalf("precondition: fanart/0 must exist with exists_flag 0; found=%v flag=%d", found, flag)
	}

	code, report, body := postRepair(t, f.router, `{"commit":true}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}
	if report.DryRun || !report.Commit {
		t.Errorf("report should be marked committed; got commit=%v dry_run=%v", report.Commit, report.DryRun)
	}

	// Re-read: the row rebuild claimed to insert must actually be there with
	// exists_flag 1, and the flag restore claimed to flip must actually be 1.
	flag, found := slot0Flag(t, f.db, f.presentID, "thumb")
	if !found || flag != 1 {
		t.Errorf("thumb/0 after commit: found=%v flag=%d; want found=true flag=1", found, flag)
	}
	flag, found = slot0Flag(t, f.db, f.presentID, "fanart")
	if !found || flag != 1 {
		t.Errorf("fanart/0 after commit: found=%v flag=%d; want found=true flag=1", found, flag)
	}

	// The counters must agree with what the re-read just proved.
	if report.Rebuilt != 1 {
		t.Errorf("rebuilt = %d, want 1", report.Rebuilt)
	}
	if report.Restored != 1 {
		t.Errorf("restored = %d, want 1", report.Restored)
	}
	// Absent/unreadable stay distinct on the commit path too, not just preview.
	if report.Absent != 1 || report.Unreadable != 1 {
		t.Errorf("absent = %d unreadable = %d; want 1 and 1 on the commit path", report.Absent, report.Unreadable)
	}
}

// TestRegistryRepair_ScopedToOneArtist pins that artist_id scopes BOTH passes.
// An unscoped run would touch the whole library, so a scoped request that
// silently ignored the field would be a far wider write than the operator
// asked for.
//
// The out-of-scope artist below is what makes that claim testable. The base
// fixture gives only presentID a cleared row, so a scoped restore and a
// library-wide one produce identical results over it: measured, hardcoding
// ArtistID: "" on the RestoreExistsFlags call -- making every scoped request
// silently do a library-wide flag restore -- left this test passing. A second
// artist with a restorable row is a row a library-wide restore WOULD flip and a
// correctly-scoped one must NOT, and the assertion is against the TABLE rather
// than a counter, because "restored 1" proves nothing about WHICH row was
// written.
func TestRegistryRepair_ScopedToOneArtist(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	// Mirrors how the fixture seeds presentID's restorable row: a fanart/0 row
	// flagged missing whose backdrop.jpg IS on disk, so it is genuinely
	// confirmable and a library-wide restore would flip it.
	const otherID = "77777777-0000-0000-0000-000000000001"
	otherDir := filepath.Join(t.TempDir(), "other")
	writeRepairImage(t, filepath.Join(otherDir, "backdrop.jpg"))
	seedRepairArtist(t, f.db, otherID, otherDir)
	seedRepairImageRow(t, f.db, otherID, "fanart")

	code, report, body := postRepair(t, f.router,
		`{"commit":true,"artist_id":"`+f.presentID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}

	// The artifact: the out-of-scope artist's flag must still be cleared.
	if flag, found := slot0Flag(t, f.db, otherID, "fanart"); !found || flag != 0 {
		t.Errorf("out-of-scope artist fanart/0 = %d (found=%v); a scoped request must not "+
			"restore another artist's flags", flag, found)
	}
	// ...and the restore pass must not even have EXAMINED its row.
	if report.Restore.Checked != 1 {
		t.Errorf("restore checked %d rows on a scoped request, want 1; a scoped restore "+
			"must only examine the one artist's rows", report.Restore.Checked)
	}
	// Only the scoped artist was looked at: the absent and unreadable
	// directories belong to other artists and must not appear in the counters.
	if report.Absent != 0 || report.Unreadable != 0 {
		t.Errorf("scoped run leaked other artists: absent=%d unreadable=%d", report.Absent, report.Unreadable)
	}
	if report.Scanned != 1 || report.Rebuilt != 1 || report.Restored != 1 {
		t.Errorf("scoped run = scanned %d rebuilt %d restored %d; want 1/1/1",
			report.Scanned, report.Rebuilt, report.Restored)
	}
}

// --- gates -------------------------------------------------------------------

// TestRegistryRepair_NonAdminForbidden: the endpoint is admin-only via
// requireForeignAdmin, so an authenticated non-admin must get 403.
func TestRegistryRepair_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/reports/registry-repair/remediate", strings.NewReader(`{"commit":true}`))
	w := httptest.NewRecorder()
	f.router.handleRegistryRepairRemediate(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", w.Code)
	}
	// The gate must run BEFORE any write. Assert the fixture's repairable row
	// is still missing, so a future reordering that put the passes ahead of the
	// admin check cannot pass this test on the status code alone.
	if _, found := slot0Flag(t, f.db, f.presentID, "thumb"); found {
		t.Error("a forbidden request must not have run the repair")
	}
}

// TestRegistryRepair_CSRFRejected checks that a browser-shaped POST carrying no
// CSRF token is rejected by the middleware chain before the handler runs.
//
// Read the scope narrowly: this is a property of the GLOBAL chain, not of this
// endpoint. The CSRF middleware wraps the entire mux and answers 403 before
// routing happens, so it responds identically for a path that is registered and
// one that is not -- measured, repointing this request at a nonexistent path
// left it passing. It therefore cannot show the endpoint is reachable, only
// that nothing exempted it. Route registration is pinned separately by
// TestRegistryRepairRoute_IsRegistered in handlers_mux_integration_test.go,
// which bypasses CSRF via the API-token path to actually reach the mux.
func TestRegistryRepair_CSRFRejected(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	hctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/reports/registry-repair/remediate", strings.NewReader(`{"commit":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.Handler(hctx).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the CSRF middleware; body: %s", w.Code, w.Body.String())
	}
	if _, found := slot0Flag(t, f.db, f.presentID, "thumb"); found {
		t.Error("a CSRF-rejected request must not have run the repair")
	}
}

// TestRegistryRepair_ConflictWhenRunning: the singleton returns 409 while a
// repair is already in flight, so two runs cannot scan-then-write the same rows.
func TestRegistryRepair_ConflictWhenRunning(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	f.router.registryRepairMu.Lock()
	f.router.registryRepairRunning = true
	f.router.registryRepairMu.Unlock()

	code, _, body := postRepair(t, f.router, `{"commit":true}`)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while a repair is running; body: %s", code, body)
	}
}

// TestRegistryRepair_ConcurrentPostsSerialize drives the singleton the way a
// real double-submit would: two concurrent POSTs, exactly one of which may run.
// This is the property the flag exists for -- the hand-set-flag test above only
// proves the 409 branch is reachable, not that the claim is atomic.
func TestRegistryRepair_ConcurrentPostsSerialize(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	codes := make([]int, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/registry-repair/remediate", strings.NewReader(`{"commit":false}`))
			w := httptest.NewRecorder()
			f.router.handleRegistryRepairRemediate(w, req)
			codes[i] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	// The two goroutines may or may not actually overlap, so a 2-0 split is a
	// legitimate scheduling outcome. What must NEVER happen is two runs held
	// simultaneously, which would show up as a status this switch rejects, or
	// as ok+conflict != 2.
	if ok+conflict != 2 || ok == 0 {
		t.Errorf("concurrent posts = %d ok, %d conflict; want at least one ok and no other status", ok, conflict)
	}
}

// TestRegistryRepair_MalformedBodyRejected: the body is decoded strictly
// (unknown fields and trailing tokens rejected), so a typo'd field name is a
// 400 rather than a silently-ignored option. That matters most for "commit":
// a request that meant to write and misspelled the key must not be accepted as
// a preview OR as a write.
func TestRegistryRepair_MalformedBodyRejected(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	for _, body := range []string{
		`{"commit":`,
		`{"commit":true}{"commit":false}`,
		`{"dry_run":true}`, // unknown field: the inverted convention is rejected
		`not json`,
	} {
		code, _, respBody := postRepair(t, f.router, body)
		if code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400; body: %s", body, code, respBody)
		}
	}
}

// TestRegistryRepair_EmptyBodyPreviews: an empty body must PREVIEW, never
// write. This is the affirmative-commit convention's whole point -- a dropped
// field or a bodyless POST is the safe mode, not the destructive one.
func TestRegistryRepair_EmptyBodyPreviews(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	before := snapshotArtistImages(t, f.db)
	code, report, body := postRepair(t, f.router, ``)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, body)
	}
	if !report.DryRun {
		t.Error("an empty body must preview, not commit")
	}
	if after := snapshotArtistImages(t, f.db); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("empty-body request mutated artist_images\n  before: %v\n  after: %v", before, after)
	}
}

// TestRegistryRepair_UnwiredServiceFailsLoud: a router with no maintenance
// service is a wiring bug. It must be a 500, never a 200 reporting a clean
// zero-count repair -- a repair endpoint that reports success while doing
// nothing is the failure class this feature exists to eliminate.
func TestRegistryRepair_UnwiredServiceFailsLoud(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &stubPipeline{}) // no MaintenanceService

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/registry-repair/remediate", strings.NewReader(`{"commit":true}`))
	w := httptest.NewRecorder()
	r.handleRegistryRepairRemediate(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an unwired maintenance service", w.Code)
	}
}

// TestRegistryRepair_LibraryUnreachableIsNotCleanZero pins the mount-down
// guard's operator-facing half. When the media mount is gone every artist
// directory reads ENOENT, which per-artist looks like "definitively absent,
// nothing to restore" -- so a total outage would otherwise be reported as a
// clean, successful, zero-count repair. That is the same catastrophe this
// feature exists to prevent, one layer up. The endpoint must surface it as a
// distinct, actionable failure instead of a 200.
func TestRegistryRepair_LibraryUnreachableIsNotCleanZero(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	libRoot := t.TempDir()
	// Every artist in the library points at a directory that is not there.
	seedRepairArtist(t, db, "44444444-0000-0000-0000-000000000001", filepath.Join(libRoot, "gone-a"))
	seedRepairArtist(t, db, "55555555-0000-0000-0000-000000000001", filepath.Join(libRoot, "gone-b"))
	r := newRegistryRepairRouter(t, db)

	code, _, body := postRepair(t, r, `{"commit":true}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the whole library is unreadable; body: %s", code, body)
	}
	if !strings.Contains(body, "library_unreachable") {
		t.Errorf("body should name the condition so an operator knows what to check; got %s", body)
	}
	// The singleton must still be released: a mount outage cannot leave the
	// endpoint permanently 409 once the mount comes back.
	r.registryRepairMu.Lock()
	running := r.registryRepairRunning
	r.registryRepairMu.Unlock()
	if running {
		t.Error("the singleton must be released after the mount-down guard fires")
	}
}

// TestRegistryRepair_SlotReleasedAfterRun: the singleton must be released when
// a run finishes, or the endpoint is permanently 409 after its first use.
func TestRegistryRepair_SlotReleasedAfterRun(t *testing.T) {
	t.Parallel()
	f := newRegistryRepairFixture(t)

	if code, _, body := postRepair(t, f.router, `{"commit":false}`); code != http.StatusOK {
		t.Fatalf("first run status = %d, want 200; body: %s", code, body)
	}
	f.router.registryRepairMu.Lock()
	running := f.router.registryRepairRunning
	f.router.registryRepairMu.Unlock()
	if running {
		t.Fatal("the singleton must be released after a completed run")
	}
	if code, _, body := postRepair(t, f.router, `{"commit":false}`); code != http.StatusOK {
		t.Fatalf("second run status = %d, want 200; body: %s", code, body)
	}
}
