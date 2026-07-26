package artist

// name_collision_race_test.go -- guards for the ATOMIC half of the #2730
// rename check (#2807).
//
// The pre-write guard (FindNameCollision, exercised by name_collision_test.go)
// and the write were separate operations. Nothing stopped two concurrent
// renames toward the SAME target name from both passing the check and both
// writing -- producing exactly the duplicate the guard exists to prevent.
//
// A test that renames one artist proves nothing about that. The load-bearing
// test here drives both renames at once and asserts the INVARIANT on the rows
// afterwards: at most one artist may hold any given identity key. It reads the
// rows back out of the database; the returned values are checked too, but the
// database is what the operator ends up living with.

import (
	"context"
	"sync"
	"testing"
)

// countArtistsWithKey reports how many rows in the artists table normalize to
// key, and returns their IDs for failure messages.
//
// It recomputes the key in Go rather than asking SQL, because the key is not a
// stored column and cannot be expressed as a SQL predicate -- that is the same
// reason the production check scans in Go, and the reason a UNIQUE INDEX is
// not available to enforce this.
func countArtistsWithKey(t *testing.T, svc *Service, key string) (int, []string) {
	t.Helper()
	db, err := svc.artistDB()
	if err != nil {
		t.Fatalf("artistDB: %v", err)
	}
	rows, err := db.QueryContext(context.Background(), `SELECT id, name FROM artists`)
	if err != nil {
		t.Fatalf("querying artists: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scanning artist: %v", err)
		}
		if NormalizeIdentityKey(name) == key {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating artists: %v", err)
	}
	return len(ids), ids
}

func nameByID(t *testing.T, svc *Service, id string) string {
	t.Helper()
	a, err := svc.artists.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", id, err)
	}
	return a.Name
}

// TestUpdateNameGuarded_ConcurrentRenamesToSameTarget is the #2807 regression
// guard: two renames toward one target name, issued at the same moment, must
// leave exactly ONE artist holding that identity.
//
// WHAT THIS TEST ACHIEVES, stated honestly: it does not force a specific
// instruction interleaving -- there is no hook in the production path to
// suspend one goroutine between the check and the write, and adding one would
// mean shipping test scaffolding inside the guard. What it does is release
// both renames from a common barrier and assert the INVARIANT on the resulting
// rows. Repeating that over many independent databases is what gives it teeth:
// the unsynchronized version of this code loses the invariant readily under
// -race (verified by reverting the fix; see the package comment on
// UpdateNameGuarded), because the two operations release the pooled connection
// between the check and the write and each rename's check runs against a
// snapshot the other is about to invalidate.
//
// The assertion is deliberately on rows read back from the database, not on
// returned values alone: a version that reported a collision and wrote anyway
// would satisfy a return-value-only check.
func TestUpdateNameGuarded_ConcurrentRenamesToSameTarget(t *testing.T) {
	t.Parallel()

	const (
		firstName  = "Southgate Winds"
		secondName = "Northfield Chorale"
		target     = "Harrowdene Ensemble"
	)
	targetKey := NormalizeIdentityKey(target)

	// Independent databases per round. One round can only ever observe one
	// interleaving; the race is a scheduling accident, so the sample size is
	// what converts "might catch it" into "reliably catches it".
	const rounds = 40

	for round := range rounds {
		svc, firstID, secondID := seedCollisionPair(t, firstName, secondName)

		// Preconditions. Without these the whole test could pass vacuously:
		// if the two artists did not exist under DIFFERENT names, or if the
		// target key were already taken, "exactly one holder afterwards"
		// would be true no matter what the code did.
		if got := nameByID(t, svc, firstID); got != firstName {
			t.Fatalf("round %d precondition: first artist name = %q, want %q", round, got, firstName)
		}
		if got := nameByID(t, svc, secondID); got != secondName {
			t.Fatalf("round %d precondition: second artist name = %q, want %q", round, got, secondName)
		}
		if NormalizeIdentityKey(firstName) == NormalizeIdentityKey(secondName) {
			t.Fatalf("round %d precondition: the two seed names share an identity key, "+
				"so the rename would not create a collision", round)
		}
		if n, _ := countArtistsWithKey(t, svc, targetKey); n != 0 {
			t.Fatalf("round %d precondition: %d artists already hold the target key; "+
				"want 0 so the collision is caused by the renames", round, n)
		}

		type outcome struct {
			collision *NameCollision
			wrote     bool
			err       error
		}
		outcomes := make([]outcome, 2)
		ids := []string{firstID, secondID}

		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		done.Add(2)

		for i := range 2 {
			go func() {
				defer done.Done()
				start.Wait() // release both as close to simultaneously as the runtime allows
				c, wrote, err := svc.UpdateNameGuarded(context.Background(), ids[i], target)
				outcomes[i] = outcome{collision: c, wrote: wrote, err: err}
			}()
		}
		start.Done()
		done.Wait()

		for i, o := range outcomes {
			if o.err != nil {
				t.Fatalf("round %d: rename %d returned an error: %v", round, i, o.err)
			}
		}

		// THE INVARIANT, read back from the database.
		n, holders := countArtistsWithKey(t, svc, targetKey)
		if n != 1 {
			t.Fatalf("round %d: %d artists hold the target identity key (ids %v), want exactly 1; "+
				"outcomes: [0]=(collision=%v wrote=%t) [1]=(collision=%v wrote=%t). "+
				"Two holders means both renames passed the check and both wrote -- the #2807 race.",
				round, n, holders,
				outcomes[0].collision != nil, outcomes[0].wrote,
				outcomes[1].collision != nil, outcomes[1].wrote)
		}

		// The returned values must agree with the rows: exactly one write and
		// exactly one refusal. A run that wrote once but reported two
		// successes would leave the caller returning 200 for a rename that
		// did not happen.
		wrote := 0
		refused := 0
		for _, o := range outcomes {
			if o.wrote {
				wrote++
			}
			if o.collision != nil {
				refused++
			}
		}
		if wrote != 1 || refused != 1 {
			t.Fatalf("round %d: %d writes and %d refusals reported, want exactly 1 of each",
				round, wrote, refused)
		}

		// The refused artist must still hold its ORIGINAL name. A refusal that
		// left a partial write behind would be worse than the race.
		for i, o := range outcomes {
			if o.collision == nil {
				continue
			}
			want := []string{firstName, secondName}[i]
			if got := nameByID(t, svc, ids[i]); got != want {
				t.Fatalf("round %d: refused artist %d name = %q, want it unchanged at %q",
					round, i, got, want)
			}
		}
	}
}

