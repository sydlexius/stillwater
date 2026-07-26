package api

// handlers_name_collision_atomic_test.go -- handler-level guard for the
// TRANSACTIONAL half of the #2730 rename check (#2807).
//
// handlers_name_collision_guard_test.go covers the pre-write fast path. This
// file covers what happens when a rename gets PAST that fast path and is
// caught by the in-transaction re-check instead: the operator must see the
// identical 409 and the write must not land.
//
// The atomicity itself is proved in internal/artist
// (TestUpdateNameGuarded_ConcurrentRenamesToSameTarget, which drives two
// simultaneous renames against a real database and asserts on the rows). What
// is unproven at that layer is the HANDLER's behavior on the second refusal
// path, which is what these tests pin.

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// staleNameRepo reports a stale name for one artist, and only for that artist.
//
// This is how the test reaches the transactional refusal deterministically
// rather than by hoping for a scheduling accident. FindNameCollision (the fast
// path) loads the artist's CURRENT name and exempts a rename whose key already
// equals it -- the "tidying your own name" case. Feeding it a stale name equal
// to the rename target makes it take that exemption and return "no collision"
// without ever scanning for a partner.
//
// That is behaviorally the same position a racing request is in: the fast path
// answered from a view of the world that no longer holds. UpdateNameGuarded
// reads the name again inside its own write transaction, from the database
// rather than from this decorator, so it sees the truth and refuses.
type staleNameRepo struct {
	artist.Repository
	artistID  string
	staleName string
}

// DB re-exposes the wrapped repository's raw handle. Embedding the
// artist.Repository INTERFACE does not promote DB(), which is not part of it,
// so without this method the decorated service cannot open a transaction at
// all and the handler answers 500 -- the fail-closed path, not the refusal
// path under test.
func (r *staleNameRepo) DB() *sql.DB {
	acc, ok := r.Repository.(interface{ DB() *sql.DB })
	if !ok {
		return nil
	}
	return acc.DB()
}

func (r *staleNameRepo) GetByID(ctx context.Context, id string) (*artist.Artist, error) {
	a, err := r.Repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if id == r.artistID {
		clone := *a
		clone.Name = r.staleName
		return &clone, nil
	}
	return a, nil
}

// withStaleFastPath rebuilds the router's artist service so the fast-path
// guard sees staleName for artistID. Returns a service built on REAL repos for
// reading persisted state back, unaffected by the injection.
func withStaleFastPath(t *testing.T, r *Router, artistID, staleName string) *artist.Service {
	t.Helper()
	reader := artist.NewService(r.db)
	artists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(r.db)
	decorated := &staleNameRepo{Repository: artists, artistID: artistID, staleName: staleName}

	// The decorator MUST keep handing back a usable DB handle. If it does not,
	// UpdateNameGuarded fails closed with a 500 and every assertion below
	// would be measuring the fail-closed path while claiming to measure the
	// transactional refusal.
	if decorated.DB() == nil {
		t.Fatal("precondition: the stale-name decorator must expose the real DB handle, " +
			"or the tests reach the fail-closed 500 instead of the 409 under test")
	}

	r.artistService = artist.NewServiceWithRepos(decorated,
		providers, members, aliases, images, platformIDs, completeness)
	return reader
}

