package connection

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// setupFKService builds a Service on the runtime open path (OpenRuntime), so
// foreign keys are enforced exactly as in production. The shared
// setupTestService helper uses database.Open, which leaves enforcement off and
// would let a bare DELETE pass vacuously. A file DB is used because the
// FK-off migration handle and the FK-on runtime handle are two separate
// sql.DB handles, and an in-memory schema would not survive between them.
func setupFKService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fk.db")

	mig, err := database.Open(path)
	if err != nil {
		t.Fatalf("opening migration db: %v", err)
	}
	if err := database.Migrate(mig); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	if err := mig.Close(); err != nil {
		t.Fatalf("closing migration db: %v", err)
	}

	db, err := database.OpenRuntime(path)
	if err != nil {
		t.Fatalf("opening runtime db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, err %v; want 1", fk, err)
	}

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	return NewService(db, enc), db
}

// oldStamp is a fixed past updated_at so a bump is visible without sleeping.
const oldStamp = "2000-01-01T00:00:00Z"

func seedConn(t *testing.T, svc *Service, name, url string) string {
	t.Helper()
	c := &Connection{Name: name, Type: TypeEmby, URL: url, APIKey: "key", Enabled: true}
	if err := svc.Create(context.Background(), c); err != nil {
		t.Fatalf("creating connection %s: %v", name, err)
	}
	return c.ID
}

func seedLib(t *testing.T, db *sql.DB, id, connID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO libraries (id, name, connection_id, updated_at) VALUES (?, ?, ?, ?)`, id, id, connID, oldStamp); err != nil {
		t.Fatalf("inserting library %s: %v", id, err)
	}
}

func libConn(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(`SELECT connection_id FROM libraries WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("reading library %s (must still exist): %v", id, err)
	}
	return v
}

func libUpdatedAt(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT updated_at FROM libraries WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("reading updated_at for %s: %v", id, err)
	}
	return v
}

func TestDelete_DetachesReferencingLibraries(t *testing.T) {
	t.Parallel()
	svc, db := setupFKService(t)
	ctx := context.Background()

	target := seedConn(t, svc, "target", "http://a:8096")
	other := seedConn(t, svc, "other", "http://b:8096")
	seedLib(t, db, "lib-1", target)
	seedLib(t, db, "lib-2", target)
	seedLib(t, db, "lib-other", other)

	// Preconditions: the references exist before the delete.
	if got := libConn(t, db, "lib-1"); got.String != target {
		t.Fatalf("precondition: lib-1 connection_id = %v, want %s", got, target)
	}
	if got := libConn(t, db, "lib-2"); got.String != target {
		t.Fatalf("precondition: lib-2 connection_id = %v, want %s", got, target)
	}

	if err := svc.Delete(ctx, target); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM connections WHERE id = ?`, target).Scan(&n); err != nil || n != 0 {
		t.Errorf("connection row count = %d, err %v; want 0", n, err)
	}
	for _, id := range []string{"lib-1", "lib-2"} {
		if got := libConn(t, db, id); got.Valid {
			t.Errorf("%s connection_id = %v, want NULL", id, got)
		}
		if got := libUpdatedAt(t, db, id); got == oldStamp {
			t.Errorf("%s updated_at was not bumped", id)
		}
	}
	if got := libConn(t, db, "lib-other"); got.String != other {
		t.Errorf("unrelated library connection_id = %v, want %s", got, other)
	}
	if got := libUpdatedAt(t, db, "lib-other"); got != oldStamp {
		t.Errorf("unrelated library updated_at = %s, want unchanged", got)
	}
}

func TestDelete_NotFoundLeavesLibrariesAlone(t *testing.T) {
	t.Parallel()
	svc, db := setupFKService(t)

	keep := seedConn(t, svc, "keep", "http://d:8096")
	seedLib(t, db, "lib-keep", keep)

	if err := svc.Delete(context.Background(), "nonexistent"); err == nil {
		t.Fatal("expected not-found error")
	}
	if got := libConn(t, db, "lib-keep"); got.String != keep {
		t.Errorf("library connection_id = %v, want %s", got, keep)
	}
}

// Atomicity: the DELETE fails after the detach has run, so the detach must roll
// back. If the UPDATE ran outside the transaction the library would stay NULL.
func TestDelete_RollsBackDetachWhenDeleteFails(t *testing.T) {
	t.Parallel()
	svc, db := setupFKService(t)

	id := seedConn(t, svc, "target", "http://e:8096")
	seedLib(t, db, "lib-a", id)
	if _, err := db.Exec(`CREATE TRIGGER block_conn_delete BEFORE DELETE ON connections
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("installing trigger: %v", err)
	}
	if got := libConn(t, db, "lib-a"); got.String != id {
		t.Fatalf("precondition: library connection_id = %v, want %s", got, id)
	}

	if err := svc.Delete(context.Background(), id); err == nil {
		t.Fatal("expected delete to fail")
	}
	if got := libConn(t, db, "lib-a"); got.String != id {
		t.Errorf("library connection_id = %v after failed delete, want still %s", got, id)
	}
}

// A failing detach must abort the delete and leave the connection in place.
func TestDelete_DetachFailureKeepsConnection(t *testing.T) {
	t.Parallel()
	svc, db := setupFKService(t)

	id := seedConn(t, svc, "target", "http://f:8096")
	seedLib(t, db, "lib-a", id)
	if _, err := db.Exec(`CREATE TRIGGER block_lib_update BEFORE UPDATE ON libraries
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("installing trigger: %v", err)
	}

	if err := svc.Delete(context.Background(), id); err == nil {
		t.Fatal("expected detach failure")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM connections WHERE id = ?`, id).Scan(&n); err != nil || n != 1 {
		t.Errorf("connection row count = %d, err %v; want 1", n, err)
	}
}

// A context canceled before the call fails at BeginTx, touching nothing.
func TestDelete_CanceledContext(t *testing.T) {
	t.Parallel()
	svc, db := setupFKService(t)

	id := seedConn(t, svc, "target", "http://g:8096")
	seedLib(t, db, "lib-a", id)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := svc.Delete(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "beginning connection delete") {
		t.Fatalf("err = %v, want beginning connection delete failure", err)
	}
	if got := libConn(t, db, "lib-a"); got.String != id {
		t.Errorf("library connection_id = %v, want still %s", got, id)
	}
}
