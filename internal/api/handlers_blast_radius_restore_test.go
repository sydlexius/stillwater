package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

// restoreTestRouter builds a router whose artist service records history, which
// is the production wiring (cmd/stillwater/main.go calls SetHistoryService).
// Without it every mutation in these tests would silently record nothing and
// the blast-radius query would see an empty table, so every assertion below
// would pass vacuously against a report with no rows.
func restoreTestRouter(t *testing.T) (*Router, *artist.Service, *artist.HistoryService) {
	t.Helper()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	return r, artistSvc, historySvc
}

// damageBase is the timestamp seeded history rows are backdated to. It is well
// in the past so that any change written later in a test at real wall-clock
// time sorts after every seeded row.
var damageBase = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// damageSeq hands each seeded row its own second. metadata_changes stores
// created_at as RFC 3339 with SECOND resolution, and the blast-radius query
// ranks rows per (artist, field) by created_at DESC with id DESC as the
// tiebreak. Two writes inside the same second therefore tie on created_at and
// fall back to comparing random UUIDs, which can rank the seed write above the
// damage write and make the fixture describe something other than damage.
// Spacing the rows removes the ambiguity from the fixture rather than relying
// on how fast the test machine runs.
var damageSeq atomic.Int64

// backdateChange rewrites one history row's created_at so the fixture's
// ordering is explicit.
func backdateChange(t *testing.T, r *Router, changeID string, ts time.Time) {
	t.Helper()
	res, err := r.db.ExecContext(context.Background(),
		`UPDATE metadata_changes SET created_at = ? WHERE id = ?`,
		ts.UTC().Format(time.RFC3339), changeID)
	if err != nil {
		t.Fatalf("backdating change %s: %v", changeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("backdating change %s: rows affected: %v", changeID, err)
	}
	if n != 1 {
		t.Fatalf("backdating change %s affected %d rows, want 1", changeID, n)
	}
}

// latestChangeID returns the id of the most recently recorded change for the
// given artist and field, failing the test when there is none. Used to capture
// the row a mutation just wrote so the fixture can stamp it.
func latestChangeID(t *testing.T, historySvc *artist.HistoryService, artistID, field string) string {
	t.Helper()
	changes, _, err := historySvc.List(context.Background(), artistID, 200, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	// List returns newest first, but every row seeded so far has been
	// backdated, so scan for the one row that still carries a real wall-clock
	// timestamp: it is the only one after damageBase.
	for i := range changes {
		if changes[i].Field == field && changes[i].CreatedAt.After(damageBase.Add(time.Hour)) {
			return changes[i].ID
		}
	}
	t.Fatalf("no freshly recorded change found for field %q", field)
	return ""
}

// damageField simulates an automated writer destroying an operator's value: it
// writes the operator value with source "manual", then overwrites it with
// source "scan". Both rows are stamped with explicit, distinct timestamps (see
// damageSeq). It returns the change id of the DAMAGE row, which is what the
// blast-radius report lists and what a restore request carries.
func damageField(t *testing.T, r *Router, artistSvc *artist.Service, historySvc *artist.HistoryService,
	artistID, field, operatorValue, automatedValue string,
) string {
	t.Helper()

	manualCtx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(manualCtx, artistID, field, operatorValue); err != nil {
		t.Fatalf("seeding operator value for %s: %v", field, err)
	}
	seedID := latestChangeID(t, historySvc, artistID, field)
	backdateChange(t, r, seedID, damageBase.Add(time.Duration(damageSeq.Add(1))*time.Second))

	scanCtx := artist.ContextWithSource(context.Background(), "scan")
	var err error
	if automatedValue == "" {
		_, err = artistSvc.ClearField(scanCtx, artistID, field)
	} else {
		_, err = artistSvc.UpdateField(scanCtx, artistID, field, automatedValue)
	}
	if err != nil {
		t.Fatalf("simulating automated write to %s: %v", field, err)
	}
	damageID := latestChangeID(t, historySvc, artistID, field)
	backdateChange(t, r, damageID, damageBase.Add(time.Duration(damageSeq.Add(1))*time.Second))

	// Precondition: the row this fixture hands back really is what the
	// blast-radius report considers the current damage for this pair. Every
	// test below builds on that, so verifying it here means a fixture defect
	// fails loudly instead of turning some later assertion vacuous.
	f := artist.BlastRadiusFilter{ArtistID: artistID, Field: field}
	f.Validate()
	rows, err := historySvc.ListBlastRadius(context.Background(), f)
	if err != nil {
		t.Fatalf("verifying fixture via ListBlastRadius: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != damageID {
		t.Fatalf("fixture: report rows for %s/%s = %+v, want exactly the damage row %s",
			artistID, field, rows, damageID)
	}
	return damageID
}

// postRestore runs the restore handler and decodes its response.
func postRestore(t *testing.T, r *Router, body string) (*httptest.ResponseRecorder, blastRestoreResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/reports/blast-radius/restore", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleBlastRadiusRestore(w, req)

	var resp blastRestoreResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v; body: %s", err, w.Body.String())
		}
	}
	return w, resp
}

// dbSnapshot captures everything a restore could possibly change: the artist's
// tracked field values and the full metadata_changes table. Comparing two
// snapshots is how the dry-run test proves nothing was written, rather than
// trusting a counter the handler itself computed.
func dbSnapshot(t *testing.T, r *Router, artistIDs ...string) string {
	t.Helper()
	ctx := context.Background()

	var sb strings.Builder
	for _, id := range artistIDs {
		a, err := r.artistService.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("snapshotting artist %s: %v", id, err)
		}
		for _, f := range artist.TrackableFields() {
			sb.WriteString(id + "|" + f + "=" + artist.FieldValueFromArtist(a, f) + "\n")
		}
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, artist_id, field, old_value, new_value, source, created_at
		 FROM metadata_changes ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshotting metadata_changes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, artistID, field, oldValue, newValue, source, createdAt string
		if err := rows.Scan(&id, &artistID, &field, &oldValue, &newValue, &source, &createdAt); err != nil {
			t.Fatalf("scanning metadata_changes: %v", err)
		}
		sb.WriteString(strings.Join(
			[]string{"change", id, artistID, field, oldValue, newValue, source, createdAt}, "|") + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating metadata_changes: %v", err)
	}
	return sb.String()
}

// TestBlastRestore_SingleHappyPath asserts the ARTIFACT: the artist row in the
// database actually holds the operator's value again afterwards. A test that
// only checked resp.Restored == 1 would pass against a handler that counted
// without writing, which is the failure mode this whole feature must not have.
func TestBlastRestore_SingleHappyPath(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Single Restore Artist")

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "the operator's bio", "")

	// Precondition: the damage really landed. Without this the restore could
	// "succeed" against a field that was never blanked.
	before, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID before: %v", err)
	}
	if before.Biography != "" {
		t.Fatalf("precondition: biography = %q, want empty (the scan should have blanked it)", before.Biography)
	}

	w, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if resp.Restored != 1 || resp.Refused != 0 {
		t.Fatalf("restored=%d refused=%d, want 1/0; items: %+v", resp.Restored, resp.Refused, resp.Items)
	}

	// THE ARTIFACT: read the persisted value back.
	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if after.Biography != "the operator's bio" {
		t.Errorf("persisted biography = %q, want %q", after.Biography, "the operator's bio")
	}
}

