package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/sydlexius/stillwater/internal/encryption"
)

// SettingsService manages provider API keys and priority configuration
// using the settings key-value table.
type SettingsService struct {
	db        *sql.DB
	encryptor *encryption.Encryptor
}

// NewSettingsService creates a new SettingsService.
func NewSettingsService(db *sql.DB, encryptor *encryption.Encryptor) *SettingsService {
	return &SettingsService{db: db, encryptor: encryptor}
}

// apiKeySettingKey returns the settings table key for a provider's API key.
func apiKeySettingKey(name ProviderName) string {
	return fmt.Sprintf("provider.%s.api_key", name)
}

// prioritySettingKey returns the settings table key for a field's priority list.
func prioritySettingKey(field string) string {
	return fmt.Sprintf("provider.priority.%s", field)
}

func priorityDisabledKey(field string) string {
	return fmt.Sprintf("provider.priority.%s.disabled", field)
}

// ctxKeyOverride is the context key for per-request API key overrides.
// This lets handlers inject an unsaved key so providers read it during
// TestConnection without persisting first.
type ctxKeyOverride struct{}

// WithAPIKeyOverride returns a child context that overrides the stored API key
// for the named provider. GetAPIKey will return this value instead of querying
// the database.
func WithAPIKeyOverride(ctx context.Context, name ProviderName, key string) context.Context {
	parentOverrides, _ := ctx.Value(ctxKeyOverride{}).(map[ProviderName]string)

	// Always create a fresh map to avoid mutating any map stored in a parent context.
	overrides := make(map[ProviderName]string, len(parentOverrides)+1)
	for k, v := range parentOverrides {
		overrides[k] = v
	}
	overrides[name] = key
	return context.WithValue(ctx, ctxKeyOverride{}, overrides)
}

// GetAPIKey retrieves and decrypts the API key for a provider.
// Returns empty string if no key is configured.
// If an override was injected via WithAPIKeyOverride, that value is returned
// instead of querying the database.
func (s *SettingsService) GetAPIKey(ctx context.Context, name ProviderName) (string, error) {
	if overrides, ok := ctx.Value(ctxKeyOverride{}).(map[ProviderName]string); ok {
		if v, found := overrides[name]; found {
			return v, nil
		}
	}

	key := apiKeySettingKey(name)
	var encrypted string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&encrypted)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading API key for %s: %w", name, err)
	}
	plaintext, err := s.encryptor.Decrypt(encrypted)
	if err != nil {
		return "", fmt.Errorf("decrypting API key for %s: %w", name, err)
	}
	return plaintext, nil
}

// SetAPIKey encrypts and stores the API key for a provider.
// The key upsert and status clear are performed in a single transaction
// so the key status never becomes stale if either operation fails.
func (s *SettingsService) SetAPIKey(ctx context.Context, name ProviderName, apiKey string) error {
	encrypted, err := s.encryptor.Encrypt(apiKey)
	if err != nil {
		return fmt.Errorf("encrypting API key for %s: %w", name, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction for %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after commit
	key := apiKeySettingKey(name)
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, encrypted, encrypted,
	); err != nil {
		return fmt.Errorf("storing API key for %s: %w", name, err)
	}
	// Clear stale status so the key shows as "untested" until re-verified.
	statusKey := keyStatusSettingKey(name)
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", statusKey); err != nil {
		return fmt.Errorf("clearing key status for %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing API key for %s: %w", name, err)
	}
	return nil
}

// DeleteAPIKey removes the API key for a provider and its associated status
// in a single transaction.
func (s *SettingsService) DeleteAPIKey(ctx context.Context, name ProviderName) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction for %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after commit
	key := apiKeySettingKey(name)
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key); err != nil {
		return fmt.Errorf("deleting API key for %s: %w", name, err)
	}
	statusKey := keyStatusSettingKey(name)
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", statusKey); err != nil {
		return fmt.Errorf("clearing key status for %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing delete for %s: %w", name, err)
	}
	return nil
}

