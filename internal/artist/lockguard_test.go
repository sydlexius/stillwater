package artist

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

// #3037: the per-field lock chokepoint on the persist path.
//
// READ THIS BEFORE EDITING. Every "the locked value survived" assertion below
// is PAIRED with a positive control on an otherwise identical UNLOCKED artist.
// An unpaired "unchanged" assertion passes vacuously when the harness never
// reached the write at all, and would report a guard that does not exist as
// working. If a control fails, fix the harness, not the assertion.

// seedLockGuardArtist creates an artist and, when fields is non-empty, pins
// them through the dedicated lock mutator -- not by putting the lock on the
// struct, which the guard would pin away, leaving every test here vacuous. It
// asserts the seed took, for the same reason.
func seedLockGuardArtist(t *testing.T, svc *Service, name, bio string, fields ...string) *Artist {
	t.Helper()
	ctx := context.Background()
	a := &Artist{Name: name, Biography: bio}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist %s: %v", name, err)
	}
	if len(fields) > 0 {
		if err := svc.SetLockedFields(ctx, a.ID, fields); err != nil {
			t.Fatalf("locking %v on %s: %v", fields, name, err)
		}
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seeded artist %s: %v", name, err)
	}
	if stored.Biography != bio {
		t.Fatalf("precondition: %s biography = %q, want %q; the seed did not persist so any later assertion is vacuous", name, stored.Biography, bio)
	}
	if len(stored.LockedFields) != len(fields) {
		t.Fatalf("precondition: %s locked_fields = %v, want %v; the lock did not persist", name, stored.LockedFields, fields)
	}
	return stored
}

// TestUpdate_RestoresLockedFieldAndWritesUnlockedOne is the headline property:
// a whole-row persist that would change a locked field has that field restored,
// while the unlocked fields in the SAME write land normally. Asserting the
// unlocked field in the same call is what proves the guard is per-FIELD.
//
// The CLEAR case is not padding. fixJunkBio blanks a.Biography before
// re-querying providers, so an EMPTY incoming value is the shape that actually
// lost data, and a guard comparing only non-empty values would let it through.
func TestUpdate_RestoresLockedFieldAndWritesUnlockedOne(t *testing.T) {
	const pinnedBio = "the operator wrote this by hand"
	for _, tc := range []struct {
		name     string
		incoming string
	}{
		{name: "replacement", incoming: "an automated writer's replacement"},
		{name: "clear", incoming: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc := NewService(newTestDB(t))

			locked := seedLockGuardArtist(t, svc, "Locked Bio", pinnedBio, "biography")
			locked.Biography = tc.incoming
			locked.Origin = "Somewhere, XX"
			if err := svc.Update(ctx, locked); err != nil {
				t.Fatalf("Update: %v", err)
			}

			stored, err := svc.GetByID(ctx, locked.ID)
			if err != nil {
				t.Fatalf("reloading: %v", err)
			}
			if stored.Biography != pinnedBio {
				t.Errorf("locked biography = %q, want %q; the chokepoint did not restore it", stored.Biography, pinnedBio)
			}
			// The unlocked field in the SAME write must land, or the guard is
			// refusing whole writes instead of protecting one field.
			if stored.Origin != "Somewhere, XX" {
				t.Errorf("unlocked origin = %q, want %q; the guard discarded an unlocked change", stored.Origin, "Somewhere, XX")
			}
			// The caller's own struct must carry the restored value too, so a
			// downstream NFO write or platform publish in the same request does
			// not ship the rejected value.
			if locked.Biography != pinnedBio {
				t.Errorf("in-memory biography = %q, want %q; a downstream publisher would ship the rejected value", locked.Biography, pinnedBio)
			}

			// POSITIVE CONTROL: the identical write on an unlocked artist lands.
			unlocked := seedLockGuardArtist(t, svc, "Unlocked Bio", pinnedBio)
			unlocked.Biography = tc.incoming
			if err := svc.Update(ctx, unlocked); err != nil {
				t.Fatalf("Update (control): %v", err)
			}
			control, err := svc.GetByID(ctx, unlocked.ID)
			if err != nil {
				t.Fatalf("reloading control: %v", err)
			}
			if control.Biography != tc.incoming {
				t.Fatalf("control biography = %q, want %q; the harness never reached the write, so the locked assertion above is vacuous", control.Biography, tc.incoming)
			}
		})
	}
}

