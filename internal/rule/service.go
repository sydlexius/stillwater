package rule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
	RuleDirectoryNameMismatch = "directory_name_mismatch"
	RuleImageDuplicate        = "image_duplicate"
	RuleImageDuplicateExact   = "image_duplicate_exact"
	RuleMetadataQuality       = "metadata_quality"
	RuleBackdropSequencing    = "backdrop_sequencing"
	RuleBackdropMinCount      = "backdrop_min_count"
	RuleLogoPadding           = "logo_padding"
	RuleNameLanguagePref      = "name_language_pref"
	RuleOriginMissing         = "origin_missing"
	RuleDiscographyPopulated  = "discography_populated"

	// Deprecated rule IDs kept for migration. These rules have been merged
	// into other rules but may still have violations in the database.
	ruleLogoTrimmableDeprecated = "logo_trimmable"
)

// defaultRules defines the built-in rules seeded on first startup.
var defaultRules = []Rule{
	{
		ID:             RuleNFOExists,
		Name:           "NFO file exists",
		Description:    "Artist directory must contain an artist.nfo file",
		Category:       RuleCategoryNFO,
		Enabled:        true,
		AutomationMode: AutomationModeAuto,
		Config:         RuleConfig{Severity: "error"},
	},
	{
		ID:          RuleNFOHasMBID,
		Name:        "NFO has MusicBrainz ID",
		Description: "The artist.nfo file must contain a MusicBrainz artist ID",
		Category:    RuleCategoryNFO,
		Enabled:     true,
		Config:      RuleConfig{Severity: "error"},
	},
	{
		ID:          RuleThumbExists,
		Name:        "Thumbnail image exists",
		Description: "Artist directory must contain a thumbnail image (folder.jpg/png)",
		Category:    RuleCategoryImage,
		Enabled:     true,
		Config:      RuleConfig{Severity: "error"},
	},
	{
		ID:          RuleThumbSquare,
		Name:        "Thumbnail is square",
		Description: "Thumbnail must be approximately square (1:1 ratio). Violations are fixed by fetching a square replacement from providers; the existing image is not cropped.",
		Category:    RuleCategoryImage,
		Enabled:     true,
		Config:      RuleConfig{AspectRatio: 1.0, Tolerance: 0.1, Severity: "warning"},
	},
	{
		ID:          RuleThumbMinRes,
		Name:        "Thumbnail minimum resolution",
		Description: "Thumbnail must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.",
		Category:    RuleCategoryImage,
		Enabled:     true,
		Config:      RuleConfig{MinWidth: 500, MinHeight: 500, Severity: "warning"},
	},
	{
		ID:          RuleFanartExists,
		Name:        "Fanart image exists",
		Description: "Artist directory must contain a fanart/backdrop image",
		Category:    RuleCategoryImage,
		Enabled:     true,
		Config:      RuleConfig{Severity: "warning"},
	},
	{
		ID:          RuleLogoExists,
		Name:        "Logo image exists",
		Description: "Artist directory must contain a logo image (logo.png)",
		Category:    RuleCategoryImage,
		Enabled:     true,
		Config:      RuleConfig{Severity: "info"},
	},
	{
		ID:          RuleBioExists,
		Name:        "Biography exists",
		Description: "Artist must have a biography populated",
		Category:    RuleCategoryMetadata,
		Enabled:     true,
		Config:      RuleConfig{MinLength: 10, Severity: "warning"},
	},
	{
		ID:          RuleFanartMinRes,
		Name:        "Fanart minimum resolution",
		Description: "Fanart/backdrop must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.",
		Category:    RuleCategoryImage,
		Enabled:     false,
		Config:      RuleConfig{MinWidth: 1920, MinHeight: 1080, Severity: "warning"},
	},
	{
		ID:          RuleFanartAspect,
		Name:        "Fanart aspect ratio",
		Description: "Fanart/backdrop should match the target aspect ratio. Violations are fixed by fetching a correctly-proportioned replacement from providers; the existing image is not cropped.",
		Category:    RuleCategoryImage,
		Enabled:     false,
		Config:      RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1, Severity: "info"},
	},
	{
		ID:          RuleLogoMinRes,
		Name:        "Logo minimum width",
		Description: "Logo should meet the minimum width for legibility. Violations are fixed by fetching a higher-resolution logo from providers.",
		Category:    RuleCategoryImage,
		Enabled:     false,
		Config:      RuleConfig{MinWidth: 400, Severity: "info"},
	},
	{
		ID:          RuleBannerExists,
		Name:        "Banner image exists",
		Description: "Artist directory should contain a banner image",
		Category:    RuleCategoryImage,
		Enabled:     false,
		Config:      RuleConfig{Severity: "info"},
	},
	{
		ID:          RuleBannerMinRes,
		Name:        "Banner minimum resolution",
		Description: "Banner must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.",
		Category:    RuleCategoryImage,
		Enabled:     false,
		Config:      RuleConfig{MinWidth: 1000, MinHeight: 185, Severity: "info"},
	},
	{
		ID:             RuleExtraneousImages,
		Name:           "Extraneous image files",
		Description:    "Flags image files that do not match filenames configured in the active platform profile. Extra files can cause duplicate or incorrect artwork on media servers. Auto-fix deletes them; manual mode lets you review changes first.",
		Category:       RuleCategoryImage,
		Enabled:        true,
		AutomationMode: "manual",
		Config:         RuleConfig{Severity: "warning"},
	},
	{
		ID:             RuleArtistIDMismatch,
		Name:           "Artist/ID mismatch",
		Description:    "Detects when an artist's filesystem folder name differs from their stored metadata name. Uses fuzzy matching to allow minor variations while flagging significant divergences.",
		Category:       RuleCategoryMetadata,
		Enabled:        false,
		AutomationMode: "manual",
		Config:         RuleConfig{Tolerance: 0.8, Severity: "warning"},
	},
	{
		ID:             RuleDirectoryNameMismatch,
		Name:           "Directory name matches artist",
		Description:    "Artist directory name should match the canonical artist name",
		Category:       RuleCategoryMetadata,
		Enabled:        true,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "warning", ArticleMode: "prefix"},
	},
	{
		ID:             RuleImageDuplicate,
		Name:           "No duplicate images",
		Description:    "Different image slots should not contain visually similar images (default threshold: 90%)",
		Category:       RuleCategoryImage,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "warning", Tolerance: 0.90},
	},
	{
		ID:   RuleImageDuplicateExact,
		Name: "No byte-identical images",
		Description: "Fanart slots should not contain byte-identical copies of the same file. " +
			"Detection compares file hashes rather than image content, so a match is exact and " +
			"the redundant copy is always safe to remove. Visually identical images that are not " +
			"byte-identical (for example a re-encoded or re-tagged copy) are the separate " +
			"'No duplicate images' rule's concern.",
		Category: RuleCategoryImage,
		// Enabled and automatic by default: unlike the perceptual rule, byte
		// equality admits no false positives, so deleting the redundant copy
		// needs no human judgement.
		Enabled:        true,
		AutomationMode: AutomationModeAuto,
		Config:         RuleConfig{Severity: "warning"},
	},
	{
		ID:             RuleMetadataQuality,
		Name:           "Metadata quality",
		Description:    "Detects placeholder or junk metadata values (e.g. biography of just '?' or 'N/A'). Violations are fixed by clearing the junk value and re-fetching from providers.",
		Category:       RuleCategoryMetadata,
		Enabled:        true,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "warning"},
	},
	{
		ID:             RuleBackdropSequencing,
		Name:           "Backdrop/fanart sequencing",
		Description:    "Detects gaps in backdrop/fanart image sequences and incorrect numbering. Violations are fixed by renaming files to fill gaps.",
		Category:       RuleCategoryImage,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "warning"},
	},
	{
		ID:             RuleBackdropMinCount,
		Name:           "Minimum backdrop count",
		Description:    "Flags artists with fewer backdrops than the configured minimum. This rule is detection-only; resolving violations requires manual upload or multiple evaluation passes.",
		Category:       RuleCategoryImage,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{MinCount: 1, Severity: "warning"},
	},
	{
		ID:             RuleLogoPadding,
		Name:           "Logo excessive padding",
		Description:    "Detects logo images where excessive transparent (PNG) or whitespace (JPG) padding surrounds the content. If the padding area exceeds the configured threshold (default 15%) of the total image area, a violation is raised. Auto-fix trims to content bounds with a configurable margin. Replaces the former logo_trimmable rule.",
		Category:       RuleCategoryImage,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{ThresholdPercent: 15, TrimMargin: 2, Severity: "info"},
	},
	{
		ID:             RuleNameLanguagePref,
		Name:           "Artist name matches preferred language",
		Description:    "Flags artists whose stored Name or SortName does not match the user's preferred metadata languages. When MusicBrainz provides a preferred-locale alias, the violation is fixable and Fix/auto mode can promote it; otherwise the violation is informational and can be edited manually or dismissed.",
		Category:       RuleCategoryMetadata,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "warning"},
	},
	{
		ID:             RuleOriginMissing,
		Name:           "Origin is populated",
		Description:    "Flags artists with an empty origin field. Violations are fixed by fetching the origin (city, region, or country) from the configured provider priority list. Auto mode applies the highest-priority non-empty result; manual mode surfaces the violation so you can pick a provider value or edit it.",
		Category:       RuleCategoryMetadata,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		Config:         RuleConfig{Severity: "info"},
	},
	{
		ID:             RuleDiscographyPopulated,
		Name:           "Discography is populated",
		Description:    "Flags artists whose artist.nfo has no album entries, or materially fewer than MusicBrainz lists. Violations are fixed by fetching release groups from MusicBrainz and merging them into the NFO; user-added albums are always preserved. Auto mode applies the merge automatically; manual mode surfaces the violation so you can review and fix it individually.",
		Category:       RuleCategoryMetadata,
		Enabled:        false,
		AutomationMode: AutomationModeManual,
		// CoverageThreshold defaults to 50%: an NFO covering fewer than half of
		// the configured-type release groups MusicBrainz reports is flagged.
		// ReleaseTypes defaults to "Album,EP" (nfo.DefaultReleaseTypeFilter).
		Config: RuleConfig{Severity: "info", CoverageThreshold: 50, ReleaseTypes: "Album,EP"},
	},
}

