package artist

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// failingGetByIDRepo makes exactly ONE stored-row read fail, then behaves
// normally. Embedding Repository rather than reimplementing it means a method
// added to the interface later cannot silently turn this into a partial fake
// that passes for the wrong reason.
type failingGetByIDRepo struct {
	Repository
	failFor string // artist ID whose next GetByID fails
	fired   bool   // whether the injected failure actually happened
}

func (r *failingGetByIDRepo) GetByID(ctx context.Context, id string) (*Artist, error) {
	if id == r.failFor && !r.fired {
		r.fired = true
		return nil, fmt.Errorf("injected read failure")
	}
	return r.Repository.GetByID(ctx, id)
}

// TestUpdate_RefusesWhenTheStoredRowCannotBeRead pins the FAIL-CLOSED contract
// on the pre-write snapshot.
//
// The old shape logged a warning, set old = nil, and let the write proceed. A
// whole-row persist that runs with no idea what it is overwriting cannot be
// reconciled afterwards: the previous values are gone, and at this commit the
// history diff for that write is silently lost with them.
//
// The read failure is INJECTED rather than simulated by deleting the row,
// because a missing row is the benign case and must still be let through -- the
// final block asserts exactly that, so the fix cannot be over-applied into
// refusing a write whose row was deleted between the caller's load and here.
//
// That final block passes a provider-ID-free artist, and its assertion is
// deliberately no broader than that: an artist carrying a provider ID fails
// later in persistNormalized on the artist_provider_ids foreign key, at this
// commit and at main alike. See the ErrNotFound comment in Service.update.
func TestUpdate_RefusesWhenTheStoredRowCannotBeRead(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	const storedBio = "the stored biography"
	a := &Artist{Name: "Unreadable Row", Biography: storedBio}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating: %v", err)
	}
	seeded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading seed: %v", err)
	}
	// Precondition: the seed persisted, or "the write did not land" below is
	// indistinguishable from "there was nothing to write".
	if seeded.Biography != storedBio {
		t.Fatalf("precondition: biography = %q, want %q; the seed did not persist", seeded.Biography, storedBio)
	}

	failing := &failingGetByIDRepo{Repository: svc.artists, failFor: a.ID}
	svc.artists = failing

	seeded.Biography = "a replacement written blind"
	updErr := svc.Update(ctx, seeded)
	if updErr == nil {
		t.Fatal("Update succeeded while the stored row was unreadable; a whole-row write that cannot see what it overwrites must be refused")
	}
	// Precondition: the INJECTED failure is what stopped it, not some unrelated
	// error. Without this the test passes against any failure at all.
	if !failing.fired {
		t.Fatal("precondition: the injected read failure never fired, so this test proves nothing about the refusal")
	}
	if !strings.Contains(updErr.Error(), "reading stored artist") {
		t.Errorf("refusal = %v, want it to name the read it could not perform", updErr)
	}

	// The write must not have landed.
	after, getErr := svc.artists.GetByID(ctx, a.ID)
	if getErr != nil {
		t.Fatalf("reloading: %v", getErr)
	}
	if after.Biography != storedBio {
		t.Errorf("biography = %q, want %q; the refusal did not prevent the write", after.Biography, storedBio)
	}

	// POSITIVE CONTROL: with the injected failure spent, the identical write
	// lands. This is what proves the refusal above came from the unreadable row
	// and not from a Service that cannot write at all.
	after.Biography = "a replacement written with the snapshot in hand"
	if err := svc.Update(ctx, after); err != nil {
		t.Fatalf("control write: %v", err)
	}
	control, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading control: %v", err)
	}
	if control.Biography != "a replacement written with the snapshot in hand" {
		t.Fatalf("control biography = %q, want the write to have landed; the refusal above proves nothing otherwise", control.Biography)
	}

	// A genuinely MISSING row stays benign: there is no stored row to read, so
	// there is nothing to lose, and the repo Update matches zero rows. This is
	// the case the fail-closed branch must NOT catch.
	//
	// The fixture carries NO provider ID, on purpose. Adding one would make this
	// assert something else entirely -- persistNormalized would fail on the
	// artist_provider_ids foreign key, which is pre-existing behavior unrelated
	// to the branch under test.
	if err := svc.Update(ctx, &Artist{ID: "no-such-artist", Name: "Ghost"}); err != nil {
		t.Errorf("Update on a nonexistent artist = %v, want nil; ErrNotFound is the benign case and must not be refused", err)
	}
}
