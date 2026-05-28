package provider

// import_test.go exercises the tx-aware import helpers added for #1693.

import (
	"context"
	"testing"
)

// TestImportSetAPIKeyTx_RoundTrip writes a key via the tx-aware import
// helper using the same s.db handle the service was constructed with, then
// reads it back through the public GetAPIKey path to confirm it was stored
// and decrypts cleanly.
func TestImportSetAPIKeyTx_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	if err := svc.ImportSetAPIKeyTx(ctx, db, NameFanartTV, "import-tx-key"); err != nil {
		t.Fatalf("ImportSetAPIKeyTx: %v", err)
	}
	got, err := svc.GetAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got != "import-tx-key" {
		t.Errorf("GetAPIKey: got %q, want import-tx-key", got)
	}
}

// TestImportSetPriorityTx_RoundTrip writes a priority via the tx-aware
// import helper and verifies it round-trips through the standard
// GetPriorities reader.
func TestImportSetPriorityTx_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	want := []ProviderName{NameMusicBrainz, NameWikipedia}
	if err := svc.ImportSetPriorityTx(ctx, db, "biography", want); err != nil {
		t.Fatalf("ImportSetPriorityTx: %v", err)
	}
	fps, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	for _, fp := range fps {
		if fp.Field != "biography" {
			continue
		}
		if len(fp.Providers) < 2 || fp.Providers[0] != NameMusicBrainz || fp.Providers[1] != NameWikipedia {
			t.Errorf("Providers prefix: got %v, want %v first", fp.Providers, want)
		}
		return
	}
	t.Fatal("biography priorities not found after import")
}

// TestImportSetAPIKeyTx_DBError verifies that an executor failure
// propagates as an error rather than being silently dropped.
func TestImportSetAPIKeyTx_DBError(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	_ = db.Close()
	err := svc.ImportSetAPIKeyTx(context.Background(), db, NameFanartTV, "k")
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportSetPriorityTx_DBError pins the same error-propagation contract
// for the priority writer.
func TestImportSetPriorityTx_DBError(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	_ = db.Close()
	err := svc.ImportSetPriorityTx(context.Background(), db, "biography", []ProviderName{NameMusicBrainz})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportSetDisabledProvidersTx_DBError covers both the clear-empty and
// the write-non-empty paths.
func TestImportSetDisabledProvidersTx_DBError(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	_ = db.Close()
	// empty list goes through the DELETE branch
	if err := svc.ImportSetDisabledProvidersTx(context.Background(), db, "biography", nil); err == nil {
		t.Error("expected error on empty list with closed DB")
	}
	// non-empty list goes through the INSERT branch
	if err := svc.ImportSetDisabledProvidersTx(context.Background(), db, "biography",
		[]ProviderName{NameWikipedia}); err == nil {
		t.Error("expected error on non-empty list with closed DB")
	}
}

// TestImportSetDisabledProvidersTx_RoundTripAndClear verifies both branches
// of the import helper: writing a non-empty list and clearing via an empty list.
func TestImportSetDisabledProvidersTx_RoundTripAndClear(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	disabled := []ProviderName{NameWikipedia}
	if err := svc.ImportSetDisabledProvidersTx(ctx, db, "biography", disabled); err != nil {
		t.Fatalf("ImportSetDisabledProvidersTx (set): %v", err)
	}
	// Also seed a priority so GetPriorities returns the row.
	if err := svc.ImportSetPriorityTx(ctx, db, "biography", []ProviderName{NameMusicBrainz}); err != nil {
		t.Fatalf("seeding priority: %v", err)
	}
	fps, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	foundDisabled := false
	for _, fp := range fps {
		if fp.Field == "biography" && len(fp.Disabled) == 1 && fp.Disabled[0] == NameWikipedia {
			foundDisabled = true
			break
		}
	}
	if !foundDisabled {
		t.Errorf("disabled list not persisted; got %v", fps)
	}

	// Empty input clears the row.
	if err := svc.ImportSetDisabledProvidersTx(ctx, db, "biography", []ProviderName{}); err != nil {
		t.Fatalf("ImportSetDisabledProvidersTx (clear): %v", err)
	}
	fps, err = svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities post-clear: %v", err)
	}
	for _, fp := range fps {
		if fp.Field == "biography" && len(fp.Disabled) > 0 {
			t.Errorf("disabled list not cleared; got %v", fp.Disabled)
		}
	}
}
