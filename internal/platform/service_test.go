package platform

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestList_BuiltinProfiles(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	profiles, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(profiles) != 5 {
		t.Fatalf("expected 5 built-in profiles, got %d", len(profiles))
	}

	// Check Kodi is the active one
	var activeCount int
	for _, p := range profiles {
		if p.IsActive {
			activeCount++
			if p.ID != "kodi" {
				t.Errorf("expected Kodi to be active, got %s", p.ID)
			}
		}
		if !p.IsBuiltin {
			t.Errorf("profile %s should be built-in", p.ID)
		}
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active profile, got %d", activeCount)
	}
}

func TestGetByID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	p, err := svc.GetByID(ctx, "emby")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if p.Name != "Emby" {
		t.Errorf("Name = %q, want Emby", p.Name)
	}
	if !p.NFOEnabled {
		t.Error("Emby should have NFO enabled")
	}
	if p.ImageNaming.PrimaryName("thumb") != "folder.jpg" {
		t.Errorf("ImageNaming.Thumb primary = %q, want folder.jpg", p.ImageNaming.PrimaryName("thumb"))
	}
}

func TestGetByID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	_, err := svc.GetByID(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent profile")
	}
}

func TestGetActive(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	p, err := svc.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if p == nil {
		t.Fatal("expected active profile, got nil")
	}
	if p.ID != "kodi" {
		t.Errorf("active profile = %q, want kodi", p.ID)
	}
}

func TestSetActive(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SetActive(ctx, "emby"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	p, err := svc.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if p.ID != "emby" {
		t.Errorf("active profile = %q, want emby", p.ID)
	}

	// Verify Kodi is no longer active
	kodi, err := svc.GetByID(ctx, "kodi")
	if err != nil {
		t.Fatalf("GetByID kodi: %v", err)
	}
	if kodi.IsActive {
		t.Error("Kodi should no longer be active")
	}
}

func TestSetActive_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.SetActive(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent profile")
	}
}

func TestCreate_CustomProfile(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	p := &Profile{
		Name:       "My Custom",
		NFOEnabled: true,
		NFOFormat:  "kodi",
		ImageNaming: ImageNaming{
			Thumb:  []string{"cover.jpg"},
			Fanart: []string{"background.jpg"},
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
	}
	if err := svc.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID == "" {
		t.Error("expected ID to be set")
	}

	got, err := svc.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "My Custom" {
		t.Errorf("Name = %q, want My Custom", got.Name)
	}
	if got.IsBuiltin {
		t.Error("custom profile should not be built-in")
	}
	if got.ImageNaming.PrimaryName("thumb") != "cover.jpg" {
		t.Errorf("ImageNaming.Thumb primary = %q, want cover.jpg", got.ImageNaming.PrimaryName("thumb"))
	}
}

func TestUpdate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	p, err := svc.GetByID(ctx, "custom")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	p.NFOEnabled = false
	p.ImageNaming = ImageNaming{
		Thumb:  []string{"artist.jpg"},
		Fanart: []string{"fanart.jpg"},
		Logo:   []string{"logo.png"},
		Banner: []string{"banner.jpg"},
	}
	if err := svc.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := svc.GetByID(ctx, "custom")
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.NFOEnabled {
		t.Error("NFOEnabled should be false after update")
	}
	if got.ImageNaming.PrimaryName("thumb") != "artist.jpg" {
		t.Errorf("ImageNaming.Thumb primary = %q, want artist.jpg", got.ImageNaming.PrimaryName("thumb"))
	}
}

func TestDelete_CustomProfile(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	p := &Profile{Name: "Deletable", NFOEnabled: true, NFOFormat: "kodi"}
	if err := svc.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := svc.GetByID(ctx, p.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestDelete_BuiltinProfile_Fails(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.Delete(context.Background(), "kodi")
	if err == nil {
		t.Error("expected error when deleting built-in profile")
	}
}

func TestImageNamingMarshal(t *testing.T) {
	n := ImageNaming{
		Thumb:  []string{"folder.jpg", "artist.jpg"},
		Fanart: []string{"fanart.jpg"},
		Logo:   []string{"logo.png"},
		Banner: []string{"banner.jpg"},
	}

	data := MarshalImageNaming(n)
	got := UnmarshalImageNaming(data)

	if len(got.Thumb) != 2 || got.Thumb[0] != "folder.jpg" || got.Thumb[1] != "artist.jpg" {
		t.Errorf("Thumb = %v, want [folder.jpg artist.jpg]", got.Thumb)
	}
	if len(got.Fanart) != 1 || got.Fanart[0] != "fanart.jpg" {
		t.Errorf("Fanart = %v, want [fanart.jpg]", got.Fanart)
	}
}

func TestImageNamingUnmarshal_LegacyFormat(t *testing.T) {
	// Legacy format: single strings per type
	legacy := `{"thumb":"folder.jpg","fanart":"fanart.jpg","logo":"logo.png","banner":"banner.jpg"}`
	got := UnmarshalImageNaming(legacy)

	if len(got.Thumb) != 1 || got.Thumb[0] != "folder.jpg" {
		t.Errorf("Thumb = %v, want [folder.jpg]", got.Thumb)
	}
	if len(got.Logo) != 1 || got.Logo[0] != "logo.png" {
		t.Errorf("Logo = %v, want [logo.png]", got.Logo)
	}
}

func TestImageNaming_PrimaryName(t *testing.T) {
	n := ImageNaming{
		Thumb:  []string{"folder.jpg", "artist.jpg"},
		Fanart: []string{"fanart.jpg"},
	}

	if got := n.PrimaryName("thumb"); got != "folder.jpg" {
		t.Errorf("PrimaryName(thumb) = %q, want folder.jpg", got)
	}
	if got := n.PrimaryName("fanart"); got != "fanart.jpg" {
		t.Errorf("PrimaryName(fanart) = %q, want fanart.jpg", got)
	}
	if got := n.PrimaryName("logo"); got != "" {
		t.Errorf("PrimaryName(logo) = %q, want empty", got)
	}
}

func TestImageNaming_ToMap(t *testing.T) {
	n := ImageNaming{
		Thumb:  []string{"folder.jpg"},
		Fanart: []string{"fanart.jpg"},
		Logo:   []string{"logo.png"},
		Banner: []string{"banner.jpg"},
	}

	m := n.ToMap()
	if len(m) != 4 {
		t.Errorf("expected 4 entries, got %d", len(m))
	}
	if m["thumb"][0] != "folder.jpg" {
		t.Errorf("thumb[0] = %q, want folder.jpg", m["thumb"][0])
	}
}
