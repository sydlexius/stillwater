package artist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// failingGetByIDRepo makes exactly ONE stored-row read fail, then behaves
// normally. Embedding Repository rather than reimplementing it means a method
// added to the interface later cannot silently turn this into a partial fake
// that passes for the wrong reason.
type failingGetByIDRepo struct {
	Repository
	failFor  string // artist ID whose next GetByID fails
	failWith error  // the error to inject; defaults to errInjectedGetByID
	fired    bool   // whether the injected failure actually happened
}

// errInjectedGetByID is a SENTINEL, not a formatted string, so a test can assert
// the refusal preserves the CAUSE CHAIN with errors.Is rather than only matching
// on message text. Service.update wraps with %w; a mutation to %v would keep
// every message assertion green while silently breaking errors.Is for every
// downstream caller, which is exactly the regression this sentinel catches.
var errInjectedGetByID = errors.New("injected read failure")

func (r *failingGetByIDRepo) GetByID(ctx context.Context, id string) (*Artist, error) {
	if id == r.failFor && !r.fired {
		r.fired = true
		if r.failWith != nil {
			return nil, r.failWith
		}
		return nil, errInjectedGetByID
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
	// TWO assertions, testing DIFFERENT things -- neither replaces the other.
	//
	// The cause chain: production wraps with %w, and errors.Is is the contract
	// every downstream caller depends on to classify this failure. Nothing else
	// in this package pins it, so a %w -> %v mutation would otherwise pass.
	if !errors.Is(updErr, errInjectedGetByID) {
		t.Errorf("errors.Is(err, errInjectedGetByID) = false for %v; the refusal dropped the cause chain, so no caller can classify it", updErr)
	}
	// The message: names the operation that failed, so an operator reading a log
	// line knows what the service was doing. A wrapped-but-unlabelled error
	// satisfies errors.Is and still tells them nothing.
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

// TestUpdate_UnreadableSnapshotLogLevelDependsOnCause pins the level
// discrimination, because getting it wrong is invisible: every case still
// refuses the write, so no behavioral test can tell them apart. Only the emitted
// record can.
//
// A caller going away -- a disconnected client, a canceled parent context during
// graceful shutdown, an elapsed request deadline -- is normal operation, not a
// fault of this process. Logging those at Error trains an operator to ignore
// Error. Every OTHER read failure is a genuine fault and must stay loud.
//
// The refusal is asserted in every case too, so a future "fix" that downgrades
// the level by skipping the refusal fails here rather than looking correct.
func TestUpdate_UnreadableSnapshotLogLevelDependsOnCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		injected  error
		wantLevel slog.Level
	}{
		{"canceled_context", context.Canceled, slog.LevelWarn},
		{"deadline_exceeded", context.DeadlineExceeded, slog.LevelWarn},
		// Wrapped, because a driver returns its own error around the cause; the
		// discrimination must survive wrapping or it fires only in tests.
		{"wrapped_cancellation", fmt.Errorf("querying artists: %w", context.Canceled), slog.LevelWarn},
		{"real_fault", errInjectedGetByID, slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc := NewService(newTestDB(t))

			a := &Artist{Name: "Level " + tc.name, Biography: "stored"}
			if err := svc.Create(ctx, a); err != nil {
				t.Fatalf("creating: %v", err)
			}
			stored, err := svc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading: %v", err)
			}

			failing := &failingGetByIDRepo{Repository: svc.artists, failFor: a.ID, failWith: tc.injected}
			svc.artists = failing

			rec := &levelRecorder{}
			restore := slog.Default()
			slog.SetDefault(slog.New(rec))
			// Restored via Cleanup, not a manual call after the write: the
			// handler is PROCESS-WIDE, and svc.Update sits between the swap and
			// any manual restore. A panic in there would leak the recorder into
			// every later test in the package, where it surfaces as failures
			// that appear to come from unrelated code. Matches the idiom already
			// used in merge_field_locks_test.go and sqlite_image_test.go. (Also
			// why these subtests do not call t.Parallel.)
			t.Cleanup(func() { slog.SetDefault(restore) })
			stored.Biography = "a replacement"
			updErr := svc.Update(ctx, stored)

			// The refusal must happen regardless of level. Without this a
			// downgrade that also stopped refusing would pass.
			if updErr == nil {
				t.Fatal("Update succeeded; the level choice must not change the fail-closed behavior")
			}
			if !failing.fired {
				t.Fatal("precondition: the injected failure never fired, so no record was produced by the branch under test")
			}
			// Precondition: exactly one record from the branch under test, or
			// "the level was X" is ambiguous about which line it describes.
			if len(rec.levels) != 1 {
				t.Fatalf("captured %d records %v, want exactly 1 from the refusal branch", len(rec.levels), rec.levels)
			}
			if rec.levels[0] != tc.wantLevel {
				t.Errorf("logged at %v, want %v; a caller going away is normal operation and must not read as a server fault, while a real fault must stay loud",
					rec.levels[0], tc.wantLevel)
			}
		})
	}
}

// levelRecorder captures the LEVEL of each record. Enabled for every level so a
// downgrade past the handler's threshold shows up as a missing record rather
// than as a silent pass.
type levelRecorder struct{ levels []slog.Level }

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.levels = append(h.levels, r.Level)
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }
