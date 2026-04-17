package settingsio

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/internal/webhook"
	"golang.org/x/crypto/pbkdf2"
)

// pbkdf2Iterations is the OWASP-recommended iteration count for PBKDF2-SHA256.
const pbkdf2Iterations = 600_000

// CurrentEnvelopeVersion is the version emitted by Export. Bump whenever the
// Payload schema changes in a way that older binaries cannot safely round-trip.
//   - "1.0": original format (settings, connections, platform profiles, webhooks,
//     provider keys, priorities)
//   - "1.1": adds rules, scraper_configs, user_preferences, plaintext summary
const CurrentEnvelopeVersion = "1.1"

// supportedEnvelopeVersions lists the envelope versions Import will accept.
// Older versions are accepted for backward compatibility (their newer fields
// are simply absent in the payload, which Import handles transparently).
var supportedEnvelopeVersions = map[string]bool{
	"1.0": true,
	"1.1": true,
}

// ErrWrongPassphrase is returned by Import when the AES-GCM tag verification
// fails, meaning the supplied passphrase does not match the one used during
// export. Callers may use errors.Is to distinguish this case from other
// import errors and surface a human-friendly hint.
var ErrWrongPassphrase = errors.New("incorrect passphrase or corrupted backup file")

// ErrUnsupportedVersion is returned by Import when the envelope's Version
// field is not one of the formats this binary knows how to read.
var ErrUnsupportedVersion = errors.New("unsupported export format version")

// Envelope is the outer JSON wrapper for an exported settings file.
// Summary is plaintext metadata -- section counts do not reveal secrets
// (which remain inside the encrypted Data blob) but give the user a
// sanity-check that the export matches their expectations.
type Envelope struct {
	Version    string        `json:"version"`
	AppVersion string        `json:"app_version"`
	CreatedAt  string        `json:"created_at"`
	Salt       string        `json:"salt"` // base64-encoded PBKDF2 salt
	Data       string        `json:"data"` // base64-encoded nonce+ciphertext
	Summary    *ImportResult `json:"summary,omitempty"`
}

// Payload is the decrypted inner content of an export.
type Payload struct {
	Settings           map[string]string     `json:"settings"`
	Connections        []ConnectionExport    `json:"connections"`
	PlatformProfiles   []platform.Profile    `json:"platform_profiles"`
	Webhooks           []webhook.Webhook     `json:"webhooks"`
	ProviderKeys       map[string]string     `json:"provider_keys"`
	ProviderPriorities []PriorityExport      `json:"provider_priorities"`
	Rules              []RuleExport          `json:"rules,omitempty"`
	ScraperConfigs     []ScraperConfigExport `json:"scraper_configs,omitempty"`
	UserPreferences    []UserPrefsExport     `json:"user_preferences,omitempty"`
}

