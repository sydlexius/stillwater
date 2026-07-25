package api

import (
	"context"
	"encoding/json"
	"errors"
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

// TestBlastRestore_RefusesUntrackedField pins the OTHER arm of the
// validateRevertable switch: the default branch that maps a non-trackable
// field to blastRefuseNotRevertible.
//
// The revert-of-revert arm is already covered by
// TestBlastRestore_RefusesNonRestorableRows/revert_row_cannot_be_restored.
// This one matters for the same reason: refuse_reason is in the OpenAPI
// contract and a client renders it, so a test that asserted only "refused"
// would pass even if both arms collapsed to a single reason.
//
// The row is inserted directly rather than through the artist service because
// no service path will record history for an untracked field -- that is the
// very property being tested. "name" is a real editable field that is
// deliberately absent from trackableFields, so there is no recorded old value
// the restore could trust.
func TestBlastRestore_RefusesUntrackedField(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Untracked Field Artist")

	// Precondition: the field really is outside history tracking. Without
	// this the test would silently stop exercising the default arm the day
	// "name" became trackable, and would keep passing for the wrong reason.
	if artist.IsTrackableField("name") {
		t.Fatal("precondition: \"name\" is now a trackable field; " +
			"pick another untracked field or this test no longer covers the default arm")
	}

	const changeID = "untracked-field-change"
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, 'name', 'Operator Name', 'Scanner Name', 'scan', ?)`,
		changeID, a.ID, damageBase.Format(time.RFC3339)); err != nil {
		t.Fatalf("seeding untracked-field change: %v", err)
	}

	nameBefore, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID before: %v", err)
	}

	w, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if resp.Refused != 1 || resp.Restored != 0 {
		t.Fatalf("refused=%d restored=%d, want 1/0; items: %+v", resp.Refused, resp.Restored, resp.Items)
	}
	// The SPECIFIC reason, not merely "refused".
	if resp.Items[0].Reason != blastRefuseNotRevertible {
		t.Errorf("reason = %q, want %q", resp.Items[0].Reason, blastRefuseNotRevertible)
	}

	// And the untracked field was not written.
	nameAfter, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if nameAfter.Name != nameBefore.Name {
		t.Errorf("name = %q, want %q unchanged: a refused restore must not write",
			nameAfter.Name, nameBefore.Name)
	}
}

// TestBlastRestore_UnchangedWhenFieldAlreadyHoldsValue pins the "unchanged"
// status, which exists so a restore count never includes rows nothing happened
// to.
//
// The setup makes the damage row genuinely current (so it passes every
// eligibility check and reaches the write) while the artist field ALREADY
// holds the value being restored. performRevert's UpdateField then short-
// circuits on its no-op check, returning changed=false without recording
// history.
//
// The load-bearing assertion is the last one: metadata_changes gained no row.
// That is what distinguishes "correctly skipped the write" from "wrote a
// no-op history row and reported it as unchanged anyway".
func TestBlastRestore_UnchangedWhenFieldAlreadyHoldsValue(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Already Restored Artist")

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "the once-lost bio", "")

	// Put the value back WITHOUT going through history, so the damage row
	// stays the newest recorded change and therefore stays eligible. Writing
	// it via the service would record a new change and make the row stale,
	// which would refuse at check 3 and never reach the write path.
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE artists SET biography = ? WHERE id = ?`, "the once-lost bio", a.ID); err != nil {
		t.Fatalf("pre-setting the field: %v", err)
	}

	// Precondition A: the field already holds the restore value, so the write
	// is genuinely a no-op rather than the test having set up a real change.
	before, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID before: %v", err)
	}
	if before.Biography != "the once-lost bio" {
		t.Fatalf("precondition: biography = %q, want it to already hold the restore value",
			before.Biography)
	}

	// Precondition B: the row is still eligible. If it were not, the handler
	// would refuse at check 3 and this test would pass while never reaching
	// the branch it exists to cover.
	f := artist.BlastRadiusFilter{ArtistID: a.ID, Field: "biography"}
	f.Validate()
	rows, err := historySvc.ListBlastRadius(context.Background(), f)
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != changeID {
		t.Fatalf("precondition: report rows = %+v, want the damage row %s still current", rows, changeID)
	}

	changesBefore, _, err := historySvc.List(context.Background(), a.ID, 200, 0)
	if err != nil {
		t.Fatalf("listing history before: %v", err)
	}

	w, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	if len(resp.Items) != 1 || resp.Items[0].Status != blastRestoreUnchanged {
		t.Fatalf("items = %+v, want one item with status %q", resp.Items, blastRestoreUnchanged)
	}
	// It counts as eligible but NOT as restored: folding it into restored
	// would report a recovery that did not happen.
	if resp.Restored != 0 {
		t.Errorf("restored = %d, want 0: a no-op must not inflate the restore count", resp.Restored)
	}
	if resp.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1", resp.Unchanged)
	}
	if resp.Eligible != 1 {
		t.Errorf("eligible = %d, want 1", resp.Eligible)
	}
	if resp.Items[0].RestoreChangeID != "" {
		t.Errorf("restore_change_id = %q, want empty: nothing was written",
			resp.Items[0].RestoreChangeID)
	}

	// THE ASSERTION WITH TEETH: no history row was added.
	changesAfter, _, err := historySvc.List(context.Background(), a.ID, 200, 0)
	if err != nil {
		t.Fatalf("listing history after: %v", err)
	}
	if len(changesAfter) != len(changesBefore) {
		t.Errorf("history rows = %d, want %d unchanged: a no-op restore must record nothing",
			len(changesAfter), len(changesBefore))
	}
}

