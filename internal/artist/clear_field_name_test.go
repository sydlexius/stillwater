package artist

import (
	"context"
	"errors"
	"testing"
)

// clear_field_name_test.go -- Service.ClearField is the OTHER verb that reached
// the name column without validation (#3037).
//
// THE DEFECT. ClearField resolved "name" through fieldColumnMap and wrote
// name = ''. Nothing rejected it: artists.name is NOT NULL but has no
// non-empty CHECK, the collision guard exempts an empty identity key by
// design, and the only check that DID reject an empty name
// (ValidateFieldUpdate) ran in one HTTP handler. A blank name leaves the row
// unmatchable by every identity mechanism that keys on the name.
//
// The caller that drives it is the history revert: performRevert
// (internal/api/handlers_history.go) branches on change.OldValue == "" and
// calls ClearField. That caller could not reach the name field when this
// refusal landed, because validateRevertable rejects any field outside
// trackableFields and "name" was outside it then -- so the refusal was a guard
// placed BEFORE the affordance that would open the hole. #3037's
// trackableFields change opened that affordance, and validateRevertable now
// answers such a row 400 before performRevert runs, leaving this refusal as
// the second of two independent gates rather than the only one.
//
// Fixing UpdateField and leaving this one is the exact two-sources-disagree
// shape the routing exists to close, which is why both verbs share ONE
// validator rather than each keeping a table of what it will write.
//
// READ THIS BEFORE EDITING. Every refusal assertion is paired with a POSITIVE
// CONTROL through the SAME call, because a ClearField that refused EVERYTHING
// would satisfy a refusal-only test -- and that over-correction would break
// every legitimate clear an operator performs.

