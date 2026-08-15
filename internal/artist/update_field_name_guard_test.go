package artist

import (
	"context"
	"errors"
	"testing"
)

// Service.UpdateField must not be a bypass around the #2730/#2807 rename guard
// (#3037).
//
// READ THIS BEFORE EDITING. Every "the write was refused" assertion below is
// paired with a POSITIVE CONTROL asserting a NON-colliding rename through the
// same call still lands. Without the pair, a harness that never reaches the
// write (a rejected field name, a no-op skip that fired early) makes the
// refusal assertion pass vacuously.

// TestUpdateField_NameRoutesThroughTheCollisionGuard is the direct case.
//
// Before this fix UpdateField wrote the name column straight through
// fieldColumnMap, so a caller who reached it -- the history-revert handler --
// produced two artists holding one identity. The routing makes the bypass
// unreachable rather than patching the callers.
func TestUpdateField_NameRoutesThroughTheCollisionGuard(t *testing.T) {
	t.Parallel()
	svc, localID, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	// POSITIVE CONTROL: a name nobody holds still writes through UpdateField.
	changed, err := svc.UpdateField(ctx, platformOnlyID, "name", "Harrowdene Ensemble")
	if err != nil {
		t.Fatalf("positive control: UpdateField(name) errored on a free name: %v", err)
	}
	if !changed {
		t.Fatal("positive control FAILED: UpdateField(name) reported no write for a free name; " +
			"the refusal assertion below would pass vacuously")
	}
	got, err := svc.GetByID(ctx, platformOnlyID)
	if err != nil {
		t.Fatalf("positive control: GetByID: %v", err)
	}
	if got.Name != "Harrowdene Ensemble" {
		t.Fatalf("positive control FAILED: name = %q, want %q; the write never reached the column",
			got.Name, "Harrowdene Ensemble")
	}

	// THE REFUSAL: rename onto the identity the OTHER artist holds.
	changed, err = svc.UpdateField(ctx, platformOnlyID, "name", "Northfield Chorale")
	if err == nil {
		t.Fatal("UpdateField(name) SUCCEEDED onto an identity another artist already holds; " +
			"this is the duplicate #2730 and #2807 exist to prevent")
	}
	if changed {
		t.Error("UpdateField reported a write it refused")
	}
	if !errors.Is(err, ErrNameCollision) {
		t.Errorf("err = %v, want one matching ErrNameCollision so callers can classify it", err)
	}

	// The refusal carries the colliding artist so a caller can name it.
	var ce *NameCollisionError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *NameCollisionError carrying the other artist", err, err)
	}
	if ce.Collision == nil || ce.Collision.ArtistID != localID {
		t.Errorf("collision = %+v, want the artist %s that holds the identity", ce.Collision, localID)
	}

	// AND NOTHING WAS WRITTEN. A refusal that still mutated the row would be
	// the defect wearing a returned error.
	got, err = svc.GetByID(ctx, platformOnlyID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Harrowdene Ensemble" {
		t.Errorf("name = %q, want the pre-refusal value %q; a refused rename must not reach the database",
			got.Name, "Harrowdene Ensemble")
	}

	// RUN IT TWICE. A refusal that corrupted transactional state would leave
	// the NEXT write broken while every assertion above stayed green -- the
	// lock-erasure shape this issue has produced before. A second legitimate
	// rename must still land, and a second attempt at the same collision must
	// still be refused identically.
	changed, err = svc.UpdateField(ctx, platformOnlyID, "name", "Verrall Sinfonia")
	if err != nil {
		t.Fatalf("second write after a refusal errored: %v; the refusal corrupted state", err)
	}
	if !changed {
		t.Error("second write after a refusal reported no write")
	}
	if _, err := svc.UpdateField(ctx, platformOnlyID, "name", "Northfield Chorale"); !errors.Is(err, ErrNameCollision) {
		t.Errorf("second refusal err = %v, want ErrNameCollision; the guard must decide the same way twice", err)
	}
}

// TestUpdateField_NameRefusalIsNotAnEmptyNoOp pins the SHAPE of the refusal,
// which is the half that reaches the operator.
//
// UpdateField's (false, nil) means "nothing needed doing", and a revert
// surface renders exactly that as a benign no-op. Returning it for a refused
// rename would tell an operator their undo was unnecessary while the duplicate
// they were trying to avoid sat one click away.
func TestUpdateField_NameRefusalIsNotAnEmptyNoOp(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")

	changed, err := svc.UpdateField(context.Background(), platformOnlyID, "name", "Northfield Chorale")
	if changed {
		t.Error("changed = true for a refused rename")
	}
	if err == nil {
		t.Fatal("err = nil for a refused rename: (false, nil) is the no-op contract, so a caller " +
			"renders it as 'this field was already at that value, nothing to change'")
	}
}

// TestUpdateField_NameOnAMissingArtistIsErrNotFound pins the classification
// the API layer branches on. Routing name writes through the transaction
// changed where the missing-row case is detected, and an unmapped
// sql.ErrNoRows would turn a 404 into a 500 on the revert surface.
func TestUpdateField_NameOnAMissingArtistIsErrNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")

	changed, err := svc.UpdateField(context.Background(), "no-such-artist", "name", "Harrowdene Ensemble")
	if changed {
		t.Error("changed = true for a rename of an artist that does not exist")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want one matching ErrNotFound so the handler renders a 404 rather "+
			"than an internal error", err)
	}
}

// TestUpdateField_NameNoOpStillReportsNoWrite keeps the no-op contract intact
// through the new route. UpdateNameGuarded has its own no-op skip, and it must
// surface as (false, nil) exactly as the pre-routing UpdateField did -- the
// revert surface reads that value.
func TestUpdateField_NameNoOpStillReportsNoWrite(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")

	changed, err := svc.UpdateField(context.Background(), platformOnlyID, "name", "Southgate Winds")
	if err != nil {
		t.Fatalf("UpdateField(name) with the stored value errored: %v", err)
	}
	if changed {
		t.Error("changed = true for a name write that matched the stored value; the no-op skip " +
			"must survive the routing")
	}
}

// TestUpdateField_SelfRenameIsNotACollision covers the exemption the guard
// already has: a cosmetic re-spelling that normalizes to the artist's OWN
// current identity key must still write. Without this, routing through the
// guard could have turned every "The Cure" -> "Cure" edit into a refusal.
func TestUpdateField_SelfRenameIsNotACollision(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	changed, err := svc.UpdateField(ctx, platformOnlyID, "name", "The Southgate Winds")
	if err != nil {
		t.Fatalf("UpdateField(name) refused a self-rename: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a self-rename that differs from the stored spelling")
	}
	got, err := svc.GetByID(ctx, platformOnlyID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "The Southgate Winds" {
		t.Errorf("name = %q, want %q", got.Name, "The Southgate Winds")
	}
}