// keyStatusSettingKey returns the settings table key for a provider's key test status.
func keyStatusSettingKey(name ProviderName) string {
	return fmt.Sprintf("provider.%s.key_status", name)
}

// SetKeyStatus persists the test result status ("ok", "invalid") for a provider key.
// An empty string deletes the status row, reverting to "untested".
func (s *SettingsService) SetKeyStatus(ctx context.Context, name ProviderName, status string) error {
	key := keyStatusSettingKey(name)
	if status == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
		if err != nil {
			return fmt.Errorf("clearing key status for %s: %w", name, err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, status, status,
	)
	if err != nil {
		return fmt.Errorf("storing key status for %s: %w", name, err)
	}
	return nil
}

// GetKeyStatus returns the persisted test status for a provider key.
// Returns empty string if no status is stored.
func (s *SettingsService) GetKeyStatus(ctx context.Context, name ProviderName) (string, error) {
	key := keyStatusSettingKey(name)
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading key status for %s: %w", name, err)
	}
	return value, nil
}

// HasAPIKey checks whether an API key is configured for a provider.
func (s *SettingsService) HasAPIKey(ctx context.Context, name ProviderName) (bool, error) {
	key := apiKeySettingKey(name)
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key = ?", key).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking API key for %s: %w", name, err)
	}
	return count > 0, nil
}

// ProviderKeyStatus describes the API key configuration state for a provider.
type ProviderKeyStatus struct {
	Name            ProviderName   `json:"name"`
	DisplayName     string         `json:"display_name"`
	RequiresKey     bool           `json:"requires_key"`
	OptionalKey     bool           `json:"optional_key"`
	HasKey          bool           `json:"has_key"`
	Status          string         `json:"status"` // "ok", "invalid", "untested", "not_required", "unconfigured"
	AccessTier      AccessTier     `json:"access_tier"`
	HelpURL         string         `json:"help_url,omitempty"`
	RateLimit       *RateLimitInfo `json:"rate_limit,omitempty"`
	SupportsBaseURL bool           `json:"supports_base_url,omitempty"`
	Mirror          *MirrorConfig  `json:"mirror,omitempty"`
}

// ListProviderKeyStatuses returns the key configuration status for all known providers.
func (s *SettingsService) ListProviderKeyStatuses(ctx context.Context) ([]ProviderKeyStatus, error) {
	caps := ProviderCapabilities()
	var statuses []ProviderKeyStatus
	for _, name := range AllProviderNames() {
		requiresKey := providerRequiresKey(name)
		optionalKey := providerHasOptionalKey(name)
		hasKey, err := s.HasAPIKey(ctx, name)
		if err != nil {
			return nil, err
		}
		status := "unconfigured"
		if !requiresKey && !optionalKey {
			status = "not_required"
		} else if !requiresKey && optionalKey && hasKey {
			status = "untested"
		} else if !requiresKey && optionalKey {
			status = "not_required"
		} else if hasKey {
			status = "untested"
		}
		// If a key is present or the provider supports base URL configuration
		// (mirror testing), check for a persisted test status so test results
		// survive page reloads.
		cap := caps[name]
		if hasKey || cap.SupportsBaseURL {
			persisted, err := s.GetKeyStatus(ctx, name)
			if err != nil {
				return nil, err
			}
			if persisted != "" {
				status = persisted
			}
		}
		pks := ProviderKeyStatus{
			Name:            name,
			DisplayName:     name.DisplayName(),
			RequiresKey:     requiresKey,
			OptionalKey:     optionalKey,
			HasKey:          hasKey,
			Status:          status,
			AccessTier:      cap.Tier,
			HelpURL:         cap.HelpURL,
			RateLimit:       cap.RateLimit,
			SupportsBaseURL: cap.SupportsBaseURL,
		}
		if cap.SupportsBaseURL {
			mirror, err := s.GetMirrorConfig(ctx, name)
			if err != nil {
				return nil, err
			}
			pks.Mirror = mirror
		}
		statuses = append(statuses, pks)
	}
	return statuses, nil
}

