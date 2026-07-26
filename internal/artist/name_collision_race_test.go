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
	"database/sql"
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
// the unguarded version of this code loses the invariant on the FIRST round
// (verified by reverting the fix to a separate FindNameCollision call followed
// by UpdateField).
//
// The reason it loses is specific. Two separate operations each take the
// pool's single connection, use it, and RELEASE it -- so the other rename's
// check slots into the gap between them and reads a row the first is about to
// change. Holding one transaction across both steps removes the gap, because
// the connection is never released mid-decision. See the doc comment on
// UpdateNameGuarded for why the statement ORDER inside that transaction is not
// what does the work.
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
// the transaction writes the name BEFORE it checks, so a rollback that failed
// to fire would leave the duplicate committed while the caller was told the
// rename was refused.
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

// --- guarded-rename error and secondary paths ------------------------------

// closedDBRepo hands back a CLOSED *sql.DB. Every attempt to begin a
// transaction on it fails, which is what makes it a faithful stand-in for the
// database becoming unreachable mid-session (file deleted, pool shut down
// during shutdown) rather than a contrived fault.
type closedDBRepo struct {
	Repository
	db *sql.DB
}

func (r *closedDBRepo) DB() *sql.DB { return r.db }

// serviceOnClosedDB builds a Service whose repository exposes a closed handle.
func serviceOnClosedDB(t *testing.T) *Service {
	t.Helper()
	db := newTestDB(t)
	artists, providers, members, aliases, images, platformIDs, completeness := NewDefaultRepos(db)

	closed := newTestDB(t)
	if err := closed.Close(); err != nil {
		t.Fatalf("closing probe db: %v", err)
	}
	// Precondition: the handle really is unusable, so a failure below is the
	// closed database and not some other fault.
	if err := closed.Ping(); err == nil {
		t.Fatal("precondition: the probe database must be closed")
	}

	return NewServiceWithRepos(&closedDBRepo{Repository: artists, db: closed},
		providers, members, aliases, images, platformIDs, completeness)
}

// TestUpdateNameGuarded_UnusableDatabaseIsAnError covers the fail-closed
// contract when the database itself is unreachable: the transaction cannot
// even begin, so the rename must be refused rather than attempted.
//
// This is the same contract as the missing-accessor case, but one layer down
// -- there the repository could not produce a handle, here the handle exists
// and is dead. Both must refuse; neither may fall through to a write.
func TestUpdateNameGuarded_UnusableDatabaseIsAnError(t *testing.T) {
	t.Parallel()
	svc := serviceOnClosedDB(t)

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), "any-id", "Harrowdene Ensemble")
	if err == nil {
		t.Fatal("err = nil: a rename must be refused when the database is unusable")
	}
	if collision != nil || wrote {
		t.Errorf("collision = %+v, wrote = %t; want (nil, false) alongside the error", collision, wrote)
	}
}

// TestUpdateNameGuarded_EmptyIdentityKeySkipsCollisionCheck covers the
// empty-key branch: a name made only of invisible characters normalizes to
// "", and an empty key is deliberately NOT treated as a collision.
//
// The fixture is a zero-width space, which is the realistic form of this --
// invisible characters ride along on names pasted from web pages, and
// NormalizeIdentityKey strips Unicode Cf characters by design. Punctuation
// does NOT produce an empty key (it survives the fold), so a punctuation-only
// name would have exercised the ordinary path instead; the precondition below
// is what pins that distinction.
//
// Two artists whose names both normalize away would otherwise be reported as
// colliding with each other, a confusing refusal for a value that field
// validation rejects on its own terms. Guarding the value itself is
// validation's job, not this function's.
func TestUpdateNameGuarded_EmptyIdentityKeySkipsCollisionCheck(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Southgate Winds", "Northfield Chorale")

	const invisible = "\u200b" // ZERO WIDTH SPACE
	// Precondition: this really does normalize away, or the test is covering
	// the ordinary path under a misleading name.
	if got := NormalizeIdentityKey(invisible); got != "" {
		t.Fatalf("precondition: NormalizeIdentityKey(%q) = %q, want an empty key", invisible, got)
	}

	collision, wrote, err := svc.UpdateNameGuarded(context.Background(), platformOnlyID, invisible)
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision != nil {
		t.Errorf("collision = %+v, want nil: an empty identity key is not a collision", collision)
	}
	if !wrote {
		t.Error("wrote = false, want true")
	}
	if got := nameByID(t, svc, platformOnlyID); got != invisible {
		t.Errorf("name = %q, want %q", got, invisible)
	}
}