// TestUpdate_LockSurvivesASecondWrite is the property a value-only guard does
// NOT have. The repository's UPDATE writes locked_fields in the same statement
// as every metadata column, so restoring the VALUE while that statement blanks
// the lock SET buys exactly one write of protection. It writes TWICE with a
// caller that zeroes the lock state, which is what an ordinary fixer-built
// struct looks like; a value-only guard passes write one and fails write two.
func TestUpdate_LockSurvivesASecondWrite(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	const pinnedBio = "still pinned after two writes"
	a := seedLockGuardArtist(t, svc, "Two Writes", pinnedBio, "biography")
	// Lock the ARTIST too, so the same loop covers the other three columns the
	// persist statement carries (locked, lock_source, locked_at). They erase
	// the same way and for the same reason.
	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}

	for i, attempt := range []string{"first clobber", "second clobber"} {
		// Zero the lock state exactly as a caller that never populated it
		// would. This is the erasing write.
		a.LockedFields = nil
		a.Locked = false
		a.LockSource = ""
		a.LockedAt = nil
		a.Biography = attempt
		if err := svc.Update(ctx, a); err != nil {
			t.Fatalf("Update %d: %v", i+1, err)
		}
		stored, err := svc.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("reloading after write %d: %v", i+1, err)
		}
		if stored.Biography != pinnedBio {
			t.Fatalf("biography = %q after write %d, want %q", stored.Biography, i+1, pinnedBio)
		}
		if len(stored.LockedFields) != 1 || stored.LockedFields[0] != "biography" {
			t.Fatalf("locked_fields = %v after write %d, want [biography]; the persist path erased the lock, so the next write is unguarded", stored.LockedFields, i+1)
		}
		if !stored.Locked || stored.LockSource != "user" || stored.LockedAt == nil {
			t.Fatalf("artist lock state after write %d: locked=%v source=%q locked_at=%v, want the stored user lock intact; Service.Lock/Unlock are the only paths that may change it",
				i+1, stored.Locked, stored.LockSource, stored.LockedAt)
		}
	}
}

// TestUpdate_RestoresLockedSliceField pins the slice path. Slice fields are
// compared and restored as slices, not through their comma-joined form: a genre
// whose own text contains a comma would be split in two on that round trip,
// making the guard a data-loss path for the field it protects.
func TestUpdate_RestoresLockedSliceField(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := &Artist{Name: "Slice Lock", Genres: []string{"Rock, Progressive", "Jazz"}, Styles: []string{"Seeded"}}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := svc.SetLockedFields(ctx, a.ID, []string{"genres"}); err != nil {
		t.Fatalf("locking genres: %v", err)
	}
	seeded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seed: %v", err)
	}
	if len(seeded.Genres) != 2 {
		t.Fatalf("precondition: genres = %v, want 2 entries (one containing a comma); the round-trip claim is untested otherwise", seeded.Genres)
	}

	seeded.Genres = []string{"Pop"}
	// The positive control rides in the SAME write: styles is an unlocked slice
	// field, so if it does not change, the write never reached the database and
	// the genres assertion below is vacuous.
	seeded.Styles = []string{"Replaced"}
	if err := svc.Update(ctx, seeded); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if len(stored.Genres) != 2 || stored.Genres[0] != "Rock, Progressive" || stored.Genres[1] != "Jazz" {
		t.Errorf("locked genres = %v, want [\"Rock, Progressive\" Jazz]", stored.Genres)
	}
	if len(stored.Styles) != 1 || stored.Styles[0] != "Replaced" {
		t.Fatalf("control styles = %v, want [Replaced]; the write never landed so the genres assertion is vacuous", stored.Styles)
	}
}

// TestLockGuardedFields_CoversEveryStructRepresentableLockToken guards the
// derivation. The set comes from lockableFieldNames, the same authority
// reportUnenforceableLocks uses, so a token meaningful there and absent here is
// a lock the operator was told they have and the chokepoint cannot enforce.
//
// The provider-ID coverage is asserted POSITIVELY, inverting what this test
// asserted before hydration existed: a widening that dropped the hydration would
// now have to remove these fields, and that fails here. "members" remains the
// one permitted absence, asserted AS an absence so a silent departure fails too.
func TestLockGuardedFields_CoversEveryStructRepresentableLockToken(t *testing.T) {
	guarded := make(map[string]bool, len(lockGuardedFields))
	for _, f := range lockGuardedFields {
		guarded[string(f)] = true
	}
	// Precondition: the provider-ID vocabulary is non-empty, or "every provider
	// ID is guarded" holds vacuously.
	if len(providerFieldMap) == 0 {
		t.Fatal("precondition: providerFieldMap is empty, so the coverage assertion below would be vacuous")
	}
	for field := range providerFieldMap {
		if !guarded[field] {
			t.Errorf("provider-ID field %q is not guarded; a lock on it protects nothing on the persist path", field)
		}
	}
	for name := range lockableFieldNames {
		if name == string(FieldMembers) {
			if guarded[name] {
				t.Errorf("%q is guarded, but no Artist field holds band members; restoring it cannot work", name)
			}
			continue
		}
		if !guarded[name] {
			t.Errorf("%q is a meaningful lock token but the chokepoint does not guard it; a lock on it protects nothing on the persist path", name)
		}
	}
}

