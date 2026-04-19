package langpref

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setupTestDB builds an in-memory SQLite database with the single
// user_preferences table the Repository depends on. The schema mirrors
// the real migration so behavior matches production.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// SQLite drivers default to a connection pool that can interleave
	// writes; pin to one connection to match the production settings.
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS user_preferences (
			user_id    TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (user_id, key)
		)
	`)
	if err != nil {
		t.Fatalf("creating user_preferences table: %v", err)
	}
	return db
}

func TestRepository_Get_DefaultWhenMissing(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	got, err := repo.Get(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{"en"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %v, want %v", got, want)
	}
}

func TestRepository_Get_EmptyUserIDReturnsDefault(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	got, err := repo.Get(context.Background(), "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{"en"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %v, want %v", got, want)
	}
}

func TestRepository_RoundTrip_PreservesOrder(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	ctx := context.Background()
	userID := "user-1"

	input := []string{"ja", "en-GB", "fr", "zh-Hant-TW"}
	if err := repo.Set(ctx, userID, input); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := repo.Get(ctx, userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, input) {
		t.Errorf("Get after Set = %v, want %v (order must be preserved)", got, input)
	}
}

func TestRepository_Set_Canonicalizes(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	ctx := context.Background()
	userID := "user-1"

	if err := repo.Set(ctx, userID, []string{"EN-gb", "ZH-hant-tw"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := repo.Get(ctx, userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{"en-GB", "zh-Hant-TW"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %v, want canonicalized %v", got, want)
	}
}

func TestRepository_Set_Overwrites(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	ctx := context.Background()
	userID := "user-1"

	if err := repo.Set(ctx, userID, []string{"en"}); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	if err := repo.Set(ctx, userID, []string{"ja", "en"}); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	got, err := repo.Get(ctx, userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{"ja", "en"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get after overwrite = %v, want %v", got, want)
	}
}

func TestRepository_Set_RejectsDuplicates(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	err := repo.Set(context.Background(), "user-1", []string{"en", "EN"})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Set with duplicates: got err %v, want ErrInvalid", err)
	}
}

func TestRepository_Set_RejectsEmpty(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	err := repo.Set(context.Background(), "user-1", nil)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Set with empty slice: got err %v, want ErrInvalid", err)
	}
}

func TestRepository_Set_RejectsInvalidTag(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	err := repo.Set(context.Background(), "user-1", []string{"en@GB"})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Set with invalid tag: got err %v, want ErrInvalid", err)
	}
}

func TestRepository_Set_RejectsEmptyUserID(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	err := repo.Set(context.Background(), "", []string{"en"})
	if err == nil {
		t.Error("Set with empty user id: expected error, got nil")
	}
}

func TestRepository_Get_MalformedRowReturnsDefault(t *testing.T) {
	db := setupTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	// Write a malformed row directly, bypassing validation. The read
	// path must still return a sane default rather than an error.
	_, err := db.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES (?, ?, ?)`,
		"user-1", PreferenceKey, "this is not json")
	if err != nil {
		t.Fatalf("seeding malformed row: %v", err)
	}

	got, err := repo.Get(ctx, "user-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{"en"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get with malformed row = %v, want default %v", got, want)
	}
}

func TestRepository_GetRaw_DefaultWhenMissing(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	got, err := repo.GetRaw(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if got != DefaultJSON {
		t.Errorf("GetRaw = %q, want %q", got, DefaultJSON)
	}
}

func TestRepository_GetRaw_NormalizesStored(t *testing.T) {
	db := setupTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	// Insert a row with non-canonical casing directly. GetRaw must
	// return the canonical form.
	_, err := db.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES (?, ?, ?)`,
		"user-1", PreferenceKey, `["EN-gb","JA"]`)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	got, err := repo.GetRaw(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	want := `["en-GB","ja"]`
	if got != want {
		t.Errorf("GetRaw = %q, want %q", got, want)
	}
}

// setupTestDBWithUsers extends setupTestDB with a users table matching the
// production shape needed by EffectiveForBackground. Kept in-file so the
// existing setupTestDB stays a minimal fixture for the rest of the suite.
func setupTestDBWithUsers(t *testing.T) *sql.DB {
	t.Helper()
	db := setupTestDB(t)
	_, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			username      TEXT NOT NULL UNIQUE,
			role          TEXT NOT NULL DEFAULT 'operator',
			is_active     INTEGER NOT NULL DEFAULT 1,
			is_protected  INTEGER NOT NULL DEFAULT 0,
			created_at    TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`)
	if err != nil {
		t.Fatalf("creating users table: %v", err)
	}
	return db
}

