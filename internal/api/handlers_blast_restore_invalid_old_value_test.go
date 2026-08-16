package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The BULK restore must hold a REFUSED value apart from a FAILED write
// (#3037). Filing a refused value under "restore_failed" tells the operator a
// retry might work, and for this class it never can.
//
// THE FIXTURE. An artist row arrives with no name -- a platform import that
// resolved no title, a partial scan -- and something later fills it in. #3037
// put "name" into trackableFields, so that fill produces a history row whose
// OLD value is the empty string. Restoring that row writes name = '', which
// persists (artists.name is NOT NULL but carries no non-empty CHECK) and
// leaves the artist unmatchable by every identity mechanism keyed on the name.
//
// These tests were held back until "name" became trackable: before that, the
// same rows were refused as errRevertNotTrackable and would have passed while
// exercising the wrong arm.
//
// SCOPE, stated so this is not read as more than it is: the blast-radius
// REPORT would not list such a row anyway, because its query keeps only rows
// where the operator HAD a value. The endpoint nevertheless accepts whatever
// change_ids a client sends, so it must classify on its own rather than trust
// that the report never offered it -- the same fail-closed posture the
// currency re-check already takes.

// seedEmptyOldNameChange builds the fixture and returns the artist id and the
// id of the history row recording the fill.
//
// The name is filled with Service.Update (the whole-row persist), NOT
// UpdateField: UpdateField's own validation refuses an empty name, so it
// cannot produce a row with an empty OLD value at all. Update diffs
// trackableFields and records the change, which is how a scan or a platform
// sync produces this row in production.
//
// It asserts its own defining properties before returning, so a fixture defect
// fails here rather than turning a later assertion vacuous.
func seedEmptyOldNameChange(t *testing.T, artistSvc *artist.Service,
	historySvc *artist.HistoryService, filledName string,
) (artistID, changeID string) {
	t.Helper()
	ctx := context.Background()

	a := &artist.Artist{Name: "", SortName: "", Type: "group", Path: "/music/" + filledName}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("fixture: creating an unnamed artist: %v", err)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("fixture: GetByID: %v", err)
	}
	// PRECONDITION: the row really starts with an empty name. If Create ever
	// begins defaulting it, the history row below would carry a non-empty old
	// value and this fixture would stop describing the defect.
	if stored.Name != "" {
		t.Fatalf("fixture: seeded artist name = %q, want empty; the empty-old-value history "+
			"row this fixture needs cannot be produced", stored.Name)
	}

	stored.Name = filledName
	stored.SortName = filledName
	if err := artistSvc.Update(artist.ContextWithSource(ctx, "scan"), stored); err != nil {
		t.Fatalf("fixture: filling in the name: %v", err)
	}

	changes, _, err := historySvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("fixture: listing history: %v", err)
	}
	for i := range changes {
		if changes[i].Field != "name" {
			continue
		}
		// PRECONDITION: the OLD value is empty. That is what makes this row's
		// restore a refusal rather than an ordinary write.
		if changes[i].OldValue != "" {
			t.Fatalf("fixture: name row old_value = %q, want empty; the refusal arm would "+
				"go unexercised", changes[i].OldValue)
		}
		changeID = changes[i].ID
		break
	}
	if changeID == "" {
		t.Fatal("fixture: no \"name\" history row was recorded; name is in trackableFields as of " +
			"#3037, so its absence means the fixture never reached the restore surface")
	}
	return a.ID, changeID
}

