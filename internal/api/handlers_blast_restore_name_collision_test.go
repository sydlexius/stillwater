package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// A bulk restore refused by the NAME COLLISION guard must report
// name_collision, not restore_failed (#3037).
//
// The plan now runs its own collision check (#3039), so this test drives the
// one window that check cannot close: the plan-to-commit window INSIDE a single
// commit request. The handler plans every row once and then writes those same
// planned items, so a collision created after the plan is seen by nothing but
// the in-transaction guard.
//
// That window is why this test calls planBlastRestore and commitBlastRestore
// DIRECTLY rather than issuing a preview POST followed by a commit POST. A
// second HTTP request re-plans from scratch, so the plan-time check (#3039)
// would refuse the row before the write was ever attempted and this test would
// pass without the commit-side classifier existing at all. The direct calls are
// the same two functions handleBlastRadiusRestore invokes, in the same order.
func TestBlastRestore_NameCollisionIsHeldApartFromAFailedWrite(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := restoreTestRouter(t)
	ctx := context.Background()

	// The refused item's own fields do not record WHERE the refusal was
	// decided, so the write is observed as ATTEMPTED through the log line
	// commitBlastRestore emits when performRevert returns an error. Asserted
	// below so a refactor that once again lets a plan-time check shadow the
	// commit arm fails loudly instead of passing for a new reason.
	var logs bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))

	// The damaged artist: an operator named it, then a scan renamed it.
	// Restoring puts the operator's name back.
	damaged := addTestArtist(t, artistSvc, "Halloway Brass")
	nameChangeID := damageField(t, r, artistSvc, historySvc, damaged.ID,
		"name", "Marlowe Ensemble", "Marlowe Ensemble (Remastered)")

	// The positive control, in the SAME request: a row that is legitimately
	// restorable. A classifier that refused everything would satisfy every
	// refusal assertion here without it.
	good := addTestArtist(t, artistSvc, "Ashgrove Wind Band")
	goodChangeID := damageField(t, r, artistSvc, historySvc, good.ID,
		"biography", "an operator wrote this", "a scan overwrote it")

	itemFor := func(t *testing.T, items []blastRestoreItem, id string) blastRestoreItem {
		t.Helper()
		for i := range items {
			if items[i].ChangeID == id {
				return items[i]
			}
		}
		t.Fatalf("no item for change %s in %+v", id, items)
		return blastRestoreItem{}
	}

	// The PLAN is taken while the identity is still FREE, so it plans the row
	// honestly. The collision is created underneath it below, and the SAME
	// planned items are then committed -- the window no plan-time check can
	// close.
	items := r.planBlastRestore(ctx, []string{nameChangeID, goodChangeID})
	if got := itemFor(t, items, nameChangeID); got.Status != blastRestorePlanned {
		t.Fatalf("planned item = %+v, want status %q; the fixture is not reaching the write "+
			"path where the collision guard runs", got, blastRestorePlanned)
	}

	// The squatter, created only AFTER the damage AND after the plan.
	// Service.Create writes the row with no collision check, which is how this
	// state arises in production: a scan or a platform import seeds an artist
	// under a name a history row still holds as its old value.
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

	r.commitBlastRestore(ctx, items)

	// The write was ATTEMPTED and refused in the transaction. Without this the
	// name_collision assertion below cannot tell the commit-side classifier
	// apart from a plan-time refusal that shadowed it.
	if !strings.Contains(logs.String(), "blast restore: writing restore") {
		t.Fatalf("the commit logged no write attempt, so performRevert was never reached and "+
			"the refusal below did not come from the in-transaction guard; logs: %s", logs.String())
	}

	refused := itemFor(t, items, nameChangeID)
	if refused.Status != blastRestoreRefused || refused.Reason != blastRefuseNameCollision {
		t.Errorf("commit item = %+v; want refused as %q. %q would tell the operator to retry the "+
			"same request, when what clears this is renaming or merging the other artist",
			refused, blastRefuseNameCollision, blastRefuseWriteFailed)
	}
	if got := itemFor(t, items, goodChangeID); got.Status != blastRestoreRestored {
		t.Errorf("positive control = %+v, want status %q; the classifier refused a row it "+
			"should have written", got, blastRestoreRestored)
	}
	// The counters the response would carry, through the same function the
	// handler uses to derive them.
	if _, restoredN, _, refusedN := summarizeBlastRestore(items); restoredN != 1 || refusedN != 1 {
		t.Errorf("restored = %d, refused = %d, want 1 and 1; items: %+v",
			restoredN, refusedN, items)
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
		{
			// ORDER PIN. Nothing in production can hand this function an error
			// satisfying BOTH sentinels: artist.Service.UpdateField runs
			// ValidateFieldUpdate first and returns on its error, so a call
			// yielding ErrInvalidFieldValue never reaches
			// updateNameThroughGuard, the only producer of ErrNameCollision.
			// But that is an invariant of internal/artist/service.go, not of
			// this file, and swapping the two arms here left the entire suite
			// green -- so the intended winner was documented nowhere and
			// protected by nothing.
			//
			// old_value_invalid wins, and the choice is not arbitrary. It says
			// no retry can ever succeed; name_collision sends the operator off
			// to rename or merge another artist first. If both hold, that
			// errand ends in the same refusal, so the permanent verdict is the
			// one that does not waste the operator's time.
			name: "both sentinels at once resolves to the permanent refusal",
			err:  errors.Join(artist.ErrInvalidFieldValue, artist.ErrNameCollision),
			want: blastRefuseInvalidOldValue,
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
