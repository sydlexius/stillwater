package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// lock_restore.go -- the guarded write verb for the locked-field damage
// repair (#3038 / #3075 fix round).
//
// WHY A NEW VERB AND NOT Service.UpdateField. UpdateField is deliberately
// lock-blind and unconditional: the operator's history revert and
// blast-radius restore both use it to write INTO a locked field on the
// operator's say-so, and neither wants a compare-and-set against a value the
// caller predicted. The repair does: its candidate was selected from a list
// read at the top of a whole pass, in a goroutine, while the server serves
// operators. Between selection and write an operator holding a grant can edit
// the field (or unlock it), and an unconditional restore would then overwrite
// the operator's fresh data -- recorded as source="revert", which the
// blast-radius damage predicate excludes, so the loss would be invisible in
// the pane. Changing UpdateField's contract to conditional would break its
// existing callers' semantics, so the repair gets its own narrowly-scoped
// verb instead.
//
// THE DECISION IS ATOMIC IN THE REPOSITORY LAYER. The stored value is read,
// compared, and overwritten inside one transaction, and the UPDATE itself
// repeats the equality condition (WHERE id = ? AND <col> = ?), so the write
// applies only if the row still holds the exact damaged value the candidate
// was selected for. A Go-side read-then-write over two statements is exactly
// the window this verb exists to close. The lock set is re-read in the same
// transaction, so unlocking between the caller's check and this write also
// diverts the restore.

// LockedFieldRestoreOutcome reports what RestoreLockedFieldGuarded decided.
// Every value is a decided outcome, not an error: the caller counts diverted
// restores rather than retrying them.
type LockedFieldRestoreOutcome int

const (
	// LockedFieldRestoreApplied reports that the conditional write landed.
	LockedFieldRestoreApplied LockedFieldRestoreOutcome = iota
	// LockedFieldRestoreValueDiverged reports that the stored value no longer
	// equals the damaged value the candidate was selected for, so the restore
	// would have overwritten newer data. Nothing was written.
	LockedFieldRestoreValueDiverged
	// LockedFieldRestoreUnlocked reports that the field is no longer in the
	// artist's locked_fields, so the operator has withdrawn the intent the
	// repair exists to serve. Nothing was written.
	LockedFieldRestoreUnlocked
)

// RestoreLockedFieldGuarded writes restoreValue into the artist's field ONLY
// if, inside a single transaction, the field is still locked and the stored
// value still equals damagedValue. It exists for the locked-field damage
// repair and is not a general-purpose update: use Service.UpdateField for an
// operator-driven write.
//
// The value is validated with the same ValidateFieldUpdate the other
// single-field verbs run, and a "name" restore runs the identity-collision
// scan the guarded rename runs, refusing with a *NameCollisionError rather
// than recreating a duplicate the operator's later rename removed. Both
// refusals are DETERMINISTIC: they recur identically on every retry, and the
// caller classifies them via errors.Is(err, ErrInvalidFieldValue) /
// errors.Is(err, ErrNameCollision).
//
// On success a history row is recorded (best-effort, matching UpdateField)
// with the source carried by ctx.
func (s *Service) RestoreLockedFieldGuarded(ctx context.Context, id, field, damagedValue, restoreValue string) (LockedFieldRestoreOutcome, error) {
	if err := ValidateFieldUpdate(field, restoreValue); err != nil {
		return 0, err
	}
	col, ok := fieldColumnMap[field]
	if !ok {
		// Provider IDs and other side-table fields have no artists column and
		// no per-field history to restore from. Deterministic by construction,
		// so it travels as an ErrInvalidFieldValue the caller retires rather
		// than retrying every boot.
		return 0, &FieldValidationError{Field: field,
			Reason: fmt.Sprintf("field %q cannot be restored: it is not stored on the artists row", field)}
	}

	db, err := s.artistDB()
	if err != nil {
		return 0, fmt.Errorf("guarded restore: %w", err)
	}
	if db == nil {
		return 0, fmt.Errorf("guarded restore: nil database handle")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("guarded restore: beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after commit success is a no-op; on the error path the original error is what callers act on

	var storedRaw, lockedRaw string
	if err := tx.QueryRowContext(ctx,
		`SELECT `+col+`, locked_fields FROM artists WHERE id = ?`, id).Scan(&storedRaw, &lockedRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("guarded restore: loading artist %s: %w", id, ErrNotFound)
		}
		return 0, fmt.Errorf("guarded restore: loading artist %s: %w", id, err)
	}

	// The lock is re-verified INSIDE the transaction, so an unlock between the
	// caller's check and this write diverts the restore instead of landing a
	// value into a field the operator just released.
	lockedFields := UnmarshalStringSlice(lockedRaw)
	if !(&Service{}).IsFieldLocked(&Artist{LockedFields: lockedFields}, FieldName(field)) {
		return LockedFieldRestoreUnlocked, nil
	}

	// Compare in value form: slice fields store JSON but history rows carry the
	// joined representation, which is what damagedValue is.
	stored := storedRaw
	if sliceFields[field] {
		stored = strings.Join(UnmarshalStringSlice(storedRaw), ", ")
	}
	if normalizeFieldValue(field, stored) != normalizeFieldValue(field, damagedValue) {
		return LockedFieldRestoreValueDiverged, nil
	}

	// A "name" restore must not recreate an identity duplicate a later rename
	// removed. Same two non-collision cases as UpdateNameGuarded: an empty key
	// (already refused by the validator above) and a cosmetic same-key rename.
	if field == string(FieldArtistName) {
		newKey := NormalizeIdentityKey(restoreValue)
		if newKey != "" && NormalizeIdentityKey(stored) != newKey {
			collision, err := findCollisionPartner(ctx, tx, id, newKey)
			if err != nil {
				return 0, fmt.Errorf("guarded restore: %w", err)
			}
			if collision != nil {
				return 0, &NameCollisionError{Collision: collision}
			}
		}
	}

	dbValue := restoreValue
	if sliceFields[field] {
		dbValue = MarshalStringSlice(splitTags(restoreValue))
	}
	// The condition is REPEATED in the statement itself: the write applies only
	// while the column still holds the exact raw value read above. At the
	// current pool cap (one connection) nothing can interleave inside this
	// transaction, but the guarded form keeps the invariant a property of the
	// statement rather than of the pool setting.
	result, err := tx.ExecContext(ctx,
		`UPDATE artists SET `+col+` = ?, updated_at = ? WHERE id = ? AND `+col+` = ?`,
		dbValue, time.Now().UTC().Format(time.RFC3339), id, storedRaw)
	if err != nil {
		return 0, fmt.Errorf("guarded restore: writing %s: %w", field, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("guarded restore: reading rows affected: %w", err)
	}
	if affected == 0 {
		return LockedFieldRestoreValueDiverged, nil
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("guarded restore: committing: %w", err)
	}

	s.markDirtyBestEffort(ctx, id)

	if s.history != nil {
		if err := s.history.Record(ctx, id, field, stored, restoreValue, sourceFromContext(ctx)); err != nil {
			slog.Warn("history: failed to record guarded restore",
				"artist_id", id, "field", field, "error", err)
		}
	}

	return LockedFieldRestoreApplied, nil
}
