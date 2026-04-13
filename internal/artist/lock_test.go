package artist

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLockAndUnlock(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Radiohead", "/music/Radiohead")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Initially unlocked.
	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Locked {
		t.Error("expected artist to start unlocked")
	}
	if got.LockSource != "" {
		t.Errorf("expected empty lock_source, got %q", got.LockSource)
	}
	if got.LockedAt != nil {
		t.Error("expected nil locked_at")
	}

	// Lock the artist.
	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	got, err = svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after lock: %v", err)
	}
	if !got.Locked {
		t.Error("expected artist to be locked")
	}
	if got.LockSource != "user" {
		t.Errorf("lock_source = %q, want %q", got.LockSource, "user")
	}
	if got.LockedAt == nil {
		t.Error("expected non-nil locked_at")
	}

	// Unlock the artist.
	if err := svc.Unlock(ctx, a.ID); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	got, err = svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after unlock: %v", err)
	}
	if got.Locked {
		t.Error("expected artist to be unlocked")
	}
	if got.LockSource != "" {
		t.Errorf("expected empty lock_source after unlock, got %q", got.LockSource)
	}
	if got.LockedAt != nil {
		t.Error("expected nil locked_at after unlock")
	}
}

func TestLockNotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	err := svc.Lock(ctx, "nonexistent-id", "user")
	if err == nil {
		t.Fatal("expected error for nonexistent artist")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestLockAlreadyLocked(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Deftones", "/music/Deftones")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Locking again should return ErrAlreadyLocked.
	err := svc.Lock(ctx, a.ID, "user")
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Errorf("expected ErrAlreadyLocked, got %v", err)
	}
}

func TestUnlockNotLocked(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Korn", "/music/Korn")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Unlocking a non-locked artist should return ErrNotLocked.
	err := svc.Unlock(ctx, a.ID)
	if !errors.Is(err, ErrNotLocked) {
		t.Errorf("expected ErrNotLocked, got %v", err)
	}
}

func TestLockInvalidSource(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Slipknot", "/music/Slipknot")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := svc.Lock(ctx, a.ID, "invalid-source")
	if err == nil {
		t.Fatal("expected error for invalid lock source")
	}
	if got := err.Error(); !strings.Contains(got, "invalid lock source") {
		t.Errorf("expected invalid lock source error, got %q", got)
	}
}

func TestLockImportedSource(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Tool", "/music/Tool")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Lock(ctx, a.ID, "imported"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LockSource != "imported" {
		t.Errorf("lock_source = %q, want %q", got.LockSource, "imported")
	}
}