// TestUpdateNameGuarded_CleanRenameWrites covers the ordinary path: no other
// artist holds the target identity, so the rename lands.
func TestUpdateNameGuarded_CleanRenameWrites(t *testing.T) {
	t.Parallel()
	svc, localID, _ := seedCollisionPair(t, "Southgate Winds", "Northfield Chorale")

	if got := nameByID(t, svc, localID); got != "Southgate Winds" {
		t.Fatalf("precondition: name = %q", got)
	}

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), localID, "Harrowdene Ensemble")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision != nil {
		t.Fatalf("collision = %+v, want nil for an unused name", collision)
	}
	if !wrote {
		t.Error("wrote = false, want true for a genuine rename")
	}
	if got := nameByID(t, svc, localID); got != "Harrowdene Ensemble" {
		t.Errorf("name = %q, want %q", got, "Harrowdene Ensemble")
	}
}

// TestUpdateNameGuarded_CollisionRefusesAndLeavesRowUntouched is the
// single-threaded refusal case. The row assertion is the load-bearing half:
// the transaction writes the name BEFORE it checks (that is how it acquires
// the write lock), so a rollback that failed to fire would leave the duplicate
// committed while the caller was told the rename was refused.
func TestUpdateNameGuarded_CollisionRefusesAndLeavesRowUntouched(t *testing.T) {
	t.Parallel()
	svc, localID, platformOnlyID := seedCollisionPair(t, "Southgate Winds", "Northfield Chorale")

	before := nameByID(t, svc, platformOnlyID)
	if before != "Northfield Chorale" {
		t.Fatalf("precondition: name = %q", before)
	}

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), platformOnlyID, "Southgate Winds")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision == nil {
		t.Fatal("collision = nil, want the existing artist reported")
	}
	if collision.ArtistID != localID {
		t.Errorf("collision.ArtistID = %q, want %q", collision.ArtistID, localID)
	}
	if wrote {
		t.Error("wrote = true, want false: a refused rename must write nothing")
	}
	if got := nameByID(t, svc, platformOnlyID); got != before {
		t.Errorf("name = %q, want it rolled back to %q", got, before)
	}
	// And the other artist is untouched too.
	if got := nameByID(t, svc, localID); got != "Southgate Winds" {
		t.Errorf("partner name = %q, want it untouched", got)
	}
}