// TestHandleFieldUpdate_TransactionalCollision_RefusesJSON is the #2807
// regression guard for the API surface: a rename that slips past the pre-write
// guard must still be refused, with the SAME 409 the fast path produces.
//
// The stored-name assertion is the load-bearing half. The transaction writes
// the name before it checks, so a rollback that failed to fire would leave the
// duplicate committed while the operator was told the rename was refused -- a
// worse outcome than the original defect, because the response would actively
// lie about it.
func TestHandleFieldUpdate_TransactionalCollision_RefusesJSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	existing := addTestArtist(t, artistSvc, "Southgate Winds")
	subject := addTestArtist(t, artistSvc, "Northfield Chorale")

	reader := withStaleFastPath(t, r, subject.ID, "Southgate Winds")

	// Preconditions. Without these the test could pass because the rename was
	// never a collision in the first place.
	if got := nameOf(t, reader, subject.ID); got != "Northfield Chorale" {
		t.Fatalf("precondition: subject name = %q, want %q", got, "Northfield Chorale")
	}
	if got := nameOf(t, reader, existing.ID); got != "Southgate Winds" {
		t.Fatalf("precondition: partner name = %q, want %q", got, "Southgate Winds")
	}
	// Precondition: the fast path really does wave this through, so the 409
	// below can only have come from the transactional check.
	collision, err := r.artistService.FindNameCollision(context.Background(), subject.ID, "Southgate Winds")
	if err != nil {
		t.Fatalf("precondition: FindNameCollision: %v", err)
	}
	if collision != nil {
		t.Fatalf("precondition: the fast-path guard caught this rename (%+v); the test must reach "+
			"the in-transaction check, otherwise it re-covers the fast path", collision)
	}

	w := patchName(t, r, subject.ID, "Southgate Winds", false)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: a rename that races past the pre-write guard must still be "+
			"refused; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	// Same body as the fast-path refusal: both go through
	// writeNameCollisionRefusal, so the operator cannot tell which caught it.
	body := w.Body.String()
	if !strings.Contains(body, artist.ErrNameCollision.Error()) {
		t.Errorf("body = %s, want it to carry the collision error text", body)
	}
	if !strings.Contains(body, existing.ID) {
		t.Errorf("body = %s, want it to name the existing artist %s", body, existing.ID)
	}

	// The write must not have landed.
	if got := nameOf(t, reader, subject.ID); got != "Northfield Chorale" {
		t.Errorf("name = %q, want it rolled back to %q: the transaction writes before it checks, "+
			"so a missing rollback commits the very duplicate this refuses", got, "Northfield Chorale")
	}
	if got := nameOf(t, reader, existing.ID); got != "Southgate Winds" {
		t.Errorf("partner name = %q, want it untouched", got)
	}
}

// TestHandleFieldUpdate_TransactionalCollision_RefusesHTMX covers the same
// refusal on the HTMX surface, which renders a fragment rather than JSON.
// Both surfaces share one writer, and this asserts that sharing holds.
func TestHandleFieldUpdate_TransactionalCollision_RefusesHTMX(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	addTestArtist(t, artistSvc, "Southgate Winds")
	subject := addTestArtist(t, artistSvc, "Northfield Chorale")

	reader := withStaleFastPath(t, r, subject.ID, "Southgate Winds")

	if got := nameOf(t, reader, subject.ID); got != "Northfield Chorale" {
		t.Fatalf("precondition: subject name = %q", got)
	}

	w := patchName(t, r, subject.ID, "Southgate Winds", true)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want an HTML fragment for an HTMX request", ct)
	}
	// The fragment names the other artist. Asserting on the rendered name
	// rather than a marker keeps this tied to what the operator actually reads.
	if !strings.Contains(w.Body.String(), "Southgate Winds") {
		t.Errorf("body = %s, want it to name the existing artist", w.Body.String())
	}
	if got := nameOf(t, reader, subject.ID); got != "Northfield Chorale" {
		t.Errorf("name = %q, want it rolled back", got)
	}
}

// TestHandleFieldUpdate_CleanRenameStillWrites is the control: with no
// colliding partner the rename lands and returns 200. Routing the name field
// through a new service method must not have broken the ordinary path.
func TestHandleFieldUpdate_CleanRenameStillWrites(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	subject := addTestArtist(t, artistSvc, "Northfield Chorale")

	if got := nameOf(t, artistSvc, subject.ID); got != "Northfield Chorale" {
		t.Fatalf("precondition: name = %q", got)
	}

	w := patchName(t, r, subject.ID, "Harrowdene Ensemble", false)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := nameOf(t, artistSvc, subject.ID); got != "Harrowdene Ensemble" {
		t.Errorf("name = %q, want %q", got, "Harrowdene Ensemble")
	}
}