// ConnectionExport is a connection with its API key decrypted for export.
type ConnectionExport struct {
	Name                 string `json:"name"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	APIKey               string `json:"api_key"`
	Enabled              bool   `json:"enabled"`
	FeatureLibraryImport bool   `json:"feature_library_import"`
	FeatureNFOWrite      bool   `json:"feature_nfo_write"`
	FeatureImageWrite    bool   `json:"feature_image_write"`
}

// PriorityExport holds a field's provider priority list.
type PriorityExport struct {
	Field     string                  `json:"field"`
	Providers []provider.ProviderName `json:"providers"`
	Disabled  []provider.ProviderName `json:"disabled,omitempty"`
}

// RuleExport holds the mutable configuration of a single rule.
// Only enabled, automation_mode, and config are exported; immutable fields
// (name, description, category) are not overwritten on import because they
// are defined by the application binary, not by the user.
type RuleExport struct {
	ID             string          `json:"id"`
	Enabled        bool            `json:"enabled"`
	AutomationMode string          `json:"automation_mode"`
	Config         rule.RuleConfig `json:"config"`
}

// ScraperConfigExport holds one scope's scraper configuration and its
// override set. The scope string identifies global ("global") or a
// connection-scoped entry.
type ScraperConfigExport struct {
	Scope     string                `json:"scope"`
	Config    scraper.ScraperConfig `json:"config"`
	Overrides *scraper.Overrides    `json:"overrides,omitempty"`
}

// UserPrefsExport holds the full preference map for a single user,
// identified by username rather than internal ID so the export is portable
// across instances.
type UserPrefsExport struct {
	Username    string            `json:"username"`
	Preferences map[string]string `json:"preferences"`
}

// ImportResult summarizes what was imported.
type ImportResult struct {
	Settings        int `json:"settings"`
	Connections     int `json:"connections"`
	Profiles        int `json:"platform_profiles"`
	Webhooks        int `json:"webhooks"`
	ProviderKeys    int `json:"provider_keys"`
	Priorities      int `json:"priorities"`
	Rules           int `json:"rules"`
	ScraperConfigs  int `json:"scraper_configs"`
	UserPreferences int `json:"user_preferences"`
}

// Service handles settings export and import.
type Service struct {
	db               *sql.DB
	providerSettings *provider.SettingsService
	connectionSvc    *connection.Service
	platformSvc      *platform.Service
	webhookSvc       *webhook.Service
	ruleService      *rule.Service
	scraperService   *scraper.Service
}

// NewService creates a settings export/import service.
func NewService(
	db *sql.DB,
	ps *provider.SettingsService,
	cs *connection.Service,
	ps2 *platform.Service,
	ws *webhook.Service,
) *Service {
	return &Service{
		db:               db,
		providerSettings: ps,
		connectionSvc:    cs,
		platformSvc:      ps2,
		webhookSvc:       ws,
	}
}

// WithRuleService attaches a rule service so rule configuration is included in
// export/import. If not set, rule config is silently skipped.
func (s *Service) WithRuleService(rs *rule.Service) *Service {
	s.ruleService = rs
	return s
}

// WithScraperService attaches a scraper service so scraper configuration is
// included in export/import. If not set, scraper config is silently skipped.
func (s *Service) WithScraperService(ss *scraper.Service) *Service {
	s.scraperService = ss
	return s
}

// Export collects all settings data, encrypts it with the given passphrase,
// and returns an Envelope. The passphrase is used with PBKDF2 to derive an
// AES-256-GCM key, making exports portable across instances.
func (s *Service) Export(ctx context.Context, passphrase string) (*Envelope, error) {
	payload := Payload{
		Settings:     make(map[string]string),
		ProviderKeys: make(map[string]string),
	}

	// Collect settings from the KV table
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("querying settings: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scanning setting: %w", err)
		}
		payload.Settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating settings: %w", err)
	}

	// Collect and decrypt provider API keys
	keyStatuses, err := s.providerSettings.ListProviderKeyStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing provider keys: %w", err)
	}
	for _, ks := range keyStatuses {
		if ks.Status == "unconfigured" {
			continue
		}
		key, err := s.providerSettings.GetAPIKey(ctx, ks.Name)
		if err != nil || key == "" {
			continue
		}
		payload.ProviderKeys[string(ks.Name)] = key
	}

	// Collect connections with decrypted API keys
	conns, err := s.connectionSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing connections: %w", err)
	}
	for _, c := range conns {
		payload.Connections = append(payload.Connections, ConnectionExport{
			Name:                 c.Name,
			Type:                 c.Type,
			URL:                  c.URL,
			APIKey:               c.APIKey,
			Enabled:              c.Enabled,
			FeatureLibraryImport: c.FeatureLibraryImport,
			FeatureNFOWrite:      c.FeatureNFOWrite,
			FeatureImageWrite:    c.FeatureImageWrite,
		})
	}

	// Collect platform profiles
	profiles, err := s.platformSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing platform profiles: %w", err)
	}
	payload.PlatformProfiles = profiles

	// Collect webhooks
	webhooks, err := s.webhookSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing webhooks: %w", err)
	}
	payload.Webhooks = webhooks

	// Collect provider priorities
	priorities, err := s.providerSettings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting priorities: %w", err)
	}
	for _, p := range priorities {
		payload.ProviderPriorities = append(payload.ProviderPriorities, PriorityExport{
			Field:     p.Field,
			Providers: p.Providers,
			Disabled:  p.Disabled,
		})
	}

	// Collect rule configuration (enabled, automation_mode, config per rule).
	// Only user-mutable fields are exported; immutable fields (name, description,
	// category) are defined by the binary and must not be overwritten on import.
	if s.ruleService != nil {
		rules, err := s.ruleService.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing rules: %w", err)
		}
		for _, r := range rules {
			payload.Rules = append(payload.Rules, RuleExport{
				ID:             r.ID,
				Enabled:        r.Enabled,
				AutomationMode: r.AutomationMode,
				Config:         r.Config,
			})
		}
	}

	// Collect scraper configurations for all scopes present in the database.
	if s.scraperService != nil {
		scopes, err := s.listScraperScopes(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing scraper scopes: %w", err)
		}
		for _, scope := range scopes {
			cfg, overrides, err := s.scraperService.GetRawConfig(ctx, scope)
			if err != nil {
				return nil, fmt.Errorf("reading scraper config for scope %q: %w", scope, err)
			}
			payload.ScraperConfigs = append(payload.ScraperConfigs, ScraperConfigExport{
				Scope:     scope,
				Config:    *cfg,
				Overrides: overrides,
			})
		}
	}

	// Collect user preferences, keyed by username for cross-instance portability.
	userPrefs, err := s.exportUserPreferences(ctx)
	if err != nil {
		return nil, fmt.Errorf("exporting user preferences: %w", err)
	}
	payload.UserPreferences = userPrefs

	// Marshal and encrypt payload
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling payload: %w", err)
	}

	data, salt, err := encryptWithPassphrase(payloadJSON, passphrase)
	if err != nil {
		return nil, fmt.Errorf("encrypting payload: %w", err)
	}

	envelope := &Envelope{
		Version:    CurrentEnvelopeVersion,
		AppVersion: version.Version,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Salt:       salt,
		Data:       data,
		Summary: &ImportResult{
			Settings:       len(payload.Settings),
			Connections:    len(payload.Connections),
			Profiles:       len(payload.PlatformProfiles),
			Webhooks:       len(payload.Webhooks),
			ProviderKeys:   len(payload.ProviderKeys),
			Priorities:     len(payload.ProviderPriorities),
			Rules:          len(payload.Rules),
			ScraperConfigs: len(payload.ScraperConfigs),
			// UserPreferences counts total (user, key) pairs so export Summary
			// matches the counter import increments per upserted row.
			UserPreferences: countUserPreferences(payload.UserPreferences),
		},
	}

	return envelope, nil
}

// Import decrypts and applies settings from an Envelope using the given
// passphrase. The passphrase must match the one used during export.
func (s *Service) Import(ctx context.Context, env *Envelope, passphrase string) (*ImportResult, error) {
	if env.Data == "" {
		return nil, fmt.Errorf("empty export data")
	}

	// Reject envelope formats this binary does not understand. An empty Version
	// is treated as legacy "1.0" so older exports without an explicit field
	// continue to import.
	v := env.Version
	if v == "" {
		v = "1.0"
	}
	if !supportedEnvelopeVersions[v] {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedVersion, env.Version)
	}

	// Decrypt. Wrap with ErrWrongPassphrase so callers can use errors.Is to
	// distinguish this from other import failures without inspecting error strings.
	// The inner AES-GCM error is retained in the chain for server-side logging.
	plaintext, err := decryptWithPassphrase(env.Data, env.Salt, passphrase)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWrongPassphrase, err) //nolint:errorlint // intentional dual-wrap (Go 1.20+)
	}

	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, fmt.Errorf("parsing export payload: %w", err)
	}

	result := &ImportResult{}

	// Import settings
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range payload.Settings {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now)
		if err != nil {
			return nil, fmt.Errorf("upserting setting %q: %w", k, err)
		}
		result.Settings++
	}

	// Import provider keys
	for name, key := range payload.ProviderKeys {
		if err := s.providerSettings.SetAPIKey(ctx, provider.ProviderName(name), key); err != nil {
			return nil, fmt.Errorf("setting provider key %q: %w", name, err)
		}
		result.ProviderKeys++
	}

	// Import connections: match by type + url, update or insert
	for _, ce := range payload.Connections {
		existing, err := s.connectionSvc.GetByTypeAndURL(ctx, ce.Type, ce.URL)
		if err != nil {
			return nil, fmt.Errorf("looking up connection %q: %w", ce.Name, err)
		}
		if existing != nil {
			// Update existing
			existing.Name = ce.Name
			existing.APIKey = ce.APIKey
			existing.Enabled = ce.Enabled
			existing.FeatureLibraryImport = ce.FeatureLibraryImport
			existing.FeatureNFOWrite = ce.FeatureNFOWrite
			existing.FeatureImageWrite = ce.FeatureImageWrite
			if err := s.connectionSvc.Update(ctx, existing); err != nil {
				return nil, fmt.Errorf("updating connection %q: %w", ce.Name, err)
			}
		} else {
			// Create new
			c := &connection.Connection{
				Name:                 ce.Name,
				Type:                 ce.Type,
				URL:                  ce.URL,
				APIKey:               ce.APIKey,
				Enabled:              ce.Enabled,
				FeatureLibraryImport: ce.FeatureLibraryImport,
				FeatureNFOWrite:      ce.FeatureNFOWrite,
				FeatureImageWrite:    ce.FeatureImageWrite,
			}
			if err := s.connectionSvc.Create(ctx, c); err != nil {
				return nil, fmt.Errorf("creating connection %q: %w", ce.Name, err)
			}
		}
		result.Connections++
	}

	// Import platform profiles: match by name, update or insert
	for _, p := range payload.PlatformProfiles {
		existing, err := s.platformSvc.GetByName(ctx, p.Name)
		if err != nil {
			return nil, fmt.Errorf("looking up platform profile %q: %w", p.Name, err)
		}
		if existing != nil {
			p.ID = existing.ID
			if err := s.platformSvc.Update(ctx, &p); err != nil {
				return nil, fmt.Errorf("updating platform profile %q: %w", p.Name, err)
			}
		} else {
			p.ID = ""          // Let Create generate a new ID
			p.IsActive = false // Avoid creating multiple active profiles on import
			if err := s.platformSvc.Create(ctx, &p); err != nil {
				return nil, fmt.Errorf("creating platform profile %q: %w", p.Name, err)
			}
		}
		result.Profiles++
	}

	// Import webhooks: match by name + url, upsert
	for _, w := range payload.Webhooks {
		existing, err := s.webhookSvc.GetByNameAndURL(ctx, w.Name, w.URL)
		if err != nil {
			return nil, fmt.Errorf("looking up webhook %q: %w", w.Name, err)
		}
		if existing != nil {
			w.ID = existing.ID
			if err := s.webhookSvc.Update(ctx, &w); err != nil {
				return nil, fmt.Errorf("updating webhook %q: %w", w.Name, err)
			}
		} else {
			w.ID = "" // Let Create generate a new ID
			if err := s.webhookSvc.Create(ctx, &w); err != nil {
				return nil, fmt.Errorf("creating webhook %q: %w", w.Name, err)
			}
		}
		result.Webhooks++
	}

	// Import provider priorities
	for _, p := range payload.ProviderPriorities {
		if err := s.providerSettings.SetPriority(ctx, p.Field, p.Providers); err != nil {
			return nil, fmt.Errorf("setting priority for %q: %w", p.Field, err)
		}
		if len(p.Disabled) > 0 {
			if err := s.providerSettings.SetDisabledProviders(ctx, p.Field, p.Disabled); err != nil {
				return nil, fmt.Errorf("setting disabled providers for %q: %w", p.Field, err)
			}
		}
		result.Priorities++
	}

	// Import rule configuration (only mutable fields: enabled, automation_mode, config).
	// Rules are matched by ID; unknown IDs are silently skipped to allow exports from
	// newer application versions to be imported without error.
	if s.ruleService != nil {
		for _, re := range payload.Rules {
			if re.ID == "" {
				continue
			}
			existing, err := s.ruleService.GetByID(ctx, re.ID)
			if err != nil {
				// Unknown rule IDs (newer export, older binary) are expected -- skip.
				// Other errors (DB connection, corruption) must surface instead of
				// silently dropping rule imports.
				if errors.Is(err, rule.ErrNotFound) {
					continue
				}
				return nil, fmt.Errorf("looking up rule %q: %w", re.ID, err)
			}
			// Validate automation_mode before writing to the DB. A tampered or
			// stale payload could carry an unrecognized value. Only the two
			// constants defined in the rule package are valid; "disabled" is not
			// a valid automation_mode -- use enabled=false to disable a rule.
			switch re.AutomationMode {
			case rule.AutomationModeAuto, rule.AutomationModeManual:
				// valid
			default:
				slog.Warn("import: skipping rule with invalid automation_mode",
					"rule_id", re.ID,
					"automation_mode", re.AutomationMode,
				)
				continue
			}
			existing.Enabled = re.Enabled
			existing.AutomationMode = re.AutomationMode
			existing.Config = re.Config
			if err := s.ruleService.Update(ctx, existing); err != nil {
				return nil, fmt.Errorf("updating rule %q: %w", re.ID, err)
			}
			result.Rules++
		}
	}

	// Import scraper configurations. Each scope is upserted via SaveConfig which
	// performs an ON CONFLICT update internally.
	if s.scraperService != nil {
		for _, sce := range payload.ScraperConfigs {
			if sce.Scope == "" {
				continue
			}
			// Clear the ID so SaveConfig resolves it from the DB, avoiding ID
			// collisions when importing across instances.
			sce.Config.ID = ""
			if err := s.scraperService.SaveConfig(ctx, sce.Scope, &sce.Config, sce.Overrides); err != nil {
				return nil, fmt.Errorf("saving scraper config for scope %q: %w", sce.Scope, err)
			}
			result.ScraperConfigs++
		}
	}

	// Import user preferences. Preferences are matched by username; rows for users
	// that do not exist on this instance are silently skipped.
	if err := s.importUserPreferences(ctx, payload.UserPreferences, result); err != nil {
		return nil, fmt.Errorf("importing user preferences: %w", err)
	}

	return result, nil
}

// listScraperScopes returns all scope strings present in the scraper_config table.
func (s *Service) listScraperScopes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope FROM scraper_config ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("querying scraper scopes: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, fmt.Errorf("scanning scraper scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// countUserPreferences returns the total number of (user, key) preference pairs
// across all exported users. This is what the import counter records per upserted
// row, so using the same shape keeps export.Summary and import.Result symmetric.
func countUserPreferences(prefs []UserPrefsExport) int {
	total := 0
	for _, up := range prefs {
		total += len(up.Preferences)
	}
	return total
}

// exportUserPreferences loads all rows from user_preferences joined with users
// and groups them by username. Password hashes and active session tokens are
// never included in user preferences; this query only touches the preferences table.
func (s *Service) exportUserPreferences(ctx context.Context) ([]UserPrefsExport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.username, p.key, p.value
		FROM user_preferences p
		JOIN users u ON u.id = p.user_id
		ORDER BY u.username, p.key
	`)
	if err != nil {
		return nil, fmt.Errorf("querying user preferences: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	// Build an ordered list preserving username ordering.
	var result []UserPrefsExport
	index := make(map[string]int) // username -> index in result
	for rows.Next() {
		var username, key, value string
		if err := rows.Scan(&username, &key, &value); err != nil {
			return nil, fmt.Errorf("scanning user preference row: %w", err)
		}
		idx, ok := index[username]
		if !ok {
			idx = len(result)
			index[username] = idx
			result = append(result, UserPrefsExport{
				Username:    username,
				Preferences: make(map[string]string),
			})
		}
		result[idx].Preferences[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user preference rows: %w", err)
	}
	return result, nil
}

// importUserPreferences upserts exported preference rows into the target instance.
// Users that do not exist on this instance are silently skipped; this avoids
// errors when the target instance has a different user set.
func (s *Service) importUserPreferences(ctx context.Context, prefs []UserPrefsExport, result *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, up := range prefs {
		// Look up the user ID by username on this instance.
		var userID string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE username = ?`, up.Username).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			// User does not exist on this instance -- expected when importing
			// across instances with different user sets. Warn because an empty
			// target users table would silently drop every preference.
			slog.Warn("import: skipping preferences for unknown user", "username", up.Username)
			continue
		} else if err != nil {
			// A real DB error (connection issue, corruption) must fail the
			// import -- silently swallowing it would return success after
			// dropping preference rows.
			return fmt.Errorf("looking up user %q for preferences: %w", up.Username, err)
		}
		for k, v := range up.Preferences {
			_, err := s.db.ExecContext(ctx,
				`INSERT INTO user_preferences (user_id, key, value, updated_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				userID, k, v, now)
			if err != nil {
				return fmt.Errorf("upserting preference %q for user %q: %w", k, up.Username, err)
			}
			result.UserPreferences++
		}
	}
	return nil
}

// deriveKey uses PBKDF2-SHA256 to derive a 32-byte AES-256 key from a
// passphrase and salt.
func deriveKey(passphrase string, salt []byte) []byte {
	return pbkdf2.Key([]byte(passphrase), salt, pbkdf2Iterations, 32, sha256.New)
}

// encryptWithPassphrase encrypts plaintext using a passphrase-derived
// AES-256-GCM key. Returns base64-encoded ciphertext and salt.
func encryptWithPassphrase(plaintext []byte, passphrase string) (data, salt string, err error) {
	saltBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, saltBytes); err != nil {
		return "", "", fmt.Errorf("generating salt: %w", err)
	}

	key := deriveKey(passphrase, saltBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext),
		base64.StdEncoding.EncodeToString(saltBytes),
		nil
}

// decryptWithPassphrase decrypts base64-encoded ciphertext using a
// passphrase-derived AES-256-GCM key with the given base64-encoded salt.
func decryptWithPassphrase(data, salt, passphrase string) ([]byte, error) {
	saltBytes, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return nil, fmt.Errorf("decoding salt: %w", err)
	}

	key := deriveKey(passphrase, saltBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decoding ciphertext: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting (wrong passphrase?): %w", err)
	}

	return plaintext, nil
}