// filesystemRules is the set of rule IDs that are truly filesystem-only with
// no API equivalent. Only rules that fundamentally cannot work without local
// file access belong here. Rules that happen to use filesystem data today but
// can be made API-compatible are tracked in separate issues:
//   - #725: image existence/dimension rules (thumb, fanart, logo, banner)
//   - #726: NFO content rules (nfo_has_mbid)
//
// artist_id_mismatch and directory_name_mismatch are inherently filesystem-only
// (they compare directory names against artist names) but are not in this map
// because they already return nil for pathless artists without needing to be
// disabled globally. They are categorized as API-compatible (no-op for API artists).
//
// extraneous_images (#728) and backdrop_sequencing now have DB-based checker
// paths that run when a.Path is empty, so they are no longer filesystem-only.
var filesystemRules = map[string]bool{
	RuleNFOExists: true, // NFO is a local file format with no API equivalent
	// discography_populated reads <album> entries from the on-disk artist.nfo
	// and writes merged release groups back to it. There is no DB or API
	// equivalent for the NFO discography, so the rule needs a local path.
	RuleDiscographyPopulated: true,
}

// IsFilesystemDependent reports whether a rule requires a local library with a
// filesystem path. Rules that only inspect database or API metadata return false.
func IsFilesystemDependent(ruleID string) bool {
	return filesystemRules[ruleID]
}

// tagFilesystemDependent sets the FilesystemDependent field on each rule
// based on the filesystemRules map. Called after loading rules from the database.
func tagFilesystemDependent(rules []Rule) {
	for i := range rules {
		rules[i].FilesystemDependent = filesystemRules[rules[i].ID]
	}
}

// DefaultRules returns a copy of the built-in rule definitions with
// FilesystemDependent populated. Intended for documentation codegen only;
// do not call in production code paths.
func DefaultRules() []Rule {
	rules := append([]Rule(nil), defaultRules...)
	tagFilesystemDependent(rules)
	return rules
}

// snapshotThrottleTTL is the minimum interval between health snapshot writes.
// Concurrent GET /api/v1/reports/health requests under load all call
// RecordHealthSnapshot; the throttle prevents them from queuing on SQLite's
// single-writer lock and flooding the health_history table with near-identical
// rows.
const snapshotThrottleTTL = 60 * time.Second

// Clock is the time source used by Service for timestamp writes. The default
// implementation delegates to time.Now. Tests inject a fake clock to advance
// time without sleeping.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock implementation.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Service provides rule data operations.
type Service struct {
	db *sql.DB

	// clock is the time source for all timestamp writes. Defaults to realClock.
	// Override with WithClock in tests to eliminate time.Sleep calls.
	clock Clock

	// logger is the package logger; defaults to slog.Default() and can be
	// overridden via WithLogger for tests that want to capture output.
	logger *slog.Logger

	// snapshotMu guards lastSnapshotAt to ensure at most one snapshot is
	// recorded per throttle window across concurrent handler goroutines.
	snapshotMu     sync.Mutex
	lastSnapshotAt time.Time

	// listCallCount tracks the number of times List has been called.
	// Accessed atomically; used by tests to verify cache hit behavior.
	listCallCount int64
}

// NewService creates a rule service.
func NewService(db *sql.DB) *Service {
	return &Service{
		db:     db,
		clock:  realClock{},
		logger: slog.Default(),
	}
}

// WithClock attaches a clock to the service. Intended for tests that need to
// control the wall time seen by SeedDefaults and Update without sleeping.
func (s *Service) WithClock(c Clock) *Service {
	if c != nil {
		s.clock = c
	}
	return s
}

// WithLogger attaches a logger for the rule service. Defaults to slog.Default()
// when not set. Useful for tests that want to capture log output.
func (s *Service) WithLogger(logger *slog.Logger) *Service {
	if logger != nil {
		s.logger = logger
	}
	return s
}

// SeedDefaults inserts built-in rules and refreshes their cosmetic fields
// (name, description) on existing rows so upgraded installs pick up improved
// copy. User-customisable columns (enabled, automation_mode, config) are
// preserved across upgrades.
//
// IMPORTANT: the cosmetic refresh deliberately does NOT bump rules.updated_at.
// The dirty-tracking query in artist.ListDirtyIDs treats rules.updated_at as
// the signal "this rule's evaluation outcomes might have changed", and a
// startup-time copy refresh has no effect on outcomes. Bumping updated_at
// here would invalidate every artist's rules_evaluated_at on every restart
// and force a full library re-evaluation on the next Run Rules pass.
//
// Newly inserted rules naturally have updated_at = now, which is the correct
// signal for the dirty-tracking query to schedule them on the next pass.
func (s *Service) SeedDefaults(ctx context.Context) error {
	now := s.clock.Now().Format(time.RFC3339)
	for i := range defaultRules {
		r := &defaultRules[i]
		autoMode := r.AutomationMode
		if autoMode == "" {
			autoMode = "auto"
		}
		// INSERT OR IGNORE: a brand-new rule gets created_at = updated_at = now
		// (and the dirty-tracking JOIN in ListDirtyIDs will pick it up via
		// updated_at > artists.rules_evaluated_at). Existing rules are not
		// touched here; the cosmetic UPDATE below handles those.
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO rules (id, name, description, category, enabled, automation_mode, config, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, r.ID, r.Name, r.Description, r.Category, dbutil.BoolToInt(r.Enabled),
			autoMode, MarshalConfig(r.Config), now, now); err != nil {
			return fmt.Errorf("seeding rule %s: %w", r.ID, err)
		}

		// Cosmetic refresh: name + description only, NO updated_at bump
		// (see function doc for why). Skips the write entirely when the
		// stored values already match so SQLite avoids a needless write.
		if _, err := s.db.ExecContext(ctx, `
			UPDATE rules
			SET name = ?, description = ?
			WHERE id = ? AND (name != ? OR description != ?)
		`, r.Name, r.Description, r.ID, r.Name, r.Description); err != nil {
			return fmt.Errorf("refreshing rule metadata for %s: %w", r.ID, err)
		}
	}

	// Migrate deprecated logo_trimmable rule: dismiss any open violations
	// and delete the rule definition so it no longer appears in the UI.
	if err := s.migrateDeprecatedRule(ctx, ruleLogoTrimmableDeprecated); err != nil {
		return fmt.Errorf("migrating deprecated rule %s: %w", ruleLogoTrimmableDeprecated, err)
	}

	return nil
}