// TestUpdateField_StillWritesALockedField is the OVER-CORRECTION guard, and it
// asserts the opposite of every test above on purpose. Locks gate AUTOMATED
// writes; the operator's own edit, and the Undo that recovers a value an
// automated write already changed, are unlocked BY DESIGN (see the
// trackableFields comment in service.go). A guard that silently no-opped a
// manual edit would be worse than the bug it fixes. This is also what keeps
// history revert and blast-radius restore working.
func TestUpdateField_StillWritesALockedField(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := seedLockGuardArtist(t, svc, "Operator Edit", "the pinned biography", "biography")
	changed, err := svc.UpdateField(ctx, a.ID, "biography", "the operator's new text")
	if err != nil {
		t.Fatalf("UpdateField on a locked field: %v", err)
	}
	if !changed {
		t.Fatal("UpdateField reported no write; a manual edit of a locked field must still be allowed")
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.Biography != "the operator's new text" {
		t.Errorf("biography = %q, want the operator's edit; the guard blocked a manual write", stored.Biography)
	}
	// The lock itself must survive the operator's edit -- editing a field is
	// not a statement about whether it should stay pinned.
	if len(stored.LockedFields) != 1 || stored.LockedFields[0] != "biography" {
		t.Errorf("locked_fields = %v after a manual edit, want [biography]", stored.LockedFields)
	}
}

// TestUpdate_RestoresEveryGuardedField is the completeness check with teeth.
// lockGuardedFields is DERIVED, so a field can enter it without anyone writing a
// restore arm -- setFieldOnArtist would then silently no-op, leaving the guard
// reporting protection it does not provide. This locks each guarded field in
// turn, writes a different value, and asserts the stored value came back. It is
// why setFieldOnArtist can stay a plain switch.
func TestUpdate_RestoresEveryGuardedField(t *testing.T) {
	// Distinct per-field seed and attacker values. Both must be non-empty and
	// different, or the restore assertion cannot tell "restored" from "never
	// written".
	const seeded, attacker = "pinned-value", "clobber-value"

	for _, f := range lockGuardedFields {
		field := string(f)
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			svc := NewService(newTestDB(t))

			a := &Artist{Name: "Guarded " + field}
			seedFieldForTest(a, field, seeded)
			if err := svc.Create(ctx, a); err != nil {
				t.Fatalf("creating: %v", err)
			}
			if err := svc.SetLockedFields(ctx, a.ID, []string{field}); err != nil {
				t.Fatalf("locking %s: %v", field, err)
			}
			stored, err := svc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading seed: %v", err)
			}
			if got := valueForTest(stored, field); got != seeded {
				t.Fatalf("precondition: seeded %s = %q, want %q; the seed did not persist, so the restore assertion would be vacuous", field, got, seeded)
			}

			seedFieldForTest(stored, field, attacker)
			if err := svc.Update(ctx, stored); err != nil {
				t.Fatalf("Update: %v", err)
			}
			after, err := svc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading: %v", err)
			}
			if got := valueForTest(after, field); got != seeded {
				t.Errorf("locked %s = %q, want %q restored; this field is in lockGuardedFields but its write is not actually guarded", field, got, seeded)
			}

			// POSITIVE CONTROL: the same write on an unlocked twin lands, so a
			// pass above cannot come from the write never happening.
			ctl := &Artist{Name: "Unguarded " + field}
			seedFieldForTest(ctl, field, seeded)
			if err := svc.Create(ctx, ctl); err != nil {
				t.Fatalf("creating control: %v", err)
			}
			seedFieldForTest(ctl, field, attacker)
			if err := svc.Update(ctx, ctl); err != nil {
				t.Fatalf("Update (control): %v", err)
			}
			control, err := svc.GetByID(ctx, ctl.ID)
			if err != nil {
				t.Fatalf("reloading control: %v", err)
			}
			if got := valueForTest(control, field); got != attacker {
				t.Fatalf("control %s = %q, want %q; the write never landed so the assertion above is vacuous", field, got, attacker)
			}
		})
	}
}

// seedFieldForTest writes value to a field, handling the slice fields as
// single-element slices. It deliberately calls the PRODUCTION setter, so a
// field the setter cannot write fails the test above rather than being quietly
// skipped by a test-only writer that happened to know about it.
func seedFieldForTest(a *Artist, field, value string) {
	if IsSliceField(field) {
		setFieldOnArtist(a, field, "", []string{value})
		return
	}
	setFieldOnArtist(a, field, value, nil)
}

// valueForTest reads a field back in the same representation seedFieldForTest
// wrote, so slice fields compare as their single element.
func valueForTest(a *Artist, field string) string {
	return FieldValueFromArtist(a, field)
}