// TestBlastRestore_BulkRestoresEveryRow asserts EVERY row's persisted value,
// not a count. A bulk handler that wrote only the first row would satisfy a
// count assertion of 3 if it also incremented the counter three times.
func TestBlastRestore_BulkRestoresEveryRow(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)

	a1 := addTestArtist(t, artistSvc, "Bulk Artist One")
	a2 := addTestArtist(t, artistSvc, "Bulk Artist Two")

	// Three rows spanning two artists, two fields, and both damage classes
	// (blanked and replaced), so no single-shape assumption can pass this.
	id1 := damageField(t, r, artistSvc, historySvc, a1.ID, "biography", "bio one", "")
	id2 := damageField(t, r, artistSvc, historySvc, a1.ID, "moods", "Melancholy", "Upbeat")
	id3 := damageField(t, r, artistSvc, historySvc, a2.ID, "styles", "Shoegaze", "")

	w, resp := postRestore(t, r,
		`{"change_ids":["`+id1+`","`+id2+`","`+id3+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if resp.Restored != 3 || resp.Refused != 0 {
		t.Fatalf("restored=%d refused=%d, want 3/0; items: %+v", resp.Restored, resp.Refused, resp.Items)
	}

	// THE ARTIFACT, per row.
	got1, err := artistSvc.GetByID(context.Background(), a1.ID)
	if err != nil {
		t.Fatalf("GetByID a1: %v", err)
	}
	if got1.Biography != "bio one" {
		t.Errorf("a1 biography = %q, want %q", got1.Biography, "bio one")
	}
	if strings.Join(got1.Moods, ", ") != "Melancholy" {
		t.Errorf("a1 moods = %v, want [Melancholy]", got1.Moods)
	}

	got2, err := artistSvc.GetByID(context.Background(), a2.ID)
	if err != nil {
		t.Fatalf("GetByID a2: %v", err)
	}
	if strings.Join(got2.Styles, ", ") != "Shoegaze" {
		t.Errorf("a2 styles = %v, want [Shoegaze]", got2.Styles)
	}
}

