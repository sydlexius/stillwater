package artist

// field_validation.go -- value-level validation for a single-field write, and
// the typed error a refusal travels as (#3037).
//
// WHERE ENFORCEMENT LIVES. Originally ValidateFieldUpdate had exactly one
// caller in the tree: internal/api's handleFieldUpdate. The rules below were
// therefore a property of ONE HTTP handler rather than of the data, and any
// service method reached by another route enforced them only insofar as its
// own callers happened to check first -- the shape that produces a defect the
// moment a second route appears. Enforcement now sits on the service write
// verbs instead. The error is typed so that a refusal on a service
// method can never be reported as UpdateField's benign (false, nil) -- both
// UpdateField and ClearField report "no write happened" that way, and callers
// render it as a no-op. A refusal is the opposite of benign.
//
// WHICH CODE CALLS THIS VALIDATOR, EXACTLY. The list below is the complete set
// of PRODUCTION call sites in the tree, and it is meant to be kept complete: an
// enumeration that claims more than the code does is a defect, not a nit.
// Established by
// `grep -rn 'ValidateFieldUpdate(' internal cmd --include='*.go' | grep -v _test`
// -- the sentence says PRODUCTION because that grep filters test callers out,
// and tests do call the validator directly as a unit.
//
//   - internal/api's handleFieldUpdate -- pre-existing, answers with a 400.
//   - Service.UpdateProviderField      -- and Service.ClearProviderField by
//     delegation, which calls it with "".
//   - Service.UpdateField              -- added by #3037's routing change.
//   - Service.ClearField               -- likewise; it asks the SAME validator
//     with "" rather than keeping a second table of clearable fields, since
//     the two tables disagreeing is what the defect was.
//   - Service.UpdateNameGuarded        -- likewise; validated there too because
//     handleFieldUpdate calls it DIRECTLY on the transactional rename path, so
//     a rule enforced only one level up in UpdateField would not cover it.
//
// NOT COVERED, deliberately: Service.Update (the whole-row persist) and
// Service.Create (the row INSERT) reach the name column without this
// validator. Both predate #3037 and are enumerated, with their production
// callers, in name_collision.go's scope block.
//
// WHY UpdateProviderField IS ON THE LIST at all, given no operator can reach a
// bad value through it today. It has two production callers, the API handler
// above (which pre-validates) and the provider_id_missing rule fixer in
// internal/rule (which does not). That fixer
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
//   - "name" must not normalize to an EMPTY identity key (see below).
//   - "musicbrainz_id" must be a valid UUID (or empty, which clears the ID).
//
// All other fields are accepted as-is (free-form text).
//
// THE EMPTY-IDENTITY-KEY RULE, and why a TrimSpace check is not enough.
// NormalizeIdentityKey folds dashes, underscores, spacing and Unicode format
// (Cf) characters out of a name entirely, so a name of "-", "_", an em-dash or
// a zero-width space passes TrimSpace and still normalizes to "". Everything
// keyed on identity then stops seeing the row: FindNameCollision and
// UpdateNameGuarded both treat an empty key as "not a collision" and skip
// their scan (name_collision.go), and the duplicates report's unionByNameKey
// skips empty keys too (duplicates.go). Two artists could both be named "-"
// and be invisible to BOTH -- a duplicate that neither the guard that exists
// to prevent it nor the report that exists to find it can see, which is
// strictly worse than the #2730 duplicate.
//
// Refusing it HERE, rather than at either of those two sites, is what makes it
// a class fix: this validator is the shared pre-write check, so one arm covers
// every path that runs it instead of patching a site. Which paths those are is
// the enumeration in the file header above. Of those, the three that can carry
// a "name" at all are handleFieldUpdate, Service.UpdateField/ClearField and
// Service.UpdateNameGuarded; UpdateProviderField and ClearProviderField return
// early for a field outside providerFieldMap, which holds only provider IDs.
//
// SCOPE: name ONLY. sort_name is deliberately NOT subject to this rule. It
// feeds display ordering, not identity -- no identity key, collision scan or
// duplicate grouping reads it -- and clearing it back to empty (so the artist
// sorts by name) is a legitimate operator act this must not block.
//
// NOT AN ALLOW-LIST OF SCRIPTS. The rule refuses a name only when the
// normalizer yields nothing at all, so scripts that carry an identity pass:
// CJK, Cyrillic, Greek, Hebrew, Arabic, Thai, Hangul and accented Latin all
// normalize to a non-empty key. Pinned by
// TestValidateFieldUpdate_AcceptsNonLatinNames, which exists precisely so a
// future tightening here cannot quietly start refusing legitimate names.
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
		// The Reason is a hand-authored literal and stays one: it reaches an
		// HTTP 400 body verbatim (Error() is passed straight through by
		// handleFieldUpdate), so it must never carry the rejected value, an
		// artist id, a column name or driver text.
		//
		// The enumeration has to match what NormalizeIdentityKey actually folds
		// away (dupkey.go), or the operator is told something other than what
		// happened. It folds spacing (step 4), Unicode format / Cf characters
		// such as a zero-width space or a soft hyphen (step 2), and dashes plus
		// underscores (steps 3 and 7) -- and "invisible formatting characters"
		// is how an operator reads that last class, not "category Cf". It does
		// NOT fold quotes or apostrophes away, so a name of "'" survives and is
		// accepted; the wording must not claim otherwise.
		if NormalizeIdentityKey(value) == "" {
			return &FieldValidationError{
				Field:  field,
				Reason: "name cannot be only dashes, underscores, spacing, or invisible formatting characters",
			}
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