// oneArtistFailingRepo makes writes fail for ONE artist and behave normally for
// every other. It exists to prove a per-row failure in a bulk restore does not
// abandon the remaining rows, which needs exactly one row of a batch to fail.
//
// This uses artist.NewDefaultRepos / NewServiceWithRepos, an EXISTING exported
// seam whose doc comment says it is exported so sibling-package tests can wrap
// one repository with a decorator and reuse the real implementations for the
// rest (internal/rule has four test files doing this). No production code was
// added or reshaped to make this branch reachable.
//
// UpdateField is the method to shadow: performRevert calls ClearField only when
// the recorded OldValue is empty, and the blast-radius report never lists a row
// whose old_value is empty, so a genuine restore always goes through
// UpdateField. ClearField is shadowed too so the decorator cannot silently stop
// failing if that ever changes.
type oneArtistFailingRepo struct {
	artist.Repository
	failArtistID string
}

// errForcedRestoreWrite is the sentinel this decorator returns. The handler
// logs it and demotes the row, so the assertions are on the response and on the
// database, never on this value traveling back to the caller.
var errForcedRestoreWrite = errors.New("forced restore write failure")

func (r *oneArtistFailingRepo) UpdateField(ctx context.Context, id, field, value string) error {
	if id == r.failArtistID {
		return errForcedRestoreWrite
	}
	return r.Repository.UpdateField(ctx, id, field, value)
}

func (r *oneArtistFailingRepo) ClearField(ctx context.Context, id, field string) error {
	if id == r.failArtistID {
		return errForcedRestoreWrite
	}
	return r.Repository.ClearField(ctx, id, field)
}

