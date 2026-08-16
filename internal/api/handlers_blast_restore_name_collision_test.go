package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// A bulk restore refused by the NAME COLLISION guard must report
// name_collision, not restore_failed (#3037).
//
// Unlike every other refusal the endpoint reports, this one cannot be caught
// at plan time: validateRevertable sees only the immutable history row, while
// whether a name collides depends on the CURRENT database. So planning says
// "planned", the commit arm fires for real, and the refusal arrives from
// inside the writing transaction -- where the generic fall-through would file
// a refusal the operator CAN clear under a token that reads as a failed write.
func TestBlastRestore_NameCollisionIsHeldApartFromAFailedWrite(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	ctx := context.Background()

	// The damaged artist: an operator named it, then a scan renamed it.
	// Restoring puts the operator's name back.
	damaged := addTestArtist(t, artistSvc, "Halloway Brass")
	nameChangeID := damageField(t, r, artistSvc, historySvc, damaged.ID,
		"name", "Marlowe Ensemble", "Marlowe Ensemble (Remastered)")

	// The squatter, created only AFTER the damage so the operator's own rename
	// above is not itself refused. Service.Create writes the row with no
	// collision check, which is how this state arises in production: a scan or
	// a platform import seeds an artist under a name a history row still holds
	// as its old value.
	squatter := addTestArtist(t, artistSvc, "Marlowe Ensemble")

	// PRECONDITION: the identity really is taken, so the guard has something to
	// refuse. Without this the test would pass vacuously the moment the fixture
	// stopped colliding.
	collision, err := artistSvc.FindNameCollision(ctx, damaged.ID, "Marlowe Ensemble")
	if err != nil {
		t.Fatalf("fixture: checking for the collision: %v", err)
	}
	if collision == nil || collision.ArtistID != squatter.ID {
		t.Fatalf("fixture: FindNameCollision = %+v, want the squatter %s; the restore below "+
			"would succeed and exercise no refusal at all", collision, squatter.ID)
	}

	// The positive control, in the SAME request: a row that is legitimately
	// restorable. A classifier that refused everything would satisfy every
	// refusal assertion here without it.
	good := addTestArtist(t, artistSvc, "Ashgrove Wind Band")
	goodChangeID := damageField(t, r, artistSvc, historySvc, good.ID,
		"biography", "an operator wrote this", "a scan overwrote it")

	body := fmt.Sprintf(`{"change_ids":[%q,%q],"commit":%%t}`, nameChangeID, goodChangeID)

	itemFor := func(t *testing.T, resp blastRestoreResponse, id string) blastRestoreItem {
		t.Helper()
		for i := range resp.Items {
			if resp.Items[i].ChangeID == id {
				return resp.Items[i]
			}
		}
		t.Fatalf("no item for change %s in %+v", id, resp.Items)
		return blastRestoreItem{}
	}

	// The PREVIEW plans the colliding row. That is not a defect being locked
	// in, it is the fact that motivates the commit-side classifier: nothing at
	// plan time can answer this question, so the commit must answer it well.
	_, preview := postRestore(t, r, fmt.Sprintf(body, false))
	if got := itemFor(t, preview, nameChangeID); got.Status != blastRestorePlanned {
		t.Fatalf("preview item = %+v, want status %q; the fixture is not reaching the write "+
			"path where the collision guard runs", got, blastRestorePlanned)
	}

	w, commit := postRestore(t, r, fmt.Sprintf(body, true))
	if w.Code != 200 {
		t.Fatalf("commit status = %d; body: %s", w.Code, w.Body.String())
	}

	refused := itemFor(t, commit, nameChangeID)
	if refused.Status != blastRestoreRefused || refused.Reason != blastRefuseNameCollision {
		t.Errorf("commit item = %+v; want refused as %q. %q would tell the operator to retry the "+
			"same request, when what clears this is renaming or merging the other artist",
			refused, blastRefuseNameCollision, blastRefuseWriteFailed)
	}
	if got := itemFor(t, commit, goodChangeID); got.Status != blastRestoreRestored {
		t.Errorf("positive control = %+v, want status %q; the classifier refused a row it "+
			"should have written", got, blastRestoreRestored)
	}
	if commit.Restored != 1 || commit.Refused != 1 {
		t.Errorf("restored = %d, refused = %d, want 1 and 1; items: %+v",
			commit.Restored, commit.Refused, commit.Items)
	}

	// THE ARTIFACTS. The refused write did not land, and the eligible one did.
	after, err := artistSvc.GetByID(ctx, damaged.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Name != "Marlowe Ensemble (Remastered)" {
		t.Errorf("name = %q, want it left as the scan set it; the refused restore reached the "+
			"database and recreated the duplicate", after.Name)
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

// TestBlastRestoreWriteRefusal_ClassifiesACollision covers the classifier
// directly, including the shape the end-to-end test cannot produce: a
// *NameCollisionError whose Collision is nil. That is still a collision, and
// the single-revert path's extra non-nil guard exists there only because it
// DEREFERENCES the pointer to render the other artist. This function returns a
// token, so adding the same guard would misfile a nil as a retryable write
// failure.
func TestBlastRestoreWriteRefusal_ClassifiesACollision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a wrapped collision carrying the other artist",
			err: fmt.Errorf("writing field: %w", &artist.NameCollisionError{
				Collision: &artist.NameCollision{ArtistID: "other", Name: "Marlowe Ensemble"},
			}),
			want: blastRefuseNameCollision,
		},
		{
			name: "a collision with no partner recorded is still a collision",
			err:  &artist.NameCollisionError{},
			want: blastRefuseNameCollision,
		},
		{
			name: "the bare sentinel matches too",
			err:  artist.ErrNameCollision,
			want: blastRefuseNameCollision,
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
