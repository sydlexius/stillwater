package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/database"
	"golang.org/x/crypto/bcrypt"
)

func TestResetPassword(t *testing.T) {
	// Create a temporary database for testing
	tmpFile := fmt.Sprintf("/tmp/stillwater-test-%s.db", uuid.New().String())
	defer os.Remove(tmpFile)

	db, err := database.Open(tmpFile)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()

	// Create a test user
	testUsername := "testuser"
	oldPassword := "oldpass123"
	oldHash, err := bcrypt.GenerateFromPassword(prehashPassword(oldPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}

	userID := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role)
		VALUES (?, ?, ?, 'admin')
	`, userID, testUsername, string(oldHash))
	if err != nil {
		t.Fatalf("creating test user: %v", err)
	}

	// Test password reset with explicit password
	newPassword := "newpass456"
	hash, err := bcrypt.GenerateFromPassword(prehashPassword(newPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hashing new password: %v", err)
	}

	_, err = db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE username = ?", string(hash), testUsername)
	if err != nil {
		t.Fatalf("updating password: %v", err)
	}

	// Verify new password works
	var storedHash string
	err = db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE username = ?", testUsername).Scan(&storedHash)
	if err != nil {
		t.Fatalf("querying user: %v", err)
	}

	err = bcrypt.CompareHashAndPassword([]byte(storedHash), prehashPassword(newPassword))
	if err != nil {
		t.Fatalf("new password verification failed: %v", err)
	}

	// Verify old password no longer works
	err = bcrypt.CompareHashAndPassword([]byte(storedHash), prehashPassword(oldPassword))
	if err == nil {
		t.Fatalf("old password should not work anymore")
	}
}

func TestResetPasswordFirstAdmin(t *testing.T) {
	// Create a temporary database for testing
	tmpFile := fmt.Sprintf("/tmp/stillwater-test-%s.db", uuid.New().String())
	defer os.Remove(tmpFile)

	db, err := database.Open(tmpFile)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()

	// Create a test admin user
	adminUsername := "admin"
	password := "testpass123"
	hash, err := bcrypt.GenerateFromPassword(prehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}

	userID := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role)
		VALUES (?, ?, ?, 'admin')
	`, userID, adminUsername, string(hash))
	if err != nil {
		t.Fatalf("creating test user: %v", err)
	}

	// Create a non-admin user
	regularUser := "regular"
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role)
		VALUES (?, ?, ?, 'viewer')
	`, uuid.New().String(), regularUser, string(hash))
	if err != nil {
		t.Fatalf("creating regular user: %v", err)
	}

	// Query for first admin user (should be the admin)
	var foundUsername string
	err = db.QueryRowContext(ctx, "SELECT username FROM users WHERE role = 'admin' LIMIT 1").Scan(&foundUsername)
	if err != nil {
		t.Fatalf("querying admin user: %v", err)
	}

	if foundUsername != adminUsername {
		t.Fatalf("expected first admin to be %s, got %s", adminUsername, foundUsername)
	}
}

func TestUserNotFound(t *testing.T) {
	tmpFile := fmt.Sprintf("/tmp/stillwater-test-%s.db", uuid.New().String())
	defer os.Remove(tmpFile)

	db, err := database.Open(tmpFile)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()

	// Try to query non-existent user
	var exists int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE username = ?", "nonexistent").Scan(&exists)
	if err != nil {
		t.Fatalf("querying user count: %v", err)
	}

	if exists != 0 {
		t.Fatalf("expected 0 users, got %d", exists)
	}
}
