package webhook

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupTestDB(t *testing.T) *Service {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db)
}

func TestCreate(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	w := &Webhook{
		Name:    "test hook",
		URL:     "https://example.com/hook",
		Type:    TypeGeneric,
		Events:  []string{"scan.completed", "bulk.completed"},
		Enabled: true,
	}
	if err := svc.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	if w.ID == "" {
		t.Error("expected ID to be set")
	}
}

func TestCreate_ValidationErrors(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	if err := svc.Create(ctx, &Webhook{URL: "https://example.com"}); err == nil {
		t.Error("expected error for missing name")
	}
	if err := svc.Create(ctx, &Webhook{Name: "test"}); err == nil {
		t.Error("expected error for missing URL")
	}
}

func TestGetByID(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	w := &Webhook{
		Name:    "get test",
		URL:     "https://example.com/hook",
		Type:    TypeDiscord,
		Events:  []string{"artist.new"},
		Enabled: true,
	}
	if err := svc.Create(ctx, w); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByID(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "get test" {
		t.Errorf("Name = %q, want %q", got.Name, "get test")
	}
	if got.Type != TypeDiscord {
		t.Errorf("Type = %q, want %q", got.Type, TypeDiscord)
	}
	if len(got.Events) != 1 || got.Events[0] != "artist.new" {
		t.Errorf("Events = %v, want [artist.new]", got.Events)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	_, err := svc.GetByID(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent webhook")
	}
}

func TestList(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	for _, name := range []string{"bravo", "alpha", "charlie"} {
		w := &Webhook{Name: name, URL: "https://example.com/" + name, Events: []string{}}
		if err := svc.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d webhooks, want 3", len(list))
	}
	// Should be ordered by name
	if list[0].Name != "alpha" {
		t.Errorf("first webhook = %q, want alpha", list[0].Name)
	}
}

func TestListByEvent(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	w1 := &Webhook{Name: "scan hook", URL: "https://example.com/1", Events: []string{"scan.completed"}, Enabled: true}
	w2 := &Webhook{Name: "bulk hook", URL: "https://example.com/2", Events: []string{"bulk.completed"}, Enabled: true}
	w3 := &Webhook{Name: "disabled", URL: "https://example.com/3", Events: []string{"scan.completed"}, Enabled: false}
	for _, w := range []*Webhook{w1, w2, w3} {
		if err := svc.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	matched, err := svc.ListByEvent(ctx, "scan.completed")
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 {
		t.Fatalf("got %d matched, want 1 (disabled excluded)", len(matched))
	}
	if matched[0].Name != "scan hook" {
		t.Errorf("matched webhook = %q, want scan hook", matched[0].Name)
	}
}

func TestUpdate(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	w := &Webhook{Name: "original", URL: "https://example.com/1", Events: []string{}, Enabled: true}
	if err := svc.Create(ctx, w); err != nil {
		t.Fatal(err)
	}

	w.Name = "updated"
	w.Enabled = false
	if err := svc.Update(ctx, w); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByID(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "updated" {
		t.Errorf("Name = %q, want updated", got.Name)
	}
	if got.Enabled {
		t.Error("expected Enabled to be false")
	}
}

func TestDelete(t *testing.T) {
	svc := setupTestDB(t)
	ctx := context.Background()

	w := &Webhook{Name: "deleteme", URL: "https://example.com/del", Events: []string{}}
	if err := svc.Create(ctx, w); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}

	_, err := svc.GetByID(ctx, w.ID)
	if err == nil {
		t.Error("expected error after deletion")
	}
}

func TestDelete_NotFound(t *testing.T) {
	svc := setupTestDB(t)
	if err := svc.Delete(context.Background(), "nope"); err == nil {
		t.Error("expected error for nonexistent webhook")
	}
}
