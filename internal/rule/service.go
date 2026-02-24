package rule

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Built-in rule IDs.
const (
	RuleNFOExists    = "nfo_exists"
	RuleNFOHasMBID   = "nfo_has_mbid"
	RuleThumbExists  = "thumb_exists"
	RuleThumbSquare  = "thumb_square"
	RuleThumbMinRes  = "thumb_min_res"
	RuleFanartExists = "fanart_exists"
	RuleLogoExists   = "logo_exists"
	RuleBioExists    = "bio_exists"
	RuleFallbackUsed = "fallback_used"
)

// defaultRules defines the built-in rules seeded on first startup.
var defaultRules = []Rule{
	{
		ID:          RuleNFOExists,
		Name:        "NFO file exists",
		Description: "Artist directory must contain an artist.nfo file",
		Category:    "nfo",
		Enabled:     true,
		Config:      RuleConfig{Severity: "error"},
	},
	{
		ID:          RuleNFOHasMBID,
		Name:        "NFO has MusicBrainz ID",
		Description: "The artist.nfo file must contain a MusicBrainz artist ID",
		Category:    "nfo",
		Enabled:     true,
		Config:      RuleConfig{Severity: "error"},
	},
	{
		ID:          RuleThumbExists,
		Name:        "Thumbnail image exists",
		Description: "Artist directory must contain a thumbnail image (folder.jpg/png)",
		Category:    "image",
		Enabled:     true,
		Config:      RuleConfig{Severity: "error"},
	},
	{
		ID:          RuleThumbSquare,
		Name:        "Thumbnail is square",
		Description: "Thumbnail image must have approximately 1:1 aspect ratio",
		Category:    "image",
		Enabled:     true,
		Config:      RuleConfig{AspectRatio: 1.0, Tolerance: 0.1, Severity: "warning"},
	},
	{
		ID:          RuleThumbMinRes,
		Name:        "Thumbnail minimum resolution",
		Description: "Thumbnail image must meet minimum resolution requirements",
		Category:    "image",
		Enabled:     true,
		Config:      RuleConfig{MinWidth: 500, MinHeight: 500, Severity: "warning"},
	},
	{
		ID:          RuleFanartExists,
		Name:        "Fanart image exists",
		Description: "Artist directory must contain a fanart/backdrop image",
		Category:    "image",
		Enabled:     true,
		Config:      RuleConfig{Severity: "warning"},
	},
	{
		ID:          RuleLogoExists,
		Name:        "Logo image exists",
		Description: "Artist directory must contain a logo image (logo.png)",
		Category:    "image",
		Enabled:     true,
		Config:      RuleConfig{Severity: "info"},
	},
	{
		ID:          RuleBioExists,
		Name:        "Biography exists",
		Description: "Artist must have a biography populated",
		Category:    "metadata",
		Enabled:     true,
		Config:      RuleConfig{MinLength: 10, Severity: "warning"},
	},
	{
		ID:          RuleFallbackUsed,
		Name:        "Fallback provider used",
		Description: "Flags when metadata fields were populated by a fallback provider instead of the configured primary",
		Category:    "metadata",
		Enabled:     true,
		Config:      RuleConfig{Severity: "info"},
	},
}

// Service provides rule data operations.
type Service struct {
	db *sql.DB
}

// NewService creates a rule service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// SeedDefaults inserts the built-in rules if they do not already exist.
func (s *Service) SeedDefaults(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range defaultRules {
		_, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO rules (id, name, description, category, enabled, config, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, r.ID, r.Name, r.Description, r.Category, boolToInt(r.Enabled),
			MarshalConfig(r.Config), now, now)
		if err != nil {
			return fmt.Errorf("seeding rule %s: %w", r.ID, err)
		}
	}
	return nil
}

// List returns all rules.
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, category, enabled, automation_mode, config, created_at, updated_at
		FROM rules ORDER BY category, name
	`)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var rules []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning rule row: %w", err)
		}
		rules = append(rules, *r)
	}
	return rules, rows.Err()
}

// GetByID retrieves a rule by primary key.
func (s *Service) GetByID(ctx context.Context, id string) (*Rule, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, category, enabled, automation_mode, config, created_at, updated_at
		FROM rules WHERE id = ?
	`, id)
	r, err := scanRule(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("rule not found: %s", id)
		}
		return nil, fmt.Errorf("getting rule by id: %w", err)
	}
	return r, nil
}

// Update modifies a rule's enabled state, automation mode, and config.
func (s *Service) Update(ctx context.Context, r *Rule) error {
	r.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE rules SET enabled = ?, automation_mode = ?, config = ?, updated_at = ? WHERE id = ?
	`, boolToInt(r.Enabled), r.AutomationMode, MarshalConfig(r.Config),
		r.UpdatedAt.Format(time.RFC3339), r.ID)
	if err != nil {
		return fmt.Errorf("updating rule: %w", err)
	}
	return nil
}

// RecordHealthSnapshot inserts a row into the health_history table.
func (s *Service) RecordHealthSnapshot(ctx context.Context, totalArtists, compliantArtists int, score float64) error {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO health_history (id, total_artists, compliant_artists, score, recorded_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, totalArtists, compliantArtists, score,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("recording health snapshot: %w", err)
	}
	return nil
}

// GetHealthHistory returns health snapshots within a time range, ordered by recorded_at.
// If from or to are zero-valued, defaults to the last 90 days.
func (s *Service) GetHealthHistory(ctx context.Context, from, to time.Time) ([]HealthSnapshot, error) {
	if from.IsZero() {
		from = time.Now().UTC().AddDate(0, -3, 0)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, total_artists, compliant_artists, score, recorded_at
		FROM health_history
		WHERE recorded_at BETWEEN ? AND ?
		ORDER BY recorded_at ASC
	`, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("querying health history: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var snapshots []HealthSnapshot
	for rows.Next() {
		snap, err := scanHealthSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning health snapshot: %w", err)
		}
		snapshots = append(snapshots, *snap)
	}
	return snapshots, rows.Err()
}

