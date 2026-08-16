package artist

import (
	"context"
	"testing"
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
// unlocked field in the same call is what proves the guard is per-FIELD rather
// than an all-or-nothing rejection (see enforceFieldLocks).
//
// The CLEAR case is not a variation for completeness. fixJunkBio blanks
// a.Biography before re-querying providers, so an EMPTY incoming value is the
// shape that actually lost data in production, and a guard comparing only
// non-empty values would let exactly it through.
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

// TestLockGuardedFields_CoversEveryArtistsRowLockToken guards the derivation.
// The set comes from lockableFieldNames, the same authority
// reportUnenforceableLocks uses, so a token meaningful there and absent here is
// a lock the operator was told they have and the chokepoint cannot enforce.
//
// TWO exclusions are asserted as exclusions rather than merely tolerated, so a
// field entering or leaving either category fails here instead of silently
// changing what is protected:
//
//   - "members": no Artist field holds it.
//   - the provider-ID fields: they are not on the artists row, so guarding them
//     without hydration would restore an empty ID over a real one. Asserting
//     they are ABSENT is what stops a well-meaning widening from reintroducing
//     that data-loss path without also bringing the hydration.
func TestLockGuardedFields_CoversEveryArtistsRowLockToken(t *testing.T) {
	guarded := make(map[string]bool, len(lockGuardedFields))
	for _, f := range lockGuardedFields {
		guarded[string(f)] = true
	}
	// Precondition: the provider-ID vocabulary is non-empty, or the exclusion
	// assertions below would hold vacuously against an empty set.
	if len(providerFieldMap) == 0 {
		t.Fatal("precondition: providerFieldMap is empty, so the exclusion assertions would be vacuous")
	}
	for name := range lockableFieldNames {
		if name == string(FieldMembers) {
			if guarded[name] {
				t.Errorf("%q is guarded, but no Artist field holds band members; restoring it cannot work", name)
			}
			continue
		}
		if _, isProviderID := providerFieldMap[name]; isProviderID {
			if guarded[name] {
				t.Errorf("%q is guarded, but it lives in artist_provider_ids and this unit does not hydrate it; the guard would restore an empty ID over a real one", name)
			}
			continue
		}
		if !guarded[name] {
			t.Errorf("%q is a meaningful lock token but the chokepoint does not guard it; a lock on it protects nothing on the persist path", name)
		}
	}
}

// TestUpdateField_StillWritesALockedField is the OVER-CORRECTION guard, and it
// is asserting the opposite of every test above on purpose. Locks gate
// AUTOMATED writes; the operator's own edit, and the Undo that recovers a value
// an automated write already changed, are unlocked BY DESIGN (see the
// trackableFields comment in service.go). A guard that silently no-opped a
// manual edit would be worse than the bug it fixes.
//
// This is also what keeps the history revert and the blast-radius restore
// working: both persist through UpdateField / ClearField, which this unit
// deliberately leaves unguarded.
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
// lockGuardedFields is DERIVED, so a field can enter it without anyone writing
// a restore arm for it -- and setFieldOnArtist would then silently no-op,
// leaving the guard reporting protection it does not provide for that field.
// This locks each guarded field in turn, writes a different value, and asserts
// the stored value came back.
//
// It is the reason setFieldOnArtist can stay a plain switch: the switch cannot
// fall out of step with the field list without failing here.
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

// TestUpdate_ArtistLevelLockSurvivesAnOrdinaryWrite covers the artist-level
// lock ALONE. TestUpdate_LockSurvivesASecondWrite locks a field as well, so a
// guard that ran pinLockState only when the stored lock SET is non-empty passes
// it -- the field lock keeps that condition true and the artist-lock assertion
// rides along proving nothing. Here nothing but the artist is locked, so the
// condition is false and the erasure is visible.
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
