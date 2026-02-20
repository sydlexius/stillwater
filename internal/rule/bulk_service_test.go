package rule

import (
	"context"
	"testing"
)

func TestBulkService_CreateJob(t *testing.T) {
	db := setupTestDB(t)
	svc := NewBulkService(db)
	ctx := context.Background()

	job, err := svc.CreateJob(ctx, BulkTypeFetchMetadata, BulkModeYOLO, 100)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.ID == "" {
		t.Error("job ID should not be empty")
	}
	if job.Type != BulkTypeFetchMetadata {
		t.Errorf("Type = %q, want %q", job.Type, BulkTypeFetchMetadata)
	}
	if job.Mode != BulkModeYOLO {
		t.Errorf("Mode = %q, want %q", job.Mode, BulkModeYOLO)
	}
	if job.Status != BulkStatusPending {
		t.Errorf("Status = %q, want %q", job.Status, BulkStatusPending)
	}
	if job.TotalItems != 100 {
		t.Errorf("TotalItems = %d, want 100", job.TotalItems)
	}
}

func TestBulkService_GetJob(t *testing.T) {
	db := setupTestDB(t)
	svc := NewBulkService(db)
	ctx := context.Background()

	created, err := svc.CreateJob(ctx, BulkTypeFetchImages, BulkModePromptNoMatch, 50)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := svc.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Type != BulkTypeFetchImages {
		t.Errorf("Type = %q, want %q", got.Type, BulkTypeFetchImages)
	}
}

func TestBulkService_GetJob_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewBulkService(db)

	_, err := svc.GetJob(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
}

func TestBulkService_UpdateJob(t *testing.T) {
	db := setupTestDB(t)
	svc := NewBulkService(db)
	ctx := context.Background()

	job, _ := svc.CreateJob(ctx, BulkTypeFetchMetadata, BulkModeYOLO, 10)

	job.Status = BulkStatusRunning
	job.ProcessedItems = 5
	job.FixedItems = 3
	job.SkippedItems = 1
	job.FailedItems = 1

	if err := svc.UpdateJob(ctx, job); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	got, _ := svc.GetJob(ctx, job.ID)
	if got.Status != BulkStatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, BulkStatusRunning)
	}
	if got.ProcessedItems != 5 {
		t.Errorf("ProcessedItems = %d, want 5", got.ProcessedItems)
	}
	if got.FixedItems != 3 {
		t.Errorf("FixedItems = %d, want 3", got.FixedItems)
	}
}

func TestBulkService_ListJobs(t *testing.T) {
	db := setupTestDB(t)
	svc := NewBulkService(db)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := svc.CreateJob(ctx, BulkTypeFetchMetadata, BulkModeYOLO, 10); err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
	}

	jobs, err := svc.ListJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Errorf("len(jobs) = %d, want 3", len(jobs))
	}
}

func TestBulkService_CreateAndListItems(t *testing.T) {
	db := setupTestDB(t)
	svc := NewBulkService(db)
	ctx := context.Background()

	job, _ := svc.CreateJob(ctx, BulkTypeFetchImages, BulkModeYOLO, 2)

	item1 := &BulkJobItem{
		JobID:      job.ID,
		ArtistID:   "artist-1",
		ArtistName: "Nirvana",
		Status:     BulkItemFixed,
		Message:    "saved thumb",
	}
	item2 := &BulkJobItem{
		JobID:      job.ID,
		ArtistID:   "artist-2",
		ArtistName: "Tool",
		Status:     BulkItemSkipped,
		Message:    "all images present",
	}

	if err := svc.CreateItem(ctx, item1); err != nil {
		t.Fatalf("CreateItem 1: %v", err)
	}
	if err := svc.CreateItem(ctx, item2); err != nil {
		t.Fatalf("CreateItem 2: %v", err)
	}

	items, err := svc.ListItems(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if items[0].ArtistName != "Nirvana" {
		t.Errorf("items[0].ArtistName = %q, want Nirvana", items[0].ArtistName)
	}
	if items[0].Status != BulkItemFixed {
		t.Errorf("items[0].Status = %q, want %q", items[0].Status, BulkItemFixed)
	}
}