// TestBlastRestore_DryRunWritesNothing is the acceptance criterion for preview
// mode. It compares a full snapshot of the artist's tracked fields AND the
// entire metadata_changes table taken before and after, and requires them to be
// byte-identical. Asserting "no field changed" alone would miss a stray history
// row; asserting "no history row" alone would miss a silent field write.
//
// It also asserts the preview is not vacuous: the plan must report the row as
// eligible with the correct restore_value, so a handler that wrote nothing
// because it planned nothing would fail.
func TestBlastRestore_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Dry Run Artist")

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "bio that must stay gone", "")

	snapBefore := dbSnapshot(t, r, a.ID)

	// Default mode (commit omitted entirely) must preview. This is the
	// affirmative-commit contract: a dropped key must not write.
	w, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// The preview must be MEANINGFUL, or "nothing written" proves nothing.
	if !resp.DryRun || resp.Commit {
		t.Errorf("dry_run=%v commit=%v, want true/false", resp.DryRun, resp.Commit)
	}
	if resp.Eligible != 1 || resp.Restored != 0 {
		t.Fatalf("eligible=%d restored=%d, want 1/0; items: %+v", resp.Eligible, resp.Restored, resp.Items)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != blastRestorePlanned {
		t.Fatalf("items = %+v, want one planned item", resp.Items)
	}
	if resp.Items[0].RestoreValue != "bio that must stay gone" {
		t.Errorf("restore_value = %q, want the operator value", resp.Items[0].RestoreValue)
	}

	// THE ASSERTION: the database is byte-identical.
	snapAfter := dbSnapshot(t, r, a.ID)
	if snapAfter != snapBefore {
		t.Errorf("dry run mutated the database\nbefore:\n%s\nafter:\n%s", snapBefore, snapAfter)
	}
}

// TestBlastRestore_ConcurrentReturns409 pins the singleton. The slot is claimed
// directly (the same way the bulk-action singleton test does it) so the test
// does not depend on timing.
func TestBlastRestore_ConcurrentReturns409(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Concurrent Restore Artist")
	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "contested bio", "")

	r.blastRestoreMu.Lock()
	r.blastRestoreRunning = true
	r.blastRestoreMu.Unlock()

	w, _ := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}

	// The blocked request must also have written nothing.
	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Biography != "" {
		t.Errorf("biography = %q, want empty: a 409'd restore must not write", after.Biography)
	}

	// And releasing the slot must let the next request through, or the 409
	// could come from a slot that is never freed.
	r.blastRestoreMu.Lock()
	r.blastRestoreRunning = false
	r.blastRestoreMu.Unlock()
	w2, resp2 := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if w2.Code != http.StatusOK || resp2.Restored != 1 {
		t.Fatalf("after release: status=%d restored=%d, want 200/1; body: %s",
			w2.Code, resp2.Restored, w2.Body.String())
	}
}