// migrateDeprecatedRule dismisses open violations for a removed rule and
// deletes its rule definition. This is idempotent: if the rule does not
// exist, no error is returned.
func (s *Service) migrateDeprecatedRule(ctx context.Context, ruleID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations SET status = 'dismissed'
		WHERE rule_id = ? AND status = 'open'
	`, ruleID)
	if err != nil {
		return fmt.Errorf("dismissing violations for %s: %w", ruleID, err)
	}

	_, err = s.db.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, ruleID)
	if err != nil {
		return fmt.Errorf("deleting rule %s: %w", ruleID, err)
	}
	return nil
}

// List returns all rules.
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	atomic.AddInt64(&s.listCallCount, 1)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, category, enabled, automation_mode, config, created_at, updated_at
		FROM rules ORDER BY category, name
	`)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var rules []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning rule row: %w", err)
		}
		rules = append(rules, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rule rows: %w", err)
	}
	tagFilesystemDependent(rules)
	return rules, nil
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
	r.FilesystemDependent = filesystemRules[r.ID]
	return r, nil
}

// Update modifies a rule's enabled state, automation mode, and config.
//
// The bumped rules.updated_at is the dirty-tracking signal: artist.ListDirtyIDs
// JOINs against rules WHERE enabled = 1 AND updated_at > rules_evaluated_at,
// so any enabled-rule update naturally schedules every affected artist for
// re-evaluation on the next pass. Disabling a rule (enabled = 0) does NOT
// schedule re-evaluation because the JOIN filters out disabled rules -- the
// remaining enabled rules' outcomes are unchanged by the disable, so a full
// library walk would be wasted work.
//
// When a rule is disabled, active violations for that rule are soft-resolved
// (status='resolved', resolved_at=now) so they stop counting against compliance
// scores. Without this cleanup the violation rows would persist indefinitely:
// the rule no longer runs, so nothing would ever mark them resolved. Historical
// dismissed/resolved rows are left alone; only open and pending_choice
// violations (the ones still surfacing in the UI as "needs attention") are
// updated.
//
// Issue #1143: prefer soft cleanup over a hard DELETE so the violation history
// (when the row was first created, when it was last seen, etc.) is preserved
// for audit. The 'resolved' state mirrors the path the auto-fix pipeline takes
// when a fixer succeeds, so downstream consumers (compliance counts, history
// charts) treat the two transitions identically.
func (s *Service) Update(ctx context.Context, r *Rule) error {
	r.UpdatedAt = s.clock.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE rules SET enabled = ?, automation_mode = ?, config = ?, updated_at = ? WHERE id = ?
	`, dbutil.BoolToInt(r.Enabled), r.AutomationMode, MarshalConfig(r.Config),
		r.UpdatedAt.Format(time.RFC3339), r.ID)
	if err != nil {
		return fmt.Errorf("updating rule: %w", err)
	}

	if !r.Enabled {
		if err := s.cleanupDisabledRuleState(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// cleanupDisabledRuleState soft-resolves any open or pending-choice violations
// for the given rule and deletes its pass/fail history rows. Called from every
// path that flips a rule from enabled to disabled (manual Update, automatic
// DisableFilesystemRules) so that all disable transitions converge on the
// same end state.
//
// Soft-resolve (UPDATE) preserves audit history per #1143; rule_results uses
// a hard DELETE because no existing row is authoritative once the rule stops
// running.
func (s *Service) cleanupDisabledRuleState(ctx context.Context, ruleID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE rule_violations
		    SET status = ?, resolved_at = ?, updated_at = ?
		  WHERE rule_id = ? AND status IN (?, ?)`,
		ViolationStatusResolved, now, now,
		ruleID, ViolationStatusOpen, ViolationStatusPendingChoice,
	); err != nil {
		return fmt.Errorf("cleaning up violations after disable: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM rule_results WHERE rule_id = ?`, ruleID,
	); err != nil {
		return fmt.Errorf("cleaning up rule_results after disable: %w", err)
	}
	return nil
}

// DisableFilesystemRules disables all enabled filesystem-dependent rules.
// Called when the last local library is removed so that rules requiring
// filesystem access are automatically turned off. Returns the number of
// rules that were disabled.
func (s *Service) DisableFilesystemRules(ctx context.Context) (int, error) {
	// Build the list of filesystem-dependent rule IDs for the IN clause.
	ids := make([]string, 0, len(filesystemRules))
	for id := range filesystemRules {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		idArgs[i] = id
	}

	// Identify which fs rules are currently enabled. We need the specific IDs
	// so the cleanup pass below only touches rules that this call actually
	// disabled, mirroring what Update does for a single rule.
	selectQuery := fmt.Sprintf( //nolint:gosec // G201: only "?" placeholders are interpolated; all values are parameterized
		`SELECT id FROM rules WHERE enabled = 1 AND id IN (%s)`,
		strings.Join(placeholders, ", "),
	)
	rows, err := s.db.QueryContext(ctx, selectQuery, idArgs...)
	if err != nil {
		return 0, fmt.Errorf("disabling filesystem rules: selecting enabled: %w", err)
	}
	var toDisable []string
	scanErr := func() error {
		defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("disabling filesystem rules: scanning enabled: %w", err)
			}
			toDisable = append(toDisable, id)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("disabling filesystem rules: iterating enabled: %w", err)
		}
		return nil
	}()
	if scanErr != nil {
		return 0, scanErr
	}
	if len(toDisable) == 0 {
		return 0, nil
	}

	// Flip enabled=0 in one statement.
	disablePlaceholders := make([]string, len(toDisable))
	disableArgs := make([]any, 0, len(toDisable)+1)
	now := s.clock.Now().Format(time.RFC3339)
	disableArgs = append(disableArgs, now)
	for i, id := range toDisable {
		disablePlaceholders[i] = "?"
		disableArgs = append(disableArgs, id)
	}
	updateQuery := fmt.Sprintf( //nolint:gosec // G201: only "?" placeholders are interpolated; all values are parameterized
		`UPDATE rules SET enabled = 0, updated_at = ? WHERE id IN (%s)`,
		strings.Join(disablePlaceholders, ", "),
	)
	if _, err := s.db.ExecContext(ctx, updateQuery, disableArgs...); err != nil {
		return 0, fmt.Errorf("disabling filesystem rules: %w", err)
	}

	// Run the same disable cleanup that Update performs, so auto-disabled
	// filesystem rules don't leave stale open violations or pass/fail rows.
	for _, id := range toDisable {
		if err := s.cleanupDisabledRuleState(ctx, id); err != nil {
			return 0, fmt.Errorf("disabling filesystem rules: cleanup for %s: %w", id, err)
		}
	}

	return len(toDisable), nil
}