// providerRequiresKey returns whether a provider needs an API key.
func providerRequiresKey(name ProviderName) bool {
	switch name {
	case NameMusicBrainz, NameWikidata, NameWikipedia, NameDeezer, NameAudioDB:
		return false
	default:
		return true
	}
}

// providerHasOptionalKey returns whether a provider accepts an optional API key
// for enhanced functionality (e.g. TheAudioDB premium tier).
func providerHasOptionalKey(name ProviderName) bool {
	return name == NameAudioDB
}

// FieldPriority represents the ordered list of providers for a metadata field.
type FieldPriority struct {
	Field     string         `json:"field"`
	Providers []ProviderName `json:"providers"`
	Disabled  []ProviderName `json:"disabled,omitempty"`
}

// EnabledProviders returns the providers list excluding any that are disabled.
func (fp FieldPriority) EnabledProviders() []ProviderName {
	if len(fp.Disabled) == 0 {
		return fp.Providers
	}
	disabled := make(map[ProviderName]bool, len(fp.Disabled))
	for _, d := range fp.Disabled {
		disabled[d] = true
	}
	var result []ProviderName
	for _, p := range fp.Providers {
		if !disabled[p] {
			result = append(result, p)
		}
	}
	return result
}

// DefaultPriorities returns the default provider priority order per field.
func DefaultPriorities() []FieldPriority {
	return []FieldPriority{
		{Field: "biography", Providers: []ProviderName{NameWikipedia, NameLastFM, NameAudioDB, NameDiscogs, NameWikidata, NameGenius}},
		{Field: "genres", Providers: []ProviderName{NameMusicBrainz, NameLastFM, NameAudioDB, NameDiscogs, NameSpotify, NameWikipedia}},
		{Field: "styles", Providers: []ProviderName{NameDiscogs, NameAudioDB, NameLastFM, NameMusicBrainz}},
		{Field: "moods", Providers: []ProviderName{NameAudioDB, NameLastFM}},
		{Field: "members", Providers: []ProviderName{NameMusicBrainz, NameWikidata, NameWikipedia}},
		{Field: "formed", Providers: []ProviderName{NameMusicBrainz, NameWikidata, NameAudioDB}},
		{Field: "thumb", Providers: []ProviderName{NameFanartTV, NameAudioDB, NameDeezer, NameSpotify}},
		{Field: "fanart", Providers: []ProviderName{NameFanartTV, NameAudioDB}},
		{Field: "logo", Providers: []ProviderName{NameFanartTV, NameAudioDB}},
		{Field: "banner", Providers: []ProviderName{NameFanartTV, NameAudioDB}},
	}
}

// GetPriorities returns all configured field priorities, falling back to defaults.
func (s *SettingsService) GetPriorities(ctx context.Context) ([]FieldPriority, error) {
	defaults := DefaultPriorities()
	result := make([]FieldPriority, len(defaults))
	for i, d := range defaults {
		key := prioritySettingKey(d.Field)
		var value string
		err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
		if err == sql.ErrNoRows {
			result[i] = d
		} else if err != nil {
			return nil, fmt.Errorf("reading priority for %s: %w", d.Field, err)
		} else {
			var providers []ProviderName
			if err := json.Unmarshal([]byte(value), &providers); err != nil {
				result[i] = d
			} else {
				// Append any default providers not present in the stored list.
				// This ensures newly-added providers appear without requiring a
				// manual settings reset.
				inStored := make(map[ProviderName]bool, len(providers))
				for _, p := range providers {
					inStored[p] = true
				}
				for _, p := range d.Providers {
					if !inStored[p] {
						providers = append(providers, p)
					}
				}
				result[i] = FieldPriority{Field: d.Field, Providers: providers}
			}
		}

		// Load disabled providers for this field.
		disKey := priorityDisabledKey(d.Field)
		var disValue string
		err = s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", disKey).Scan(&disValue)
		if err == nil {
			var disabled []ProviderName
			if err := json.Unmarshal([]byte(disValue), &disabled); err == nil {
				result[i].Disabled = disabled
			}
		}
	}
	return result, nil
}