// TestUpdateNameGuarded_RecordsHistory covers the history branch, which only
// runs after a successful commit.
//
// The ordering matters and is what this asserts: history must record the
// PRE-rename name as the old value. Recording the post-write name on both
// sides would produce a history row claiming the artist was renamed from its
// new name to itself, silently destroying the audit trail for renames while
// every other assertion about the write stayed green.
func TestUpdateNameGuarded_RecordsHistory(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := NewService(db)
	hsvc := NewHistoryService(db)
	svc.SetHistoryService(hsvc)
	ctx := context.Background()

	a := testArtist("Southgate Winds", "")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Precondition: no history for this artist yet, so the row asserted below
	// is the one this rename produced.
	if _, total, err := hsvc.List(ctx, a.ID, 10, 0); err != nil {
		t.Fatalf("List (precondition): %v", err)
	} else if total != 0 {
		t.Fatalf("precondition: %d history entries, want 0", total)
	}

	collision, wrote, err := svc.UpdateNameGuarded(ctx, a.ID, "Harrowdene Ensemble")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision != nil || !wrote {
		t.Fatalf("collision = %+v, wrote = %t; want (nil, true)", collision, wrote)
	}

	changes, total, err := hsvc.List(ctx, a.ID, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Fatalf("history entries = %d, want 1", total)
	}
	c := changes[0]
	if c.Field != "name" {
		t.Errorf("field = %q, want %q", c.Field, "name")
	}
	if c.OldValue != "Southgate Winds" {
		t.Errorf("old value = %q, want the PRE-rename name %q", c.OldValue, "Southgate Winds")
	}
	if c.NewValue != "Harrowdene Ensemble" {
		t.Errorf("new value = %q, want %q", c.NewValue, "Harrowdene Ensemble")
	}
}

// TestUpdateNameGuarded_RefusedRenameRecordsNoHistory is the companion: a
// rename that collides must leave no audit trail, because nothing happened.
// The history write sits after the commit precisely so a refusal cannot reach
// it, and this pins that placement.
func TestUpdateNameGuarded_RefusedRenameRecordsNoHistory(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := NewService(db)
	hsvc := NewHistoryService(db)
	svc.SetHistoryService(hsvc)
	ctx := context.Background()

	existing := testArtist("Southgate Winds", "")
	if err := svc.Create(ctx, existing); err != nil {
		t.Fatalf("Create existing: %v", err)
	}
	subject := testArtist("Northfield Chorale", "")
	if err := svc.Create(ctx, subject); err != nil {
		t.Fatalf("Create subject: %v", err)
	}

	collision, wrote, err := svc.UpdateNameGuarded(ctx, subject.ID, "Southgate Winds")
	if err != nil {
		t.Fatalf("UpdateNameGuarded: %v", err)
	}
	if collision == nil {
		t.Fatal("collision = nil, want the rename refused")
	}
	if wrote {
		t.Error("wrote = true, want false")
	}

	if _, total, err := hsvc.List(ctx, subject.ID, 10, 0); err != nil {
		t.Fatalf("List: %v", err)
	} else if total != 0 {
		t.Errorf("history entries = %d, want 0: a refused rename must leave no audit trail", total)
	}
}
