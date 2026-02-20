package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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

// GetAPIKey retrieves and decrypts the API key for a provider.
// Returns empty string if no key is configured.
func (s *SettingsService) GetAPIKey(ctx context.Context, name ProviderName) (string, error) {
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
func (s *SettingsService) SetAPIKey(ctx context.Context, name ProviderName, apiKey string) error {
	encrypted, err := s.encryptor.Encrypt(apiKey)
	if err != nil {
		return fmt.Errorf("encrypting API key for %s: %w", name, err)
	}
	key := apiKeySettingKey(name)
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, encrypted, encrypted,
	)
	if err != nil {
		return fmt.Errorf("storing API key for %s: %w", name, err)
	}
	return nil
}

// DeleteAPIKey removes the API key for a provider.
func (s *SettingsService) DeleteAPIKey(ctx context.Context, name ProviderName) error {
	key := apiKeySettingKey(name)
	_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	if err != nil {
		return fmt.Errorf("deleting API key for %s: %w", name, err)
	}
	return nil
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
	Name        ProviderName `json:"name"`
	DisplayName string       `json:"display_name"`
	RequiresKey bool         `json:"requires_key"`
	HasKey      bool         `json:"has_key"`
	Status      string       `json:"status"` // "ok", "invalid", "untested", "not_required", "unconfigured"
}

// ListProviderKeyStatuses returns the key configuration status for all known providers.
func (s *SettingsService) ListProviderKeyStatuses(ctx context.Context) ([]ProviderKeyStatus, error) {
	var statuses []ProviderKeyStatus
	for _, name := range AllProviderNames() {
		requiresKey := providerRequiresKey(name)
		hasKey, err := s.HasAPIKey(ctx, name)
		if err != nil {
			return nil, err
		}
		status := "unconfigured"
		if !requiresKey {
			status = "not_required"
		} else if hasKey {
			status = "untested"
		}
		statuses = append(statuses, ProviderKeyStatus{
			Name:        name,
			DisplayName: name.DisplayName(),
			RequiresKey: requiresKey,
			HasKey:      hasKey,
			Status:      status,
		})
	}
	return statuses, nil
}

// providerRequiresKey returns whether a provider needs an API key.
func providerRequiresKey(name ProviderName) bool {
	switch name {
	case NameMusicBrainz, NameWikidata:
		return false
	default:
		return true
	}
}

// FieldPriority represents the ordered list of providers for a metadata field.
type FieldPriority struct {
	Field     string         `json:"field"`
	Providers []ProviderName `json:"providers"`
}

// DefaultPriorities returns the default provider priority order per field.
func DefaultPriorities() []FieldPriority {
	return []FieldPriority{
		{Field: "biography", Providers: []ProviderName{NameMusicBrainz, NameLastFM, NameAudioDB, NameDiscogs, NameWikidata}},
		{Field: "genres", Providers: []ProviderName{NameMusicBrainz, NameLastFM, NameAudioDB, NameDiscogs}},
		{Field: "styles", Providers: []ProviderName{NameAudioDB, NameDiscogs}},
		{Field: "moods", Providers: []ProviderName{NameAudioDB}},
		{Field: "members", Providers: []ProviderName{NameMusicBrainz, NameWikidata}},
		{Field: "formed", Providers: []ProviderName{NameMusicBrainz, NameWikidata, NameAudioDB}},
		{Field: "thumb", Providers: []ProviderName{NameFanartTV, NameAudioDB}},
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
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading priority for %s: %w", d.Field, err)
		}
		var providers []ProviderName
		if err := json.Unmarshal([]byte(value), &providers); err != nil {
			result[i] = d
			continue
		}
		result[i] = FieldPriority{Field: d.Field, Providers: providers}
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
