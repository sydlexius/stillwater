package artist

import (
	"context"
	"testing"
)

// TestUpdateFieldNoopSkipsWriteAndHistory verifies that calling UpdateField with
// the same value that is already stored does not touch the DB or produce a
// history entry.
func TestUpdateFieldNoopSkipsWriteAndHistory(t *testing.T) {
	t.Parallel()
	svc, hsvc := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Pearl Jam", "/music/Pearl Jam")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Re-writing the same biography should be a no-op.
	if err := svc.UpdateField(ctx, a.ID, "biography", "A test artist."); err != nil {
		t.Fatalf("UpdateField (same value): %v", err)
	}

	_, total, err := hsvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	if total != 0 {
		t.Errorf("expected 0 history entries for no-op UpdateField, got %d", total)
	}

	// Also verify the underlying DB value was not touched (updatedAt should
	// be identical to what was set on Create -- we can't easily check that
	// directly, but confirming the value is still the same is sufficient).
	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Biography != "A test artist." {
		t.Errorf("Biography = %q, want %q", got.Biography, "A test artist.")
	}
}

// TestUpdateFieldRealChangeWritesAndRecords verifies that a genuine value
// change still writes to the DB and produces a history entry.
func TestUpdateFieldRealChangeWritesAndRecords(t *testing.T) {
	t.Parallel()
	svc, hsvc := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Mudhoney", "/music/Mudhoney")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.UpdateField(ctx, a.ID, "biography", "Updated biography."); err != nil {
		t.Fatalf("UpdateField (new value): %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Biography != "Updated biography." {
		t.Errorf("Biography = %q, want %q", got.Biography, "Updated biography.")
	}

	_, total, err := hsvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 history entry for real change, got %d", total)
	}
}

// TestClearFieldAlreadyEmptyIsNoop verifies that ClearField on a field that is
// already empty produces no DB write and no history entry.
func TestClearFieldAlreadyEmptyIsNoop(t *testing.T) {
	t.Parallel()
	svc, hsvc := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Screaming Trees", "/music/Screaming Trees")
	// biography starts non-empty; clear it first so the field is empty.
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.ClearField(ctx, a.ID, "biography"); err != nil {
		t.Fatalf("ClearField (first): %v", err)
	}

	// Second clear should be a no-op.
	if err := svc.ClearField(ctx, a.ID, "biography"); err != nil {
		t.Fatalf("ClearField (second, already empty): %v", err)
	}

	_, total, err := hsvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	// Only the first clear should have generated an entry.
	if total != 1 {
		t.Errorf("expected 1 history entry (first clear only), got %d", total)
	}
}

// TestHistoryServiceRecordNoopGuard verifies that HistoryService.Record returns
// nil without writing when oldValue == newValue.
func TestHistoryServiceRecordNoopGuard(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	hsvc := NewHistoryService(db)
	ctx := context.Background()

	// Supply a valid source and identical old/new values.
	if err := hsvc.Record(ctx, "artist-id-1", "biography", "same", "same", "manual"); err != nil {
		t.Fatalf("Record with identical values: %v", err)
	}

	// Nothing should have been persisted.
	changes, total, err := hsvc.List(ctx, "artist-id-1", 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	_ = changes
	if total != 0 {
		t.Errorf("expected 0 history entries for no-op Record, got %d", total)
	}
}

// TestUpdateFieldScalarPaddedValueIsNotNoop verifies that a corrective write
// for a scalar field whose stored value has leading/trailing whitespace is
// never silently dropped. The repository stores scalar values verbatim, so
// comparing "  rock  " vs "rock" must be treated as a real change.
func TestUpdateFieldScalarPaddedValueIsNotNoop(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc, hsvc := func() (*Service, *HistoryService) {
		s := NewService(db)
		h := NewHistoryService(db)
		s.SetHistoryService(h)
		return s, h
	}()
	ctx := context.Background()

	a := testArtist("Temple of the Dog", "/music/TempleDog")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write a padded biography directly to the DB, bypassing the service layer,
	// to simulate data that arrived with surrounding whitespace.
	if _, err := db.ExecContext(ctx,
		"UPDATE artists SET biography = ? WHERE id = ?",
		"  A test artist.  ", a.ID,
	); err != nil {
		t.Fatalf("direct DB write of padded value: %v", err)
	}

	// A corrective write with the trimmed value must NOT be treated as a no-op.
	if err := svc.UpdateField(ctx, a.ID, "biography", "A test artist."); err != nil {
		t.Fatalf("UpdateField (corrective trim): %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Biography != "A test artist." {
		t.Errorf("Biography = %q, want %q after corrective write", got.Biography, "A test artist.")
	}

	_, total, err := hsvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 history entry for corrective write, got %d (must not be silent no-op)", total)
	}
}

// TestUpdateFieldNoopSliceField verifies the no-op check works for slice fields
// (genres, styles, moods) with varying spacing/comma formatting.
func TestUpdateFieldNoopSliceField(t *testing.T) {
	t.Parallel()
	svc, hsvc := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Soundgarden", "/music/Soundgarden")
	// testArtist sets Genres: ["Rock", "Alternative"]
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write the same genres with different comma spacing -- should be a no-op.
	if err := svc.UpdateField(ctx, a.ID, "genres", "Rock,Alternative"); err != nil {
		t.Fatalf("UpdateField (same genres, diff spacing): %v", err)
	}

	_, total, err := hsvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	if total != 0 {
		t.Errorf("expected 0 history entries for same-genre no-op, got %d", total)
	}
}