// TestUpdate_UnreadableStoredRowDoesNotEraseLockState asserts the LOCK COLUMNS'
// post-write state, which no other test in this package does on the
// unreadable-row path.
//
// Not redundant with TestUpdate_RefusesWhenTheStoredRowCannotBeRead, which
// asserts on Biography only and detects "an unreadable row was not refused".
// Lock erasure is a downstream consequence of failing open, so killing that
// cause blocks this route -- but a test watching only the refusal would not
// notice a guard that reads the snapshot correctly and restores the WRONG
// columns. The damage is not self-healing: one unguarded write erases the lock
// set, and every later write is then unguarded too.
func TestUpdate_UnreadableStoredRowDoesNotEraseLockState(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	const pinnedBio = "the operator wrote this by hand"
	a := seedLockGuardArtist(t, svc, "Unreadable And Locked", pinnedBio, "biography")
	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}

	failing := &failingGetByIDRepo{Repository: svc.artists, failFor: a.ID}
	svc.artists = failing

	// The caller zeroes the lock state, which is what an ordinary fixer-built
	// struct looks like, and carries a clobbering value.
	a.LockedFields = nil
	a.Locked = false
	a.LockSource = ""
	a.LockedAt = nil
	a.Biography = "an automated writer's replacement"

	if err := svc.Update(ctx, a); err == nil {
		t.Fatal("Update succeeded while the stored row was unreadable; the write must be refused rather than performed with locks disabled")
	}
	// Precondition: the injected failure is what stopped it.
	if !failing.fired {
		t.Fatal("precondition: the injected read failure never fired, so this test proves nothing")
	}

	stored, err := svc.artists.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if len(stored.LockedFields) != 1 || stored.LockedFields[0] != "biography" {
		t.Errorf("locked_fields = %v, want [biography]; the unguarded write erased the lock, so every later write is unguarded too", stored.LockedFields)
	}
	if !stored.Locked || stored.LockSource != "user" {
		t.Errorf("artist lock: locked=%v source=%q, want the stored user lock intact", stored.Locked, stored.LockSource)
	}
	if stored.Biography != pinnedBio {
		t.Errorf("biography = %q, want %q; the refusal did not prevent the write", stored.Biography, pinnedBio)
	}
}

// TestUpdate_ArtistLevelLockSurvivesAnOrdinaryWrite covers the artist-level lock
// ALONE. TestUpdate_LockSurvivesASecondWrite locks a field as well, so a guard
// running pinLockState only when the stored lock SET is non-empty passes it --
// the field lock keeps that condition true and the artist-lock assertion rides
// along proving nothing. Here nothing but the artist is locked.
func TestUpdate_ArtistLevelLockSurvivesAnOrdinaryWrite(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := seedLockGuardArtist(t, svc, "Artist Lock Only", "a biography")
	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}
	seeded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seed: %v", err)
	}
	// Preconditions: the artist lock persisted and NO field is locked, or the
	// distinction this test exists to draw does not hold in the fixture.
	if !seeded.Locked || seeded.LockSource != "user" {
		t.Fatalf("precondition: locked=%v source=%q, want a persisted user lock", seeded.Locked, seeded.LockSource)
	}
	if len(seeded.LockedFields) != 0 {
		t.Fatalf("precondition: locked_fields = %v, want empty; a field lock here would make the mutant's condition true and this test vacuous", seeded.LockedFields)
	}

	seeded.Locked = false
	seeded.LockSource = ""
	seeded.LockedAt = nil
	seeded.Biography = "an ordinary unrelated write"
	if err := svc.Update(ctx, seeded); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if !stored.Locked || stored.LockSource != "user" || stored.LockedAt == nil {
		t.Errorf("artist lock after one ordinary write: locked=%v source=%q locked_at=%v, want the stored user lock intact; Service.Lock/Unlock are the only paths that may change it",
			stored.Locked, stored.LockSource, stored.LockedAt)
	}
	// Positive control: the unlocked field in that same write did land, so the
	// assertion above cannot pass because the write never happened.
	if stored.Biography != "an ordinary unrelated write" {
		t.Fatalf("biography = %q, want the write to have landed; the assertion above is vacuous otherwise", stored.Biography)
	}
}

// TestUpdate_IncomingStructCannotCreateALock is pinLockState's other direction.
// The persist path is not where lock intent is expressed, so a struct that ADDS
// locks is overridden exactly like one that removes them. Without this, any
// automated writer could pin a field on the operator's behalf -- locks would
// appear that no operator set, and the lock UI would be reporting state the
// operator never chose.
func TestUpdate_IncomingStructCannotCreateALock(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := seedLockGuardArtist(t, svc, "No Locks", "a biography")
	// Precondition: genuinely unlocked to start, or "no lock was created" is
	// indistinguishable from "a lock was already there".
	if len(a.LockedFields) != 0 || a.Locked {
		t.Fatalf("precondition: locked_fields=%v locked=%v, want an unlocked artist", a.LockedFields, a.Locked)
	}

	a.LockedFields = []string{"biography"}
	a.Locked = true
	a.LockSource = "user"
	a.Biography = "a new biography"
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if len(stored.LockedFields) != 0 {
		t.Errorf("locked_fields = %v, want empty; the persist path created a lock the operator never set", stored.LockedFields)
	}
	if stored.Locked {
		t.Errorf("locked = true, want false; the persist path locked the artist on its own")
	}
	// Positive control: the write itself landed, so the assertions above are not
	// passing because nothing happened.
	if stored.Biography != "a new biography" {
		t.Fatalf("biography = %q, want the write to have landed; the assertions above are vacuous otherwise", stored.Biography)
	}
}

// TestEnforceFieldLocks_ReturnsRestoredNamesInStableOrder gives the documented
// return contract teeth. No production caller reads the value yet, so without
// this the ordering claim is untested and the accumulation looks like dead code
// a later reader would be right to delete -- taking the follow-up unit's
// lock-refusal reporting with it.
//
// Order matters because the consumer puts these names in an operator-facing log
// line; map iteration order would make that line differ run to run for one
// artist, reading as churn rather than one repeated event.
func TestEnforceFieldLocks_ReturnsRestoredNamesInStableOrder(t *testing.T) {
	ctx := context.Background()

	// Locked in an order deliberately unlike the sorted result, so a function
	// returning "whatever order the lock set iterated" cannot pass by accident.
	locked := []string{"origin", "biography", "genres", "type"}
	want := []string{"biography", "genres", "origin", "type"} // lockGuardedFields is sorted

	// Run repeatedly: a single pass can match a randomized order by luck, and
	// map iteration is randomized per run, so one pass proves little.
	for i := range 8 {
		svc := NewService(newTestDB(t))
		a := &Artist{
			Name:      "Stable Order",
			Biography: "stored bio",
			Origin:    "stored origin",
			Type:      "Person",
			Genres:    []string{"stored genre"},
		}
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("creating: %v", err)
		}
		if err := svc.SetLockedFields(ctx, a.ID, locked); err != nil {
			t.Fatalf("locking: %v", err)
		}
		stored, err := svc.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("reloading: %v", err)
		}
		if len(stored.LockedFields) != len(locked) {
			t.Fatalf("precondition: locked_fields = %v, want %d entries", stored.LockedFields, len(locked))
		}

		// Change every locked field, so every one of them must be restored and
		// therefore appear in the returned slice.
		incoming := *stored
		incoming.Biography = "clobbered bio"
		incoming.Origin = "clobbered origin"
		incoming.Type = "Group"
		incoming.Genres = []string{"clobbered genre"}

		got := enforceFieldLocks(ctx, stored, &incoming)
		if len(got) != len(want) {
			t.Fatalf("pass %d: restored = %v, want %v; a field that was restored is missing from the report", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("pass %d: restored = %v, want %v; the order is not stable, so the consumer's log line churns", i, got, want)
			}
		}
	}
}

// attrRecorder captures every attribute of every record, so a test can assert
// what a log line does NOT contain as well as what it does.
type attrRecorder struct{ records []map[string]string }

func (h *attrRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *attrRecorder) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{"__msg": r.Message, "__level": r.Level.String()}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.records = append(h.records, attrs)
	return nil
}
func (h *attrRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *attrRecorder) WithGroup(string) slog.Handler      { return h }

// TestEnforceFieldLocks_NeverLogsTheRejectedValue is a PRIVACY assertion, and it
// exists because the redactor cannot cover this line. internal/logging's
// RedactingReplaceAttr matches an ALLOWLIST of credential-ish key names
// (api_key, password, token, ...), so an artist-metadata value handed to a log
// attribute is written verbatim -- a whole biography, repeatedly, on the
// automated path, for every artist in a library-wide rule pass.
//
// The test asserts on EVERY attribute rather than a named one, so re-adding the
// payload under any key fails here. It also asserts the length IS reported,
// because that is what distinguishes a rule clearing a pinned field from one
// overwriting it, and the two need different operator responses.
func TestEnforceFieldLocks_NeverLogsTheRejectedValue(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	const pinnedBio = "the operator's own carefully written biography"
	const rejectedBio = "SECRET-PAYLOAD an automated writer tried to store here"
	a := seedLockGuardArtist(t, svc, "Privacy", pinnedBio, "biography")

	rec := &attrRecorder{}
	restore := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(restore) })

	a.Biography = rejectedBio
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Precondition: the guard actually fired, or there is no line to inspect and
	// every assertion below passes vacuously.
	var line map[string]string
	for _, r := range rec.records {
		if strings.Contains(r["__msg"], "refused an automated write") {
			line = r
			break
		}
	}
	if line == nil {
		t.Fatalf("no restoration was logged; captured %d records, so this test asserts nothing", len(rec.records))
	}

	// THE PRIVACY ASSERTION: the rejected text appears under no key at all.
	for k, v := range line {
		if strings.Contains(v, rejectedBio) || strings.Contains(v, "SECRET-PAYLOAD") {
			t.Errorf("log attr %q = %q contains the rejected value; artist metadata must never reach the log, and the redactor's allowlist does not cover it", k, v)
		}
		// The stored value is equally private -- it is the operator's own text.
		if strings.Contains(v, pinnedBio) {
			t.Errorf("log attr %q = %q contains the stored value; the pinned text is user data too", k, v)
		}
	}

	// The diagnostics an operator does need.
	if line["artist_id"] != a.ID {
		t.Errorf("artist_id = %q, want %q; the line must identify which artist", line["artist_id"], a.ID)
	}
	if line["field"] != "biography" {
		t.Errorf("field = %q, want %q", line["field"], "biography")
	}
	if got, want := line["rejected_len"], strconv.Itoa(len(rejectedBio)); got != want {
		t.Errorf("rejected_len = %q, want %q; the length distinguishes a CLEAR of a pinned field from an overwrite", got, want)
	}
	if line["__level"] != slog.LevelError.String() {
		t.Errorf("level = %q, want ERROR; a pinned field colliding with an auto-mode rule recurs every pass until an operator acts", line["__level"])
	}
}