func insertUser(t *testing.T, db *sql.DB, id, username, role string, active, protected int, createdAt string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, role, is_active, is_protected, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, username, role, active, protected, createdAt)
	if err != nil {
		t.Fatalf("inserting user %s: %v", username, err)
	}
}

func TestRepository_EffectiveForBackground_NoUsers(t *testing.T) {
	repo := NewRepository(setupTestDBWithUsers(t))
	got := repo.EffectiveForBackground(context.Background())
	if !reflect.DeepEqual(got, DefaultTags()) {
		t.Errorf("EffectiveForBackground with no users = %v, want %v", got, DefaultTags())
	}
}

func TestRepository_EffectiveForBackground_PicksProtectedAdmin(t *testing.T) {
	db := setupTestDBWithUsers(t)
	repo := NewRepository(db)
	ctx := context.Background()

	// Non-protected admin created first, protected admin created later.
	// Protected wins regardless of created_at order.
	insertUser(t, db, "u-first", "earlyadmin", "administrator", 1, 0, "2026-01-01T00:00:00Z")
	insertUser(t, db, "u-protected", "bootstrap", "administrator", 1, 1, "2026-03-01T00:00:00Z")

	if err := repo.Set(ctx, "u-first", []string{"fr"}); err != nil {
		t.Fatalf("Set u-first: %v", err)
	}
	if err := repo.Set(ctx, "u-protected", []string{"en-US", "en-GB", "en"}); err != nil {
		t.Fatalf("Set u-protected: %v", err)
	}

	got := repo.EffectiveForBackground(ctx)
	want := []string{"en-US", "en-GB", "en"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveForBackground = %v, want protected admin's prefs %v", got, want)
	}
}

func TestRepository_EffectiveForBackground_ProtectedWinsAtSameAge(t *testing.T) {
	db := setupTestDBWithUsers(t)
	repo := NewRepository(db)
	ctx := context.Background()

	// Same created_at: is_protected must break the tie in favor of the
	// protected admin. This is the "protected breaks ties" case for
	// deployments where multiple admins were created in the same tick.
	insertUser(t, db, "u-plain", "alice", "administrator", 1, 0, "2026-01-01T00:00:00Z")
	insertUser(t, db, "u-protected", "bootstrap", "administrator", 1, 1, "2026-01-01T00:00:00Z")

	if err := repo.Set(ctx, "u-plain", []string{"fr"}); err != nil {
		t.Fatalf("Set u-plain: %v", err)
	}
	if err := repo.Set(ctx, "u-protected", []string{"ja"}); err != nil {
		t.Fatalf("Set u-protected: %v", err)
	}

	got := repo.EffectiveForBackground(ctx)
	want := []string{"ja"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveForBackground with tied created_at = %v, want protected admin's prefs %v", got, want)
	}
}

func TestRepository_EffectiveForBackground_OldestAdminWhenNoneProtected(t *testing.T) {
	db := setupTestDBWithUsers(t)
	repo := NewRepository(db)
	ctx := context.Background()

	insertUser(t, db, "u-old", "alice", "administrator", 1, 0, "2026-01-15T00:00:00Z")
	insertUser(t, db, "u-new", "bob", "administrator", 1, 0, "2026-02-15T00:00:00Z")

	if err := repo.Set(ctx, "u-old", []string{"ja"}); err != nil {
		t.Fatalf("Set u-old: %v", err)
	}

	got := repo.EffectiveForBackground(ctx)
	want := []string{"ja"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveForBackground = %v, want oldest admin's prefs %v", got, want)
	}
}

func TestRepository_EffectiveForBackground_SkipsInactiveAdmins(t *testing.T) {
	db := setupTestDBWithUsers(t)
	repo := NewRepository(db)
	ctx := context.Background()

	insertUser(t, db, "u-deactivated", "retired", "administrator", 0, 0, "2026-01-01T00:00:00Z")
	insertUser(t, db, "u-active", "current", "administrator", 1, 0, "2026-02-01T00:00:00Z")

	if err := repo.Set(ctx, "u-deactivated", []string{"fr"}); err != nil {
		t.Fatalf("Set u-deactivated: %v", err)
	}
	if err := repo.Set(ctx, "u-active", []string{"de"}); err != nil {
		t.Fatalf("Set u-active: %v", err)
	}

	got := repo.EffectiveForBackground(ctx)
	want := []string{"de"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveForBackground = %v, want active admin's prefs %v", got, want)
	}
}

