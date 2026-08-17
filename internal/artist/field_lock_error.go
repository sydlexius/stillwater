package artist

// field_lock_error.go -- the typed error a LOCK refusal travels as, and the
// single validator the provider-ID write verbs ask (#3037).
//
// WHY A REFUSAL HERE, WHEN THE PERSIST CHOKEPOINT ALREADY RESTORES. The two
// answer different questions and both are needed.
//
// The chokepoint (lockguard.go) guards a WHOLE-ROW persist, where the caller
// handed over an entire artist and most of it is legitimate. It has no natural
// refusal: rejecting the write would discard the unlocked changes too. So it
// restores the locked field and lets the rest land.
//
// UpdateProviderField is not that shape. It is a SINGLE-FIELD verb: the one
// field it exists to write is the locked one, so "restore it and continue"
// means the entire operation was reverted while the method returned nil. Every
// caller then reports success for a write that did not happen -- the API
// answered 200, and the rule fixer counted a backfill it did not get. A verb
// whose only effect was refused must say so.
//
// THE REASON IS A HAND-AUTHORED LITERAL, and stays one. It is the same contract
// FieldValidationError.Reason carries: internal/api's fieldRefusalReason reads
// the typed field precisely so a future wrap of the error cannot leak a rendered
// error chain into a client response. It must never carry the rejected value, an
// artist id, a column name or driver text.

import (
	"context"
	"errors"
	"strings"
)

// ErrFieldLocked reports that a write was refused because the operator has that
// field locked. It is a sentinel so a caller can classify the refusal with
// errors.Is rather than matching on message text -- the same contract
// ErrInvalidFieldValue gives a validation refusal.
var ErrFieldLocked = errors.New("field is locked")

// FieldLockedError is the typed form of a write refused by a field lock.
// Reason is the operator-facing sentence and already names the field, so an API
// handler can pass Error() straight into a response body.
type FieldLockedError struct {
	// Field is the API field name whose lock refused the write.
	Field string
	// Reason is the human-readable explanation and the remedy.
	Reason string
}

// Error renders the operator-facing reason.
func (e *FieldLockedError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrFieldLocked.Error()
	}
	return e.Reason
}

// Unwrap makes errors.Is(err, ErrFieldLocked) true, so a caller that only needs
// to classify the failure never has to type-assert.
func (e *FieldLockedError) Unwrap() error { return ErrFieldLocked }

// refuseIfFieldLocked reports a *FieldLockedError when the STORED artist has
// field locked and ctx carries no grant for that same field.
//
// ONE VALIDATOR, BOTH VERBS. ClearProviderField is UpdateProviderField with "",
// so the single call site in UpdateProviderField covers the clear path too and
// the two verbs cannot drift into disagreeing about what a lock means. That is
// the same reason ValidateFieldUpdate is asked once rather than per verb.
//
// THE STORED ROW IS THE ONLY SOURCE OF THE LOCK SET, matching the chokepoint:
// a is the artist just read back by GetByID, never a caller-supplied struct
// whose LockedFields might be unpopulated.
//
// IT REFUSES WHETHER OR NOT THE VALUE WOULD CHANGE. A no-op write to a pinned
// field is rare (the rule backfill only writes a field it found empty), and
// "you may not write this field" is a contract a caller can reason about, where
// "you may write it as long as you happen to write what is already there" is
// not.
//
// The grant is the operator's, set per request and per field by the field-edit
// handlers -- see ContextWithLockOverride for why a blanket bypass on this verb
// would be wrong.
func (s *Service) refuseIfFieldLocked(ctx context.Context, a *Artist, field string) error {
	if a == nil || !s.IsFieldLocked(a, FieldName(field)) {
		return nil
	}
	// Normalized on both sides. ContextWithLockOverride lowercases and trims what
	// it stores, and IsFieldLocked above compares case-insensitively, so an
	// un-normalized compare here would be the one arm in the chain that could
	// silently decide a real grant does not match and revert an operator's edit.
	if granted, ok := lockOverrideField(ctx); ok && granted == strings.ToLower(strings.TrimSpace(field)) {
		return nil
	}
	return &FieldLockedError{
		Field:  field,
		Reason: "the " + field + " field is locked; unlock it before changing this value",
	}
}