// SetPriority stores the provider priority order for a metadata field.
func (s *SettingsService) SetPriority(ctx context.Context, field string, providers []ProviderName) error {
	data, err := json.Marshal(providers)
	if err != nil {
		return fmt.Errorf("marshaling priority for %s: %w", field, err)
	}
	key := prioritySettingKey(field)
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, string(data), string(data),
	)
	if err != nil {
		return fmt.Errorf("storing priority for %s: %w", field, err)
	}
	return nil
}

// SetDisabledProviders stores the list of disabled providers for a metadata field.
func (s *SettingsService) SetDisabledProviders(ctx context.Context, field string, disabled []ProviderName) error {
	key := priorityDisabledKey(field)
	if len(disabled) == 0 {
		// Remove the key entirely when no providers are disabled.
		_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
		if err != nil {
			return fmt.Errorf("clearing disabled providers for %s: %w", field, err)
		}
		return nil
	}
	data, err := json.Marshal(disabled)
	if err != nil {
		return fmt.Errorf("marshaling disabled providers for %s: %w", field, err)
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, string(data), string(data),
	)
	if err != nil {
		return fmt.Errorf("storing disabled providers for %s: %w", field, err)
	}
	return nil
}

// AvailableProviderNames returns the set of provider names that are configured
// (either they do not require a key, or they have one stored). Unconfigured
// providers are excluded so the orchestrator can skip them without producing
// noisy ErrAuthRequired warnings.
func (s *SettingsService) AvailableProviderNames(ctx context.Context) (map[ProviderName]bool, error) {
	available := make(map[ProviderName]bool)
	for _, name := range AllProviderNames() {
		if !providerRequiresKey(name) {
			available[name] = true
			continue
		}
		hasKey, err := s.HasAPIKey(ctx, name)
		if err != nil {
			return nil, err
		}
		if hasKey {
			available[name] = true
		}
	}
	return available, nil
}

// webSearchEnabledKey returns the settings table key for a web search provider's enabled state.
func webSearchEnabledKey(name ProviderName) string {
	return fmt.Sprintf("provider.websearch.%s.enabled", name)
}

// WebSearchProviderStatus describes the enabled state of a web search provider.
type WebSearchProviderStatus struct {
	Name        ProviderName `json:"name"`
	DisplayName string       `json:"display_name"`
	Enabled     bool         `json:"enabled"`
}

// IsWebSearchEnabled checks whether a web search provider is enabled.
// Returns false if not configured (disabled by default).
func (s *SettingsService) IsWebSearchEnabled(ctx context.Context, name ProviderName) (bool, error) {
	key := webSearchEnabledKey(name)
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading web search enabled for %s: %w", name, err)
	}
	return value == "true", nil
}

// SetWebSearchEnabled stores the enabled state for a web search provider.
func (s *SettingsService) SetWebSearchEnabled(ctx context.Context, name ProviderName, enabled bool) error {
	key := webSearchEnabledKey(name)
	val := "false"
	if enabled {
		val = "true"
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, val, val,
	)
	if err != nil {
		return fmt.Errorf("storing web search enabled for %s: %w", name, err)
	}
	return nil
}

// ListWebSearchStatuses returns the enabled state for all known web search providers.
func (s *SettingsService) ListWebSearchStatuses(ctx context.Context) ([]WebSearchProviderStatus, error) {
	var statuses []WebSearchProviderStatus
	for _, name := range AllWebSearchProviderNames() {
		enabled, err := s.IsWebSearchEnabled(ctx, name)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, WebSearchProviderStatus{
			Name:        name,
			DisplayName: name.DisplayName(),
			Enabled:     enabled,
		})
	}
	return statuses, nil
}

