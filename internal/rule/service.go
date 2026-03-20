package rule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/dbutil"
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
	// Image quality rule IDs.
	RuleFanartMinRes          = "fanart_min_res"
	RuleFanartAspect          = "fanart_aspect"
	RuleLogoMinRes            = "logo_min_res"
	RuleBannerExists          = "banner_exists"
	RuleBannerMinRes          = "banner_min_res"
	RuleExtraneousImages      = "extraneous_images"
	RuleArtistIDMismatch      = "artist_id_mismatch"
	RuleLogoTrimmable         = "logo_trimmable"
	RuleDirectoryNameMismatch = "directory_name_mismatch"
	RuleImageDuplicate        = "image_duplicate"
	RuleMetadataQuality       = "metadata_quality"
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
		Description: "Thumbnail must be approximately square (1:1 ratio). Violations are fixed by fetching a square replacement from providers; the existing image is not cropped.",
		Category:    "image",
		Enabled:     true,
		Config:      RuleConfig{AspectRatio: 1.0, Tolerance: 0.1, Severity: "warning"},
	},
	{
		ID:          RuleThumbMinRes,
		Name:        "Thumbnail minimum resolution",
		Description: "Thumbnail must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.",
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
		ID:          RuleFanartMinRes,
		Name:        "Fanart minimum resolution",
		Description: "Fanart/backdrop must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.",
		Category:    "image",
		Enabled:     false,
		Config:      RuleConfig{MinWidth: 1920, MinHeight: 1080, Severity: "warning"},
	},
	{
		ID:          RuleFanartAspect,
		Name:        "Fanart aspect ratio",
		Description: "Fanart/backdrop should match the target aspect ratio. Violations are fixed by fetching a correctly-proportioned replacement from providers; the existing image is not cropped.",
		Category:    "image",
		Enabled:     false,
		Config:      RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1, Severity: "info"},
	},
	{
		ID:          RuleLogoMinRes,
		Name:        "Logo minimum width",
		Description: "Logo should meet the minimum width for legibility. Violations are fixed by fetching a higher-resolution logo from providers.",
		Category:    "image",
		Enabled:     false,
		Config:      RuleConfig{MinWidth: 400, Severity: "info"},
	},
	{
		ID:          RuleBannerExists,
		Name:        "Banner image exists",
		Description: "Artist directory should contain a banner image",
		Category:    "image",
		Enabled:     false,
		Config:      RuleConfig{Severity: "info"},
	},
	{
		ID:          RuleBannerMinRes,
		Name:        "Banner minimum resolution",
		Description: "Banner must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.",
		Category:    "image",
		Enabled:     false,
		Config:      RuleConfig{MinWidth: 1000, MinHeight: 185, Severity: "info"},
	},
	{
		ID:             RuleExtraneousImages,
		Name:           "Extraneous image files",
		Description:    "Flags image files that do not match filenames configured in the active platform profile. Extra files can cause duplicate or incorrect artwork on media servers. Auto-fix deletes them; manual mode lets you review changes first.",
		Category:       "image",
		Enabled:        true,
		AutomationMode: "manual",
		Config:         RuleConfig{Severity: "warning"},
	},
	{
		ID:             RuleArtistIDMismatch,
		Name:           "Artist/ID mismatch",
		Description:    "Detects when an artist's filesystem folder name differs from their stored metadata name. Uses fuzzy matching to allow minor variations while flagging significant divergences.",
		Category:       "metadata",
		Enabled:        false,
		AutomationMode: "manual",
		Config:         RuleConfig{Tolerance: 0.8, Severity: "warning"},
	},
	{
		ID:             RuleLogoTrimmable,
		Name:           "Logo transparent padding",
		Description:    "Logos with large transparent borders waste space and display inconsistently across media servers. This rule detects logos where transparent padding exceeds the configured threshold (default 5%) on any edge, and can automatically trim the padding to produce a tighter, cleaner logo.",
		Category:       "image",
		Enabled:        false,
		AutomationMode: "manual",
		Config:         RuleConfig{ThresholdPercent: 5, Severity: "info"},
	},
	{
		ID:             RuleDirectoryNameMismatch,
		Name:           "Directory name matches artist",
		Description:    "Artist directory name should match the canonical artist name",
		Category:       "metadata",
		Enabled:        true,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "warning", ArticleMode: "prefix"},
	},
	{
		ID:          RuleImageDuplicate,
		Name:        "No duplicate images",
		Description: "Different image slots should not contain visually similar images (default threshold: 90%)",
		Category:    "image",
		Enabled:     false,
		Config:      RuleConfig{Severity: "warning", Tolerance: 0.90},
	},
	{
		ID:          RuleMetadataQuality,
		Name:        "Metadata quality",
		Description: "Detects placeholder or junk metadata values (e.g. biography of just '?' or 'N/A'). Violations are fixed by clearing the junk value and re-fetching from providers.",
		Category:    "metadata",
		Enabled:     true,
		Config:      RuleConfig{Severity: "warning"},
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

// SeedDefaults inserts built-in rules and updates their name and description on conflict.
// The enabled state, automation_mode, and config of existing rules are never overwritten,
// so user customisations are preserved across upgrades.
func (s *Service) SeedDefaults(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range defaultRules {
		autoMode := r.AutomationMode
		if autoMode == "" {
			autoMode = "auto"
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO rules (id, name, description, category, enabled, automation_mode, config, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name        = excluded.name,
				description = excluded.description,
				updated_at  = excluded.updated_at
		`, r.ID, r.Name, r.Description, r.Category, dbutil.BoolToInt(r.Enabled),
			autoMode, MarshalConfig(r.Config), now, now)
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
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
	`, dbutil.BoolToInt(r.Enabled), r.AutomationMode, MarshalConfig(r.Config),
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

// UpsertViolation inserts or updates a rule violation in the notifications store.
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
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status, candidates, dismissed_at, resolved_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_id, artist_id) DO UPDATE SET
			artist_name = excluded.artist_name,
			severity = excluded.severity,
			message = excluded.message,
			fixable = excluded.fixable,
			status = excluded.status,
			candidates = excluded.candidates,
			dismissed_at = excluded.dismissed_at,
			resolved_at = excluded.resolved_at,
			updated_at = excluded.updated_at
	`, v.ID, v.RuleID, v.ArtistID, v.ArtistName, v.Severity, v.Message,
		dbutil.BoolToInt(v.Fixable), v.Status, marshalCandidates(v.Candidates),
		dbutil.NilableTime(v.DismissedAt), dbutil.NilableTime(v.ResolvedAt),
		v.CreatedAt.Format(time.RFC3339), v.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upserting violation: %w", err)
	}
	return nil
}

// ListViolations returns rule violations filtered by status.
// If status is empty, returns all violations.
func (s *Service) ListViolations(ctx context.Context, status string) ([]RuleViolation, error) {
	query := `SELECT id, rule_id, artist_id, artist_name, severity, message, fixable, status, candidates, dismissed_at, resolved_at, created_at, updated_at FROM rule_violations`
	args := []any{}
	if status == "active" {
		// active = open + pending_choice (violations that need attention)
		query += ` WHERE status IN (?, ?)`
		args = append(args, ViolationStatusOpen, ViolationStatusPendingChoice)
	} else if status != "" {
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

// ListViolationsFiltered returns violations matching the given params with dynamic SQL.
func (s *Service) ListViolationsFiltered(ctx context.Context, p ViolationListParams) ([]RuleViolation, error) {
	var (
		whereClauses []string
		args         []any
		needJoin     bool
	)

	// Status filter (same "active" logic as ListViolations)
	switch p.Status {
	case "active":
		whereClauses = append(whereClauses, "rv.status IN (?, ?)")
		args = append(args, ViolationStatusOpen, ViolationStatusPendingChoice)
	case "":
		// no filter
	default:
		whereClauses = append(whereClauses, "rv.status = ?")
		args = append(args, p.Status)
	}

	// Severity filter
	if p.Severity != "" {
		whereClauses = append(whereClauses, "rv.severity = ?")
		args = append(args, p.Severity)
	}

	// Rule ID filter
	if p.RuleID != "" {
		whereClauses = append(whereClauses, "rv.rule_id = ?")
		args = append(args, p.RuleID)
	}

	// Category filter requires joining the rules table
	if p.Category != "" {
		needJoin = true
		whereClauses = append(whereClauses, "r.category = ?")
		args = append(args, p.Category)
	}

	// Build query
	query := `SELECT rv.id, rv.rule_id, rv.artist_id, rv.artist_name, rv.severity, rv.message, rv.fixable, rv.status, rv.candidates, rv.dismissed_at, rv.resolved_at, rv.created_at, rv.updated_at FROM rule_violations rv`
	if needJoin {
		query += ` JOIN rules r ON r.id = rv.rule_id`
	}
	if len(whereClauses) > 0 {
		query += " WHERE " + joinStrings(whereClauses, " AND ") //nolint:gosec // G202: all clauses use parameterized placeholders
	}

	// Sort with whitelisted columns
	severityRank := `CASE rv.severity WHEN 'error' THEN 3 WHEN 'warning' THEN 2 WHEN 'info' THEN 1 ELSE 0 END`

	sortCols := map[string]string{
		"artist_name": "rv.artist_name",
		"severity":    severityRank,
		"rule_id":     "rv.rule_id",
		"created_at":  "rv.created_at",
	}

	order := "DESC"
	if p.Order == "asc" {
		order = "ASC"
	}

	if col, ok := sortCols[p.Sort]; ok {
		query += " ORDER BY " + col + " " + order //nolint:gosec // G202: col is from whitelist map, not user input
		if p.Sort != "created_at" {
			query += ", rv.created_at DESC"
		}
	} else {
		// Default sort: severity DESC (errors first), then newest
		query += " ORDER BY " + severityRank + " DESC, rv.created_at DESC" //nolint:gosec // G202: severityRank is a constant CASE expression
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing filtered violations: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var violations []RuleViolation
	for rows.Next() {
		v, err := scanViolation(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning filtered violation row: %w", err)
		}
		violations = append(violations, *v)
	}
	return violations, rows.Err()
}

// GroupViolations groups violations by the specified field.
func GroupViolations(violations []RuleViolation, groupBy string) []ViolationGroup {
	if groupBy == "" {
		return []ViolationGroup{{
			Key:        "all",
			Label:      "All Violations",
			Count:      len(violations),
			Violations: violations,
		}}
	}

	// categoryFromRuleID extracts a category hint from the rule ID prefix.
	categoryFromRuleID := func(ruleID string) string {
		prefixMap := map[string]string{
			"nfo":        "nfo",
			"thumb":      "image",
			"fanart":     "image",
			"logo":       "image",
			"banner":     "image",
			"extraneous": "image",
			"bio":        "metadata",
			"artist":     "metadata",
		}
		for prefix, cat := range prefixMap {
			if len(ruleID) >= len(prefix) && ruleID[:len(prefix)] == prefix {
				return cat
			}
		}
		return "other"
	}

	keyFunc := func(v RuleViolation) (string, string) {
		switch groupBy {
		case "artist":
			return v.ArtistID, v.ArtistName
		case "rule":
			return v.RuleID, v.RuleID
		case "severity":
			return v.Severity, v.Severity
		case "category":
			cat := categoryFromRuleID(v.RuleID)
			return cat, cat
		default:
			return "all", "All Violations"
		}
	}

	// Preserve insertion order with a slice of keys and a map for lookup.
	var order []string
	groups := make(map[string]*ViolationGroup)

	for _, v := range violations {
		key, label := keyFunc(v)
		g, exists := groups[key]
		if !exists {
			g = &ViolationGroup{Key: key, Label: label}
			groups[key] = g
			order = append(order, key)
		}
		g.Violations = append(g.Violations, v)
		g.Count++
	}

	result := make([]ViolationGroup, 0, len(order))
	for _, key := range order {
		result = append(result, *groups[key])
	}
	return result
}

// GetViolationByID retrieves a single rule violation by its primary key.
func (s *Service) GetViolationByID(ctx context.Context, id string) (*RuleViolation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, rule_id, artist_id, artist_name, severity, message, fixable, status, candidates, dismissed_at, resolved_at, created_at, updated_at
		FROM rule_violations WHERE id = ?
	`, id)
	v, err := scanViolation(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("violation not found: %s", id)
		}
		return nil, fmt.Errorf("getting violation by id: %w", err)
	}
	return v, nil
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

// BulkDismissViolations marks all active (open + pending_choice) violations as dismissed.
// If ids is non-empty, only those violations are dismissed; otherwise all active violations are dismissed.
func (s *Service) BulkDismissViolations(ctx context.Context, ids []string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var res sql.Result
	var err error
	if len(ids) > 0 {
		// Build a parameterised IN clause
		placeholders := make([]string, len(ids))
		args := make([]any, 0, len(ids)+3)
		args = append(args, ViolationStatusDismissed, now, now)
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		//nolint:gosec // G202: only "?" placeholders concatenated, no user input
		query := "UPDATE rule_violations SET status = ?, dismissed_at = ?, updated_at = ? WHERE id IN (" +
			joinStrings(placeholders, ",") + ") AND status IN (?, ?)"
		args = append(args, ViolationStatusOpen, ViolationStatusPendingChoice)
		res, err = s.db.ExecContext(ctx, query, args...)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE rule_violations SET status = ?, dismissed_at = ?, updated_at = ?
			WHERE status IN (?, ?)
		`, ViolationStatusDismissed, now, now, ViolationStatusOpen, ViolationStatusPendingChoice)
	}
	if err != nil {
		return 0, fmt.Errorf("bulk dismissing violations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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

// CountActiveViolationsBySeverity returns the count of active (open + pending_choice)
// violations grouped by severity level.
func (s *Service) CountActiveViolationsBySeverity(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT severity, COUNT(*) FROM rule_violations
		WHERE status IN (?, ?)
		GROUP BY severity
	`, ViolationStatusOpen, ViolationStatusPendingChoice)
	if err != nil {
		return nil, fmt.Errorf("counting active violations by severity: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	counts := map[string]int{"error": 0, "warning": 0, "info": 0}
	for rows.Next() {
		var severity string
		var count int
		if err := rows.Scan(&severity, &count); err != nil {
			return nil, fmt.Errorf("scanning violation count: %w", err)
		}
		switch severity {
		case "error", "warning", "info":
			counts[severity] = count
		default:
			// Ignore unknown severities to keep the return shape stable.
		}
	}
	return counts, rows.Err()
}

// DismissOrphanedViolations dismisses all active violations whose artist_id
// no longer exists in the artists table. Returns the number dismissed.
func (s *Service) DismissOrphanedViolations(ctx context.Context) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations
		SET status = ?, dismissed_at = ?, updated_at = ?
		WHERE status IN (?, ?)
		AND artist_id NOT IN (SELECT id FROM artists)
	`, ViolationStatusDismissed, now, now, ViolationStatusOpen, ViolationStatusPendingChoice)
	if err != nil {
		return 0, fmt.Errorf("dismissing orphaned violations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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

// GetComplianceForArtists returns a compliance status map for the given artist IDs.
// Each artist ID maps to a ComplianceStatus based on the highest-severity active
// violation (open or pending_choice). Artists with no active violations are
// mapped to ComplianceCompliant.
func (s *Service) GetComplianceForArtists(ctx context.Context, artistIDs []string) (map[string]artist.ComplianceStatus, error) {
	result := make(map[string]artist.ComplianceStatus, len(artistIDs))
	if len(artistIDs) == 0 {
		return result, nil
	}

	// Default all artists to compliant; the query only returns artists with violations.
	for _, id := range artistIDs {
		result[id] = artist.ComplianceCompliant
	}

	// Build parameterised IN clause for the artist IDs.
	placeholders := make([]string, len(artistIDs))
	args := make([]any, 0, len(artistIDs)+2)
	args = append(args, ViolationStatusOpen, ViolationStatusPendingChoice)
	for i, id := range artistIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	//nolint:gosec // G202: only "?" placeholders concatenated, no user input
	query := `SELECT artist_id,
	       MAX(CASE severity WHEN 'error' THEN 3 WHEN 'warning' THEN 2 ELSE 1 END) AS max_sev
	FROM rule_violations
	WHERE status IN (?, ?)
	  AND artist_id IN (` + strings.Join(placeholders, ",") + `)
	GROUP BY artist_id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying compliance for artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var artistID string
		var maxSev int
		if err := rows.Scan(&artistID, &maxSev); err != nil {
			return nil, fmt.Errorf("scanning compliance row: %w", err)
		}
		switch maxSev {
		case 3:
			result[artistID] = artist.ComplianceError
		case 2, 1:
			result[artistID] = artist.ComplianceWarning
		default:
			// Unknown severity rank from the SQL CASE expression; treat as
			// warning but this should not happen with valid data.
			result[artistID] = artist.ComplianceWarning
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating compliance rows: %w", err)
	}
	return result, nil
}

func scanHealthSnapshot(row interface{ Scan(...any) error }) (*HealthSnapshot, error) {
	var snap HealthSnapshot
	var recordedAt string
	err := row.Scan(&snap.ID, &snap.TotalArtists, &snap.CompliantArtists, &snap.Score, &recordedAt)
	if err != nil {
		return nil, err
	}
	snap.RecordedAt = dbutil.ParseTime(recordedAt)
	return &snap, nil
}

// scanViolation scans a database row into a RuleViolation struct.
func scanViolation(row interface{ Scan(...any) error }) (*RuleViolation, error) {
	var v RuleViolation
	var fixable int
	var candidates string
	var createdAt, updatedAt, dismissedAt, resolvedAt sql.NullString

	err := row.Scan(&v.ID, &v.RuleID, &v.ArtistID, &v.ArtistName, &v.Severity, &v.Message,
		&fixable, &v.Status, &candidates, &dismissedAt, &resolvedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}

	v.Fixable = fixable == 1
	v.Candidates = unmarshalCandidates(candidates)
	v.CreatedAt = dbutil.ParseTime(createdAt.String)
	v.UpdatedAt = dbutil.ParseTime(updatedAt.String)
	if dismissedAt.Valid {
		t := dbutil.ParseTime(dismissedAt.String)
		v.DismissedAt = &t
	}
	if resolvedAt.Valid {
		t := dbutil.ParseTime(resolvedAt.String)
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
	r.CreatedAt = dbutil.ParseTime(createdAt)
	r.UpdatedAt = dbutil.ParseTime(updatedAt)

	return &r, nil
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

func marshalCandidates(cs []ImageCandidate) string {
	if len(cs) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(cs)
	return string(b)
}

func unmarshalCandidates(s string) []ImageCandidate {
	if s == "" || s == "[]" {
		return nil
	}
	var cs []ImageCandidate
	_ = json.Unmarshal([]byte(s), &cs)
	return cs
}
