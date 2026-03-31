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
	"fmt"
	"io"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/internal/webhook"
	"golang.org/x/crypto/pbkdf2"
)

// pbkdf2Iterations is the OWASP-recommended iteration count for PBKDF2-SHA256.
const pbkdf2Iterations = 600_000

// currentVersion is the export format version written on new exports.
const currentVersion = "1.1"

// Envelope is the outer JSON wrapper for an exported settings file.
type Envelope struct {
	Version    string `json:"version"`
	AppVersion string `json:"app_version"`
	CreatedAt  string `json:"created_at"`
	Salt       string `json:"salt"` // base64-encoded PBKDF2 salt
	Data       string `json:"data"` // base64-encoded nonce+ciphertext
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
	UserPreferences    []UserPrefExport      `json:"user_preferences,omitempty"`
	Users              []UserExport          `json:"users,omitempty"`
	Invites            []InviteExport        `json:"invites,omitempty"`
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

// RuleExport holds the user-configurable state of a rule.
// Rules are seeded by the server; export captures only what users can customize.
type RuleExport struct {
	ID             string          `json:"id"`
	Enabled        bool            `json:"enabled"`
	AutomationMode string          `json:"automation_mode"`
	Config         json.RawMessage `json:"config"`
}

// ScraperConfigExport holds the raw scraper configuration for a scope.
type ScraperConfigExport struct {
	Scope         string          `json:"scope"`
	ConfigJSON    json.RawMessage `json:"config_json"`
	OverridesJSON json.RawMessage `json:"overrides_json"`
}

// UserPrefExport holds a single user preference key/value pair.
type UserPrefExport struct {
	UserID string `json:"user_id"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

// UserExport holds user account data without credentials.
// Credentials are not exported; imported users must authenticate via
// federated auth or use password reset.
type UserExport struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	DisplayName  string  `json:"display_name"`
	Role         string  `json:"role"`
	AuthProvider string  `json:"auth_provider"`
	ProviderID   string  `json:"provider_id,omitempty"`
	IsActive     bool    `json:"is_active"`
	IsProtected  bool    `json:"is_protected"`
	InvitedBy    *string `json:"invited_by,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

// InviteExport holds a pending (unredeemed) invite.
type InviteExport struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	Role      string `json:"role"`
	CreatedBy string `json:"created_by"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

// ImportResult summarizes what was imported.
type ImportResult struct {
	Settings        int      `json:"settings"`
	Connections     int      `json:"connections"`
	Profiles        int      `json:"platform_profiles"`
	Webhooks        int      `json:"webhooks"`
	ProviderKeys    int      `json:"provider_keys"`
	Priorities      int      `json:"priorities"`
	Rules           int      `json:"rules"`
	ScraperConfigs  int      `json:"scraper_configs"`
	UserPreferences int      `json:"user_preferences"`
	Users           int      `json:"users"`
	Invites         int      `json:"invites"`
	Warnings        []string `json:"warnings,omitempty"`
}

// Service handles settings export and import.
type Service struct {
	db               *sql.DB
	providerSettings *provider.SettingsService
	connectionSvc    *connection.Service
	platformSvc      *platform.Service
	webhookSvc       *webhook.Service
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

// ImportOptions controls optional categories during import.
type ImportOptions struct {
	// ImportUsers controls whether user accounts are imported.
	// Defaults to false; imported users have empty password hashes and must
	// authenticate via federated auth or use password reset.
	ImportUsers bool
	// ImportInvites controls whether pending invites are imported.
	ImportInvites bool
}

// ExportOptions controls optional categories during export.
type ExportOptions struct {
	// ExportUsers controls whether user accounts are included in the export.
	// Defaults to false; user exports include identity metadata but no credentials.
	ExportUsers bool
	// ExportInvites controls whether pending (unredeemed) invites are included.
	ExportInvites bool
}

// Export collects all settings data (excluding users and invites), encrypts it
// with the given passphrase, and returns an Envelope. To include users or
// invites, use ExportWithOptions.
func (s *Service) Export(ctx context.Context, passphrase string) (*Envelope, error) {
	return s.ExportWithOptions(ctx, passphrase, ExportOptions{})
}

// ExportWithOptions collects all settings data, encrypts it with the given
// passphrase, and returns an Envelope. User accounts and pending invites are
// only included when explicitly requested via ExportOptions.
func (s *Service) ExportWithOptions(ctx context.Context, passphrase string, opts ExportOptions) (*Envelope, error) {
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

	// Collect rules (user-configurable state only)
	ruleRows, err := s.db.QueryContext(ctx,
		`SELECT id, enabled, automation_mode, config FROM rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying rules: %w", err)
	}
	defer ruleRows.Close() //nolint:errcheck
	for ruleRows.Next() {
		var re RuleExport
		var enabled int
		var configStr string
		if err := ruleRows.Scan(&re.ID, &enabled, &re.AutomationMode, &configStr); err != nil {
			return nil, fmt.Errorf("scanning rule: %w", err)
		}
		re.Enabled = enabled != 0
		// Guard against corrupted rule config rows; fall back to "{}" so the
		// export succeeds and json.Marshal(payload) doesn't fail on invalid JSON.
		if json.Valid([]byte(configStr)) {
			re.Config = json.RawMessage(configStr)
		} else {
			re.Config = json.RawMessage("{}")
		}
		payload.Rules = append(payload.Rules, re)
	}
	if err := ruleRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rules: %w", err)
	}

	// Collect scraper configurations
	scraperRows, err := s.db.QueryContext(ctx,
		`SELECT scope, config_json, overrides_json FROM scraper_config ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("querying scraper configs: %w", err)
	}
	defer scraperRows.Close() //nolint:errcheck
	for scraperRows.Next() {
		var sce ScraperConfigExport
		var configStr, overridesStr string
		if err := scraperRows.Scan(&sce.Scope, &configStr, &overridesStr); err != nil {
			return nil, fmt.Errorf("scanning scraper config: %w", err)
		}
		// Guard against corrupted scraper config JSON; fall back to "{}" so the
		// export succeeds and json.Marshal(payload) doesn't fail on invalid JSON.
		if json.Valid([]byte(configStr)) {
			sce.ConfigJSON = json.RawMessage(configStr)
		} else {
			sce.ConfigJSON = json.RawMessage("{}")
		}
		if json.Valid([]byte(overridesStr)) {
			sce.OverridesJSON = json.RawMessage(overridesStr)
		} else {
			sce.OverridesJSON = json.RawMessage("{}")
		}
		payload.ScraperConfigs = append(payload.ScraperConfigs, sce)
	}
	if err := scraperRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating scraper configs: %w", err)
	}

	// Collect user preferences
	prefRows, err := s.db.QueryContext(ctx,
		`SELECT user_id, key, value FROM user_preferences ORDER BY user_id, key`)
	if err != nil {
		return nil, fmt.Errorf("querying user preferences: %w", err)
	}
	defer prefRows.Close() //nolint:errcheck
	for prefRows.Next() {
		var up UserPrefExport
		if err := prefRows.Scan(&up.UserID, &up.Key, &up.Value); err != nil {
			return nil, fmt.Errorf("scanning user preference: %w", err)
		}
		payload.UserPreferences = append(payload.UserPreferences, up)
	}
	if err := prefRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user preferences: %w", err)
	}

	// Collect users (opt-in; credentials are never exported)
	if opts.ExportUsers {
		userRows, err := s.db.QueryContext(ctx, `
			SELECT id, username, display_name, role, auth_provider,
			       COALESCE(provider_id, ''), is_active, is_protected,
			       invited_by, created_at
			FROM users ORDER BY created_at ASC
		`)
		if err != nil {
			return nil, fmt.Errorf("querying users: %w", err)
		}
		defer userRows.Close() //nolint:errcheck
		for userRows.Next() {
			var ue UserExport
			var invitedBy sql.NullString
			var isActive, isProtected int
			if err := userRows.Scan(
				&ue.ID, &ue.Username, &ue.DisplayName, &ue.Role, &ue.AuthProvider,
				&ue.ProviderID, &isActive, &isProtected, &invitedBy, &ue.CreatedAt,
			); err != nil {
				return nil, fmt.Errorf("scanning user: %w", err)
			}
			ue.IsActive = isActive != 0
			ue.IsProtected = isProtected != 0
			if invitedBy.Valid {
				ue.InvitedBy = &invitedBy.String
			}
			payload.Users = append(payload.Users, ue)
		}
		if err := userRows.Err(); err != nil {
			return nil, fmt.Errorf("iterating users: %w", err)
		}
	}

	// Collect unredeemed invites (opt-in)
	if opts.ExportInvites {
		inviteRows, err := s.db.QueryContext(ctx, `
			SELECT id, code, role, created_by, expires_at, created_at
			FROM invites WHERE redeemed_at IS NULL ORDER BY created_at ASC
		`)
		if err != nil {
			return nil, fmt.Errorf("querying invites: %w", err)
		}
		defer inviteRows.Close() //nolint:errcheck
		for inviteRows.Next() {
			var ie InviteExport
			if err := inviteRows.Scan(
				&ie.ID, &ie.Code, &ie.Role, &ie.CreatedBy, &ie.ExpiresAt, &ie.CreatedAt,
			); err != nil {
				return nil, fmt.Errorf("scanning invite: %w", err)
			}
			payload.Invites = append(payload.Invites, ie)
		}
		if err := inviteRows.Err(); err != nil {
			return nil, fmt.Errorf("iterating invites: %w", err)
		}
	}

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
		Version:    currentVersion,
		AppVersion: version.Version,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Salt:       salt,
		Data:       data,
	}

	return envelope, nil
}

// Import decrypts and applies settings from an Envelope using the given
// passphrase. The passphrase must match the one used during export.
// Users and invites in the payload are not imported; use ImportWithOptions
// to import those optional categories.
func (s *Service) Import(ctx context.Context, env *Envelope, passphrase string) (*ImportResult, error) {
	return s.ImportWithOptions(ctx, env, passphrase, ImportOptions{})
}

// ImportWithOptions decrypts and applies settings from an Envelope using the
// given passphrase and options. Users and invites are only imported when
// explicitly requested via ImportOptions, since imported users have empty
// password hashes and must authenticate via federated auth or password reset.
func (s *Service) ImportWithOptions(ctx context.Context, env *Envelope, passphrase string, opts ImportOptions) (*ImportResult, error) {
	if env.Data == "" {
		return nil, fmt.Errorf("empty export data")
	}

	plaintext, err := decryptWithPassphrase(env.Data, env.Salt, passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypting export data (wrong passphrase?): %w", err)
	}

	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, fmt.Errorf("parsing export payload: %w", err)
	}

	return s.importPayload(ctx, &payload, opts)
}

// importPayload applies all categories from a decrypted Payload.
func (s *Service) importPayload(ctx context.Context, payload *Payload, opts ImportOptions) (*ImportResult, error) {
	result := &ImportResult{}
	now := time.Now().UTC().Format(time.RFC3339)

	// Import settings
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
			w.ID = ""
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

	// Import rules: update enabled, automation_mode, and config for existing rules only.
	// Rules are seeded by the server; unrecognized rule IDs are skipped with no error.
	for _, re := range payload.Rules {
		configStr := "{}"
		if len(re.Config) > 0 && json.Valid([]byte(re.Config)) {
			configStr = string(re.Config)
		}
		enabled := 0
		if re.Enabled {
			enabled = 1
		}
		// Coerce invalid automation_mode to "manual" rather than letting invalid
		// values reach the DB (rule update handlers only accept "auto"/"manual").
		automationMode := re.AutomationMode
		if automationMode != "auto" && automationMode != "manual" {
			automationMode = "manual"
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE rules SET enabled = ?, automation_mode = ?, config = ?, updated_at = ?
			WHERE id = ?
		`, enabled, automationMode, configStr, now, re.ID)
		if err != nil {
			return nil, fmt.Errorf("updating rule %q: %w", re.ID, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			result.Rules++
		}
	}

	// Import scraper configurations: upsert by scope. Always generate a new ID
	// on insert; ON CONFLICT(scope) preserves the existing row's ID on update.
	// Both config_json and overrides_json must be JSON objects; non-object values
	// (null, array, string) are coerced to "{}" to prevent unmarshal errors in
	// scraper.Service.
	for _, sce := range payload.ScraperConfigs {
		configStr := normalizeJSONObject(sce.ConfigJSON)
		overridesStr := normalizeJSONObject(sce.OverridesJSON)
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO scraper_config (id, scope, config_json, overrides_json, created_at, updated_at)
			VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?, ?)
			ON CONFLICT(scope) DO UPDATE SET
				config_json = excluded.config_json,
				overrides_json = excluded.overrides_json,
				updated_at = excluded.updated_at
		`, sce.Scope, configStr, overridesStr, now, now)
		if err != nil {
			return nil, fmt.Errorf("upserting scraper config for scope %q: %w", sce.Scope, err)
		}
		result.ScraperConfigs++
	}

	// Import users (optional): password_hash is always set to empty string.
	// Imported users must authenticate via federated auth or use password reset.
	// Users are imported before preferences so foreign key checks succeed.
	if opts.ImportUsers && len(payload.Users) > 0 {
		result.Warnings = append(result.Warnings,
			"imported users have empty password hashes; accounts must use federated auth or password reset")

		// Preload IDs that will exist after this import pass so that invited_by
		// references can be validated before inserting each user.
		importedIDs := make(map[string]bool)
		existingIDRows, err := s.db.QueryContext(ctx, `SELECT id FROM users`)
		if err != nil {
			return nil, fmt.Errorf("loading existing user IDs: %w", err)
		}
		defer existingIDRows.Close() //nolint:errcheck
		for existingIDRows.Next() {
			var id string
			if err := existingIDRows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scanning existing user id: %w", err)
			}
			importedIDs[id] = true
		}
		if err := existingIDRows.Err(); err != nil {
			return nil, fmt.Errorf("iterating existing user IDs: %w", err)
		}
		// Also mark the IDs that will be imported in this batch so invited_by
		// forward-references within the same export are resolved correctly.
		for _, ue := range payload.Users {
			importedIDs[ue.ID] = true
		}

		for _, ue := range payload.Users {
			// Null out invited_by if the referenced user is not present on the
			// target instance and is not part of this import batch; otherwise
			// the INSERT will fail with a foreign-key violation.
			var invitedByVal sql.NullString
			if ue.InvitedBy != nil && *ue.InvitedBy != "" && importedIDs[*ue.InvitedBy] {
				invitedByVal = sql.NullString{String: *ue.InvitedBy, Valid: true}
			}
			isActive := 0
			if ue.IsActive {
				isActive = 1
			}
			isProtected := 0
			if ue.IsProtected {
				isProtected = 1
			}
			// INSERT OR IGNORE handles both primary-key (id) and username unique
			// conflicts idempotently, preventing partial-import aborts.
			res, err := s.db.ExecContext(ctx, `
				INSERT OR IGNORE INTO users (id, username, display_name, password_hash, role, auth_provider,
				                            provider_id, is_active, is_protected, invited_by, created_at, updated_at)
				VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)
			`, ue.ID, ue.Username, ue.DisplayName, ue.Role, ue.AuthProvider,
				ue.ProviderID, isActive, isProtected, invitedByVal, ue.CreatedAt, now)
			if err != nil {
				return nil, fmt.Errorf("importing user %q: %w", ue.Username, err)
			}
			n, _ := res.RowsAffected()
			if n > 0 {
				result.Users++
			} else {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("skipped user %q: conflicts with an existing user on target instance", ue.Username))
			}
		}
	}

	// Import user preferences: upsert by (user_id, key).
	// Preferences referencing unknown users are skipped with a warning.
	// Preload existing user IDs once to avoid an N+1 query per preference.
	if len(payload.UserPreferences) > 0 {
		existingUsers := make(map[string]bool)
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM users`)
		if err != nil {
			return nil, fmt.Errorf("loading user IDs for preference import: %w", err)
		}
		defer rows.Close() //nolint:errcheck
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scanning user id: %w", err)
			}
			existingUsers[id] = true
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating user IDs: %w", err)
		}

		for _, up := range payload.UserPreferences {
			if !existingUsers[up.UserID] {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("skipped preference %q for unknown user %q", up.Key, up.UserID))
				continue
			}
			_, err := s.db.ExecContext(ctx, `
				INSERT INTO user_preferences (user_id, key, value, updated_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
			`, up.UserID, up.Key, up.Value, now)
			if err != nil {
				return nil, fmt.Errorf("upserting preference %q for user %q: %w", up.Key, up.UserID, err)
			}
			result.UserPreferences++
		}
	}

	// Import invites (optional): only unredeemed invites are exported/imported.
	// Invites whose creator (created_by) does not exist in the target DB are
	// skipped with a warning to avoid foreign key constraint violations.
	// INSERT OR IGNORE handles conflicts on both the id and code unique constraints.
	// User IDs are preloaded once to avoid an N+1 query per invite.
	if opts.ImportInvites && len(payload.Invites) > 0 {
		inviteUserIDs := make(map[string]bool)
		invUserRows, err := s.db.QueryContext(ctx, `SELECT id FROM users`)
		if err != nil {
			return nil, fmt.Errorf("loading user IDs for invite import: %w", err)
		}
		defer invUserRows.Close() //nolint:errcheck
		for invUserRows.Next() {
			var id string
			if err := invUserRows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scanning user id for invite import: %w", err)
			}
			inviteUserIDs[id] = true
		}
		if err := invUserRows.Err(); err != nil {
			return nil, fmt.Errorf("iterating user IDs for invite import: %w", err)
		}

		for _, ie := range payload.Invites {
			if !inviteUserIDs[ie.CreatedBy] {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("skipped invite %q: creator user %q not found", ie.ID, ie.CreatedBy))
				continue
			}
			res, err := s.db.ExecContext(ctx, `
				INSERT OR IGNORE INTO invites (id, code, role, created_by, expires_at, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, ie.ID, ie.Code, ie.Role, ie.CreatedBy, ie.ExpiresAt, ie.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("importing invite %q: %w", ie.ID, err)
			}
			n, _ := res.RowsAffected()
			if n > 0 {
				result.Invites++
			}
		}
	}

	return result, nil
}

// normalizeJSONObject ensures raw is a syntactically valid JSON object string.
// If raw is nil, empty, syntactically invalid, or not a JSON object
// (e.g. null, array, string), it returns "{}". This prevents unmarshal
// failures in services that expect an object at the destination column.
func normalizeJSONObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return "{}"
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(canonical)
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
