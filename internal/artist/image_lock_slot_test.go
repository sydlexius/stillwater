package artist

import (
	"context"
	"testing"
)

// TestSetImageLockBySlot covers the #2533 helper that manual-save code uses to
// auto-lock a just-saved slot: it resolves imageType+slot to the row ID and
// locks it, and no-ops gracefully when no matching row exists yet.
func TestSetImageLockBySlot(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Slot Lock", "/music/Slot Lock")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Seed two fanart slots so the resolver has to match on slot index.
	for _, slot := range []int{0, 1} {
		if err := svc.UpsertImage(ctx, &ArtistImage{ArtistID: a.ID, ImageType: "fanart", SlotIndex: slot, Exists: true}); err != nil {
			t.Fatalf("UpsertImage slot %d: %v", slot, err)
		}
	}

	// Locks exactly the addressed slot, leaving the sibling untouched.
	if err := svc.SetImageLockBySlot(ctx, a.ID, "fanart", 1, true); err != nil {
		t.Fatalf("SetImageLockBySlot(fanart,1): %v", err)
	}
	imgs, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	locked := map[int]bool{}
	for i := range imgs {
		if imgs[i].ImageType == "fanart" {
			locked[imgs[i].SlotIndex] = imgs[i].Locked
		}
	}
	if !locked[1] {
		t.Error("fanart slot 1 should be locked")
	}
	if locked[0] {
		t.Error("fanart slot 0 must not be locked (only slot 1 was addressed)")
	}

	// A slot with no row is a graceful no-op (nil error), not a failure.
	if err := svc.SetImageLockBySlot(ctx, a.ID, "fanart", 9, true); err != nil {
		t.Errorf("SetImageLockBySlot for a nonexistent slot should no-op nil, got %v", err)
	}
	// A different image type with no row is likewise a no-op.
	if err := svc.SetImageLockBySlot(ctx, a.ID, "thumb", 0, true); err != nil {
		t.Errorf("SetImageLockBySlot for a nonexistent thumb row should no-op nil, got %v", err)
	}

	// Unlock path resolves the same way.
	if err := svc.SetImageLockBySlot(ctx, a.ID, "fanart", 1, false); err != nil {
		t.Fatalf("SetImageLockBySlot unlock: %v", err)
	}
	imgs, _ = svc.GetImagesForArtist(ctx, a.ID)
	for i := range imgs {
		if imgs[i].ImageType == "fanart" && imgs[i].SlotIndex == 1 && imgs[i].Locked {
			t.Error("fanart slot 1 should be unlocked after SetImageLockBySlot(false)")
		}
	}

	// A read error propagates rather than being swallowed.
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	if err := svc.SetImageLockBySlot(ctx, a.ID, "fanart", 1, true); err == nil {
		t.Error("SetImageLockBySlot must surface a lookup error, not swallow it")
	}
}