func TestRepository_EffectiveForBackground_SkipsOperators(t *testing.T) {
	db := setupTestDBWithUsers(t)
	repo := NewRepository(db)
	ctx := context.Background()

	insertUser(t, db, "u-op", "operator", "operator", 1, 0, "2026-01-01T00:00:00Z")
	if err := repo.Set(ctx, "u-op", []string{"zh"}); err != nil {
		t.Fatalf("Set u-op: %v", err)
	}

	got := repo.EffectiveForBackground(ctx)
	if !reflect.DeepEqual(got, DefaultTags()) {
		t.Errorf("EffectiveForBackground = %v, want default %v (operators are not admins)", got, DefaultTags())
	}
}

func TestRepository_EffectiveForBackground_AdminWithoutStoredPrefs_ReturnsDefault(t *testing.T) {
	db := setupTestDBWithUsers(t)
	repo := NewRepository(db)

	insertUser(t, db, "u-admin", "admin", "administrator", 1, 1, "2026-01-01T00:00:00Z")

	got := repo.EffectiveForBackground(context.Background())
	if !reflect.DeepEqual(got, DefaultTags()) {
		t.Errorf("EffectiveForBackground = %v, want %v (admin has no stored prefs)", got, DefaultTags())
	}
}

func TestRepository_Delete_RemovesRow(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	ctx := context.Background()
	userID := "user-1"

	if err := repo.Set(ctx, userID, []string{"ja", "en"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := repo.Delete(ctx, userID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := repo.Get(ctx, userID)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if !reflect.DeepEqual(got, DefaultTags()) {
		t.Errorf("Get after Delete = %v, want %v (default)", got, DefaultTags())
	}
}

func TestRepository_Delete_NoRowIsNotAnError(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	if err := repo.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("Delete on missing row: got err %v, want nil (SQL DELETE is a no-op on zero matches)", err)
	}
}

func TestRepository_Delete_EmptyUserIDRejected(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	if err := repo.Delete(context.Background(), ""); err == nil {
		t.Error("Delete with empty user id: expected error, got nil")
	}
}

// TestRepository_Delete_WrapsDBError verifies the error path in Delete:
// the DB Exec failure is wrapped with "langpref: deleting preference for
// user ..." so callers can identify the operation in logs without
// unwrapping. Simulated by closing the underlying DB before the call.
func TestRepository_Delete_WrapsDBError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewRepository(db)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}
	err := repo.Delete(context.Background(), "user-1")
	if err == nil {
		t.Fatal("Delete against closed db: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "deleting preference for user user-1") {
		t.Errorf("Delete error = %q, want it to mention the user id and operation", err.Error())
	}
}

func TestRepository_Delete_IsolatedFromOtherUsers(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	ctx := context.Background()

	if err := repo.Set(ctx, "alice", []string{"fr"}); err != nil {
		t.Fatalf("Set alice: %v", err)
	}
	if err := repo.Set(ctx, "bob", []string{"de"}); err != nil {
		t.Fatalf("Set bob: %v", err)
	}
	if err := repo.Delete(ctx, "alice"); err != nil {
		t.Fatalf("Delete alice: %v", err)
	}

	// Bob's row must be untouched.
	bob, err := repo.Get(ctx, "bob")
	if err != nil {
		t.Fatalf("Get bob: %v", err)
	}
	if !reflect.DeepEqual(bob, []string{"de"}) {
		t.Errorf("bob after deleting alice = %v, want [de] (delete must not be cross-user)", bob)
	}
}

func TestRepository_MultipleUsersIsolated(t *testing.T) {
	repo := NewRepository(setupTestDB(t))
	ctx := context.Background()

	if err := repo.Set(ctx, "alice", []string{"en", "fr"}); err != nil {
		t.Fatalf("Set alice: %v", err)
	}
	if err := repo.Set(ctx, "bob", []string{"ja"}); err != nil {
		t.Fatalf("Set bob: %v", err)
	}

	alice, err := repo.Get(ctx, "alice")
	if err != nil {
		t.Fatalf("Get alice: %v", err)
	}
	bob, err := repo.Get(ctx, "bob")
	if err != nil {
		t.Fatalf("Get bob: %v", err)
	}
	if !reflect.DeepEqual(alice, []string{"en", "fr"}) {
		t.Errorf("alice = %v", alice)
	}
	if !reflect.DeepEqual(bob, []string{"ja"}) {
		t.Errorf("bob = %v", bob)
	}
}
