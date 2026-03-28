package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

func TestLocalProviderAuthenticate(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := NewService(db)
	ctx := context.Background()

	// Create a test user via Setup (first user is always admin).
	if _, err := svc.Setup(ctx, "testadmin", "password123"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	provider := NewLocalProvider(db)

	// Valid credentials.
	identity, err := provider.Authenticate(ctx, Credentials{Username: "testadmin", Password: "password123"})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.DisplayName != "testadmin" {
		t.Errorf("DisplayName = %q, want %q", identity.DisplayName, "testadmin")
	}
	if identity.ProviderType != "local" {
		t.Errorf("ProviderType = %q, want %q", identity.ProviderType, "local")
	}

	// Invalid password.
	_, err = provider.Authenticate(ctx, Credentials{Username: "testadmin", Password: "wrong"})
	if err == nil {
		t.Fatal("expected error for invalid password")
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("invalid password: expected ErrInvalidCredentials, got: %v", err)
	}

	// Nonexistent user.
	_, err = provider.Authenticate(ctx, Credentials{Username: "nobody", Password: "password123"})
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("nonexistent user: expected ErrInvalidCredentials, got: %v", err)
	}
}

func TestLocalProviderType(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	provider := NewLocalProvider(db)
	if provider.Type() != "local" {
		t.Errorf("Type() = %q, want %q", provider.Type(), "local")
	}
}

func TestLocalProviderAutoProvision(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	provider := NewLocalProvider(db)
	if provider.CanAutoProvision(nil) {
		t.Error("local provider should never auto-provision")
	}
}

func TestNewLocalProvider_NilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil db")
		}
	}()
	NewLocalProvider(nil)
}
