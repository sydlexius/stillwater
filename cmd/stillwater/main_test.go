package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/config"
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

func TestBuildTLSStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		cfg              *config.Config
		wantMode         string
		wantHTTPPort     int
		wantHTTPSPort    int
		wantRedirectPort int
		wantAcmeDomain   string
	}{
		{
			name: "off when no TLS configured",
			cfg: &config.Config{
				Server: config.ServerConfig{Port: 1973},
			},
			wantMode:     "off",
			wantHTTPPort: 1973,
		},
		{
			name: "byo collapse: TLS.Port unset reuses Server.Port",
			cfg: &config.Config{
				Server: config.ServerConfig{
					Port: 1973,
					TLS:  config.TLSConfig{CertFile: "/c", KeyFile: "/k"},
				},
			},
			wantMode:      "byo",
			wantHTTPSPort: 1973,
		},
		{
			name: "byo split: TLS.Port wins over Server.Port",
			cfg: &config.Config{
				Server: config.ServerConfig{
					Port: 80,
					TLS:  config.TLSConfig{CertFile: "/c", KeyFile: "/k", Port: 443},
				},
			},
			wantMode:      "byo",
			wantHTTPSPort: 443,
		},
		{
			name: "byo with redirect port forwards through",
			cfg: &config.Config{
				Server: config.ServerConfig{
					Port:         80,
					TLS:          config.TLSConfig{CertFile: "/c", KeyFile: "/k", Port: 443},
					HTTPRedirect: config.HTTPRedirectConfig{Port: 80},
				},
			},
			wantMode:         "byo",
			wantHTTPSPort:    443,
			wantRedirectPort: 80,
		},
		{
			name: "ACME.Domain alone reports off, not acme: autocert listener is not wired yet",
			cfg: &config.Config{
				Server: config.ServerConfig{Port: 1973},
				ACME:   config.ACMEConfig{Domain: "example.com"},
			},
			wantMode:     "off",
			wantHTTPPort: 1973,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTLSStatus(tc.cfg)
			if got.Mode != tc.wantMode {
				t.Errorf("Mode = %q; want %q", got.Mode, tc.wantMode)
			}
			if got.HTTPPort != tc.wantHTTPPort {
				t.Errorf("HTTPPort = %d; want %d", got.HTTPPort, tc.wantHTTPPort)
			}
			if got.HTTPSPort != tc.wantHTTPSPort {
				t.Errorf("HTTPSPort = %d; want %d", got.HTTPSPort, tc.wantHTTPSPort)
			}
			if got.HTTPRedirectPort != tc.wantRedirectPort {
				t.Errorf("HTTPRedirectPort = %d; want %d", got.HTTPRedirectPort, tc.wantRedirectPort)
			}
			if got.AcmeDomain != tc.wantAcmeDomain {
				t.Errorf("AcmeDomain = %q; want %q", got.AcmeDomain, tc.wantAcmeDomain)
			}
		})
	}
}