// RecordHealthSnapshot inserts a row into the health_history table, subject to
// a per-service rate limit of one write per snapshotThrottleTTL window.
// Concurrent requests that arrive within the throttle window are silently
// skipped to avoid queuing writes on SQLite's single-writer lock and to
// prevent near-duplicate rows in the history table.
func (s *Service) RecordHealthSnapshot(ctx context.Context, totalArtists, compliantArtists int, score float64) error {
	now := time.Now()

	s.snapshotMu.Lock()
	if !s.lastSnapshotAt.IsZero() && now.Sub(s.lastSnapshotAt) < snapshotThrottleTTL {
		s.snapshotMu.Unlock()
		return nil
	}
	s.lastSnapshotAt = now
	s.snapshotMu.Unlock()

	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO health_history (id, total_artists, compliant_artists, score, recorded_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, totalArtists, compliantArtists, score,
		now.UTC().Format(time.RFC3339))
	if err != nil {
		// Only reset lastSnapshotAt if this goroutine's slot is still current.
		// Without the equality check, a racing goroutine that already claimed a
		// newer slot could have its throttle cleared by our error path.
		s.snapshotMu.Lock()
		if s.lastSnapshotAt.Equal(now) {
			s.lastSnapshotAt = time.Time{}
		}
		s.snapshotMu.Unlock()
		return fmt.Errorf("recording health snapshot: %w", err)
	}
	return nil
}

// ViolationSummary tracks how many non-excluded artists currently fail a specific rule.
type ViolationSummary struct {
	RuleID   string `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Count    int    `json:"count"`
	Severity string `json:"severity"`
}

// TopViolationSummaries returns the most common open violations grouped by rule,
// limited to the given count. Only non-excluded artists are considered.
func (s *Service) TopViolationSummaries(ctx context.Context, limit int) ([]ViolationSummary, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT rv.rule_id, r.name, COUNT(*) AS cnt,
		       CASE MAX(CASE rv.severity
		           WHEN 'error' THEN 3
		           WHEN 'warning' THEN 2
		           WHEN 'info' THEN 1
		           ELSE 0
		       END)
		           WHEN 3 THEN 'error'
		           WHEN 2 THEN 'warning'
		           WHEN 1 THEN 'info'
		           ELSE 'warning'
		       END AS severity
		FROM rule_violations rv
		JOIN rules r ON r.id = rv.rule_id
		JOIN artists a ON a.id = rv.artist_id AND a.is_excluded = 0
		WHERE rv.status IN ('open', 'pending_choice')
		GROUP BY rv.rule_id
		ORDER BY cnt DESC, rv.rule_id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying top violation summaries: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var results []ViolationSummary
	for rows.Next() {
		var vs ViolationSummary
		if err := rows.Scan(&vs.RuleID, &vs.RuleName, &vs.Count, &vs.Severity); err != nil {
			return nil, fmt.Errorf("scanning violation summary: %w", err)
		}
		results = append(results, vs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating violation summaries: %w", err)
	}

	return results, nil
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
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// GetViolationTrend returns daily violation creation and resolution counts over a date range.
// days is the number of past days to include; if <= 0, defaults to 30.
// The result includes one entry per calendar day within the range, even if both counts are zero.
func (s *Service) GetViolationTrend(ctx context.Context, days int) ([]ViolationTrendPoint, error) {
	if days <= 0 {
		days = 30
	}
	// Hard cap prevents unbounded allocations from untrusted input.
	const maxDays = 365
	if days > maxDays {
		days = maxDays
	}

	// Build a full list of date strings covering the range.
	now := time.Now().UTC()
	start := now.AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)
	// end is the start of the day after "now", so the range [start, end) covers all days.
	end := now.Truncate(24*time.Hour).AddDate(0, 0, 1)

	startStr := start.Format(time.RFC3339)
	endStr := end.Format(time.RFC3339)

	// Use the constant directly so CodeQL can verify the bound statically.
	dateMap := make(map[string]*ViolationTrendPoint, maxDays)
	dates := make([]string, 0, maxDays)
	for i := range days {
		d := start.AddDate(0, 0, i).Format(time.DateOnly)
		dates = append(dates, d)
		dateMap[d] = &ViolationTrendPoint{Date: d}
	}

	// Query daily created counts. Use raw timestamp range for index friendliness;
	// date() is only used in SELECT/GROUP BY for bucketing.
	createdRows, err := s.db.QueryContext(ctx, `
		SELECT date(created_at) AS day, COUNT(*) AS cnt
		FROM rule_violations
		WHERE created_at >= ? AND created_at < ?
		GROUP BY day
	`, startStr, endStr)
	if err != nil {
		return nil, fmt.Errorf("querying violation created trend: %w", err)
	}
	defer createdRows.Close() //nolint:errcheck // Close error not actionable on cleanup

	for createdRows.Next() {
		var day string
		var cnt int
		if err := createdRows.Scan(&day, &cnt); err != nil {
			return nil, fmt.Errorf("scanning created trend row: %w", err)
		}
		if p, ok := dateMap[day]; ok {
			p.Created = cnt
		}
	}
	if err := createdRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating created trend rows: %w", err)
	}

	// Query daily resolved counts. Same raw timestamp range approach.
	resolvedRows, err := s.db.QueryContext(ctx, `
		SELECT date(resolved_at) AS day, COUNT(*) AS cnt
		FROM rule_violations
		WHERE resolved_at IS NOT NULL
		  AND resolved_at >= ? AND resolved_at < ?
		GROUP BY day
	`, startStr, endStr)
	if err != nil {
		return nil, fmt.Errorf("querying violation resolved trend: %w", err)
	}
	defer resolvedRows.Close() //nolint:errcheck // Close error not actionable on cleanup

	for resolvedRows.Next() {
		var day string
		var cnt int
		if err := resolvedRows.Scan(&day, &cnt); err != nil {
			return nil, fmt.Errorf("scanning resolved trend row: %w", err)
		}
		if p, ok := dateMap[day]; ok {
			p.Resolved = cnt
		}
	}
	if err := resolvedRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating resolved trend rows: %w", err)
	}

	// Assemble in date order.
	result := make([]ViolationTrendPoint, 0, maxDays)
	for _, d := range dates {
		result = append(result, *dateMap[d])
	}
	return result, nil
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting latest health snapshot: %w", err)
	}
	return snap, nil
}

// UpsertViolation inserts or updates a rule violation in the notifications
// store, and atomically writes the sibling rule_results row that records
// "this (artist, rule) pair currently fails" (issue #699 slice 1). Both
// writes happen inside a single transaction so a pipeline pass never leaves
// the two tables disagreeing about whether a rule is failing.
//
// The rule_results row uses COALESCE on first_failed_at so the "how long has
// this been broken" timestamp survives across repeated fails. The violation
// row continues to use (rule_id, artist_id) as its natural upsert key.
func (s *Service) UpsertViolation(ctx context.Context, v *RuleViolation) error {
	if v.ID == "" {
		v.ID = uuid.New().String()
	}
	v.UpdatedAt = s.clock.Now()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = v.UpdatedAt
	}

	// Resolved / dismissed violations leave rule_results alone: a resolved
	// violation means the rule is no longer failing, and the next pipeline
	// pass will stamp the pass row via UpsertRuleResultPass. Writing fail
	// rows for them would be wrong (and would re-arm first_failed_at).
	writeResultRow := v.Status == ViolationStatusOpen || v.Status == ViolationStatusPendingChoice

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning upsert-violation transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after commit success is a no-op; on error path the original error is what callers act on

	// Issue #1107: dismissed is terminal from the user's perspective. Once a
	// row is stored as 'dismissed', re-evaluation must NOT clobber the status
	// or dismissed_at back to fresh values; otherwise every Run Rules pass
	// resurrects a violation the user explicitly hid. We preserve status and
	// dismissed_at when the existing row is dismissed, while still letting
	// resolved -> open transitions happen normally (a user-deleted file
	// should re-open the violation that previously fixed it).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status, candidates, dismissed_at, resolved_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_id, artist_id) DO UPDATE SET
			artist_name = excluded.artist_name,
			severity = excluded.severity,
			message = excluded.message,
			fixable = excluded.fixable,
			status = CASE WHEN rule_violations.status = 'dismissed'
			              THEN rule_violations.status
			              ELSE excluded.status END,
			candidates = excluded.candidates,
			dismissed_at = CASE WHEN rule_violations.status = 'dismissed'
			                    THEN rule_violations.dismissed_at
			                    ELSE excluded.dismissed_at END,
			resolved_at = excluded.resolved_at,
			updated_at = excluded.updated_at
	`, v.ID, v.RuleID, v.ArtistID, v.ArtistName, v.Severity, v.Message,
		dbutil.BoolToInt(v.Fixable), v.Status, marshalCandidates(v.Candidates),
		dbutil.NilableTime(v.DismissedAt), dbutil.NilableTime(v.ResolvedAt),
		v.CreatedAt.Format(time.RFC3339), v.UpdatedAt.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("upserting violation: %w", err)
	}

	// On repeat failures, the ON CONFLICT branch above preserves the
	// existing rule_violations.id and ignores the fresh v.ID we generated.
	// If we pass the fresh (non-persisted) v.ID to upsertRuleResultFailExec,
	// the rule_results.violation_id FK points at a row that does not exist
	// and the transaction rolls back. Re-read the persisted id inside the
	// same tx and sync v.ID so the result-row write lands cleanly and the
	// caller observes the authoritative id.
	//
	// Issue #1107: also re-read the persisted status so writeResultRow
	// reflects the post-upsert state. When the stored row was 'dismissed',
	// the ON CONFLICT preserves it and we must NOT write a fail row even if
	// the incoming v.Status is 'open'. Otherwise rule_results would carry a
	// fresh fail for a violation the user explicitly hid.
	var persistedStatus string
	if err := tx.QueryRowContext(ctx,
		`SELECT id, status FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		v.RuleID, v.ArtistID,
	).Scan(&v.ID, &persistedStatus); err != nil {
		return fmt.Errorf("loading persisted violation id: %w", err)
	}
	writeResultRow = writeResultRow &&
		(persistedStatus == ViolationStatusOpen || persistedStatus == ViolationStatusPendingChoice)

	if writeResultRow {
		if err := upsertRuleResultFailExec(ctx, tx, v.ArtistID, v.RuleID, v.ID, v.Message, v.UpdatedAt); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing upsert-violation transaction: %w", err)
	}
	return nil
}

// ListViolations returns rule violations filtered by status.
// If status is empty, returns all violations.
func (s *Service) ListViolations(ctx context.Context, status string) ([]RuleViolation, error) {
	query := `SELECT rv.id, rv.rule_id, rv.artist_id, rv.artist_name, COALESCE(l.name, '') AS library_name, rv.severity, rv.message, rv.fixable, rv.status, rv.candidates, rv.dismissed_at, rv.resolved_at, rv.created_at, rv.updated_at FROM rule_violations rv LEFT JOIN artists a ON a.id = rv.artist_id LEFT JOIN libraries l ON l.id = (SELECT al.library_id FROM artist_libraries al WHERE al.artist_id = a.id ORDER BY datetime(al.added_at), al.library_id LIMIT 1)`
	args := []any{}
	if status == "active" {
		// active = open + pending_choice (violations that need attention)
		query += ` WHERE rv.status IN (?, ?)`
		args = append(args, ViolationStatusOpen, ViolationStatusPendingChoice)
	} else if status != "" {
		query += ` WHERE rv.status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY rv.created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing violations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// triClauses appends parameterized IN / NOT IN clauses for a tri-state filter
// over the given column expression. Include values produce "<col> IN (?,?...)",
// exclude values produce "<col> NOT IN (?,?...)", and an empty side produces no
// clause. Only "?" placeholders are concatenated into the SQL text; the values
// themselves are always passed as args, never interpolated, so this is safe
// against SQL injection.
func triClauses(col string, f TriFilter, whereClauses []string, args []any) ([]string, []any) {
	build := func(op string, values []string) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, len(values))
		for i, v := range values {
			placeholders[i] = "?"
			args = append(args, v)
		}
		whereClauses = append(whereClauses, col+" "+op+" ("+strings.Join(placeholders, ", ")+")")
	}
	build("IN", f.Include)
	build("NOT IN", f.Exclude)
	return whereClauses, args
}

