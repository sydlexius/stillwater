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
		SELECT id, name, description, category, enabled, config, created_at, updated_at
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
		SELECT id, name, description, category, enabled, config, created_at, updated_at
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

// Update modifies a rule's enabled state and config.
func (s *Service) Update(ctx context.Context, r *Rule) error {
	r.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE rules SET enabled = ?, config = ?, updated_at = ? WHERE id = ?
	`, boolToInt(r.Enabled), MarshalConfig(r.Config),
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

// scanRule scans a database row into a Rule struct.
func scanRule(row interface{ Scan(...any) error }) (*Rule, error) {
	var r Rule
	var enabled int
	var config string
	var createdAt, updatedAt string

	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Category,
		&enabled, &config, &createdAt, &updatedAt)
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

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