// TestRestoreLockedField_ReportsLengthNotValue pins the boundary at the source:
// the rejected text never leaves restoreLockedField, so no future caller can log
// it by accident. The zero case is asserted because a rule CLEARING a pinned
// field is the destructive shape (fixJunkBio blanks a biography), and a length
// of zero is how an operator tells it apart from an overwrite.
func TestRestoreLockedField_ReportsLengthNotValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		field      string
		storedVal  string
		incomingV  string
		wantLen    int
		wantChange bool
	}{
		{"overwrite", "biography", "stored", "a longer replacement", len("a longer replacement"), true},
		{"clear", "biography", "stored", "", 0, true},
		{"unchanged", "biography", "same", "same", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := &Artist{ID: "x"}
			incoming := &Artist{ID: "x"}
			setFieldOnArtist(stored, tc.field, tc.storedVal, nil)
			setFieldOnArtist(incoming, tc.field, tc.incomingV, nil)

			gotLen, changed := restoreLockedField(stored, incoming, tc.field)
			if changed != tc.wantChange {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChange)
			}
			if gotLen != tc.wantLen {
				t.Errorf("rejectedLen = %d, want %d", gotLen, tc.wantLen)
			}
			if tc.wantChange && FieldValueFromArtist(incoming, tc.field) != tc.storedVal {
				t.Errorf("incoming %s = %q, want the stored value restored", tc.field, FieldValueFromArtist(incoming, tc.field))
			}
		})
	}
}

// TestUpdate_RestoresALockedProviderIDAndItsCompanions covers the widening.
// Each assertion is a distinct defect an ID-string-only restore would leave: an
// un-hydrated compare reads the stored ID as empty and "restores" "" over it,
// and the timestamp and provenance travel with the ID because
// extractProviderIDs persists all three from the same struct.
func TestUpdate_RestoresALockedProviderIDAndItsCompanions(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	fetched := time.Date(2024, time.March, 4, 5, 6, 7, 0, time.UTC)
	a := &Artist{
		Name:               "Pinned Identity",
		MusicBrainzID:      "stored-mbid",
		DiscogsID:          "stored-discogs",
		DiscogsIDFetchedAt: &fetched,
		MetadataSources:    map[string]string{SourceKeyMusicBrainzID: SourceOperatorConfirmed},
	}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := svc.SetLockedFields(ctx, a.ID, []string{"musicbrainz_id", "discogs_id"}); err != nil {
		t.Fatalf("locking: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seed: %v", err)
	}
	// Preconditions: the side-table seed persisted, or "restored" is
	// indistinguishable from "never written".
	if stored.MusicBrainzID != "stored-mbid" || stored.DiscogsID != "stored-discogs" {
		t.Fatalf("precondition: mbid=%q discogs=%q, want the seeded IDs", stored.MusicBrainzID, stored.DiscogsID)
	}
	if stored.DiscogsIDFetchedAt == nil || stored.MetadataSources[SourceKeyMusicBrainzID] != SourceOperatorConfirmed {
		t.Fatalf("precondition: fetched_at=%v provenance=%q, want both seeded",
			stored.DiscogsIDFetchedAt, stored.MetadataSources[SourceKeyMusicBrainzID])
	}

	clobbered := time.Date(2025, time.December, 25, 0, 0, 0, 0, time.UTC)
	stored.MusicBrainzID = "rule-picked-mbid"
	stored.DiscogsID = ""
	stored.DiscogsIDFetchedAt = &clobbered
	stored.MetadataSources[SourceKeyMusicBrainzID] = SourceMachinePicked
	stored.Origin = "Somewhere, XX" // unlocked, rides along as the control
	if err := svc.Update(ctx, stored); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.MusicBrainzID != "stored-mbid" {
		t.Errorf("musicbrainz_id = %q, want %q restored", after.MusicBrainzID, "stored-mbid")
	}
	if after.DiscogsID != "stored-discogs" {
		t.Errorf("discogs_id = %q, want %q; an un-hydrated compare would have written the empty value", after.DiscogsID, "stored-discogs")
	}
	if after.DiscogsIDFetchedAt == nil || !after.DiscogsIDFetchedAt.Equal(fetched) {
		t.Errorf("discogs fetched_at = %v, want %v; a restored ID with the rejected write's timestamp is false provenance", after.DiscogsIDFetchedAt, fetched)
	}
	if got := after.MetadataSources[SourceKeyMusicBrainzID]; got != SourceOperatorConfirmed {
		t.Errorf("mbid provenance = %q, want %q; the restore relabelled a confirmed identity as a guess", got, SourceOperatorConfirmed)
	}
	if after.Origin != "Somewhere, XX" {
		t.Fatalf("control origin = %q, want the unlocked change to land; the assertions above are vacuous otherwise", after.Origin)
	}
}

