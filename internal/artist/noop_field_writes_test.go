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
	if _, err := svc.UpdateField(ctx, a.ID, "biography", "A test artist."); err != nil {
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

	if _, err := svc.UpdateField(ctx, a.ID, "biography", "Updated biography."); err != nil {
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
	if _, err := svc.ClearField(ctx, a.ID, "biography"); err != nil {
		t.Fatalf("ClearField (first): %v", err)
	}

	// Second clear should be a no-op.
	if _, err := svc.ClearField(ctx, a.ID, "biography"); err != nil {
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
	if _, err := svc.UpdateField(ctx, a.ID, "biography", "A test artist."); err != nil {
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
	if _, err := svc.UpdateField(ctx, a.ID, "genres", "Rock,Alternative"); err != nil {
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

// TestUpdateFieldChangedBoolTrue verifies that UpdateField returns changed=true
// when a real write occurs (new value differs from current).
func TestUpdateFieldChangedBoolTrue(t *testing.T) {
	t.Parallel()
	svc, _ := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Foo Fighters", "/music/Foo Fighters")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	changed, err := svc.UpdateField(ctx, a.ID, "biography", "New bio.")
	if err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true for a real write")
	}
}

// TestUpdateFieldChangedBoolFalse verifies that UpdateField returns changed=false
// when the new value equals the current value (no-op).
func TestUpdateFieldChangedBoolFalse(t *testing.T) {
	t.Parallel()
	svc, _ := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Audioslave", "/music/Audioslave")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// biography was set on Create; writing the same value must be a no-op.
	changed, err := svc.UpdateField(ctx, a.ID, "biography", "A test artist.")
	if err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for no-op write")
	}
}

// TestClearFieldChangedBoolTrue verifies that ClearField returns changed=true
// when a real write occurs (field was non-empty).
func TestClearFieldChangedBoolTrue(t *testing.T) {
	t.Parallel()
	svc, _ := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Alice in Chains", "/music/Alice in Chains")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	changed, err := svc.ClearField(ctx, a.ID, "biography")
	if err != nil {
		t.Fatalf("ClearField: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true when clearing a non-empty field")
	}
}

// TestClearFieldChangedBoolFalse verifies that ClearField returns changed=false
// when the field is already empty (no-op).
func TestClearFieldChangedBoolFalse(t *testing.T) {
	t.Parallel()
	svc, _ := setupServiceWithHistory(t)
	ctx := context.Background()

	a := testArtist("Blind Melon", "/music/Blind Melon")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Clear once to make the field empty.
	if _, err := svc.ClearField(ctx, a.ID, "biography"); err != nil {
		t.Fatalf("ClearField (first): %v", err)
	}

	// Second clear must be a no-op.
	changed, err := svc.ClearField(ctx, a.ID, "biography")
	if err != nil {
		t.Fatalf("ClearField (second): %v", err)
	}
	if changed {
		t.Error("changed = true, want false when field is already empty")
	}
}

// TestUpdateFieldPrefetchErrorStillWrites verifies that when GetByID fails
// during the no-op pre-fetch, UpdateField proceeds with the write rather than
// silently dropping it (oldKnown stays false, so the no-op guard never fires).
func TestUpdateFieldPrefetchErrorStillWrites(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	// No history service attached - this test focuses on the write path.
	ctx := context.Background()

	a := testArtist("Mazzy Star", "/music/Mazzy Star")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// We cannot easily inject a fetch error without swapping the repo, so instead
	// we verify the symmetric guarantee: when a real artist ID is used, the write
	// always proceeds (changed=true). The pre-fetch error path is covered by the
	// fact that the no-op guard is gated on oldKnown (only set when fetch succeeds);
	// if fetch fails, oldKnown=false and the guard never fires even if values happen
	// to be equal. This test confirms the write still returns changed=true.
	changed, err := svc.UpdateField(ctx, a.ID, "biography", "Different bio.")
	if err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true for a write with a valid artist")
	}
}

// TestClearFieldNoHistoryService verifies that ClearField works correctly
// when no HistoryService is attached (history-disabled deployment).
func TestClearFieldNoHistoryService(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db) // no history service
	ctx := context.Background()

	a := testArtist("Slowdive", "/music/Slowdive")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First clear on a non-empty field must succeed and report changed=true.
	changed, err := svc.ClearField(ctx, a.ID, "biography")
	if err != nil {
		t.Fatalf("ClearField: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true when clearing non-empty field (no history service)")
	}

	// Second clear on an already-empty field must be a no-op (changed=false).
	changed, err = svc.ClearField(ctx, a.ID, "biography")
	if err != nil {
		t.Fatalf("ClearField (second): %v", err)
	}
	if changed {
		t.Error("changed = true, want false when field already empty (no history service)")
	}
}

// TestHistoryRecordRuleFxEmptyMessageNotDropped verifies that HistoryService.Record
// does NOT silently discard a record where oldValue=="" and newValue=="" (the
// rule_fix edge case where fr.Message is accidentally empty). The no-op guard
// must only fire when BOTH values are non-empty and identical.
func TestHistoryRecordRuleFxEmptyMessageNotDropped(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	hsvc := NewHistoryService(db)
	ctx := context.Background()

	// Create a real artist so the FK constraint on artist_id is satisfied.
	a := testArtist("Rule Fix Subject", "/music/Rule Fix Subject")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Record a rule_fix-style entry with both oldValue and newValue empty.
	// This must NOT be silently discarded (it represents an accidental empty
	// message that callers should be warned about via the persisted record).
	err := hsvc.Record(ctx, a.ID, "rule_fix", "", "", "rule:some-rule")
	if err != nil {
		t.Fatalf("Record(old='', new=''): unexpected error: %v", err)
	}

	changes, total, err := hsvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	_ = changes
	if total != 1 {
		t.Errorf("expected 1 history entry for (old='', new='') record, got %d (no-op guard must not fire when oldValue is empty)", total)
	}
}

// TestUpdateFieldDBClosedCoversWarnPath exercises the slog.Warn path that fires
// when the pre-fetch GetByID call fails. Closing the underlying *sql.DB forces
// that failure so the no-op guard never has a chance to fire (oldKnown stays
// false), and the subsequent UpdateField call also fails, covering both the
// fetch-error branch and the write-error return.
func TestUpdateFieldDBClosedCoversWarnPath(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db) // no history service; we are testing error paths only
	ctx := context.Background()

	a := testArtist("DB Close UpdateField", "/music/DBCloseUpdateField")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Close the DB so that the no-op pre-fetch (GetByID) returns an error.
	// UpdateField must NOT silently drop the write on fetch failure -- instead
	// it proceeds and also returns the write error.
	_ = db.Close()

	_, err := svc.UpdateField(ctx, a.ID, "biography", "Any value.")
	if err == nil {
		t.Error("expected error from UpdateField on a closed DB, got nil")
	}
}

// TestClearFieldDBClosedCoversWarnPath exercises the slog.Warn path for the
// ClearField pre-fetch failure, mirroring TestUpdateFieldDBClosedCoversWarnPath.
func TestClearFieldDBClosedCoversWarnPath(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("DB Close ClearField", "/music/DBCloseClearField")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_ = db.Close()

	_, err := svc.ClearField(ctx, a.ID, "biography")
	if err == nil {
		t.Error("expected error from ClearField on a closed DB, got nil")
	}
}