// TestBlastRestore_EmptyNameIsRefusedIdenticallyInPreviewAndCommit is the
// point of the change: the PREVIEW and the COMMIT must give the same answer
// for the same row, so an operator is never offered a restore that is only
// refused after they approve it.
//
// Each request carries a legitimately restorable row alongside the refused
// one. That is the POSITIVE CONTROL: a classifier that refused everything
// would satisfy every refusal assertion here.
func TestBlastRestore_EmptyNameIsRefusedIdenticallyInPreviewAndCommit(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	ctx := context.Background()

	badArtistID, badChangeID := seedEmptyOldNameChange(t, artistSvc, historySvc, "Wrenfield Consort")
	good := addTestArtist(t, artistSvc, "Ashgrove Players")
	goodChangeID := damageField(t, r, artistSvc, historySvc, good.ID,
		"biography", "an operator wrote this", "a scan overwrote it")

	body := fmt.Sprintf(`{"change_ids":[%q,%q],"commit":%%t}`, badChangeID, goodChangeID)

	// reasonFor pulls one item out of a response by change id, so the
	// assertions never depend on item ordering.
	reasonFor := func(t *testing.T, resp blastRestoreResponse, id string) blastRestoreItem {
		t.Helper()
		for i := range resp.Items {
			if resp.Items[i].ChangeID == id {
				return resp.Items[i]
			}
		}
		t.Fatalf("no item for change %s in %+v", id, resp.Items)
		return blastRestoreItem{}
	}

	w, preview := postRestore(t, r, fmt.Sprintf(body, false))
	if w.Code != 200 {
		t.Fatalf("preview status = %d; body: %s", w.Code, w.Body.String())
	}
	previewBad := reasonFor(t, preview, badChangeID)
	if previewBad.Status != blastRestoreRefused || previewBad.Reason != blastRefuseInvalidOldValue {
		t.Errorf("preview item = %+v; want refused as %q, so the operator learns the row is "+
			"unrestorable while still deciding", previewBad, blastRefuseInvalidOldValue)
	}
	if got := reasonFor(t, preview, goodChangeID); got.Status != blastRestorePlanned {
		t.Errorf("preview item for the restorable row = %+v, want status %q; the classifier "+
			"refused a row it should have planned", got, blastRestorePlanned)
	}
	if preview.Eligible != 1 {
		t.Errorf("preview eligible = %d, want 1 (the restorable row only)", preview.Eligible)
	}

	w, commit := postRestore(t, r, fmt.Sprintf(body, true))
	if w.Code != 200 {
		t.Fatalf("commit status = %d; body: %s", w.Code, w.Body.String())
	}
	commitBad := reasonFor(t, commit, badChangeID)
	if commitBad.Status != previewBad.Status || commitBad.Reason != previewBad.Reason {
		t.Errorf("commit reported %s/%s but the preview reported %s/%s for the same row; the two "+
			"surfaces must agree", commitBad.Status, commitBad.Reason, previewBad.Status, previewBad.Reason)
	}
	if commit.Restored != 1 || commit.Refused != 1 {
		t.Errorf("restored = %d, refused = %d, want 1 and 1; items: %+v",
			commit.Restored, commit.Refused, commit.Items)
	}
	if commit.Unchanged != 0 {
		t.Errorf("unchanged = %d, want 0; reporting a refusal as \"the field already held that "+
			"value\" would be false", commit.Unchanged)
	}

	// THE ARTIFACTS. The refused restore did not reach the database, and the
	// eligible one did.
	after, err := artistSvc.GetByID(ctx, badArtistID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Name != "Wrenfield Consort" {
		t.Errorf("name = %q, want the filled-in value; the refused restore reached the database "+
			"and blanked the artist", after.Name)
	}
	restored, err := artistSvc.GetByID(ctx, good.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if restored.Biography != "an operator wrote this" {
		t.Errorf("biography = %q, want the operator value put back; the positive control did "+
			"not actually restore", restored.Biography)
	}
}

// TestBlastRestoreWriteRefusal_ClassifiesAFieldRefusal covers the commit-path
// classifier at unit level. It has to be tested here rather than end to end:
// planning already refuses these rows, so no request can drive performRevert
// into returning ErrInvalidFieldValue, and the arm exists for the case where
// that stops being true.
func TestBlastRestoreWriteRefusal_ClassifiesAFieldRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a field refusal is held apart from a failed write",
			err: fmt.Errorf("writing field: %w",
				&artist.FieldValidationError{Field: "name", Reason: "name cannot be empty"}),
			want: blastRefuseInvalidOldValue,
		},
		{
			name: "a bare sentinel matches too",
			err:  artist.ErrInvalidFieldValue,
			want: blastRefuseInvalidOldValue,
		},
		{
			name: "anything else is still a failed write the operator may retry",
			err:  errors.New("database is locked"),
			want: blastRefuseWriteFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := blastRestoreWriteRefusal(tc.err); got != tc.want {
				t.Errorf("blastRestoreWriteRefusal(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