// TestUpdate_UnlockedProviderIDStillUpdates is the OVER-CORRECTION control.
// The provider_id_missing fixer backfills empty IDs on unlocked artists; a guard
// that froze those would break the ordinary path while claiming to protect the
// pinned one.
func TestUpdate_UnlockedProviderIDStillUpdates(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := &Artist{Name: "Unpinned Identity", DiscogsID: "old-discogs"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if len(stored.LockedFields) != 0 {
		t.Fatalf("precondition: locked_fields = %v, want none", stored.LockedFields)
	}

	stored.DiscogsID = "provider-supplied-discogs"
	if err := svc.Update(ctx, stored); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.DiscogsID != "provider-supplied-discogs" {
		t.Errorf("discogs_id = %q, want the update to land; guarding pinned IDs must not freeze unpinned ones", after.DiscogsID)
	}
}

// TestEnforceLocks_RefusesWhenALockedProviderIDCannotBeRead is the fail-loud
// branch: with no stored ID to compare, treating that as "nothing changed" lets
// the write through -- the data-loss path the hydration closes.
func TestEnforceLocks_RefusesWhenALockedProviderIDCannotBeRead(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := &Artist{Name: "Unreadable Provider ID", DiscogsID: "pinned"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := svc.SetLockedFields(ctx, a.ID, []string{"discogs_id"}); err != nil {
		t.Fatalf("locking discogs_id: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.DiscogsID != "pinned" {
		t.Fatalf("precondition: discogs_id = %q, want the seeded value", stored.DiscogsID)
	}

	// Strip the capability the hydration needs. NewService always wires it.
	svc.providers = nil

	stored.Biography = "some other change"
	if err := svc.Update(ctx, stored); err == nil {
		t.Fatal("Update succeeded with an unverifiable provider-ID lock; the write must be refused rather than performed unguarded")
	} else if !strings.Contains(err.Error(), "lock") {
		t.Errorf("refusal = %v, want it to name the lock it could not verify", err)
	}

	after, getErr := svc.artists.GetByID(ctx, a.ID)
	if getErr != nil {
		t.Fatalf("reloading: %v", getErr)
	}
	if after.Biography != "" {
		t.Errorf("biography = %q, want it unwritten; the refusal did not prevent the write", after.Biography)
	}
}

// failingProviderRepo makes hydration fail. Embedding the interface means a
// method added later cannot silently turn this into a partial fake.
type failingProviderRepo struct {
	ProviderIDRepository
	err error
}

func (r *failingProviderRepo) GetForArtist(context.Context, string) ([]ProviderID, error) {
	return nil, r.err
}

// TestEnforceLocks_RefusesWhenHydrationErrors covers the hydration-FAILURE
// branch, as distinct from the nil-repository branch. Both refuse, but only one
// was tested: swallowing the error and returning nil would compare a locked
// provider ID against an un-hydrated empty value, conclude nothing changed, and
// let the write through -- the data-loss path the hydration exists to close.
func TestEnforceLocks_RefusesWhenHydrationErrors(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := &Artist{Name: "Hydration Fails", DiscogsID: "pinned"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := svc.SetLockedFields(ctx, a.ID, []string{"discogs_id"}); err != nil {
		t.Fatalf("locking: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.DiscogsID != "pinned" {
		t.Fatalf("precondition: discogs_id = %q, want the seeded value", stored.DiscogsID)
	}

	sentinel := errors.New("injected hydration failure")
	svc.providers = &failingProviderRepo{ProviderIDRepository: svc.providers, err: sentinel}

	stored.Biography = "some other change"
	updErr := svc.Update(ctx, stored)
	if updErr == nil {
		t.Fatal("Update succeeded when hydration failed; an unverifiable lock must not be treated as an absent one")
	}
	// The cause chain must survive, or a caller cannot classify the failure.
	if !errors.Is(updErr, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false for %v; the refusal dropped the cause", updErr)
	}
	after, getErr := svc.artists.GetByID(ctx, a.ID)
	if getErr != nil {
		t.Fatalf("reloading: %v", getErr)
	}
	if after.Biography != "" {
		t.Errorf("biography = %q, want it unwritten; the refusal did not prevent the write", after.Biography)
	}
}

// TestUpdate_PinnedProviderIDCompanionsSurviveAnUnchangedID closes the gap
// where companions were only restored when the ID STRING changed. A write
// carrying the same ID with a tampered timestamp or provenance is still an
// automated write to a locked field.
//
// Table-driven across the three fields that HAVE a fetched-at column, so
// dropping any one arm fails here rather than only the discogs one.
func TestUpdate_PinnedProviderIDCompanionsSurviveAnUnchangedID(t *testing.T) {
	seeded := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	tampered := time.Date(2030, time.June, 6, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		field string
		get   func(*Artist) *time.Time
		set   func(*Artist, *time.Time)
	}{
		{"audiodb_id", func(a *Artist) *time.Time { return a.AudioDBIDFetchedAt }, func(a *Artist, t *time.Time) { a.AudioDBIDFetchedAt = t }},
		{"discogs_id", func(a *Artist) *time.Time { return a.DiscogsIDFetchedAt }, func(a *Artist, t *time.Time) { a.DiscogsIDFetchedAt = t }},
		{"wikidata_id", func(a *Artist) *time.Time { return a.WikidataIDFetchedAt }, func(a *Artist, t *time.Time) { a.WikidataIDFetchedAt = t }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			ctx := context.Background()
			svc := NewService(newTestDB(t))

			a := &Artist{Name: "Unchanged " + tc.field, MusicBrainzID: "mb-stored",
				MetadataSources: map[string]string{SourceKeyMusicBrainzID: SourceOperatorConfirmed}}
			setFieldOnArtist(a, tc.field, "id-stored", nil)
			tc.set(a, &seeded)
			if err := svc.Create(ctx, a); err != nil {
				t.Fatalf("creating: %v", err)
			}
			if err := svc.SetLockedFields(ctx, a.ID, []string{tc.field, "musicbrainz_id"}); err != nil {
				t.Fatalf("locking: %v", err)
			}
			stored, err := svc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading: %v", err)
			}
			if tc.get(stored) == nil || !tc.get(stored).Equal(seeded) {
				t.Fatalf("precondition: %s fetched_at = %v, want %v seeded", tc.field, tc.get(stored), seeded)
			}

			// The IDs are left UNCHANGED; only the companions are tampered with.
			tc.set(stored, &tampered)
			stored.MetadataSources[SourceKeyMusicBrainzID] = SourceMachinePicked
			if err := svc.Update(ctx, stored); err != nil {
				t.Fatalf("Update: %v", err)
			}
			after, err := svc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading: %v", err)
			}
			if got := tc.get(after); got == nil || !got.Equal(seeded) {
				t.Errorf("%s fetched_at = %v, want %v; a companion is part of the locked field's state, not a side effect of changing the ID", tc.field, got, seeded)
			}
			if got := after.MetadataSources[SourceKeyMusicBrainzID]; got != SourceOperatorConfirmed {
				t.Errorf("mbid provenance = %q, want %q; an unchanged ID does not license relabelling it", got, SourceOperatorConfirmed)
			}
		})
	}
}

// TestUpdateProviderField_OperatorGrantBeatsTheLock is the OVER-CORRECTION
// guard for the widening. A lock gates AUTOMATED writes; the operator keeps
// control of their own data, which is already true for the fourteen
// artists-row fields because their edit bypasses the chokepoint entirely.
//
// The paired negative is the point: the SAME method without a grant -- which is
// how the rule engine's backfill calls it -- must still be refused, or the grant
// would be a blanket bypass rather than an operator affordance.
func TestUpdateProviderField_OperatorGrantBeatsTheLock(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := &Artist{Name: "Operator Edit", DiscogsID: "dg-old"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := svc.SetLockedFields(ctx, a.ID, []string{"discogs_id"}); err != nil {
		t.Fatalf("locking: %v", err)
	}

	// WITHOUT a grant: an automated write is still reverted.
	if err := svc.UpdateProviderField(ctx, a.ID, "discogs_id", "rule-derived"); err != nil {
		t.Fatalf("UpdateProviderField (no grant): %v", err)
	}
	mid, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if mid.DiscogsID != "dg-old" {
		t.Fatalf("discogs_id = %q after an ungranted write, want %q; the lock must still stop the rule engine", mid.DiscogsID, "dg-old")
	}

	// WITH a grant for this field: the operator's own edit lands.
	granted := ContextWithLockOverride(ctx, "discogs_id")
	if err := svc.UpdateProviderField(granted, a.ID, "discogs_id", "dg-operator-typed"); err != nil {
		t.Fatalf("UpdateProviderField (granted): %v", err)
	}
	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.DiscogsID != "dg-operator-typed" {
		t.Errorf("discogs_id = %q, want the operator's value; refusing an operator's own edit is worse than the bug", after.DiscogsID)
	}
	if len(after.LockedFields) != 1 || after.LockedFields[0] != "discogs_id" {
		t.Errorf("locked_fields = %v, want [discogs_id]; the grant must not clear the lock itself", after.LockedFields)
	}
}

// TestLockOverride_IsScopedToTheNamedField proves the grant is a per-field
// affordance rather than a blanket bypass. Without this, `granted == name`
// could degrade to "any grant unlocks everything" and every test above would
// still pass -- the operator editing one pinned ID would silently license an
// automated write to a different pinned field in the same persist.
func TestLockOverride_IsScopedToTheNamedField(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	a := &Artist{Name: "Scoped Grant", DiscogsID: "dg-stored", SpotifyID: "sp-stored"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := svc.SetLockedFields(ctx, a.ID, []string{"discogs_id", "spotify_id"}); err != nil {
		t.Fatalf("locking: %v", err)
	}
	stored, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.DiscogsID != "dg-stored" || stored.SpotifyID != "sp-stored" {
		t.Fatalf("precondition: discogs=%q spotify=%q, want both seeded", stored.DiscogsID, stored.SpotifyID)
	}

	// One grant, for discogs_id only, on a write that changes BOTH pinned IDs.
	granted := ContextWithLockOverride(ctx, "discogs_id")
	stored.DiscogsID = "dg-operator-typed"
	stored.SpotifyID = "sp-sneaked-in"
	if err := svc.Update(granted, stored); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.DiscogsID != "dg-operator-typed" {
		t.Errorf("discogs_id = %q, want the granted field to land", after.DiscogsID)
	}
	if after.SpotifyID != "sp-stored" {
		t.Errorf("spotify_id = %q, want %q; a grant for one field must not unlock another", after.SpotifyID, "sp-stored")
	}
}