// deadDBRepo answers reads normally but hands back a CLOSED *sql.DB, so
// anything that opens a transaction fails while ordinary repository lookups
// still succeed.
//
// That split is exactly what this test needs. It stands in for the database
// becoming unreachable partway through a request (pool shut down, file pulled
// out from under the process), which is a real failure mode rather than an
// invented one.
type deadDBRepo struct {
	artist.Repository
	dead *sql.DB
}

func (r *deadDBRepo) DB() *sql.DB { return r.dead }

// TestHandleFieldUpdate_GuardedRenameError_Returns500 covers the handler's
// error branch for the transactional rename: when UpdateNameGuarded cannot
// complete, the operator gets a 500 and the write does not happen.
//
// Reaching this branch requires the pre-write guard to PASS and the
// transactional call to FAIL, which sounds contrived until you look at how the
// two differ. FindNameCollision short-circuits on a cosmetic self-rename -- a
// new name whose identity key equals the artist's own -- and returns before it
// ever asks for a database handle. UpdateNameGuarded always opens a
// transaction. So a cosmetic rename against a dead database splits them
// naturally: the guard is satisfied without touching the database, and the
// transaction is what fails.
//
// The 500 is the point. "The write could not be completed" must never be
// reported as success, and the raw driver error must not reach the client.
func TestHandleFieldUpdate_GuardedRenameError_Returns500(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	subject := addTestArtist(t, artistSvc, "The Southgate Winds")

	// A reader on the real repos, unaffected by the fault injection below.
	reader := artist.NewService(r.db)

	// A closed handle. Precondition-checked so a failure below is attributable
	// to this and not to some other fault.
	dead := newTestDB(t)
	if err := dead.Close(); err != nil {
		t.Fatalf("closing throwaway db: %v", err)
	}
	if err := dead.Ping(); err == nil {
		t.Fatal("precondition: the throwaway database must be closed")
	}

	artists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(r.db)
	r.artistService = artist.NewServiceWithRepos(
		&deadDBRepo{Repository: artists, dead: dead},
		providers, members, aliases, images, platformIDs, completeness)

	// "The Southgate Winds" -> "Southgate Winds" is a cosmetic self-rename:
	// same identity key, different bytes. Precondition, because the whole
	// routing of this test depends on it.
	const newName = "Southgate Winds"
	if artist.NormalizeIdentityKey(newName) != artist.NormalizeIdentityKey("The Southgate Winds") {
		t.Fatal("precondition: the rename must keep the same identity key, " +
			"or the pre-write guard consults the database and fails first")
	}
	if collision, err := r.artistService.FindNameCollision(context.Background(), subject.ID, newName); err != nil {
		t.Fatalf("precondition: the pre-write guard must SUCCEED here, so the 500 below is "+
			"attributable to the transactional call: %v", err)
	} else if collision != nil {
		t.Fatalf("precondition: the pre-write guard must find no collision, got %+v", collision)
	}

	w := patchName(t, r, subject.ID, newName, false)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: a rename whose transaction cannot complete must be "+
			"reported as a failure; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	// The STATUS ALONE DOES NOT PIN THIS BRANCH, so the message is asserted
	// too. With the rename error swallowed, the handler carries on to the
	// post-write re-fetch, which fails against the same dead database and
	// answers 500 as well -- with "failed to reload artist". A status-only
	// check passes against that, and would report a branch as covered while
	// the error was being ignored. Verified by mutation: deleting the error
	// check leaves the status at 500 and changes only this string.
	body := w.Body.String()
	if !strings.Contains(body, "failed to update field") {
		t.Errorf("body = %s, want the UPDATE failure message: a 500 from the later re-fetch "+
			"would mean the rename error was swallowed rather than handled", body)
	}
	// The raw driver error must not reach the client.
	if strings.Contains(body, "sql: database is closed") {
		t.Errorf("response leaks the raw driver error:\n%s", body)
	}
	// And the name is untouched.
	if got := nameOf(t, reader, subject.ID); got != "The Southgate Winds" {
		t.Errorf("name = %q, want it unchanged at %q", got, "The Southgate Winds")
	}
}