// TestBlastRestore_RefusesNonRestorableRows covers every arm of the positive
// allow-list. Each case must be REFUSED with the right reason and must leave
// the database alone.
func TestBlastRestore_RefusesNonRestorableRows(t *testing.T) {
	t.Parallel()

	t.Run("unknown change id", func(t *testing.T) {
		t.Parallel()
		r, artistSvc, _ := restoreTestRouter(t)
		addTestArtist(t, artistSvc, "Unknown ID Artist")

		w, resp := postRestore(t, r, `{"change_ids":["no-such-change"],"commit":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if resp.Refused != 1 || resp.Restored != 0 {
			t.Fatalf("refused=%d restored=%d, want 1/0", resp.Refused, resp.Restored)
		}
		if resp.Items[0].Reason != blastRefuseNotFound {
			t.Errorf("reason = %q, want %q", resp.Items[0].Reason, blastRefuseNotFound)
		}
	})

	t.Run("revert row cannot be restored", func(t *testing.T) {
		t.Parallel()
		r, artistSvc, historySvc := restoreTestRouter(t)
		a := addTestArtist(t, artistSvc, "Revert Row Artist")

		// A revert-sourced row: restoring one would chain undo onto undo.
		revertCtx := artist.ContextWithSource(context.Background(), "revert")
		if _, err := artistSvc.UpdateField(revertCtx, a.ID, "biography", "put back by an undo"); err != nil {
			t.Fatalf("seeding revert row: %v", err)
		}
		changes, _, err := historySvc.List(context.Background(), a.ID, 10, 0)
		if err != nil || len(changes) == 0 {
			t.Fatalf("listing history: err=%v len=%d", err, len(changes))
		}

		w, resp := postRestore(t, r, `{"change_ids":["`+changes[0].ID+`"],"commit":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if resp.Refused != 1 || resp.Items[0].Reason != blastRefuseRevertOfRevert {
			t.Fatalf("refused=%d reason=%q, want 1/%q", resp.Refused, resp.Items[0].Reason, blastRefuseRevertOfRevert)
		}
	})

	t.Run("superseded row is refused not written", func(t *testing.T) {
		t.Parallel()
		r, artistSvc, historySvc := restoreTestRouter(t)
		a := addTestArtist(t, artistSvc, "Stale Selection Artist")

		// The operator's value is destroyed, the operator is handed that
		// change id, and THEN the field is written again. The id in hand is
		// now stale: writing it would clobber the newer value.
		staleID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "old operator bio", "")
		manualCtx := artist.ContextWithSource(context.Background(), "manual")
		if _, err := artistSvc.UpdateField(manualCtx, a.ID, "biography", "newer deliberate bio"); err != nil {
			t.Fatalf("writing newer value: %v", err)
		}

		w, resp := postRestore(t, r, `{"change_ids":["`+staleID+`"],"commit":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if resp.Refused != 1 || resp.Restored != 0 {
			t.Fatalf("refused=%d restored=%d, want 1/0; items: %+v", resp.Refused, resp.Restored, resp.Items)
		}
		if resp.Items[0].Reason != blastRefuseNotCurrent {
			t.Errorf("reason = %q, want %q", resp.Items[0].Reason, blastRefuseNotCurrent)
		}

		// THE ARTIFACT: the newer value survived.
		after, err := artistSvc.GetByID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if after.Biography != "newer deliberate bio" {
			t.Errorf("biography = %q, want the newer value to survive", after.Biography)
		}
	})

	t.Run("empty change_ids is rejected", func(t *testing.T) {
		t.Parallel()
		r, _, _ := restoreTestRouter(t)
		w, _ := postRestore(t, r, `{"change_ids":[],"commit":true}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})
}

// TestBlastRestore_IsRecordedInHistory asserts requirement 3: a restore is
// itself auditable. It checks the metadata_changes table directly rather than
// the response, and asserts the row's actual content, so a handler that wrote
// a history row with the wrong values would still fail.
func TestBlastRestore_IsRecordedInHistory(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Audited Restore Artist")

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "auditable bio", "")

	beforeChanges, _, err := historySvc.List(context.Background(), a.ID, 100, 0)
	if err != nil {
		t.Fatalf("listing history before: %v", err)
	}

	_, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if resp.Restored != 1 {
		t.Fatalf("restored = %d, want 1; items: %+v", resp.Restored, resp.Items)
	}
	if resp.Items[0].RestoreChangeID == "" {
		t.Fatal("restore_change_id is empty; the restore reported no audit row")
	}

	afterChanges, _, err := historySvc.List(context.Background(), a.ID, 100, 0)
	if err != nil {
		t.Fatalf("listing history after: %v", err)
	}
	if len(afterChanges) != len(beforeChanges)+1 {
		t.Fatalf("history rows = %d, want %d (exactly one added)",
			len(afterChanges), len(beforeChanges)+1)
	}

	// The reported id must resolve, and its content must describe the restore.
	rec, err := historySvc.GetByID(context.Background(), resp.Items[0].RestoreChangeID)
	if err != nil {
		t.Fatalf("fetching the restore's own history row: %v", err)
	}
	if rec.Source != "revert" {
		t.Errorf("source = %q, want %q", rec.Source, "revert")
	}
	if rec.Field != "biography" {
		t.Errorf("field = %q, want biography", rec.Field)
	}
	if rec.NewValue != "auditable bio" {
		t.Errorf("new_value = %q, want the restored operator value", rec.NewValue)
	}
}

// TestBlastRestore_RestoredRowLeavesTheReport is the round-trip: after a
// restore the blast-radius report must no longer list that artist and field.
// This is what makes a second restore of the same id refuse rather than write
// again, and it verifies the restore integrates with the query layer rather
// than merely writing a value.
func TestBlastRestore_RestoredRowLeavesTheReport(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Round Trip Artist")

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "round trip bio", "")

	// Precondition: the report lists it before the restore.
	f := artist.BlastRadiusFilter{ArtistID: a.ID}
	f.Validate()
	rowsBefore, err := historySvc.ListBlastRadius(context.Background(), f)
	if err != nil {
		t.Fatalf("ListBlastRadius before: %v", err)
	}
	if len(rowsBefore) != 1 || rowsBefore[0].ID != changeID {
		t.Fatalf("precondition: report rows = %+v, want exactly the damage row", rowsBefore)
	}

	if _, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`); resp.Restored != 1 {
		t.Fatalf("restored = %d, want 1", resp.Restored)
	}

	rowsAfter, err := historySvc.ListBlastRadius(context.Background(), f)
	if err != nil {
		t.Fatalf("ListBlastRadius after: %v", err)
	}
	if len(rowsAfter) != 0 {
		t.Errorf("report rows after restore = %+v, want none", rowsAfter)
	}

	// Replaying the same id must now refuse, not write a second time.
	_, replay := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if replay.Refused != 1 || replay.Restored != 0 {
		t.Errorf("replay: refused=%d restored=%d, want 1/0; items: %+v",
			replay.Refused, replay.Restored, replay.Items)
	}
}

// TestBlastRestore_DoesNotRepublishOrRetriggerWriter is the guard test for
// requirement 4, and it wires the machinery that would misbehave rather than
// asserting a flag against a nil collaborator.
//
// A real event.Bus is attached to the router with a real subscriber counting
// event.ArtistUpdated. In production that event is what the rule pipeline's
// dirty and health subscribers consume to re-evaluate the artist, which is the
// automated writer that caused the damage in the first place. If the restore
// emitted it, the recovered value would be handed straight back to the writer
// that destroyed it.
//
// The test asserts BOTH halves:
//
//	PRECONDITION -- an ordinary field update through the SAME router and the
//	SAME bus DOES emit ArtistUpdated. Without this the "zero events" assertion
//	would pass just as well against a broken bus, an unstarted dispatcher, or a
//	subscription that never registered.
//	THE GUARD -- the restore emits zero ArtistUpdated events while still
//	writing the value.
func TestBlastRestore_DoesNotRepublishOrRetriggerWriter(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "No Cascade Artist")

	// Wire a real bus with a real subscriber. events is guarded because the
	// bus dispatches on its own goroutine.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := event.NewBus(logger, 64)
	var mu sync.Mutex
	var artistUpdated int
	done := make(chan struct{})
	bus.Subscribe(event.ArtistUpdated, func(event.Event) {
		mu.Lock()
		artistUpdated++
		mu.Unlock()
	})
	// A sentinel event type the test publishes last. Because the bus dispatches
	// strictly in channel order, observing the sentinel proves every event
	// published before it has already been dispatched. This is what makes the
	// "zero ArtistUpdated" assertion a real observation rather than a race
	// against a dispatcher that simply had not caught up yet.
	bus.Subscribe(event.ScanCompleted, func(event.Event) { close(done) })
	go bus.Start()
	t.Cleanup(bus.Stop)
	r.eventBus = bus

	countUpdates := func() int {
		mu.Lock()
		defer mu.Unlock()
		return artistUpdated
	}

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "must not cascade", "")

	// PRECONDITION: the ordinary edit path publishes ArtistUpdated through
	// this exact bus, so the subscriber demonstrably works.
	updReq := httptest.NewRequest(http.MethodPatch,
		"/api/v1/artists/"+a.ID+"/fields/moods", strings.NewReader("value=Calm"))
	updReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updReq.SetPathValue("id", a.ID)
	updReq.SetPathValue("field", "moods")
	updReq = updReq.WithContext(adminContext())
	updW := httptest.NewRecorder()
	r.handleFieldUpdate(updW, updReq)
	if updW.Code != http.StatusOK {
		t.Fatalf("precondition field update: status = %d; body: %s", updW.Code, updW.Body.String())
	}

	bus.Publish(event.Event{Type: event.ScanCompleted})
	<-done
	baseline := countUpdates()
	if baseline == 0 {
		t.Fatal("precondition failed: an ordinary field update emitted no ArtistUpdated, " +
			"so a zero count after the restore would prove nothing")
	}

	// THE GUARD: the restore must add none.
	_, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if resp.Restored != 1 {
		t.Fatalf("restored = %d, want 1; items: %+v", resp.Restored, resp.Items)
	}

	// Flush the bus again with a fresh sentinel so any event the restore
	// published has certainly been dispatched before the count is read.
	flushed := make(chan struct{})
	bus.Subscribe(event.ScanCompleted, func(event.Event) { close(flushed) })
	bus.Publish(event.Event{Type: event.ScanCompleted})
	<-flushed

	if got := countUpdates(); got != baseline {
		t.Errorf("ArtistUpdated count = %d, want %d unchanged: a restore must not re-trigger "+
			"the rule pipeline that caused the original overwrite", got, baseline)
	}

	// And the restore still did its job.
	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Biography != "must not cascade" {
		t.Errorf("biography = %q, want the restored value", after.Biography)
	}
}

// TestBlastRestore_RequiresAdmin pins the authorization boundary: a restore
// writes artist metadata, so a non-admin session must not reach the plan.
func TestBlastRestore_RequiresAdmin(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Non Admin Artist")
	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "guarded bio", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/reports/blast-radius/restore",
		strings.NewReader(`{"change_ids":["`+changeID+`"],"commit":true}`))
	req.Header.Set("Content-Type", "application/json")
	viewerCtx := middleware.WithTestUserID(context.Background(), "test-viewer")
	req = req.WithContext(middleware.WithTestRole(viewerCtx, "viewer"))
	w := httptest.NewRecorder()
	r.handleBlastRadiusRestore(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}

	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Biography != "" {
		t.Errorf("biography = %q, want empty: a 403'd restore must not write", after.Biography)
	}
}

// TestDedupeChangeIDs covers the request-normalization helper directly: blanks
// and repeats drop, order is preserved.
func TestDedupeChangeIDs(t *testing.T) {
	t.Parallel()
	got := dedupeChangeIDs([]string{"b", "", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupeChangeIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeChangeIDs = %v, want %v", got, want)
		}
	}
}
