package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The PREVIEW must refuse a name restore that would collide, so the plan an
// operator approves matches what will land (#3039).
//
// Before this check the preview reported the row eligible, the summary read
// "1 of 1 selected change(s) can be restored", and only the commit refused it.
// Nothing wrong was written -- the safety property was always intact -- but the
// operator approved a plan that overstated itself.
//
// The positive control in the same request is what keeps this honest: a check
// that refused every name row would satisfy the refusal assertion on its own.
func TestBlastRestorePreview_RefusesANameRestoreThatWouldCollide(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	ctx := context.Background()

	// The colliding row: an operator's name, overwritten by a scan.
	damaged := addTestArtist(t, artistSvc, "Halloway Brass")
	collidingID := damageField(t, r, artistSvc, historySvc, damaged.ID,
		"name", "Marlowe Ensemble", "Marlowe Ensemble (Remastered)")

	// The squatter takes the identity the restore would put back. Created after
	// the damage so the rename above is not itself refused.
	squatter := addTestArtist(t, artistSvc, "Marlowe Ensemble")

	// POSITIVE CONTROL: an otherwise identical name row whose old value nobody
	// holds. It must still plan as eligible.
	free := addTestArtist(t, artistSvc, "Ashgrove Wind Band")
	freeID := damageField(t, r, artistSvc, historySvc, free.ID,
		"name", "Ashgrove Winds", "Ashgrove Winds (Live)")

	// PRECONDITIONS, asserted rather than assumed. A fixture that silently
	// stopped colliding would make the refusal assertion below pass vacuously,
	// and a row that is not a "name" row would never reach the new check at all.
	for _, tc := range []struct {
		id        string
		wantField string
	}{{collidingID, "name"}, {freeID, "name"}} {
		change, err := historySvc.GetByID(ctx, tc.id)
		if err != nil {
			t.Fatalf("fixture: loading change %s: %v", tc.id, err)
		}
		if change.Field != tc.wantField {
			t.Fatalf("fixture: change %s has field %q, want %q; the collision check only "+
				"runs for a name row", tc.id, change.Field, tc.wantField)
		}
	}
	collision, err := artistSvc.FindNameCollision(ctx, damaged.ID, "Marlowe Ensemble")
	if err != nil {
		t.Fatalf("fixture: checking for the collision: %v", err)
	}
	if collision == nil || collision.ArtistID != squatter.ID {
		t.Fatalf("fixture: FindNameCollision = %+v, want the squatter %s; nothing would be "+
			"refused and this test would assert nothing", collision, squatter.ID)
	}
	if c, err := artistSvc.FindNameCollision(ctx, free.ID, "Ashgrove Winds"); err != nil || c != nil {
		t.Fatalf("fixture: control FindNameCollision = %+v, err = %v; want no collision, "+
			"otherwise the control cannot distinguish a check from a blanket refusal", c, err)
	}

	w, plan := postRestore(t, r,
		fmt.Sprintf(`{"change_ids":[%q,%q],"commit":false}`, collidingID, freeID))
	if w.Code != 200 {
		t.Fatalf("preview status = %d; body: %s", w.Code, w.Body.String())
	}

	byID := map[string]blastRestoreItem{}
	for _, it := range plan.Items {
		byID[it.ChangeID] = it
	}

	if got := byID[collidingID]; got.Status != blastRestoreRefused || got.Reason != blastRefuseNameCollision {
		t.Errorf("colliding item = %+v, want refused as %q at PLAN time; the operator is being "+
			"offered a restore that the commit will refuse", got, blastRefuseNameCollision)
	}
	if got := byID[freeID]; got.Status != blastRestorePlanned {
		t.Errorf("control item = %+v, want status %q; the preview is refusing a legitimate "+
			"name restore, which is worse than the bug being fixed", got, blastRestorePlanned)
	}
	if plan.Eligible != 1 || plan.Refused != 1 {
		t.Errorf("eligible = %d, refused = %d, want 1 and 1; the summary line the operator "+
			"reads is built from these counts. items: %+v", plan.Eligible, plan.Refused, plan.Items)
	}

	// A preview writes nothing, collision or not.
	after, err := artistSvc.GetByID(ctx, damaged.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Name != "Marlowe Ensemble (Remastered)" {
		t.Errorf("name = %q after a preview, want it untouched", after.Name)
	}
}

// A collision check that could not RUN must refuse, not fall through. "The
// guard could not answer" is not evidence the restore is safe -- the standing
// rule for this guard everywhere else it appears (#2730).
func TestPlanNameCollisionRefusal_FailsClosedWhenTheCheckCannotRun(t *testing.T) {
	t.Parallel()
	r, _, _ := restoreTestRouter(t)

	// An artist id that resolves to nothing makes FindNameCollision's own
	// lookup fail, which is the only way this helper sees an error.
	reason, refuse := r.planNameCollisionRefusal(context.Background(), &artist.MetadataChange{
		ID: "change-1", ArtistID: "no-such-artist", Field: "name", OldValue: "Marlowe Ensemble",
	})
	if !refuse || reason != blastRefuseNotCurrent {
		t.Errorf("planNameCollisionRefusal = (%q, %v), want (%q, true); an unanswerable check "+
			"must refuse rather than let the row plan as eligible", reason, refuse, blastRefuseNotCurrent)
	}
}