// TestUpdateNameGuarded_NormalizedVariantIsRefused pins that the transactional
// check uses NormalizeIdentityKey, not raw string equality. A guard keyed on
// exact bytes would let this through and DetectDuplicates would then group the
// two rows it just allowed.
func TestUpdateNameGuarded_NormalizedVariantIsRefused(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Southgate Winds", "Northfield Chorale")

	// Same identity under the normalizer (leading article + case + spacing),
	// a different byte string.
	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), platformOnlyID, "  the  SOUTHGATE   winds ")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision == nil {
		t.Fatal("collision = nil: a normalized-equal name must be refused, " +
			"or the duplicates report will group what this allowed")
	}
	if wrote {
		t.Error("wrote = true, want false")
	}
	if got := nameByID(t, svc, platformOnlyID); got != "Northfield Chorale" {
		t.Errorf("name = %q, want it unchanged", got)
	}
}

// TestUpdateNameGuarded_CosmeticSelfRenameIsAllowed covers the exemption
// FindNameCollision also makes: a new name whose key equals the artist's OWN
// current key is not a second identity, so tidying it stays allowed.
func TestUpdateNameGuarded_CosmeticSelfRenameIsAllowed(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Southgate Winds", "The Northfield Chorale")

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), platformOnlyID, "Northfield Chorale")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision != nil {
		t.Fatalf("collision = %+v, want nil: dropping a leading article is the artist's own identity",
			collision)
	}
	if !wrote {
		t.Error("wrote = false, want true: the stored bytes did change")
	}
	if got := nameByID(t, svc, platformOnlyID); got != "Northfield Chorale" {
		t.Errorf("name = %q, want %q", got, "Northfield Chorale")
	}
}

// TestUpdateNameGuarded_NoopSkipsWrite mirrors UpdateField's no-op contract:
// an unchanged value writes nothing and reports no write.
func TestUpdateNameGuarded_NoopSkipsWrite(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Southgate Winds", "Northfield Chorale")

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), platformOnlyID, "Northfield Chorale")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision != nil {
		t.Errorf("collision = %+v, want nil", collision)
	}
	if wrote {
		t.Error("wrote = true, want false for an identical value")
	}
}

// TestUpdateNameGuarded_UnknownArtistIsAnError pins that a rename of a
// nonexistent artist fails rather than silently succeeding. The transaction
// loads the current name first precisely so this is caught.
func TestUpdateNameGuarded_UnknownArtistIsAnError(t *testing.T) {
	t.Parallel()
	svc, _, _ := seedCollisionPair(t, "Southgate Winds", "Northfield Chorale")

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), "no-such-artist", "Harrowdene Ensemble")
	if err == nil {
		t.Fatal("err = nil, want an error for an unknown artist")
	}
	if collision != nil || wrote {
		t.Errorf("collision = %+v, wrote = %t; want (nil, false) alongside the error", collision, wrote)
	}
}

// TestUpdateNameGuarded_UnrunnableCheckIsAnError covers the fail-closed
// contract at the service seam. "The check could not run" is not evidence the
// rename is safe, so both broken-handle shapes must error rather than fall
// through to an unguarded write -- which would be the pre-#2730 behavior
// wearing the new method's name.
func TestUpdateNameGuarded_UnrunnableCheckIsAnError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// decorate wraps the real repository in a fault-injecting one.
		decorate func(Repository) Repository
	}{
		{"missing DB accessor", func(r Repository) Repository { return &noDBAccessorRepo{Repository: r} }},
		{"nil DB handle", func(r Repository) Repository { return &nilDBRepo{Repository: r} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t)
			ctx := context.Background()

			// Seed through a real service so a genuine row exists. Without it
			// the test could pass for the wrong reason -- erroring on a
			// missing artist rather than on the unrunnable check.
			seedSvc := NewService(db)
			a := testArtist("Southgate Winds", "")
			if err := seedSvc.Create(ctx, a); err != nil {
				t.Fatalf("Create: %v", err)
			}

			artists, providers, members, aliases, images, platformIDs, completeness := NewDefaultRepos(db)
			svc := NewServiceWithRepos(tc.decorate(artists),
				providers, members, aliases, images, platformIDs, completeness)

			collision, wrote, err := svc.UpdateNameGuarded(ctx, a.ID, "Harrowdene Ensemble")
			if err == nil {
				t.Fatal("err = nil: a rename whose in-transaction check cannot run must be refused, " +
					"never allowed through")
			}
			if collision != nil || wrote {
				t.Errorf("collision = %+v, wrote = %t; want (nil, false) alongside the error", collision, wrote)
			}
			// And nothing was written.
			if got := nameByID(t, seedSvc, a.ID); got != "Southgate Winds" {
				t.Errorf("name = %q, want it untouched at %q", got, "Southgate Winds")
			}
		})
	}
}
