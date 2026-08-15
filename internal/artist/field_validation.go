package artist

// field_validation.go -- value-level validation for a single-field write, and
// the typed error a refusal travels as (#3037).
//
// WHERE ENFORCEMENT LIVES. Before this change, ValidateFieldUpdate had exactly
// one caller in the tree: internal/api's handleFieldUpdate. The rules below
// were therefore a property of ONE HTTP handler rather than of the data, and
// any service method reached by another route enforced them only insofar as
// its own callers happened to check first. This file is where that starts
// moving. The typed error lands first, so that a refusal on a service method
// can never be reported as UpdateField's benign (false, nil) -- both
// UpdateField and ClearField report "no write happened" that way, and callers
// render it as a no-op. A refusal is the opposite of benign.
//
// WHICH CODE CALLS THIS VALIDATOR, EXACTLY. The list below is the complete set
// of call sites in the tree, and it is meant to be kept complete: an
// enumeration that claims more than the code does is a defect, not a nit.
//
//   - internal/api's handleFieldUpdate -- pre-existing, answers with a 400.
//   - Service.UpdateProviderField      -- added by this change.
//   - Service.ClearProviderField       -- by delegation; calls the above with "".
//
// NOT COVERED, deliberately: Service.UpdateField, Service.ClearField and
// Service.UpdateNameGuarded do not call this validator, so an empty "name"
// reaching one of them is not refused here. Those are separate changes and
// this list grows when they land. Service.Update, the whole-row persist, is a
// wider pre-existing gap that is not in scope at all.
//
// WHY UpdateProviderField IS FIRST. It is the smallest complete case: it has
// two production callers, the API handler above (which pre-validates) and the
// provider_id_missing rule fixer in internal/rule (which does not). That fixer
// writes only discogs_id, deezer_id and spotify_id, none of which carry a rule
// below, so this closes the gap by CONSTRUCTION rather than by fixing an
// operator-reachable defect: the method stops depending on its callers
// validating for it. Adding a shape rule for another provider ID tomorrow is
// then a one-line change here, not a hunt for every writer.
//
// Note this file does NOT hold the only MBID shape check in the codebase --
// internal/api's identify handlers and album gate, and this package's own
// mbidcandidate.go, each call IsValidMBID for their own purposes. The narrow
// claim is about the single-field WRITE path reached through
// UpdateProviderField.
//
// A CLEAR IS AN UPDATE TO THE EMPTY STRING, and must stay one. ClearProviderField
// routes through UpdateProviderField with value "", and clearing a wrong ID is
// a legitimate operator act -- so the MBID rule accepts "" on purpose. Any rule
// added below has to be correct for that question too.

import (
	"errors"
	"strings"
)

// ErrInvalidFieldValue reports that a requested single-field write carried a
// value the field does not accept. Callers translate it to HTTP 400 and show
// the operator the specific reason; it is never a success and never a no-op.
var ErrInvalidFieldValue = errors.New("invalid field value")

// FieldValidationError is the typed form of a refused field write. Reason is
// the operator-facing sentence (it already names the field), so an API handler
// can pass Error() straight into a 400 body.
type FieldValidationError struct {
	// Field is the API field name that was refused.
	Field string
	// Reason is the human-readable explanation, e.g. "name cannot be empty".
	Reason string
}

// Error renders the operator-facing reason. Kept identical to the string the
// API's 400 body carried before this type existed, so the message an operator
// sees did not change when the error gained a type.
func (e *FieldValidationError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrInvalidFieldValue.Error()
	}
	return e.Reason
}

// Unwrap makes errors.Is(err, ErrInvalidFieldValue) true, so a caller that
// only needs to classify the failure never has to type-assert.
func (e *FieldValidationError) Unwrap() error { return ErrInvalidFieldValue }

// ValidateFieldUpdate returns a non-nil error when the field value is
// invalid. Validation rules:
//   - "name" must not be empty or whitespace-only.
//   - "musicbrainz_id" must be a valid UUID (or empty, which clears the ID).
//
// All other fields are accepted as-is (free-form text).
//
// CALLED WITH value == "" IT ALSO ANSWERS "may this field be cleared?", which
// is what lets a clear path share it instead of keeping a second table of
// clearable fields. "name" is refused; an MBID is allowed through, because
// clearing a wrong ID is a legitimate operator act.
func ValidateFieldUpdate(field, value string) error {
	switch field {
	case string(FieldArtistName):
		if strings.TrimSpace(value) == "" {
			return &FieldValidationError{Field: field, Reason: "name cannot be empty"}
		}
	case "musicbrainz_id":
		if value != "" && !isValidMBID(value) {
			return &FieldValidationError{
				Field:  field,
				Reason: "invalid MusicBrainz ID format (expected UUID)",
			}
		}
	}
	return nil
}
