package maintenance

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// historyRow is the part of a metadata_changes row this file asserts on.
type historyRow struct {
	ID, OldValue, NewValue, Source, Producer, CreatedAt string
}

// biographyRows returns a1's biography history rows recorded under source.
func (e *lockDamageEnv) biographyRows(source string) []historyRow {
	e.t.Helper()
	rows, err := e.db.Query(
		`SELECT id, old_value, new_value, source, producer, created_at FROM metadata_changes
		 WHERE artist_id = 'a1' AND field = 'biography' AND source = ? ORDER BY id`, source)
	if err != nil {
		e.t.Fatalf("querying %s rows: %v", source, err)
	}
	defer rows.Close()
	var out []historyRow
	for rows.Next() {
		var r historyRow
		if err := rows.Scan(&r.ID, &r.OldValue, &r.NewValue, &r.Source, &r.Producer, &r.CreatedAt); err != nil {
			e.t.Fatalf("scanning %s row: %v", source, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("iterating %s rows: %v", source, err)
	}
	return out
}

// TestRepairLockDamage_RestoreRecordsRestoreProducer pins the producer the
// locked-field damage repair records (#3078). The repair puts an older stored
// value back, so its row says "restore": it must not claim the operator wrote
// the text, and it must not inherit the producer of the damage it undoes.
func TestRepairLockDamage_RestoreRecordsRestoreProducer(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	// Give the damage row a producer of its own, so "the restore row did not
	// copy it" and "the damage row was left alone" are both observable.
	const damageSource = "rule:metadata_quality"
	if _, err := env.db.Exec(
		`UPDATE metadata_changes SET producer = ? WHERE id = 'a1-dmg-biography'`, damageSource); err != nil {
		t.Fatalf("stamping the damage row: %v", err)
	}

	// PRECONDITIONS: the lock is real, the damage row is stamped as seeded,
	// and no restore has been recorded yet.
	env.requireLockedBio()
	damage := env.biographyRows(damageSource)
	if len(damage) != 1 || damage[0].Producer != damageSource {
		t.Fatalf("fixture: damage rows = %+v, want one row with producer %q", damage, damageSource)
	}
	if n := len(env.biographyRows("revert")); n != 0 {
		t.Fatalf("fixture: %d revert rows before the repair, want 0", n)
	}

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if len(res.Restored) != 1 {
		t.Fatalf("restored %d pairs, want exactly 1; without a restore there is no row to check", len(res.Restored))
	}

	reverts := env.biographyRows("revert")
	if len(reverts) != 1 {
		t.Fatalf("revert rows = %d, want exactly 1", len(reverts))
	}
	if got := reverts[0]; got.Producer != artist.ProducerRestore || got.NewValue != "curated bio" {
		t.Errorf("restore row producer/new value = %q/%q, want restore and the value put back",
			got.Producer, got.NewValue)
	}

	// The repair adds a row; the damage row it acted on stays as recorded.
	if after := env.biographyRows(damageSource); len(after) != 1 || after[0] != damage[0] {
		t.Errorf("the damage row changed: got %+v, want %+v", after, damage)
	}
}
