package nfo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE nfo_snapshots (
			id TEXT PRIMARY KEY,
			artist_id TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX idx_nfo_snapshots_artist_id ON nfo_snapshots(artist_id);
	`)
	if err != nil {
		t.Fatalf("creating table: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSnapshotService_SaveAndList(t *testing.T) {
	db := setupTestDB(t)
	svc := NewSnapshotService(db)
	ctx := context.Background()

	snap1, err := svc.Save(ctx, "artist-1", "<artist><name>Old</name></artist>")
	if err != nil {
		t.Fatalf("saving snapshot 1: %v", err)
	}
	if snap1.ID == "" {
		t.Error("expected non-empty snapshot ID")
	}

	snap2, err := svc.Save(ctx, "artist-1", "<artist><name>New</name></artist>")
	if err != nil {
		t.Fatalf("saving snapshot 2: %v", err)
	}

	_ = snap2 // used to verify save succeeded

	snapshots, err := svc.List(ctx, "artist-1")
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}

	// Verify both are present
	ids := map[string]bool{snapshots[0].ID: true, snapshots[1].ID: true}
	if !ids[snap1.ID] {
		t.Error("snap1 not found in list")
	}
}

func TestSnapshotService_GetByID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewSnapshotService(db)
	ctx := context.Background()

	saved, err := svc.Save(ctx, "artist-1", "<artist><name>Test</name></artist>")
	if err != nil {
		t.Fatalf("saving: %v", err)
	}

	got, err := svc.GetByID(ctx, saved.ID)
	if err != nil {
		t.Fatalf("getting by id: %v", err)
	}
	if got.Content != saved.Content {
		t.Errorf("content mismatch: %q != %q", got.Content, saved.Content)
	}
	if got.ArtistID != "artist-1" {
		t.Errorf("artist_id mismatch: %q", got.ArtistID)
	}
}

func TestSnapshotService_GetByID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewSnapshotService(db)
	ctx := context.Background()

	_, err := svc.GetByID(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent snapshot")
	}
}

func TestSnapshotService_ListEmpty(t *testing.T) {
	db := setupTestDB(t)
	svc := NewSnapshotService(db)
	ctx := context.Background()

	snapshots, err := svc.List(ctx, "artist-1")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(snapshots) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(snapshots))
	}
}

func TestSnapshotService_ListIsolation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewSnapshotService(db)
	ctx := context.Background()

	_, _ = svc.Save(ctx, "artist-1", "content-1")
	_, _ = svc.Save(ctx, "artist-2", "content-2")

	list1, _ := svc.List(ctx, "artist-1")
	if len(list1) != 1 {
		t.Errorf("expected 1 snapshot for artist-1, got %d", len(list1))
	}

	list2, _ := svc.List(ctx, "artist-2")
	if len(list2) != 1 {
		t.Errorf("expected 1 snapshot for artist-2, got %d", len(list2))
	}
}