// TestClearField_RefusesTheNameField is the direct case.
func TestClearField_RefusesTheNameField(t *testing.T) {
	t.Parallel()
	svc, _, targetID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	// POSITIVE CONTROL: a genuinely clearable field still clears through the
	// same method.
	if _, err := svc.UpdateField(ctx, targetID, "biography", "a stored biography"); err != nil {
		t.Fatalf("positive control: seeding biography: %v", err)
	}
	cleared, err := svc.ClearField(ctx, targetID, "biography")
	if err != nil {
		t.Fatalf("positive control: ClearField(biography) errored: %v", err)
	}
	if !cleared {
		t.Fatal("positive control FAILED: ClearField(biography) reported no write; " +
			"the refusal assertion below would pass vacuously")
	}
	got, err := svc.GetByID(ctx, targetID)
	if err != nil {
		t.Fatalf("positive control: GetByID: %v", err)
	}
	if got.Biography != "" {
		t.Fatalf("positive control FAILED: biography = %q, want cleared", got.Biography)
	}
	nameBefore := got.Name
	if nameBefore == "" {
		t.Fatal("fixture FAILED: the seeded artist has no name, so blanking it would be undetectable")
	}

	// THE REFUSAL.
	changed, err := svc.ClearField(ctx, targetID, "name")
	if err == nil {
		t.Fatal("ClearField(name) SUCCEEDED; a blank name persists (NOT NULL, no non-empty CHECK) " +
			"and leaves the artist unmatchable by every identity mechanism that keys on it")
	}
	if changed {
		t.Error("ClearField reported a write it refused")
	}
	if !errors.Is(err, ErrInvalidFieldValue) {
		t.Errorf("err = %v, want one matching ErrInvalidFieldValue so callers can classify it", err)
	}

	// AND NOTHING WAS WRITTEN.
	got, err = svc.GetByID(ctx, targetID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != nameBefore {
		t.Errorf("name = %q, want the pre-refusal value %q; a refused clear must not reach the database",
			got.Name, nameBefore)
	}

	// RUN IT TWICE. The refusal must not have left the row or the service in a
	// state that breaks the NEXT write: a second clearable field still clears,
	// and a second attempt at the name is refused identically.
	if _, err := svc.UpdateField(ctx, targetID, "origin", "Harrowdene"); err != nil {
		t.Fatalf("seeding origin after a refusal: %v", err)
	}
	cleared, err = svc.ClearField(ctx, targetID, "origin")
	if err != nil || !cleared {
		t.Errorf("ClearField(origin) after a refusal = (%t, %v), want (true, nil); "+
			"the refusal corrupted state", cleared, err)
	}
	if _, err := svc.ClearField(ctx, targetID, "name"); !errors.Is(err, ErrInvalidFieldValue) {
		t.Errorf("second ClearField(name) err = %v, want ErrInvalidFieldValue; "+
			"the validator must decide the same way twice", err)
	}
}

// TestUpdateField_RefusesAnEmptyName covers the same class from the UPDATE
// side. UpdateField DOES route through UpdateNameGuarded, but the collision
// guard exempts an empty identity key on purpose, so without the shared
// validator nothing would reject the value.
func TestUpdateField_RefusesAnEmptyName(t *testing.T) {
	t.Parallel()
	svc, _, targetID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	// POSITIVE CONTROL: a real rename through the same call still lands.
	changed, err := svc.UpdateField(ctx, targetID, "name", "Harrowdene Ensemble")
	if err != nil {
		t.Fatalf("positive control: UpdateField(name) errored on a free name: %v", err)
	}
	if !changed {
		t.Fatal("positive control FAILED: UpdateField(name) reported no write for a free name")
	}

	// Blank and punctuation-only both normalize to an empty identity key; the
	// two arms of the rule are asserted through the write path here rather
	// than re-tested as validator units (punctuation_only_name_test.go already
	// owns that).
	for _, blank := range []string{"", "   ", "-"} {
		changed, err := svc.UpdateField(ctx, targetID, "name", blank)
		if err == nil {
			t.Fatalf("UpdateField(name, %q) SUCCEEDED; it normalizes to an empty identity key, "+
				"so the artist becomes invisible to the collision guard and the duplicates report", blank)
		}
		if changed {
			t.Errorf("UpdateField(name, %q) reported a write it refused", blank)
		}
		if !errors.Is(err, ErrInvalidFieldValue) {
			t.Errorf("UpdateField(name, %q) err = %v, want one matching ErrInvalidFieldValue", blank, err)
		}
	}

	got, err := svc.GetByID(ctx, targetID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Harrowdene Ensemble" {
		t.Errorf("name = %q, want the pre-refusal value; a refused write must not reach the database", got.Name)
	}
}

// TestUpdateNameGuarded_RefusesAnEmptyName pins the refusal on the method
// handleFieldUpdate calls DIRECTLY.
//
// This is the site that would be missed by validating only in UpdateField: the
// API's transactional rename path calls UpdateNameGuarded itself, so a rule
// enforced one level up would not cover it. That is the same
// one-entry-point-of-two shape the defect took.
func TestUpdateNameGuarded_RefusesAnEmptyName(t *testing.T) {
	t.Parallel()
	svc, _, targetID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	// POSITIVE CONTROL through the same method.
	collision, changed, err := svc.UpdateNameGuarded(ctx, targetID, "Harrowdene Ensemble")
	if err != nil || collision != nil || !changed {
		t.Fatalf("positive control FAILED: UpdateNameGuarded on a free name = (%+v, %t, %v), "+
			"want (nil, true, nil)", collision, changed, err)
	}

	collision, changed, err = svc.UpdateNameGuarded(ctx, targetID, "")
	if err == nil {
		t.Fatal("UpdateNameGuarded(\"\") SUCCEEDED; the collision check exempts an empty " +
			"identity key by design, so the guard's own exemption becomes the hole")
	}
	if collision != nil || changed {
		t.Errorf("collision = %+v, changed = %t; want (nil, false) alongside the refusal", collision, changed)
	}
	if !errors.Is(err, ErrInvalidFieldValue) {
		t.Errorf("err = %v, want one matching ErrInvalidFieldValue", err)
	}

	got, err := svc.GetByID(ctx, targetID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Harrowdene Ensemble" {
		t.Errorf("name = %q, want the pre-refusal value; a refused write must not reach the column", got.Name)
	}
}

// TestClearField_SharesTheValidatorWithUpdateField is the property that keeps
// the two verbs from disagreeing, asserted directly rather than inferred from
// the cases above.
//
// The defect was two sources of truth: UpdateField's rules and ClearField's
// implicit "any field may be cleared". Asking the SAME validator with value ""
// is what makes a divergence impossible, so this pins the equivalence across
// every editable field rather than the handful a case test happens to touch.
func TestClearField_SharesTheValidatorWithUpdateField(t *testing.T) {
	t.Parallel()
	svc, _, targetID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	// PRECONDITION: the validator refuses at least one field with "", or the
	// equivalence below would hold vacuously against a validator that accepts
	// everything.
	if ValidateFieldUpdate(string(FieldArtistName), "") == nil {
		t.Fatal("PRECONDITION FAILED: the validator accepts an empty name, so this test " +
			"cannot distinguish a shared validator from no validator at all")
	}

	for _, field := range EditableFieldsList() {
		// Provider-ID fields are excluded: ClearField does not own them
		// (ClearProviderField does), and on this fixture they are already
		// empty, so ClearField's no-op skip would return before the validator
		// mattered and the case would pass without proving anything.
		if IsProviderIDField(field) {
			continue
		}
		wantRefused := ValidateFieldUpdate(field, "") != nil
		_, err := svc.ClearField(ctx, targetID, field)
		gotRefused := errors.Is(err, ErrInvalidFieldValue)
		if gotRefused != wantRefused {
			t.Errorf("ClearField(%q): refused = %t, but ValidateFieldUpdate(%q, \"\") refuses = %t; "+
				"the clear path is deciding from a second source of truth", field, gotRefused, field, wantRefused)
		}
		if !wantRefused && err != nil {
			t.Errorf("ClearField(%q) errored on a field the validator accepts: %v", field, err)
		}
	}
}
