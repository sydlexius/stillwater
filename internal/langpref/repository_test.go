package langpref

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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
