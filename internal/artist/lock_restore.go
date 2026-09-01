package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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
// On success a history row is recorded ATOMICALLY, inside the same
// transaction as the artist write (#3088) -- unlike UpdateField's best-effort,
// post-commit history write. A history-insert failure here fails the whole
// restore with the transaction rolled back, rather than leaving the artist
// row restored with no record it happened. See the comment above the insert
// for why this verb's contract differs from UpdateField's. The source is
// carried by ctx.
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
	defer tx.Rollback() //nolint:errcheck // Rollback returns sql.ErrTxDone after a successful Commit; on the error path the original error is what callers act on

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
	stored := StoredFieldValue(field, storedRaw)
	if !FieldValueStillDamaged(field, stored, damagedValue) {
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

	// THE HISTORY ROW COMMITS IN THE SAME TRANSACTION AS THE ARTIST WRITE
	// (#3088). Service.UpdateField and the other operator-driven verbs
	// deliberately record history AFTER commit, best-effort: an operator who
	// just watched the write happen can tolerate a lost history row as a
	// cosmetic loss, and re-running the read-modify-write inside one
	// transaction there would widen a lock scope no caller needs. This verb
	// has no such caller -- RestoreLockedFieldGuarded exists solely for the
	// unattended startup repair (internal/maintenance), which runs in a
	// goroutine nobody watches, so a history row lost between commit and a
	// separate insert is not cosmetic: it is the ONLY record the restore
	// happened, invisible in Recent Activity and the blast-radius pane. Since
	// this is the verb's only caller, moving the insert inside the
	// transaction costs nothing and closes the split outright, rather than
	// merely narrowing the shutdown-drain window -- see startLockDamageRepair
	// (cmd/stillwater/lock_damage_repair.go) for the drain that bounds what
	// this cannot: a boot that never reaches this point at all.
	// Recorded unconditionally: this verb's only caller is the unattended
	// repair, and an audit row for an atomic write must not depend on whether
	// an (unrelated, best-effort) HistoryService happens to be attached to
	// this Service value. recordHistoryTx never touches s.history -- it
	// inserts directly on tx -- so a guard on s.history here gated nothing
	// about what actually ran; it only produced a restore with no audit row
	// on a Service built without SetHistoryService (#3088 fix round, N3).
	if err := recordHistoryTx(ctx, tx, id, field, stored, restoreValue, sourceFromContext(ctx)); err != nil {
		return 0, fmt.Errorf("guarded restore: recording history: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("guarded restore: committing: %w", err)
	}

	s.markDirtyBestEffort(ctx, id)

	return LockedFieldRestoreApplied, nil
}

// recordHistoryTx inserts one metadata_changes row on tx, mirroring
// HistoryService.Record's validation and ID-assignment rules ("source
// required" check, HistoryIDFromContext precedence) without going through the
// repository's own *sql.DB handle -- HistoryRepository has no
// INSERT-on-a-transaction method, and adding one would widen an interface
// every other caller (which does not need transactional history) also has to
// implement. RestoreLockedFieldGuarded is the ONLY caller that must commit the
// history row atomically with its artist write (#3088), so the transactional
// insert lives here rather than on the interface.
//
// DELIBERATELY WITHOUT HistoryService.Record's no-op skip (#3088 fix round,
// F1). Record's skip -- "don't insert when oldValue and newValue are both
// non-empty and identical" -- is correct for Record's callers, which pass
// already-decided changes. It is WRONG at this call site, because the caller
// decides "did anything change" earlier, with normalizeFieldValue, against
// values that are NOT the same strings this function receives:
// RestoreLockedFieldGuarded compares stored (the joined form of the current
// column) against damagedValue for slice fields, but passes stored and
// restoreValue here -- restoreValue is never normalized. A slice-field pair
// that differs only in comma-spacing can pass the caller's normalize-equality
// check (so the UPDATE fires and affected == 1), and independently have its
// raw restoreValue happen to string-equal stored -- at which point the old
// skip fired, the artist row committed, and NOTHING recorded that it
// happened: outcome reports Applied with zero history rows, and the pair
// stays a candidate forever (no revert row exists to exclude it from
// lockDamageQuery), so every future boot restores it again. That is the exact
// "restored field with no history row" state #3088 exists to eliminate.
//
// The fix is to drop the skip, not to normalize its inputs to match. Be
// precise about WHY, because the tempting argument is false: a genuine no-op
// CAN reach this insert. SQLite reports affected == 1 for a WHERE-matched row
// even when SET writes a byte-identical value, so affected == 1 means the
// caller's guarded compare-and-set HELD -- not that the column moved. For a
// slice field whose damage row differs from the stored value only in comma
// spacing, stored and restoreValue are byte-identical by the time they get
// here, and this function is called with oldValue == newValue. Measured, not
// assumed. The affected == 0 early return above filters DIVERGENCE, not
// no-ops.
//
// The row must be written anyway, and that is the actual justification. It is
// the newest row for the pair (rn = 1) and carries source="revert", and
// lockDamageQuery's predicate excludes both source='revert' and
// old_value == new_value -- so writing it is the ONE thing that retires the
// pair from the candidate set. Skip it and the pair stays a candidate
// forever: every boot re-selects it, "restores" it again, counts a Restored,
// and leaves no trace. A skip here would filter exactly the rows that must
// not be filtered.
//
// Do not backport this removal to HistoryService.Record: Record's callers
// (UpdateField and friends) pass values with no such decision already made,
// and rely on the skip to keep an accidental identical write out of the audit
// log.
func recordHistoryTx(ctx context.Context, tx *sql.Tx, artistID, field, oldValue, newValue, source string) error {
	if artistID == "" {
		return fmt.Errorf("artist_id is required")
	}
	if field == "" {
		return fmt.Errorf("field is required")
	}
	if source == "" {
		return fmt.Errorf("source is required")
	}
	if !validHistorySource(source) {
		return fmt.Errorf("invalid source: %s", source)
	}
	id := HistoryIDFromContext(ctx)
	if id == "" {
		id = uuid.New().String()
	}
	// producer is read off ctx the same way Record does (producerForField),
	// keyed on field like every other producer resolution in this PR. Nothing
	// in this PR (#3078 PR 1) puts a producer on the context ahead of this
	// call, so this writes ProducerUnrecorded ("") exactly like every other
	// path -- see history_producer.go's doc block. This is the ONLY change to
	// this function; the no-op-skip asymmetry documented above is untouched.
	producer := producerForField(ctx, field)
	const q = `
		INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, producer, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		id, artistID, field, oldValue, newValue, source, producer,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("inserting metadata change: %w", err)
	}
	return nil
}
