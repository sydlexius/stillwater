package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// openTestDB opens an in-process SQLite database in t.TempDir and runs migrations.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	return db
}

// insertUser creates a user row in the test database.
func insertUser(t *testing.T, ctx context.Context, db *sql.DB, username, password, role string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword(auth.PrehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hashing password for %s: %v", username, err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role)
		VALUES (?, ?, ?, ?)
	`, "test-id-"+username, username, string(hash), role)
	if err != nil {
		t.Fatalf("inserting user %s: %v", username, err)
	}
}

// assertPassword verifies that password matches the stored hash for username.
func assertPassword(t *testing.T, ctx context.Context, db *sql.DB, username, password string) {
	t.Helper()
	var storedHash string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE username = ?", username).Scan(&storedHash); err != nil {
		t.Fatalf("querying hash for %s: %v", username, err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(storedHash), auth.PrehashPassword(password)); err != nil {
		t.Fatalf("password mismatch for %s: %v", username, err)
	}
}

// assertPasswordWrong verifies that password does NOT match the stored hash.
func assertPasswordWrong(t *testing.T, ctx context.Context, db *sql.DB, username, password string) {
	t.Helper()
	var storedHash string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE username = ?", username).Scan(&storedHash); err != nil {
		t.Fatalf("querying hash for %s: %v", username, err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(storedHash), auth.PrehashPassword(password)); err == nil {
		t.Fatalf("expected password mismatch for %s but it matched", username)
	}
}

func TestResetPasswordWithExplicitUser(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertUser(t, ctx, db, "alice", "oldpass", "admin")

	if err := resetPasswordDB(ctx, db, "alice", "newpass"); err != nil {
		t.Fatalf("resetPasswordDB: %v", err)
	}
	assertPassword(t, ctx, db, "alice", "newpass")
	assertPasswordWrong(t, ctx, db, "alice", "oldpass")
}

func TestResetPasswordDefaultsToFirstAdmin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertUser(t, ctx, db, "admin", "oldpass", "admin")

	if err := resetPasswordDB(ctx, db, "", "newpass"); err != nil {
		t.Fatalf("resetPasswordDB: %v", err)
	}
	assertPassword(t, ctx, db, "admin", "newpass")
}

func TestResetPasswordUserNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	err := resetPasswordDB(ctx, db, "ghost", "pass")
	if err == nil {
		t.Fatal("expected error for missing user, got nil")
	}
}

func TestResetPasswordNoAdminUsers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	err := resetPasswordDB(ctx, db, "", "pass")
	if err == nil {
		t.Fatal("expected error when no admin users exist, got nil")
	}
}

func TestResetPasswordNonAdminUser(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertUser(t, ctx, db, "viewer", "oldpass", "viewer")

	if err := resetPasswordDB(ctx, db, "viewer", "newpass"); err != nil {
		t.Fatalf("resetPasswordDB: %v", err)
	}
	assertPassword(t, ctx, db, "viewer", "newpass")
	assertPasswordWrong(t, ctx, db, "viewer", "oldpass")
}