// AnyWebSearchEnabled returns true if any web search provider is enabled.
func (s *SettingsService) AnyWebSearchEnabled(ctx context.Context) (bool, error) {
	for _, name := range AllWebSearchProviderNames() {
		enabled, err := s.IsWebSearchEnabled(ctx, name)
		if err != nil {
			return false, err
		}
		if enabled {
			return true, nil
		}
	}
	return false, nil
}

// webScraperEnabledKey returns the settings table key for a web scraper provider's enabled state.
func webScraperEnabledKey(name ProviderName) string {
	return fmt.Sprintf("provider.webscraper.%s.enabled", name)
}

// WebScraperProviderStatus describes the enabled state of a web scraper provider.
type WebScraperProviderStatus struct {
	Name        ProviderName `json:"name"`
	DisplayName string       `json:"display_name"`
	Enabled     bool         `json:"enabled"`
}

// IsWebScraperEnabled checks whether a web scraper provider is enabled.
// Returns false if not configured (disabled by default).
func (s *SettingsService) IsWebScraperEnabled(ctx context.Context, name ProviderName) (bool, error) {
	key := webScraperEnabledKey(name)
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading web scraper enabled for %s: %w", name, err)
	}
	return value == "true", nil
}

// SetWebScraperEnabled stores the enabled state for a web scraper provider.
func (s *SettingsService) SetWebScraperEnabled(ctx context.Context, name ProviderName, enabled bool) error {
	key := webScraperEnabledKey(name)
	val := "false"
	if enabled {
		val = "true"
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, val, val,
	)
	if err != nil {
		return fmt.Errorf("storing web scraper enabled for %s: %w", name, err)
	}
	return nil
}

// ListWebScraperStatuses returns the enabled state for all known web scraper providers.
func (s *SettingsService) ListWebScraperStatuses(ctx context.Context) ([]WebScraperProviderStatus, error) {
	var statuses []WebScraperProviderStatus
	for _, name := range AllWebScraperProviderNames() {
		enabled, err := s.IsWebScraperEnabled(ctx, name)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, WebScraperProviderStatus{
			Name:        name,
			DisplayName: name.DisplayName(),
			Enabled:     enabled,
		})
	}
	return statuses, nil
}

// AnyWebScraperEnabled returns true if any web scraper provider is enabled.
func (s *SettingsService) AnyWebScraperEnabled(ctx context.Context) (bool, error) {
	for _, name := range AllWebScraperProviderNames() {
		enabled, err := s.IsWebScraperEnabled(ctx, name)
		if err != nil {
			return false, err
		}
		if enabled {
			return true, nil
		}
	}
	return false, nil
}

// baseURLSettingKey returns the settings table key for a provider's mirror base URL.
func baseURLSettingKey(name ProviderName) string {
	return fmt.Sprintf("provider.%s.base_url", name)
}

// rateLimitSettingKey returns the settings table key for a provider's custom rate limit.
func rateLimitSettingKey(name ProviderName) string {
	return fmt.Sprintf("provider.%s.rate_limit", name)
}

// GetBaseURL returns the configured mirror base URL for a provider.
// Returns empty string if no custom URL is configured.
func (s *SettingsService) GetBaseURL(ctx context.Context, name ProviderName) (string, error) {
	key := baseURLSettingKey(name)
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading base URL for %s: %w", name, err)
	}
	return value, nil
}

// SetBaseURL stores a mirror base URL for a provider.
func (s *SettingsService) SetBaseURL(ctx context.Context, name ProviderName, baseURL string) error {
	key := baseURLSettingKey(name)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, baseURL, baseURL,
	)
	if err != nil {
		return fmt.Errorf("storing base URL for %s: %w", name, err)
	}
	return nil
}

