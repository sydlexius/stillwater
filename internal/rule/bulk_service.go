package rule

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// BulkService provides persistence for bulk jobs.
type BulkService struct {
	db *sql.DB
}

// NewBulkService creates a BulkService.
func NewBulkService(db *sql.DB) *BulkService {
	return &BulkService{db: db}
}

// CreateJob inserts a new bulk job.
func (s *BulkService) CreateJob(ctx context.Context, jobType, mode string, totalItems int) (*BulkJob, error) {
	job := &BulkJob{
		ID:         uuid.New().String(),
		Type:       jobType,
		Mode:       mode,
		Status:     BulkStatusPending,
		TotalItems: totalItems,
		CreatedAt:  time.Now().UTC(),
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bulk_jobs (id, type, mode, status, total_items, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, job.ID, job.Type, job.Mode, job.Status, job.TotalItems,
		job.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("creating bulk job: %w", err)
	}

	return job, nil
}

// GetJob retrieves a bulk job by ID.
func (s *BulkService) GetJob(ctx context.Context, id string) (*BulkJob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, type, mode, status, total_items, processed_items,
		       fixed_items, skipped_items, failed_items, error,
		       created_at, started_at, completed_at
		FROM bulk_jobs WHERE id = ?
	`, id)

	return scanBulkJob(row)
}

// ListJobs returns recent bulk jobs ordered by creation time descending.
func (s *BulkService) ListJobs(ctx context.Context, limit int) ([]BulkJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, mode, status, total_items, processed_items,
		       fixed_items, skipped_items, failed_items, error,
		       created_at, started_at, completed_at
		FROM bulk_jobs ORDER BY created_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing bulk jobs: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var jobs []BulkJob
	for rows.Next() {
		job, err := scanBulkJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning bulk job: %w", err)
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

// UpdateJob updates a bulk job's mutable fields.
func (s *BulkService) UpdateJob(ctx context.Context, job *BulkJob) error {
	var startedAt, completedAt *string
	if job.StartedAt != nil {
		s := job.StartedAt.Format(time.RFC3339)
		startedAt = &s
	}
	if job.CompletedAt != nil {
		s := job.CompletedAt.Format(time.RFC3339)
		completedAt = &s
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE bulk_jobs
		SET status = ?, processed_items = ?, fixed_items = ?, skipped_items = ?,
		    failed_items = ?, error = ?, started_at = ?, completed_at = ?
		WHERE id = ?
	`, job.Status, job.ProcessedItems, job.FixedItems, job.SkippedItems,
		job.FailedItems, job.Error, startedAt, completedAt, job.ID)
	if err != nil {
		return fmt.Errorf("updating bulk job: %w", err)
	}
	return nil
}

// CreateItem inserts a bulk job item.
func (s *BulkService) CreateItem(ctx context.Context, item *BulkJobItem) error {
	item.ID = uuid.New().String()
	item.CreatedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bulk_job_items (id, job_id, artist_id, artist_name, status, message, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, item.ID, item.JobID, item.ArtistID, item.ArtistName, item.Status,
		item.Message, item.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("creating bulk job item: %w", err)
	}
	return nil
}

// ListItems returns all items for a bulk job.
func (s *BulkService) ListItems(ctx context.Context, jobID string) ([]BulkJobItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, artist_id, artist_name, status, message, created_at
		FROM bulk_job_items WHERE job_id = ? ORDER BY created_at
	`, jobID)
	if err != nil {
		return nil, fmt.Errorf("listing bulk job items: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var items []BulkJobItem
	for rows.Next() {
		var item BulkJobItem
		var message sql.NullString
		var createdAt string
		if err := rows.Scan(&item.ID, &item.JobID, &item.ArtistID, &item.ArtistName,
			&item.Status, &message, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning bulk job item: %w", err)
		}
		if message.Valid {
			item.Message = message.String
		}
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanBulkJob(row interface{ Scan(...any) error }) (*BulkJob, error) {
	var job BulkJob
	var errStr sql.NullString
	var createdAt string
	var startedAt, completedAt sql.NullString

	err := row.Scan(&job.ID, &job.Type, &job.Mode, &job.Status,
		&job.TotalItems, &job.ProcessedItems, &job.FixedItems,
		&job.SkippedItems, &job.FailedItems, &errStr,
		&createdAt, &startedAt, &completedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("bulk job not found")
		}
		return nil, err
	}

	if errStr.Valid {
		job.Error = errStr.String
	}
	job.CreatedAt = parseTime(createdAt)
	if startedAt.Valid {
		t := parseTime(startedAt.String)
		job.StartedAt = &t
	}
	if completedAt.Valid {
		t := parseTime(completedAt.String)
		job.CompletedAt = &t
	}

	return &job, nil
}