// GetLatestHealthSnapshot returns the most recent health snapshot, or nil if none exist.
func (s *Service) GetLatestHealthSnapshot(ctx context.Context) (*HealthSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, total_artists, compliant_artists, score, recorded_at
		FROM health_history
		ORDER BY recorded_at DESC LIMIT 1
	`)
	snap, err := scanHealthSnapshot(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("getting latest health snapshot: %w", err)
	}
	return snap, nil
}

// UpsertViolation inserts or updates a rule violation in the inbox.
// Uses (rule_id, artist_id) as the natural key for upsert.
func (s *Service) UpsertViolation(ctx context.Context, v *RuleViolation) error {
	if v.ID == "" {
		v.ID = uuid.New().String()
	}
	v.UpdatedAt = time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = v.UpdatedAt
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status, dismissed_at, resolved_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_id, artist_id) DO UPDATE SET
			artist_name = excluded.artist_name,
			severity = excluded.severity,
			message = excluded.message,
			fixable = excluded.fixable,
			status = excluded.status,
			dismissed_at = excluded.dismissed_at,
			resolved_at = excluded.resolved_at,
			updated_at = excluded.updated_at
	`, v.ID, v.RuleID, v.ArtistID, v.ArtistName, v.Severity, v.Message,
		boolToInt(v.Fixable), v.Status, nilableTime(v.DismissedAt), nilableTime(v.ResolvedAt),
		v.CreatedAt.Format(time.RFC3339), v.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upserting violation: %w", err)
	}
	return nil
}

// ListViolations returns rule violations filtered by status.
// If status is empty, returns all violations.
func (s *Service) ListViolations(ctx context.Context, status string) ([]RuleViolation, error) {
	query := `SELECT id, rule_id, artist_id, artist_name, severity, message, fixable, status, dismissed_at, resolved_at, created_at, updated_at FROM rule_violations`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing violations: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var violations []RuleViolation
	for rows.Next() {
		v, err := scanViolation(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning violation row: %w", err)
		}
		violations = append(violations, *v)
	}
	return violations, rows.Err()
}

// DismissViolation marks a violation as dismissed.
func (s *Service) DismissViolation(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations SET status = ?, dismissed_at = ?, updated_at = ? WHERE id = ?
	`, ViolationStatusDismissed, now.Format(time.RFC3339), now.Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("dismissing violation: %w", err)
	}
	return nil
}

// ResolveViolation marks a violation as resolved.
func (s *Service) ResolveViolation(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations SET status = ?, resolved_at = ?, updated_at = ? WHERE id = ?
	`, ViolationStatusResolved, now.Format(time.RFC3339), now.Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("resolving violation: %w", err)
	}
	return nil
}

// ClearResolvedViolations deletes resolved violations older than the given age.
func (s *Service) ClearResolvedViolations(ctx context.Context, daysOld int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -daysOld)
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM rule_violations WHERE status = ? AND resolved_at < ?
	`, ViolationStatusResolved, cutoff.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("clearing resolved violations: %w", err)
	}
	return nil
}

func scanHealthSnapshot(row interface{ Scan(...any) error }) (*HealthSnapshot, error) {
	var snap HealthSnapshot
	var recordedAt string
	err := row.Scan(&snap.ID, &snap.TotalArtists, &snap.CompliantArtists, &snap.Score, &recordedAt)
	if err != nil {
		return nil, err
	}
	snap.RecordedAt = parseTime(recordedAt)
	return &snap, nil
}

// scanViolation scans a database row into a RuleViolation struct.
func scanViolation(row interface{ Scan(...any) error }) (*RuleViolation, error) {
	var v RuleViolation
	var fixable int
	var createdAt, updatedAt, dismissedAt, resolvedAt sql.NullString

	err := row.Scan(&v.ID, &v.RuleID, &v.ArtistID, &v.ArtistName, &v.Severity, &v.Message,
		&fixable, &v.Status, &dismissedAt, &resolvedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}

	v.Fixable = fixable == 1
	v.CreatedAt = parseTime(createdAt.String)
	v.UpdatedAt = parseTime(updatedAt.String)
	if dismissedAt.Valid {
		t := parseTime(dismissedAt.String)
		v.DismissedAt = &t
	}
	if resolvedAt.Valid {
		t := parseTime(resolvedAt.String)
		v.ResolvedAt = &t
	}

	return &v, nil
}

// scanRule scans a database row into a Rule struct.
func scanRule(row interface{ Scan(...any) error }) (*Rule, error) {
	var r Rule
	var enabled int
	var config string
	var createdAt, updatedAt string

	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Category,
		&enabled, &r.AutomationMode, &config, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}

	r.Enabled = enabled == 1
	r.Config = UnmarshalConfig(config)
	r.CreatedAt = parseTime(createdAt)
	r.UpdatedAt = parseTime(updatedAt)

	return &r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nilableTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
