package artist

import (
	"context"
	"errors"
	"testing"
)

// TestUpdateProviderField_ValidatesTheValue covers the behavior change in
// #3037: on this write path the musicbrainz_id shape rule ran only in the
// API's handleFieldUpdate, so the method's guarantee depended on its callers
// checking first, and the rule fixer that also calls it does not.
//
// Both controls below are load-bearing. The positive control proves the
// harness actually reaches the write, without which the refusal assertion
// would pass vacuously. The clear control pins the boundary the fix must not
// overreach: ClearProviderField routes through here with "", so a validator
// that refused every empty value would silently disable the clear affordance.
func TestUpdateProviderField_ValidatesTheValue(t *testing.T) {
	t.Parallel()
	svc, targetID, _ := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	const goodMBID = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"

	// POSITIVE CONTROL: a well-formed ID still writes.
	if err := svc.UpdateProviderField(ctx, targetID, "musicbrainz_id", goodMBID); err != nil {
		t.Fatalf("positive control: a valid MBID was refused: %v", err)
	}
	got, err := svc.GetByID(ctx, targetID)
	if err != nil {
		t.Fatalf("positive control: GetByID: %v", err)
	}
	if got.MusicBrainzID != goodMBID {
		t.Fatalf("positive control FAILED: musicbrainz_id = %q, want %q; the write never landed, "+
			"so the refusal assertion below would pass vacuously", got.MusicBrainzID, goodMBID)
	}

	// THE REFUSAL.
	err = svc.UpdateProviderField(ctx, targetID, "musicbrainz_id", "not-a-uuid")
	if err == nil {
		t.Fatal("UpdateProviderField accepted a malformed MusicBrainz ID")
	}
	if !errors.Is(err, ErrInvalidFieldValue) {
		t.Errorf("err = %v, want one matching ErrInvalidFieldValue", err)
	}
	if got, err = svc.GetByID(ctx, targetID); err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MusicBrainzID != goodMBID {
		t.Errorf("musicbrainz_id = %q, want the pre-refusal value; the refused write landed",
			got.MusicBrainzID)
	}

	// BOUNDARY CONTROL: clearing must still work.
	if err := svc.ClearProviderField(ctx, targetID, "musicbrainz_id"); err != nil {
		t.Fatalf("ClearProviderField was refused: %v; clearing a wrong ID is a legitimate act", err)
	}
	if got, err = svc.GetByID(ctx, targetID); err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MusicBrainzID != "" {
		t.Errorf("musicbrainz_id = %q, want cleared", got.MusicBrainzID)
	}
}

// TestFieldValidationError_RendersSafelyOnEveryShape pins the error type's own
// branches. Error() is what a log line prints and what can reach a 400 body,
// so a nil or reason-less value must render the generic sentinel rather than
// an empty string: an empty message reads as a refusal with no explanation.
func TestFieldValidationError_RendersSafelyOnEveryShape(t *testing.T) {
	t.Parallel()

	full := &FieldValidationError{Field: "name", Reason: "name cannot be empty"}
	if got := full.Error(); got != "name cannot be empty" {
		t.Errorf("Error() = %q, want the reason verbatim", got)
	}
	if !errors.Is(full, ErrInvalidFieldValue) {
		t.Error("errors.Is(err, ErrInvalidFieldValue) is false; callers classify on the sentinel")
	}

	noReason := &FieldValidationError{Field: "name"}
	if got := noReason.Error(); got != ErrInvalidFieldValue.Error() {
		t.Errorf("Error() with no reason = %q, want the generic sentinel", got)
	}

	var nilErr *FieldValidationError
	if got := nilErr.Error(); got != ErrInvalidFieldValue.Error() {
		t.Errorf("Error() on nil = %q, want the generic sentinel rather than a panic", got)
	}
}