// TestBlastRestore_PerRowWriteFailureDoesNotAbandonBatch pins the safety
// property of a bulk recovery operation: when one row's write fails, that row
// is reported as refused with the write-failure reason and EVERY OTHER ROW
// STILL RESTORES.
//
// The failure mode this guards against is a handler that returns on the first
// error. That leaves the operator with a partially recovered library and no
// record of which rows were never attempted, which on a recovery path is worse
// than failing outright, because the response still looks like a 200.
//
// The failing row is deliberately in the MIDDLE of the batch so the test
// distinguishes "continued past the failure" from "happened to process the
// good rows first".
func TestBlastRestore_PerRowWriteFailureDoesNotAbandonBatch(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)

	good1 := addTestArtist(t, artistSvc, "Batch Good One")
	bad := addTestArtist(t, artistSvc, "Batch Failing")
	good2 := addTestArtist(t, artistSvc, "Batch Good Two")

	id1 := damageField(t, r, artistSvc, historySvc, good1.ID, "biography", "good bio one", "")
	idBad := damageField(t, r, artistSvc, historySvc, bad.ID, "biography", "doomed bio", "")
	id2 := damageField(t, r, artistSvc, historySvc, good2.ID, "biography", "good bio two", "")

	// Swap in a service whose artist repository fails writes for `bad` only.
	// Everything else is the real SQLite repo set, so the good rows exercise
	// the genuine write path.
	realArtists, providers, members, aliases, images, platformIDs, completeness :=
		artist.NewDefaultRepos(r.db)
	failingSvc := artist.NewServiceWithRepos(
		&oneArtistFailingRepo{Repository: realArtists, failArtistID: bad.ID},
		providers, members, aliases, images, platformIDs, completeness,
	)
	failingSvc.SetHistoryService(historySvc)
	r.artistService = failingSvc

	// Precondition: the decorator really does fail that artist and really does
	// let the others through. Without this the test could pass against a
	// decorator that failed everything or nothing.
	if _, err := failingSvc.UpdateField(context.Background(), bad.ID, "moods", "Probe"); !errors.Is(err, errForcedRestoreWrite) {
		t.Fatalf("precondition: write to the failing artist returned %v, want errForcedRestoreWrite", err)
	}
	if _, err := failingSvc.UpdateField(context.Background(), good1.ID, "moods", "Probe"); err != nil {
		t.Fatalf("precondition: write to a healthy artist failed: %v", err)
	}

	// The failing row sits in the middle of the request.
	w, resp := postRestore(t, r,
		`{"change_ids":["`+id1+`","`+idBad+`","`+id2+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	if resp.Restored != 2 || resp.Refused != 1 {
		t.Fatalf("restored=%d refused=%d, want 2/1; items: %+v", resp.Restored, resp.Refused, resp.Items)
	}

	// The failing row carries the write-failure reason specifically, not a
	// generic refusal: an operator needs to know this row was eligible and the
	// WRITE failed, which is re-runnable, rather than that it was rejected.
	byID := make(map[string]blastRestoreItem, len(resp.Items))
	for _, it := range resp.Items {
		byID[it.ChangeID] = it
	}
	if got := byID[idBad]; got.Status != blastRestoreRefused || got.Reason != blastRefuseWriteFailed {
		t.Errorf("failing row: status=%q reason=%q, want %q/%q",
			got.Status, got.Reason, blastRestoreRefused, blastRefuseWriteFailed)
	}
	for _, id := range []string{id1, id2} {
		if got := byID[id]; got.Status != blastRestoreRestored {
			t.Errorf("row %s: status=%q, want %q", id, got.Status, blastRestoreRestored)
		}
	}

	// THE ARTIFACT, both directions: the good rows really were written and the
	// failing row really was not. Asserting only the response would pass
	// against a handler that reported outcomes it never performed.
	gotGood1, err := artistSvc.GetByID(context.Background(), good1.ID)
	if err != nil {
		t.Fatalf("GetByID good1: %v", err)
	}
	if gotGood1.Biography != "good bio one" {
		t.Errorf("good1 biography = %q, want restored", gotGood1.Biography)
	}
	gotGood2, err := artistSvc.GetByID(context.Background(), good2.ID)
	if err != nil {
		t.Fatalf("GetByID good2: %v", err)
	}
	if gotGood2.Biography != "good bio two" {
		t.Errorf("good2 biography = %q, want restored", gotGood2.Biography)
	}
	gotBad, err := artistSvc.GetByID(context.Background(), bad.ID)
	if err != nil {
		t.Fatalf("GetByID bad: %v", err)
	}
	if gotBad.Biography != "" {
		t.Errorf("failing artist biography = %q, want empty: its write failed", gotBad.Biography)
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

// forceChangeIdentity rewrites one history row's id AND created_at in a single
// statement so a fixture can pin exactly where that row lands in the report's
// `ORDER BY created_at DESC, id DESC` ranking.
//
// Both columns move together because that ordering is the thing under test:
// setting one without the other leaves the tie half-constructed and the test
// would pass or fail on whichever random UUID the service happened to mint.
func forceChangeIdentity(t *testing.T, r *Router, oldID, newID string, ts time.Time) {
	t.Helper()
	res, err := r.db.ExecContext(context.Background(),
		`UPDATE metadata_changes SET id = ?, created_at = ? WHERE id = ?`,
		newID, ts.UTC().Format(time.RFC3339), oldID)
	if err != nil {
		t.Fatalf("forcing identity of change %s: %v", oldID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("forcing identity of change %s: rows affected: %v", oldID, err)
	}
	if n != 1 {
		t.Fatalf("forcing identity of change %s affected %d rows, want 1", oldID, n)
	}
}

// TestBlastRestore_SameSecondTieDoesNotClobberNewerValue is the regression test
// for the data-loss defect this endpoint shipped with.
//
// THE DEFECT. Eligibility check 3 asks the blast-radius report whether the
// requested row is still the row it would list for that (artist, field). That
// question is answered by the report's ranking, which is
// `ORDER BY mc.created_at DESC, mc.id DESC`, and every production history write
// stamps created_at at RFC 3339 SECOND resolution. When a newer operator edit
// and the damage row land inside the SAME SECOND, created_at TIES and the
// tiebreak compares two random UUIDs. About half the time the STALE damage row
// sorts first, check 3 passes, and the restore writes the old value over the
// operator's newer one -- returning 200 restored=1, so the operator has no
// signal that they just lost an edit. A one-second window needs only an
// operator editing a field while a scan or a bulk rule pass touches it.
//
// THE FIXTURE makes that coin-flip deterministic rather than hoping for it: it
// stamps both rows to the same second and forces the newer row's id to sort
// BELOW the damage row's, so the damage row provably wins the tiebreak every
// run. The precondition below asserts that it does; without it this test would
// silently stop exercising the defect the day the ranking changed.
//
// THE ASSERTION is the operator's value in the database afterwards, not a
// status code. A handler that refused for the wrong reason would still pass a
// status assertion; only reading the field back proves the edit survived.
func TestBlastRestore_SameSecondTieDoesNotClobberNewerValue(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Same Second Tie Artist")

	const (
		olderValue = "old operator bio"
		newerValue = "NEWER deliberate bio"
	)

	damageID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", olderValue, "")

	// The operator edits the field again, moments later. In production this is
	// an ordinary manual write racing a scan.
	manualCtx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(manualCtx, a.ID, "biography", newerValue); err != nil {
		t.Fatalf("writing the newer operator value: %v", err)
	}
	newerID := latestChangeID(t, historySvc, a.ID, "biography")

	// Collapse the two rows into one second and force the newer row's id to
	// lose the `id DESC` tiebreak. "0000..." sorts below any hex UUID, and the
	// damage row keeps the UUID the service minted for it.
	tie := damageBase.Add(time.Duration(damageSeq.Add(1)) * time.Second)
	backdateChange(t, r, damageID, tie)
	forceChangeIdentity(t, r, newerID, "00000000-0000-0000-0000-000000000000", tie)

	// PRECONDITION: the ranking really does put the STALE row first, so
	// eligibility check 3 passes and this test reaches check 4. If the report
	// already refused this row, the assertion below would pass vacuously
	// against the very bug it exists to catch.
	f := artist.BlastRadiusFilter{ArtistID: a.ID, Field: "biography"}
	f.Validate()
	rows, err := historySvc.ListBlastRadius(context.Background(), f)
	if err != nil {
		t.Fatalf("ListBlastRadius (precondition): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != damageID {
		t.Fatalf("precondition: report ranked %+v first, want the STALE damage row %s; "+
			"the same-second tie is not constructed, so this test would not exercise the defect",
			rows, damageID)
	}

	// PRECONDITION: the newer value is really in the field right now.
	before, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID before: %v", err)
	}
	if before.Biography != newerValue {
		t.Fatalf("precondition: biography = %q, want %q", before.Biography, newerValue)
	}

	// The PREVIEW must refuse too, and it is asserted separately from the
	// commit: the preview is what an operator reads before deciding, so a plan
	// that offers to write this row is already wrong even if the commit path
	// later catches it. Asserting only the commit would leave the plan-time
	// check untested, because the pre-write re-check would cover for it.
	_, preview := postRestore(t, r, `{"change_ids":["`+damageID+`"]}`)
	if preview.Eligible != 0 || preview.Refused != 1 {
		t.Fatalf("preview: eligible=%d refused=%d, want 0/1; a plan that lists this row as "+
			"restorable tells the operator a stale write is safe; items: %+v",
			preview.Eligible, preview.Refused, preview.Items)
	}
	if preview.Items[0].Reason != blastRefuseNotCurrent {
		t.Errorf("preview reason = %q, want %q", preview.Items[0].Reason, blastRefuseNotCurrent)
	}

	w, resp := postRestore(t, r, `{"change_ids":["`+damageID+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// THE ARTIFACT, read back from the database: the operator's newer edit
	// survived. This is the assertion that fails against the shipped defect.
	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if after.Biography != newerValue {
		t.Fatalf("biography = %q, want %q: the restore overwrote a NEWER operator value "+
			"with a stale one because the two history rows tied on created_at",
			after.Biography, newerValue)
	}

	if resp.Refused != 1 || resp.Restored != 0 {
		t.Fatalf("refused=%d restored=%d, want 1/0; items: %+v", resp.Refused, resp.Restored, resp.Items)
	}
	if resp.Items[0].Reason != blastRefuseNotCurrent {
		t.Errorf("reason = %q, want %q", resp.Items[0].Reason, blastRefuseNotCurrent)
	}
}

// TestBlastRestore_RefusesAnOlderDamageRowForTheSamePair pins the
// `rows[0].ID == change.ID` clause of eligibility check 3 specifically.
//
// The existing superseded-row test does not reach that clause: its newer edit
// recovers the pair, so the report returns NO row at all and the len(rows)==1
// half of the predicate already refuses. Deleting the id comparison therefore
// leaves that test green.
//
// This fixture damages the SAME (artist, field) TWICE. The report still lists
// the pair, so len(rows)==1 holds, but the row it lists is the SECOND damage
// row. Requesting the FIRST one is what only the id comparison can catch.
//
// The two damage rows deliberately share a new_value (both blank the field),
// so the live-value check 4 passes for either row and cannot mask the clause
// under test: check 3's id comparison is the ONLY thing refusing here.
func TestBlastRestore_RefusesAnOlderDamageRowForTheSamePair(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Twice Damaged Artist")

	firstDamageID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "first operator bio", "")
	secondDamageID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "second operator bio", "")

	if firstDamageID == secondDamageID {
		t.Fatalf("fixture: both damage rows have id %s; there is no older row to request", firstDamageID)
	}

	// PRECONDITION A: the report DOES still list this pair, so len(rows)==1 is
	// satisfied and the id comparison is the only clause left to do the work.
	f := artist.BlastRadiusFilter{ArtistID: a.ID, Field: "biography"}
	f.Validate()
	rows, err := historySvc.ListBlastRadius(context.Background(), f)
	if err != nil {
		t.Fatalf("ListBlastRadius (precondition): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("precondition: report rows = %+v, want exactly one; with no row the "+
			"len() clause would refuse and this test would not cover the id comparison", rows)
	}
	if rows[0].ID != secondDamageID {
		t.Fatalf("precondition: report lists %s, want the SECOND damage row %s", rows[0].ID, secondDamageID)
	}

	// PRECONDITION B: the live value equals BOTH damage rows' new_value, so
	// check 4 would accept the older row. Only the id comparison refuses it.
	before, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID before: %v", err)
	}
	if before.Biography != "" {
		t.Fatalf("precondition: biography = %q, want empty (both damage rows blanked it)",
			before.Biography)
	}

	w, resp := postRestore(t, r, `{"change_ids":["`+firstDamageID+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if resp.Refused != 1 || resp.Restored != 0 {
		t.Fatalf("refused=%d restored=%d, want 1/0; an older damage row for a pair the report "+
			"still lists must be refused: items: %+v", resp.Refused, resp.Restored, resp.Items)
	}
	if resp.Items[0].Reason != blastRefuseNotCurrent {
		t.Errorf("reason = %q, want %q", resp.Items[0].Reason, blastRefuseNotCurrent)
	}

	// THE ARTIFACT: the superseded operator value was NOT written back.
	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if after.Biography != "" {
		t.Errorf("biography = %q, want empty: restoring the FIRST damage row would resurrect "+
			"a value two writes out of date", after.Biography)
	}
}

// listBlastErrHistoryRepo delegates every history operation to a real repo
// except ListBlastRadius, which always fails. Isolating that one method is
// what makes the eligibility query's error path reachable while check 1
// (GetByID) still resolves the row normally, so the refusal provably comes
// from the currency check rather than from an id that stopped resolving.
//
// This uses artist.NewHistoryServiceWithRepo, an existing exported seam that
// three other test files in this package already decorate the same way. No
// production code was added to make this branch reachable.
type listBlastErrHistoryRepo struct {
	delegate artist.HistoryRepository
}

// errForcedBlastQuery is the sentinel the decorator returns. The handler logs
// it and refuses the row, so assertions are on the response and the database,
// never on this value reaching the caller.
var errForcedBlastQuery = errors.New("forced blast-radius query failure")

func (l listBlastErrHistoryRepo) Record(ctx context.Context, c *artist.MetadataChange) error {
	return l.delegate.Record(ctx, c)
}

func (l listBlastErrHistoryRepo) GetByID(ctx context.Context, id string) (*artist.MetadataChange, error) {
	return l.delegate.GetByID(ctx, id)
}

func (l listBlastErrHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) (
	[]artist.MetadataChange, int, error,
) {
	return l.delegate.List(ctx, artistID, limit, offset)
}

func (l listBlastErrHistoryRepo) ListGlobal(ctx context.Context, filter artist.GlobalHistoryFilter) (
	[]artist.MetadataChangeWithArtist, int, error,
) {
	return l.delegate.ListGlobal(ctx, filter)
}

func (l listBlastErrHistoryRepo) ListBlastRadius(_ context.Context, _ artist.BlastRadiusFilter) (
	[]artist.BlastRadiusRow, error,
) {
	return nil, errForcedBlastQuery
}

func (l listBlastErrHistoryRepo) CountBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) (
	artist.BlastRadiusCounts, error,
) {
	return l.delegate.CountBlastRadius(ctx, f)
}

// TestBlastRestore_EligibilityQueryErrorRefuses pins the documented safety
// property of isCurrentBlastRow: when the eligibility query cannot be answered,
// the row is REFUSED, never written.
//
// This is the predicate's default DIRECTION, and it is the whole reason the
// function returns (false, err) instead of an error the caller might read as
// "assume current". Inverting that one line to `return true, nil` leaves every
// other test in the suite green while turning an unreadable database into an
// unconditional license to overwrite operator data -- on the one endpoint whose
// entire purpose is recovery.
//
// The row is otherwise fully eligible: it resolves, it is trackable, it is not
// a revert, and the live field still holds the damage. The ONLY thing wrong is
// that the currency check cannot run.
func TestBlastRestore_EligibilityQueryErrorRefuses(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Query Failure Artist")

	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "bio behind a broken query", "")

	// PRECONDITION: with the real repo this exact request RESTORES. Without
	// this the test could pass against a fixture that was never eligible, and
	// "refused" would prove nothing about the error path.
	_, okResp := postRestore(t, r, `{"change_ids":["`+changeID+`"]}`)
	if okResp.Eligible != 1 {
		t.Fatalf("precondition: eligible = %d with a healthy query, want 1; items: %+v",
			okResp.Eligible, okResp.Items)
	}

	failing := artist.NewHistoryServiceWithRepo(listBlastErrHistoryRepo{delegate: historySvc.Repo()})
	r.historyService = failing

	// PRECONDITION: the decorator really does fail the eligibility query while
	// leaving id resolution intact, so the refusal below cannot come from the
	// row simply ceasing to exist.
	f := artist.BlastRadiusFilter{ArtistID: a.ID, Field: "biography"}
	f.Validate()
	if _, err := failing.ListBlastRadius(context.Background(), f); !errors.Is(err, errForcedBlastQuery) {
		t.Fatalf("precondition: ListBlastRadius returned %v, want errForcedBlastQuery", err)
	}
	if _, err := failing.GetByID(context.Background(), changeID); err != nil {
		t.Fatalf("precondition: GetByID through the decorator failed: %v", err)
	}

	w, resp := postRestore(t, r, `{"change_ids":["`+changeID+`"],"commit":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if resp.Refused != 1 || resp.Restored != 0 {
		t.Fatalf("refused=%d restored=%d, want 1/0: an unreadable eligibility check must "+
			"refuse, not assume the row is current; items: %+v", resp.Refused, resp.Restored, resp.Items)
	}
	if resp.Items[0].Reason != blastRefuseNotCurrent {
		t.Errorf("reason = %q, want %q", resp.Items[0].Reason, blastRefuseNotCurrent)
	}

	// THE ARTIFACT: nothing was written.
	after, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if after.Biography != "" {
		t.Errorf("biography = %q, want empty: a refused restore must not write", after.Biography)
	}
}

// TestBlastRestore_CommitRecheckRefusesAWriteRacedAfterPlanning pins the
// live-value re-check that runs immediately BEFORE each write.
//
// A check that runs only while the plan is built cannot close a window that
// opens after planning. The restore singleton excludes other RESTORES only; it
// does not exclude the scanner, the rule pipeline, or an ordinary field edit,
// any of which can land between the plan and the write. Without the re-check
// the handler writes a plan it verified against state that no longer exists,
// which on this endpoint means overwriting the very edit an operator made
// while reviewing the preview.
//
// The two phases are driven directly, plan then external write then commit,
// because that sequence is exactly the window and no HTTP-level fixture can
// interleave a third-party write inside one request.
func TestBlastRestore_CommitRecheckRefusesAWriteRacedAfterPlanning(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	a := addTestArtist(t, artistSvc, "Raced Commit Artist")

	const racedValue = "written while the operator was reading the preview"
	changeID := damageField(t, r, artistSvc, historySvc, a.ID, "biography", "bio at plan time", "")

	ctx := context.Background()
	items := r.planBlastRestore(ctx, []string{changeID})

	// PRECONDITION: the plan really did decide to write. If it had refused,
	// the commit loop would skip the row and the re-check below would never
	// run, leaving this test green for the wrong reason.
	if len(items) != 1 || items[0].Status != blastRestorePlanned {
		t.Fatalf("precondition: plan = %+v, want one planned item", items)
	}

	// The race: something else writes the field between the plan and the
	// commit. A plain operator edit stands in for the scanner or rule pass.
	manualCtx := artist.ContextWithSource(ctx, "manual")
	if _, err := artistSvc.UpdateField(manualCtx, a.ID, "biography", racedValue); err != nil {
		t.Fatalf("racing a write in after the plan: %v", err)
	}

	r.commitBlastRestore(ctx, items)

	// THE ARTIFACT: the raced-in value survived. This is the assertion that
	// fails when the commit path trusts the plan.
	after, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if after.Biography != racedValue {
		t.Fatalf("biography = %q, want %q: a write that landed after planning was "+
			"overwritten by a plan nobody re-verified", after.Biography, racedValue)
	}

	if items[0].Status != blastRestoreRefused || items[0].Reason != blastRefuseNotCurrent {
		t.Errorf("item status=%q reason=%q, want %q/%q",
			items[0].Status, items[0].Reason, blastRestoreRefused, blastRefuseNotCurrent)
	}
	// The refused item reports the value that caused the refusal, so an
	// operator reading the response sees what is actually in the field.
	if items[0].CurrentValue != racedValue {
		t.Errorf("current_value = %q, want %q", items[0].CurrentValue, racedValue)
	}
	if items[0].RestoreChangeID != "" {
		t.Errorf("restore_change_id = %q, want empty: nothing was written", items[0].RestoreChangeID)
	}
}