// DeleteBaseURL removes the mirror base URL for a provider, reverting to the default.
func (s *SettingsService) DeleteBaseURL(ctx context.Context, name ProviderName) error {
	key := baseURLSettingKey(name)
	_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	if err != nil {
		return fmt.Errorf("deleting base URL for %s: %w", name, err)
	}
	return nil
}

// GetRateLimit returns the configured custom rate limit (requests per second) for a provider.
// Returns 0 if no custom rate limit is configured.
func (s *SettingsService) GetRateLimit(ctx context.Context, name ProviderName) (float64, error) {
	key := rateLimitSettingKey(name)
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading rate limit for %s: %w", name, err)
	}
	limit, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing rate limit for %s: %w", name, err)
	}
	return limit, nil
}

// SetRateLimit stores a custom rate limit (requests per second) for a provider.
func (s *SettingsService) SetRateLimit(ctx context.Context, name ProviderName, limit float64) error {
	key := rateLimitSettingKey(name)
	value := strconv.FormatFloat(limit, 'f', -1, 64)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, value, value,
	)
	if err != nil {
		return fmt.Errorf("storing rate limit for %s: %w", name, err)
	}
	return nil
}

// DeleteRateLimit removes the custom rate limit for a provider, reverting to the default.
func (s *SettingsService) DeleteRateLimit(ctx context.Context, name ProviderName) error {
	key := rateLimitSettingKey(name)
	_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	if err != nil {
		return fmt.Errorf("deleting rate limit for %s: %w", name, err)
	}
	return nil
}

// MirrorConfig describes the mirror configuration for a provider.
type MirrorConfig struct {
	BaseURL   string  `json:"base_url"`
	RateLimit float64 `json:"rate_limit"`
}

// GetMirrorConfig returns the mirror configuration for a provider.
func (s *SettingsService) GetMirrorConfig(ctx context.Context, name ProviderName) (*MirrorConfig, error) {
	baseURL, err := s.GetBaseURL(ctx, name)
	if err != nil {
		return nil, err
	}
	rateLimit, err := s.GetRateLimit(ctx, name)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		return nil, nil
	}
	return &MirrorConfig{BaseURL: baseURL, RateLimit: rateLimit}, nil
}

// nameSimilarityThresholdKey is the settings table key for the name similarity threshold.
const nameSimilarityThresholdKey = "provider.name_similarity_threshold"

// GetNameSimilarityThreshold returns the configured name similarity threshold (0-100).
// Returns DefaultNameSimilarityThreshold if no custom value is stored.
// A value of 0 disables name validation (accepts any result).
func (s *SettingsService) GetNameSimilarityThreshold(ctx context.Context) (int, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", nameSimilarityThresholdKey).Scan(&value)
	if err == sql.ErrNoRows {
		return DefaultNameSimilarityThreshold, nil
	}
	if err != nil {
		return DefaultNameSimilarityThreshold, fmt.Errorf("reading name similarity threshold: %w", err)
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		// Corrupt stored value; fall back to default silently.
		return DefaultNameSimilarityThreshold, nil //nolint:nilerr // intentional fallback for corrupt DB value
	}
	if n < 0 || n > 100 {
		return DefaultNameSimilarityThreshold, nil
	}
	return n, nil
}

// SetNameSimilarityThreshold stores the name similarity threshold (0-100).
func (s *SettingsService) SetNameSimilarityThreshold(ctx context.Context, threshold int) error {
	if threshold < 0 || threshold > 100 {
		return fmt.Errorf("name similarity threshold must be between 0 and 100, got %d", threshold)
	}
	value := strconv.Itoa(threshold)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		nameSimilarityThresholdKey, value, value,
	)
	if err != nil {
		return fmt.Errorf("storing name similarity threshold: %w", err)
	}
	return nil
}