// buildPlaceholders returns a comma-separated "?, ?, ..." string with one
// placeholder per value, plus the values as an []any ready to append to a
// query's arg list. Used by the library EXISTS subqueries, which embed an
// IN (...) list. Values are never interpolated into SQL; only the "?" markers
// are concatenated.
func buildPlaceholders(values []string) (placeholders string, args []any) {
	marks := make([]string, len(values))
	args = make([]any, len(values))
	for i, v := range values {
		marks[i] = "?"
		args[i] = v
	}
	return strings.Join(marks, ", "), args
}

// fixableToBits maps the user-facing fixable values ("yes"/"no") to the
// rv.fixable column's stored integers (1/0), dropping any unrecognized value.
// It is used to translate a fixable TriFilter into a tri-state filter over the
// integer column.
func fixableToBits(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		switch v {
		case "yes":
			out = append(out, "1")
		case "no":
			out = append(out, "0")
		}
	}
	return out
}

// buildViolationFilter constructs WHERE clauses and args from ViolationListParams.
// It returns the clauses, args, whether a JOIN on the rules table is needed
// (needJoin, for the category filter), and whether the artists table (`a`)
// must be joined (needArtistJoin, set when either library side is non-empty
// because the library [NOT] EXISTS subquery references the `a` alias). Callers
// use needArtistJoin instead of scanning the emitted SQL for the
// "FROM artist_libraries" substring.
func buildViolationFilter(p ViolationListParams) (whereClauses []string, args []any, needJoin bool, needArtistJoin bool) {
	// Defensively normalize every tri-state dimension before emitting SQL so an
	// overlapping include/exclude set can never produce clause semantics that
	// disagree with TriFilter.Normalized() (dedupe, exclude-wins on single-value
	// full overlap, whitelist: a non-empty Include drops a stale Exclude). The
	// parse boundary (parseTriFilter) already normalizes user input, but internal
	// or future callers that construct ViolationListParams directly may not.
	p.Severity = p.Severity.Normalized()
	p.Category = p.Category.Normalized()
	p.RuleID = p.RuleID.Normalized()
	p.LibraryID = p.LibraryID.Normalized()
	p.Fixable = p.Fixable.Normalized()

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

	// Severity filter (tri-state include/exclude over rv.severity).
	whereClauses, args = triClauses("rv.severity", p.Severity, whereClauses, args)

	// Rule ID filter (tri-state include/exclude over rv.rule_id).
	whereClauses, args = triClauses("rv.rule_id", p.RuleID, whereClauses, args)

	// Artist ID filter (scalar; programmatic single-artist scope, not user tri-state).
	if p.ArtistID != "" {
		whereClauses = append(whereClauses, "rv.artist_id = ?")
		args = append(args, p.ArtistID)
	}

	// Category filter requires joining the rules table. Tri-state over r.category.
	if !p.Category.IsEmpty() {
		needJoin = true
		whereClauses, args = triClauses("r.category", p.Category, whereClauses, args)
	}

	// Free-text search across artist name, message, and rule ID.
	//
	// User input is interpolated into a SQL LIKE pattern, so the LIKE
	// metacharacters %, _, and the escape character itself (\) must be
	// escaped before composing the pattern. Without this, a user searching
	// for "100%" would match every row containing "100" because % is the
	// "any sequence" wildcard. The escape character is escaped first so a
	// literal backslash in the user input is not silently doubled. The
	// SQL clause declares ESCAPE '\' so SQLite treats the escapes as
	// literals at match time.
	if p.Search != "" {
		escaped := dbutil.EscapeLike(p.Search)
		like := "%" + escaped + "%"
		whereClauses = append(whereClauses, `(rv.artist_name LIKE ? ESCAPE '\' OR rv.message LIKE ? ESCAPE '\' OR rv.rule_id LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}

	// Library filter via artist_libraries membership (the artists table is
	// already joined for the library_name display column). Tri-state: include
	// means "artist belongs to one of these libraries" (EXISTS ... IN (...)),
	// exclude means "artist belongs to none of these" (NOT EXISTS ... IN (...)).
	// Either side sets needArtistJoin so the facet-count helpers materialize the
	// `a` (artists) join the subquery references, without scanning the emitted
	// SQL string.
	if len(p.LibraryID.Include) > 0 {
		needArtistJoin = true
		placeholders, libArgs := buildPlaceholders(p.LibraryID.Include)
		whereClauses = append(whereClauses, "EXISTS (SELECT 1 FROM artist_libraries al WHERE al.artist_id = a.id AND al.library_id IN ("+placeholders+"))")
		args = append(args, libArgs...)
	}
	if len(p.LibraryID.Exclude) > 0 {
		needArtistJoin = true
		placeholders, libArgs := buildPlaceholders(p.LibraryID.Exclude)
		whereClauses = append(whereClauses, "NOT EXISTS (SELECT 1 FROM artist_libraries al WHERE al.artist_id = a.id AND al.library_id IN ("+placeholders+"))")
		args = append(args, libArgs...)
	}

	// Fixable filter (tri-state). The user-facing values "yes"/"no" map to the
	// rv.fixable integer column (1/0) before building the IN / NOT IN clause.
	fixableBits := TriFilter{
		Include: fixableToBits(p.Fixable.Include),
		Exclude: fixableToBits(p.Fixable.Exclude),
	}
	whereClauses, args = triClauses("rv.fixable", fixableBits, whereClauses, args)

	return whereClauses, args, needJoin, needArtistJoin
}

// buildViolationFromClause returns the FROM/JOIN portion of a violation query.
func buildViolationFromClause(needJoin bool) string {
	q := ` FROM rule_violations rv LEFT JOIN artists a ON a.id = rv.artist_id LEFT JOIN libraries l ON l.id = (SELECT al.library_id FROM artist_libraries al WHERE al.artist_id = a.id ORDER BY datetime(al.added_at), al.library_id LIMIT 1)`
	if needJoin {
		q += ` JOIN rules r ON r.id = rv.rule_id`
	}
	return q
}

// buildViolationOrderClause returns the ORDER BY portion for a violation
// query. The HTTP boundary (dbutil.ValidateSortKey via the api package
// helpers) rejects unknown sort keys with 400, so by the time control
// reaches this helper p.Sort is either empty or a member of the allowlist.
// The default branch ("severity DESC, created_at DESC") remains as a second
// line of defense for non-HTTP callers (tests, internal call sites).
func buildViolationOrderClause(p ViolationListParams) string {
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
		clause := " ORDER BY " + col + " " + order
		if p.Sort != "created_at" {
			clause += ", rv.created_at DESC"
		}
		return clause
	}
	// Default sort: severity DESC (errors first), then newest
	return " ORDER BY " + severityRank + " DESC, rv.created_at DESC"
}

// ListViolationsFiltered returns violations matching the given params with dynamic SQL.
func (s *Service) ListViolationsFiltered(ctx context.Context, p ViolationListParams) ([]RuleViolation, error) {
	// The artists table is always joined by buildViolationFromClause, so the
	// library subquery's `a` alias is satisfied regardless of needArtistJoin.
	whereClauses, args, needJoin, _ := buildViolationFilter(p)

	// Build query -- always join artists/libraries for library_name context
	query := `SELECT rv.id, rv.rule_id, rv.artist_id, rv.artist_name, COALESCE(l.name, '') AS library_name, rv.severity, rv.message, rv.fixable, rv.status, rv.candidates, rv.dismissed_at, rv.resolved_at, rv.created_at, rv.updated_at`
	query += buildViolationFromClause(needJoin)
	if len(whereClauses) > 0 {
		query += " WHERE " + joinStrings(whereClauses, " AND ")
	}
	query += buildViolationOrderClause(p) //nolint:gosec // G202: order clause uses whitelisted column map

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing filtered violations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// ListViolationsFilteredPaged returns violations matching the given params with pagination.
// It returns the matching violations, the total count (before pagination), and any error.
// When p.Limit <= 0, all results are returned (no LIMIT clause) but the total count is
// still computed for caller convenience.
func (s *Service) ListViolationsFilteredPaged(ctx context.Context, p ViolationListParams) ([]RuleViolation, int, error) {
	// The artists table is always joined by buildViolationFromClause, so the
	// library subquery's `a` alias is satisfied regardless of needArtistJoin.
	whereClauses, args, needJoin, _ := buildViolationFilter(p)

	fromClause := buildViolationFromClause(needJoin)
	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = " WHERE " + joinStrings(whereClauses, " AND ")
	}

	// Count total matching rows.
	countQuery := "SELECT COUNT(*)" + fromClause + whereClause
	var total int
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting filtered violations: %w", err)
	}

	// Build the data query with the same filters.
	query := `SELECT rv.id, rv.rule_id, rv.artist_id, rv.artist_name, COALESCE(l.name, '') AS library_name, rv.severity, rv.message, rv.fixable, rv.status, rv.candidates, rv.dismissed_at, rv.resolved_at, rv.created_at, rv.updated_at`
	query += fromClause + whereClause
	query += buildViolationOrderClause(p) //nolint:gosec // G202: order clause uses whitelisted column map

	// Append LIMIT/OFFSET when a positive limit is specified.
	dataArgs := append([]any{}, args...)
	if p.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		dataArgs = append(dataArgs, p.Limit, p.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, dataArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing filtered violations (paged): %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var violations []RuleViolation
	for rows.Next() {
		v, err := scanViolation(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scanning filtered violation row: %w", err)
		}
		violations = append(violations, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return violations, total, nil
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
			"backdrop":   "image",
			"extraneous": "image",
			"bio":        "metadata",
			"artist":     "metadata",
			"name":       "metadata",
			"origin":     "metadata",
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

	for i := range violations {
		v := &violations[i]
		key, label := keyFunc(*v)
		g, exists := groups[key]
		if !exists {
			g = &ViolationGroup{Key: key, Label: label}
			groups[key] = g
			order = append(order, key)
		}
		g.Violations = append(g.Violations, *v)
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
		SELECT rv.id, rv.rule_id, rv.artist_id, rv.artist_name, COALESCE(l.name, '') AS library_name, rv.severity, rv.message, rv.fixable, rv.status, rv.candidates, rv.dismissed_at, rv.resolved_at, rv.created_at, rv.updated_at
		FROM rule_violations rv LEFT JOIN artists a ON a.id = rv.artist_id LEFT JOIN libraries l ON l.id = (SELECT al.library_id FROM artist_libraries al WHERE al.artist_id = a.id ORDER BY datetime(al.added_at), al.library_id LIMIT 1) WHERE rv.id = ?
	`, id)
	v, err := scanViolation(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
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

// ResolveViolationIfActive transitions an open or pending_choice violation
// for the given (rule_id, artist_id) pair to status='resolved'. Dismissed and
// already-resolved rows are left untouched: dismissed is terminal (#1107) and
// resolved is idempotent. Returns true when a row was actually updated so the
// caller can decide whether to log or emit an event.
//
// Issue #1105: the rule pipeline calls this for every (rule, artist) pair
// where the rule was considered but no longer reports a violation, so a row
// the user fixed out-of-band (e.g. dropped a logo.png into the artist
// directory) finally clears from the dashboard on the next Run Rules.
func (s *Service) ResolveViolationIfActive(ctx context.Context, ruleID, artistID string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations
		   SET status = ?, resolved_at = ?, updated_at = ?
		 WHERE rule_id = ? AND artist_id = ? AND status IN (?, ?)
	`, ViolationStatusResolved, now, now,
		ruleID, artistID, ViolationStatusOpen, ViolationStatusPendingChoice)
	if err != nil {
		return false, fmt.Errorf("resolving active violation: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountActiveViolationsForArtist returns the count of active (open + pending_choice)
// violations for a specific artist.
func (s *Service) CountActiveViolationsForArtist(ctx context.Context, artistID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rule_violations
		WHERE artist_id = ? AND status IN (?, ?)
	`, artistID, ViolationStatusOpen, ViolationStatusPendingChoice).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting active violations for artist: %w", err)
	}
	return count, nil
}

// CountActiveViolationsBySeverity returns the count of active (open + pending_choice)
// violations grouped by severity level. Applies the facet-count pattern: all
// filter dimensions in p EXCEPT severity are applied. Pass a zero-value
// ViolationListParams to get global unfiltered counts.
func (s *Service) CountActiveViolationsBySeverity(ctx context.Context, p ViolationListParams) (map[string]int, error) {
	where, args, needJoin, needArtistJoin := countActiveWithFilter(p, "severity")
	// Severity is on rv; use rv alias consistently even when no join.
	from := ` FROM rule_violations rv`
	if needJoin {
		from += ` LEFT JOIN artists a ON a.id = rv.artist_id JOIN rules r ON r.id = rv.rule_id`
	} else if needArtistJoin {
		// The library [NOT] EXISTS subquery references the `a` alias, so we must
		// materialize the artists join even when the rules-table join is not
		// otherwise needed.
		from += ` LEFT JOIN artists a ON a.id = rv.artist_id`
	}
	query := `SELECT rv.severity, COUNT(*)` + from + where + ` GROUP BY rv.severity` //nolint:gosec // G202: from/where are built from whitelisted clauses with parameterized placeholders

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("counting active violations by severity: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// RuleViolationCount holds a rule ID, its human-readable name, and the count
// of active violations for that rule.
type RuleViolationCount struct {
	RuleID   string `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Count    int    `json:"count"`
}

// countActiveWithFilter builds the WHERE/JOIN portion of a facet-count query
// given a filter. It always forces Status=active and clears the dimension the
// caller is counting (so that dimension's own filter is not applied to its
// own counts, yielding standard facet-count semantics: "if I selected X,
// how many violations would remain?").
//
// excludeDim is the ViolationListParams field to clear before building the
// filter: "severity", "category", "rule", "library", or "fixable". Pass ""
// to apply the filter as-is.
//
// needArtistJoin is true when the remaining filter still references the `a`
// (artists) alias via a library [NOT] EXISTS subquery, so the caller must
// materialize that join even when needJoin (the rules-table join) is false.
func countActiveWithFilter(p ViolationListParams, excludeDim string) (where string, args []any, needJoin bool, needArtistJoin bool) {
	p.Status = "active"
	switch excludeDim {
	case "severity":
		p.Severity = TriFilter{}
	case "category":
		p.Category = TriFilter{}
	case "rule":
		p.RuleID = TriFilter{}
	case "library":
		p.LibraryID = TriFilter{}
	case "fixable":
		p.Fixable = TriFilter{}
	}
	clauses, args, needJoin, needArtistJoin := buildViolationFilter(p)
	if len(clauses) == 0 {
		return "", args, needJoin, needArtistJoin
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, needJoin, needArtistJoin
}

// CountActiveViolationsByRule returns the count of active (open + pending_choice)
// violations grouped by rule, along with the rule name. Only rules with at
// least one matching violation are returned. Applies the facet-count pattern:
// all filter dimensions in p EXCEPT rule/rule_id are applied, so the caller
// sees counts within the current filter context.
func (s *Service) CountActiveViolationsByRule(ctx context.Context, p ViolationListParams) ([]RuleViolationCount, error) {
	where, args, needJoin, needArtistJoin := countActiveWithFilter(p, "rule")
	// The rule counts always need the rules join for r.name and category filtering.
	from := ` FROM rule_violations rv JOIN rules r ON r.id = rv.rule_id`
	// needJoin signals the category filter needs `r` (already joined above).
	// needArtistJoin signals the library [NOT] EXISTS subquery references `a`.
	if needJoin || needArtistJoin {
		from += ` LEFT JOIN artists a ON a.id = rv.artist_id`
	}
	query := `SELECT rv.rule_id, r.name, COUNT(*) AS cnt` + from + where + ` GROUP BY rv.rule_id ORDER BY cnt DESC, rv.rule_id ASC` //nolint:gosec // G202: from/where are built from whitelisted clauses with parameterized placeholders

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("counting active violations by rule: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var counts []RuleViolationCount
	for rows.Next() {
		var rc RuleViolationCount
		if err := rows.Scan(&rc.RuleID, &rc.RuleName, &rc.Count); err != nil {
			return nil, fmt.Errorf("scanning rule violation count: %w", err)
		}
		counts = append(counts, rc)
	}
	return counts, rows.Err()
}

// CountActiveViolationsByLibrary returns the count of active (open + pending_choice)
// violations grouped by library ID. Applies the facet-count pattern:
// all filter dimensions in p EXCEPT library are applied.
func (s *Service) CountActiveViolationsByLibrary(ctx context.Context, p ViolationListParams) (map[string]int, error) {
	where, args, needJoin, _ := countActiveWithFilter(p, "library")
	// Library counts join artist_libraries (the M:N membership table)
	// directly so per-library facets reflect the authoritative
	// membership record. An artist that is a member of multiple
	// libraries contributes to each library's count.
	from := ` FROM rule_violations rv JOIN artist_libraries al ON al.artist_id = rv.artist_id JOIN artists a ON a.id = rv.artist_id`
	if needJoin {
		// needJoin signals a rules-table join for category filter.
		from += ` JOIN rules r ON r.id = rv.rule_id`
	}
	query := `SELECT al.library_id, COUNT(*) AS cnt` + from + where + ` GROUP BY al.library_id` //nolint:gosec // G202: from/where are built from whitelisted clauses with parameterized placeholders

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("counting active violations by library: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	counts := make(map[string]int)
	for rows.Next() {
		var libID string
		var cnt int
		if err := rows.Scan(&libID, &cnt); err != nil {
			return nil, fmt.Errorf("scanning library violation count: %w", err)
		}
		counts[libID] = cnt
	}
	return counts, rows.Err()
}

// CountActiveViolationsByFixable returns the count of active (open + pending_choice)
// violations grouped by fixable status. Applies the facet-count pattern:
// all filter dimensions in p EXCEPT fixable are applied.
func (s *Service) CountActiveViolationsByFixable(ctx context.Context, p ViolationListParams) (fixable int, notFixable int, err error) {
	where, args, needJoin, needArtistJoin := countActiveWithFilter(p, "fixable")
	from := ` FROM rule_violations rv`
	if needJoin {
		from += ` LEFT JOIN artists a ON a.id = rv.artist_id JOIN rules r ON r.id = rv.rule_id`
	} else if needArtistJoin {
		// The library filter requires the artist join even when category is not set.
		from += ` LEFT JOIN artists a ON a.id = rv.artist_id`
	}
	query := `SELECT rv.fixable, COUNT(*) AS cnt` + from + where + ` GROUP BY rv.fixable` //nolint:gosec // G202: from/where are built from whitelisted clauses with parameterized placeholders

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("counting active violations by fixable: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	for rows.Next() {
		var f, cnt int
		if err := rows.Scan(&f, &cnt); err != nil {
			return 0, 0, fmt.Errorf("scanning fixable violation count: %w", err)
		}
		if f == 1 {
			fixable = cnt
		} else {
			notFixable = cnt
		}
	}
	return fixable, notFixable, rows.Err()
}

// DismissViolationsForLibrary dismisses all active violations for artists
// that belong to the given library. This should be called before deleting
// a library with deleteArtists=false, because the cascade removes the
// artist_libraries membership row and the association is lost. Returns
// the number of violations dismissed.
func (s *Service) DismissViolationsForLibrary(ctx context.Context, libraryID string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	// In the M:N model, only dismiss violations for artists whose ONLY library
	// membership is the one being deleted. Artists that still belong to another
	// library after this deletion should keep their active violations.
	res, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations
		SET status = ?, dismissed_at = ?, updated_at = ?
		WHERE status IN (?, ?)
		AND artist_id IN (
			SELECT al.artist_id
			FROM artist_libraries al
			WHERE al.library_id = ?
			  AND NOT EXISTS (
				SELECT 1
				FROM artist_libraries other
				WHERE other.artist_id = al.artist_id
				  AND other.library_id <> ?
			  )
		)
	`, ViolationStatusDismissed, now, now, ViolationStatusOpen, ViolationStatusPendingChoice, libraryID, libraryID)
	if err != nil {
		return 0, fmt.Errorf("dismissing violations for library %s: %w", libraryID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountActiveViolationsByCategory returns the count of active (open + pending_choice)
// violations grouped by rule category (nfo, image, metadata). Applies the
// facet-count pattern: all filter dimensions in p EXCEPT category are applied.
// Pass a zero-value ViolationListParams to get global unfiltered counts.
func (s *Service) CountActiveViolationsByCategory(ctx context.Context, p ViolationListParams) (map[string]int, error) {
	where, args, _, needArtistJoin := countActiveWithFilter(p, "category")
	// Category counts always need the rules join (GROUP BY r.category).
	from := ` FROM rule_violations rv JOIN rules r ON r.id = rv.rule_id`
	if needArtistJoin {
		from += ` LEFT JOIN artists a ON a.id = rv.artist_id`
	}
	query := `SELECT r.category, COUNT(*)` + from + where + ` GROUP BY r.category` //nolint:gosec // G202: from/where are built from whitelisted clauses with parameterized placeholders

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("counting active violations by category: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	counts := map[string]int{"nfo": 0, "image": 0, "metadata": 0}
	for rows.Next() {
		var category string
		var count int
		if err := rows.Scan(&category, &count); err != nil {
			return nil, fmt.Errorf("scanning category count: %w", err)
		}
		switch category {
		case "nfo", "image", "metadata":
			counts[category] = count
		default:
			// Ignore unknown categories to keep the return shape stable.
		}
	}
	return counts, rows.Err()
}

// DismissOrphanedViolations dismisses all active violations whose artist_id
// no longer exists in the artists table OR whose artist holds no library
// membership (the artist was retained but every library it belonged to was
// removed). Returns the number dismissed.
func (s *Service) DismissOrphanedViolations(ctx context.Context) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations
		SET status = ?, dismissed_at = ?, updated_at = ?
		WHERE status IN (?, ?)
		AND (
			artist_id NOT IN (SELECT id FROM artists)
			OR artist_id NOT IN (SELECT artist_id FROM artist_libraries)
		)
	`, ViolationStatusDismissed, now, now, ViolationStatusOpen, ViolationStatusPendingChoice)
	if err != nil {
		return 0, fmt.Errorf("dismissing orphaned violations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReopenViolation resets a resolved violation back to open status, clearing
// the resolved_at timestamp. This is used by the undo mechanism to restore a
// violation that was resolved by a fix that was subsequently reverted.
func (s *Service) ReopenViolation(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE rule_violations
		SET status = ?, resolved_at = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, ViolationStatusOpen, now, id, ViolationStatusResolved)
	if err != nil {
		return fmt.Errorf("reopening violation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reopening violation (rows affected): %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrViolationNotFound, id)
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
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// GetViolationsForArtists batch-loads active violations (open and pending_choice)
// for the given artist IDs, returning a map keyed by artist ID. Each value is a
// slice of Violation structs derived from the rule_violations table (joined with
// rules for name and category). If artistIDs is empty, an empty map is returned
// without querying the database.
//
// Artist IDs are processed in chunks of 500 to stay within SQLite's default
// host-parameter limit of 999.
func (s *Service) GetViolationsForArtists(ctx context.Context, artistIDs []string) (map[string][]Violation, error) {
	if len(artistIDs) == 0 {
		return map[string][]Violation{}, nil
	}

	result := make(map[string][]Violation, len(artistIDs))
	const chunkSize = 500

	for i := 0; i < len(artistIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(artistIDs) {
			end = len(artistIDs)
		}
		// queryChunk is an inline function so that defer rows.Close() fires once
		// per chunk rather than at function return, keeping resource lifetimes
		// short and satisfying the sqlclosecheck linter.
		if err := s.queryViolationChunk(ctx, artistIDs[i:end], result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// queryViolationChunk executes a single IN-clause query for a chunk of artist IDs
// and merges the results into dest. It is called by GetViolationsForArtists to keep
// each rows.Close() in a deferred position.
func (s *Service) queryViolationChunk(ctx context.Context, chunk []string, dest map[string][]Violation) error {
	// Build parameterised IN clause for this chunk of artist IDs.
	// Status constants are passed as the first two bind parameters so that
	// the total parameter count per batch is chunkSize+2 (≤502), well under
	// SQLite's 999-parameter limit.
	placeholders := make([]string, len(chunk))
	args := make([]any, 0, len(chunk)+2)
	args = append(args, ViolationStatusOpen, ViolationStatusPendingChoice)
	for j, id := range chunk {
		placeholders[j] = "?"
		args = append(args, id)
	}

	//nolint:gosec // G202: only "?" placeholders concatenated, no user input
	query := `SELECT rv.artist_id, rv.rule_id, r.name, r.category, rv.severity, rv.message, rv.fixable
	FROM rule_violations rv
	JOIN rules r ON r.id = rv.rule_id
	WHERE rv.status IN (?, ?)
	  AND rv.artist_id IN (` + strings.Join(placeholders, ",") + `)
	ORDER BY rv.artist_id, rv.rule_id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying violations for artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	for rows.Next() {
		var artistID string
		var fixable int
		var v Violation
		if err := rows.Scan(&artistID, &v.RuleID, &v.RuleName, &v.Category, &v.Severity, &v.Message, &fixable); err != nil {
			return fmt.Errorf("scanning violation for artist: %w", err)
		}
		v.Fixable = fixable == 1
		dest[artistID] = append(dest[artistID], v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating violations for artists: %w", err)
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
	snap.RecordedAt = dbutil.ParseTime(recordedAt)
	return &snap, nil
}

// scanViolation scans a database row into a RuleViolation struct.
func scanViolation(row interface{ Scan(...any) error }) (*RuleViolation, error) {
	var v RuleViolation
	var fixable int
	var candidates string
	var createdAt, updatedAt, dismissedAt, resolvedAt sql.NullString
	var libraryName sql.NullString

	err := row.Scan(&v.ID, &v.RuleID, &v.ArtistID, &v.ArtistName, &libraryName, &v.Severity, &v.Message,
		&fixable, &v.Status, &candidates, &dismissedAt, &resolvedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}

	v.Fixable = fixable == 1
	if libraryName.Valid {
		v.LibraryName = libraryName.String
	}
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