func TestListFilterLocked(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create two artists, lock one.
	a1 := testArtist("Locked Band", "/music/Locked")
	a2 := testArtist("Free Band", "/music/Free")
	if err := svc.Create(ctx, a1); err != nil {
		t.Fatalf("Create a1: %v", err)
	}
	if err := svc.Create(ctx, a2); err != nil {
		t.Fatalf("Create a2: %v", err)
	}
	if err := svc.Lock(ctx, a1.ID, "user"); err != nil {
		t.Fatalf("Lock a1: %v", err)
	}

	// Filter=locked should return only the locked artist.
	locked, total, err := svc.List(ctx, ListParams{
		Page: 1, PageSize: 50, Filter: "locked",
	})
	if err != nil {
		t.Fatalf("List locked: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(locked) != 1 || locked[0].ID != a1.ID {
		t.Errorf("expected only locked artist, got %d results", len(locked))
	}

	// Filter=not_locked should return only the unlocked artist.
	unlocked, total, err := svc.List(ctx, ListParams{
		Page: 1, PageSize: 50, Filter: "not_locked",
	})
	if err != nil {
		t.Fatalf("List not_locked: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(unlocked) != 1 || unlocked[0].ID != a2.ID {
		t.Errorf("expected only unlocked artist, got %d results", len(unlocked))
	}
}

func TestLockPreservedOnUpdate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Muse", "/music/Muse")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Lock the artist.
	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Update the artist (simulating a metadata update).
	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	got.Biography = "Updated biography"
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Lock should still be set after update.
	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if !after.Locked {
		t.Error("expected lock to be preserved after Update")
	}
	if after.LockSource != "user" {
		t.Errorf("lock_source = %q, want %q", after.LockSource, "user")
	}
}

func TestFieldLocks(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Björk", "/music/Bjork")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Initially no locked fields.
	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.LockedFields) != 0 {
		t.Errorf("expected empty locked_fields, got %v", got.LockedFields)
	}

	// Add a field lock; values are normalized to lowercase.
	if err := svc.AddLockedField(ctx, a.ID, "Biography"); err != nil {
		t.Fatalf("AddLockedField: %v", err)
	}
	got, _ = svc.GetByID(ctx, a.ID)
	if !svc.IsFieldLocked(got, "biography") {
		t.Errorf("expected biography to be locked, got %v", got.LockedFields)
	}
	if !svc.IsFieldLocked(got, "BIOGRAPHY") {
		t.Error("IsFieldLocked should be case-insensitive")
	}

	// Adding a duplicate (different casing) should not produce two entries.
	if err := svc.AddLockedField(ctx, a.ID, "biography"); err != nil {
		t.Fatalf("AddLockedField dup: %v", err)
	}
	got, _ = svc.GetByID(ctx, a.ID)
	if len(got.LockedFields) != 1 {
		t.Errorf("expected 1 locked field after dup add, got %v", got.LockedFields)
	}

	// Remove the lock.
	if err := svc.RemoveLockedField(ctx, a.ID, "biography"); err != nil {
		t.Fatalf("RemoveLockedField: %v", err)
	}
	got, _ = svc.GetByID(ctx, a.ID)
	if svc.IsFieldLocked(got, "biography") {
		t.Errorf("expected biography unlocked, got %v", got.LockedFields)
	}

	// SetLockedFields replaces the entire set.
	if err := svc.SetLockedFields(ctx, a.ID, []string{"Name", "Genres", "Name"}); err != nil {
		t.Fatalf("SetLockedFields: %v", err)
	}
	got, _ = svc.GetByID(ctx, a.ID)
	if len(got.LockedFields) != 2 {
		t.Errorf("expected 2 fields after SetLockedFields, got %v", got.LockedFields)
	}

	// Unknown artist.
	err = svc.SetLockedFields(ctx, "nonexistent", []string{"biography"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing artist, got %v", err)
	}
}

func TestImageLocks(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Portishead", "/music/Portishead")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	img := &ArtistImage{
		ArtistID:  a.ID,
		ImageType: "thumb",
		SlotIndex: 0,
		Exists:    true,
	}
	if err := svc.UpsertImage(ctx, img); err != nil {
		// Fall back: use the raw repo through the service's GetImagesForArtist
		// path. Test existing service API.
		t.Fatalf("UpsertImage: %v", err)
	}

	imgs, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	if len(imgs) == 0 {
		t.Fatal("expected one image")
	}
	if imgs[0].Locked {
		t.Error("expected new image to start unlocked")
	}

	if err := svc.SetImageLock(ctx, imgs[0].ID, true); err != nil {
		t.Fatalf("SetImageLock: %v", err)
	}
	imgs, _ = svc.GetImagesForArtist(ctx, a.ID)
	if !imgs[0].Locked {
		t.Error("expected image to be locked after SetImageLock(true)")
	}

	if err := svc.SetImageLock(ctx, imgs[0].ID, false); err != nil {
		t.Fatalf("SetImageLock false: %v", err)
	}
	imgs, _ = svc.GetImagesForArtist(ctx, a.ID)
	if imgs[0].Locked {
		t.Error("expected image to be unlocked after SetImageLock(false)")
	}
}
