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

// defaultPBKDF2Iterations is the OWASP-recommended iteration count for
// PBKDF2-SHA256 and the value production always runs at.
const defaultPBKDF2Iterations = 600_000

// pbkdf2Iterations is the active PBKDF2-SHA256 iteration count used to derive
// the envelope key. It defaults to the OWASP value and is ONLY lowered by
// tests (settingsio TestMain) so the deliberately expensive KDF does not
// dominate the -race suite -- at 600k iterations every Export/Import spends
// hundreds of ms in PBKDF2, making settingsio a ~128s CPU long-pole that a
// full `go test -race ./...` run drags on. Production never mutates this.
//
// Lowering it in tests is self-consistent: envelopes carry no iteration count
// (see Envelope), so decryptWithPassphrase re-derives the key from whatever
// value is active, and every test encrypts and decrypts within the same run.
// No persisted/committed fixture ciphertext exists that a lowered value could
// fail to decrypt.
var pbkdf2Iterations = defaultPBKDF2Iterations

// CurrentEnvelopeVersion is the version emitted by Export. Bump whenever the
// Payload schema changes in a way that older binaries cannot safely round-trip.
//   - "1.0": original format (settings, connections, platform profiles, webhooks,
//     provider keys, priorities)
//   - "1.1": adds rules, scraper_configs, user_preferences, plaintext summary
//   - "1.2": adds libraries (connection refs remapped by type+url) and api_tokens
//     (token_hash + metadata only; never plaintext)
//   - "1.3": adds users block so cross-instance restore can recreate absent
//     owners before remapping api_tokens / user_preferences (#1283). The
//     password_hash inside Users is a bcrypt digest -- never plaintext --
//     and only crosses the wire inside the passphrase-encrypted envelope.
//   - "1.4": adds id to UserExport and user_id to UserPrefsExport so the
//     target instance can match users by UUID (stable across installs)
//     instead of remapping by username, and so a username collision under
//     a different id can fail the import instead of silently remapping.
//     Restore-from-OOBE flows also rely on the id stability so a restored
//     backup keeps every downstream reference intact.
//   - "1.5": added verify_path_after_update to ConnectionExport so the Lidarr
//     post-update path-verification opt-in survived export/import (#1692).
//     That toggle was retired in #2563 and the field is no longer exported.
//     A legacy envelope that still carries verify_path_after_update imports
//     cleanly -- the unknown key is ignored, not an error.
//   - "1.6": provider `api_key`/`key_status` rows are no longer duplicated
//     into the generic settings blob; they are carried solely by the
//     dedicated ProviderKeys section (decrypted at export, re-encrypted under
//     the target key at import). This fixes an import-order collision where
//     the generic blob's source-encrypted ciphertext overwrote the
//     re-encrypted key and left it undecryptable on the target (#2277). The
//     import-side skip is unconditional across all versions, so legacy
//     envelopes carrying the duplicated rows are repaired on import too.
//   - "1.7": adds path_mappings to ConnectionExport so the Lidarr
//     host<->platform path-mapping list survives export/import (#2303).
//     Pre-1.7 envelopes lack the field, so legacy imports must preserve the
//     target's existing mappings instead of clobbering them with a decoded
//     nil.
const CurrentEnvelopeVersion = "1.7"

// supportedEnvelopeVersions lists the envelope versions Import will accept.
// Older versions are accepted for backward compatibility (their newer fields
// are simply absent in the payload, which Import handles transparently).
var supportedEnvelopeVersions = map[string]bool{
	"1.0": true,
	"1.1": true,
	"1.2": true,
	"1.3": true,
	"1.4": true,
	"1.5": true,
	"1.6": true,
	"1.7": true,
}

// envelopeCarriesConnectionV14Fields reports whether an envelope of the given
// version is known to carry the v1.4 connection feature fields
// (FeatureMetadataPush, FeatureTriggerRefresh, FeatureManageServerFiles,
// PreStillwaterConfigJSON). Pre-1.4 envelopes lack those fields, so
// deserializing leaves them at their zero values; copying those zeros onto
// an existing target row would silently disable toggles the operator had
// set. Returning false here lets importConnections preserve the target's
// existing values instead.
//
// When the schema adds a new envelope version that also carries these fields,
// add it to the allow-set here. Parameter name `envelopeVersion` is spelled
// out to avoid shadowing the imported `version` package.
func envelopeCarriesConnectionV14Fields(envelopeVersion string) bool {
	switch envelopeVersion {
	case "1.4", "1.5", "1.6", "1.7":
		return true
	default:
		return false
	}
}

// envelopeCarriesConnectionV17Fields reports whether an envelope of the given
// version carries the v1.7-only connection field (PathMappings). Pre-1.7
// envelopes lack the field, so deserializing leaves it nil; copying that nil
// onto an existing target row would silently wipe the Lidarr path mappings the
// operator had set. Returning false here lets importConnections preserve the
// target's existing mappings instead.
//
// When introducing a v1.8+ envelope that ALSO carries PathMappings, add the
// new version to the case below.
func envelopeCarriesConnectionV17Fields(envelopeVersion string) bool {
	switch envelopeVersion {
	case "1.7":
		return true
	default:
		return false
	}
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
	Libraries          []LibraryExport       `json:"libraries,omitempty"`
	APITokens          []APITokenExport      `json:"api_tokens,omitempty"`
	Users              []UserExport          `json:"users,omitempty"`
}

// ConnectionExport is a connection with its API key decrypted for export.
//
// Every operator-set per-connection toggle is carried so a restore
// reconstructs the source instance's behavioral configuration in full.
// Losing any of these would silently degrade the restored instance --
// "Let Stillwater manage server files" would turn off, push-on-change
// would stop, and the snapshot used to roll the platform back on opt-out
// would be gone. Fields that have no value on the receiving instance
// (platform_user_id, platform_server_id) are still carried so a
// restore-into-fresh-install path picks them up; the connection-test
// flow will overwrite them with live values on the first health check.
type ConnectionExport struct {
	Name                     string `json:"name"`
	Type                     string `json:"type"`
	URL                      string `json:"url"`
	APIKey                   string `json:"api_key"`
	Enabled                  bool   `json:"enabled"`
	FeatureImageWrite        bool   `json:"feature_image_write"`
	FeatureMetadataPush      bool   `json:"feature_metadata_push,omitempty"`
	FeatureTriggerRefresh    bool   `json:"feature_trigger_refresh,omitempty"`
	FeatureManageServerFiles bool   `json:"feature_manage_server_files,omitempty"`
	PreStillwaterConfigJSON  string `json:"pre_stillwater_config_json,omitempty"`
	PlatformUserID           string `json:"platform_user_id,omitempty"`
	PlatformServerID         string `json:"platform_server_id,omitempty"`
	// PathMappings carries the Lidarr host<->platform path-mapping list so a
	// split-mount deployment's rename/merge remapping config survives a
	// restore (#2303). Empty for non-Lidarr connections and shared-mount
	// Lidarr connections.
	PathMappings []connection.PathMapping `json:"path_mappings,omitempty"`
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

// UserPrefsExport holds the full preference map for a single user. From
// envelope v1.4 onward the user_id (UUID) is also carried so the import
// side can match by id first (stable across installs once the Users block
// also restores ids) and fall back to username for backwards compatibility
// with pre-1.4 envelopes (versions 1.0-1.3) that carried only the username.
type UserPrefsExport struct {
	UserID      string            `json:"user_id,omitempty"`
	Username    string            `json:"username"`
	Preferences map[string]string `json:"preferences"`
}

// ImportResult summarizes what was imported. Skip counters track rows that
// were intentionally omitted (e.g. a library whose connection is missing on
// the target, or a token whose owning user is absent); they let callers
// surface a per-domain "imported / skipped" breakdown without reparsing logs.
type ImportResult struct {
	Settings         int `json:"settings"`
	Connections      int `json:"connections"`
	Profiles         int `json:"platform_profiles"`
	Webhooks         int `json:"webhooks"`
	ProviderKeys     int `json:"provider_keys"`
	Priorities       int `json:"priorities"`
	Rules            int `json:"rules"`
	ScraperConfigs   int `json:"scraper_configs"`
	UserPreferences  int `json:"user_preferences"`
	Libraries        int `json:"libraries"`
	LibrariesSkipped int `json:"libraries_skipped,omitempty"`
	APITokens        int `json:"api_tokens"`
	APITokensSkipped int `json:"api_tokens_skipped,omitempty"`
	// UsersImported counts user rows freshly inserted on the target from
	// the envelope because they were absent under both id and username
	// (#1283). The id-hit refresh path (envelope brought a user whose UUID
	// already exists on the target) and the username-hit skip path
	// (pre-1.4 envelope row whose username is already taken) do NOT
	// increment this counter -- those are refresh/skip, not recreation,
	// and folding them in would overreport on subsequent re-imports
	// against the same target.
	UsersImported int `json:"users_imported,omitempty"`
	// OwnershipReassigned counts api_tokens whose original owner is absent
	// on the target AND who were attributed to the importing admin via the
	// admin-fallback opt-in. This is a deliberate ownership change and is
	// surfaced in the result so it cannot be silent (#1283).
	OwnershipReassigned int `json:"ownership_reassigned,omitempty"`
}

// ImportOptions controls optional behaviors at import time. The zero value
// reproduces the historical behavior: tokens whose owning username is absent
// on the target are skipped (their count surfaces via APITokensSkipped) and
// no automatic ownership reassignment occurs.
type ImportOptions struct {
	// AdminFallbackTokens, when true, attributes api_tokens whose original
	// username remains absent on the target (after the envelope's Users
	// block has been applied) to ImportingAdminUserID. Each reassignment
	// increments ImportResult.OwnershipReassigned so the audit is visible
	// to the operator.
	//
	// This is opt-in because silent ownership reassignment surprises
	// operators who rely on the historical "skip unknown owner" semantics
	// for cross-environment exports (e.g. prod -> staging clones).
	AdminFallbackTokens bool

	// ImportingAdminUserID is the user_id to attribute reassigned tokens to
	// when AdminFallbackTokens is true. The HTTP handler resolves it from
	// the authenticated session before calling ImportWithOptions.
	ImportingAdminUserID string
}

// dbExecutor is the subset of *sql.DB used by the per-section import
// helpers. Both *sql.DB and *sql.Tx satisfy it, so the helpers can run
// either standalone (against s.db, e.g. unit tests that exercise a single
// section) or inside the orchestrator's single transaction. Every section's
// owning service exposes Import*Tx methods that also take a *Tx-compatible
// executor (#1693) so the full import is now atomic across all sections.
type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
//
//nolint:gocognit // Export aggregates 11 distinct surface-area sections (KV settings, provider keys, naming, custom rules, scraper config, libraries with platform tokens, identify session, language prefs, watcher, update channel, webhooks) and each section has bespoke decryption and skip-if-empty handling; the linear sectioned form mirrors the envelope schema and is the version-history anchor for the export format.
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
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scanning setting: %w", err)
		}
		// Provider API keys and statuses are carried solely by the dedicated
		// ProviderKeys section (decrypted here, re-encrypted under the target
		// key on import). Duplicating the source-encrypted ciphertext into the
		// generic settings blob served no purpose and caused an import-order
		// collision (see isProviderKeyOwnedSetting); exclude it from the blob.
		if isProviderKeyOwnedSetting(k) {
			continue
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
	for i := range keyStatuses {
		if keyStatuses[i].Status == "unconfigured" {
			continue
		}
		key, err := s.providerSettings.GetAPIKey(ctx, keyStatuses[i].Name)
		if err != nil || key == "" {
			continue
		}
		payload.ProviderKeys[string(keyStatuses[i].Name)] = key
	}

	// Collect connections with decrypted API keys
	conns, err := s.connectionSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing connections: %w", err)
	}
	for i := range conns {
		c := &conns[i]
		payload.Connections = append(payload.Connections, ConnectionExport{
			Name:                     c.Name,
			Type:                     c.Type,
			URL:                      c.URL,
			APIKey:                   c.APIKey,
			Enabled:                  c.Enabled,
			FeatureImageWrite:        c.GetFeatureImageWrite(),
			FeatureMetadataPush:      c.GetFeatureMetadataPush(),
			FeatureTriggerRefresh:    c.GetFeatureTriggerRefresh(),
			FeatureManageServerFiles: c.FeatureManageServerFiles,
			PreStillwaterConfigJSON:  c.PreStillwaterConfigJSON,
			PlatformUserID:           c.GetPlatformUserID(),
			PlatformServerID:         c.GetPlatformServerID(),
			PathMappings:             c.GetPathMappings(),
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
		for i := range rules {
			r := &rules[i]
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

	// Collect libraries. Connection IDs are not exported; instead the owning
	// connection's (type, url) is carried so the target instance can remap to
	// its own locally-generated connection_id during import.
	libs, err := s.exportLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("exporting libraries: %w", err)
	}
	payload.Libraries = libs

	// Collect API tokens. Only the stored hash + metadata are exported (the
	// plaintext is never persisted in the DB and so cannot be exported even
	// in principle). user_id is replaced with the owner's username for
	// cross-instance portability, mirroring user_preferences.
	tokens, err := s.exportAPITokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("exporting api tokens: %w", err)
	}
	payload.APITokens = tokens

	// Collect users so the import side can recreate any owner that is
	// absent on the target instance, before remapping api_tokens and
	// user_preferences (#1283). Without this, a backup whose admin had a
	// different username than the target's admin would silently lose every
	// API token. Users that already exist on the target are NOT modified
	// on import; this block is restore data, not authoritative state.
	users, err := s.exportUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("exporting users: %w", err)
	}
	payload.Users = users

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
			Libraries:       len(payload.Libraries),
			APITokens:       len(payload.APITokens),
			// UsersImported in the export Summary reports the count of user
			// rows the envelope is carrying (not yet imported). The import
			// path increments this same field with the count of users
			// actually inserted on the target.
			UsersImported: len(payload.Users),
		},
	}

	return envelope, nil
}

// Import decrypts and applies settings from an Envelope using the given
// passphrase. The passphrase must match the one used during export.
//
// This is a thin wrapper around ImportWithOptions that preserves the
// historical default (no admin-fallback for token ownership). New callers
// that need to opt into admin-fallback should call ImportWithOptions directly.
func (s *Service) Import(ctx context.Context, env *Envelope, passphrase string) (*ImportResult, error) {
	return s.ImportWithOptions(ctx, env, passphrase, ImportOptions{})
}

// ImportWithOptions decrypts and applies settings from an Envelope, honoring
// the supplied ImportOptions. See ImportOptions for the available knobs.
func (s *Service) ImportWithOptions(ctx context.Context, env *Envelope, passphrase string, opts ImportOptions) (*ImportResult, error) {
	// Reject nil envelope before touching any field. The HTTP handler is the
	// only documented caller and constructs env from a decoded JSON body, but
	// the receiver is exported and a nil pass would otherwise panic in the
	// middle of the import path -- worse than a clean 400.
	if env == nil {
		return nil, fmt.Errorf("nil envelope")
	}
	if env.Data == "" {
		return nil, fmt.Errorf("empty export data")
	}

	// Reject misconfigured admin-fallback up front. The HTTP handler resolves
	// ImportingAdminUserID from the session, so an empty value here means the
	// caller asked for fallback but supplied no fallback target. Silently
	// dropping every orphan token (the prior behavior) hides the misconfig
	// behind an APITokensSkipped count; failing fast surfaces it as the
	// configuration error it is.
	if opts.AdminFallbackTokens && opts.ImportingAdminUserID == "" {
		return nil, fmt.Errorf("admin_fallback_tokens=true requires a non-empty ImportingAdminUserID")
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
		return nil, fmt.Errorf("%w: %w", ErrWrongPassphrase, err)
	}

	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, fmt.Errorf("parsing export payload: %w", err)
	}

	result := &ImportResult{}

	// All sections run inside a single transaction so any mid-import failure
	// rolls back every row written by every prior section. Per-section
	// helpers thread the tx through tx-aware ImportXxxTx methods on each
	// owning service (connection, platform, webhook, provider, rule,
	// scraper); the s.db-direct helpers (settings, users, user_preferences,
	// libraries, api_tokens) accept a dbExecutor and run against the tx
	// directly. Public Create/Update signatures on those services stay
	// unchanged so non-import callers see no surface drift (#1693).
	//
	// Sections are called in dependency order: connections must precede
	// libraries (which remap by (type, url)), and users must precede both
	// user_preferences and api_tokens (which remap by username).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning import tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			// Reset the result struct so the caller sees zero counts plus
			// an error, never the partial counts that got rolled back.
			*result = ImportResult{}
		}
	}()

	if err := s.importProviderKeys(ctx, tx, payload.ProviderKeys, result); err != nil {
		return nil, fmt.Errorf("importing provider keys: %w", err)
	}
	if err := s.importConnections(ctx, tx, payload.Connections, result, envelopeCarriesConnectionV14Fields(v), envelopeCarriesConnectionV17Fields(v)); err != nil {
		return nil, fmt.Errorf("importing connections: %w", err)
	}
	if err := s.importPlatformProfiles(ctx, tx, payload.PlatformProfiles, result); err != nil {
		return nil, fmt.Errorf("importing platform profiles: %w", err)
	}
	if err := s.importWebhooks(ctx, tx, payload.Webhooks, result); err != nil {
		return nil, fmt.Errorf("importing webhooks: %w", err)
	}
	if err := s.importProviderPriorities(ctx, tx, payload.ProviderPriorities, result); err != nil {
		return nil, fmt.Errorf("importing provider priorities: %w", err)
	}
	if err := s.importRules(ctx, tx, payload.Rules, result); err != nil {
		return nil, fmt.Errorf("importing rules: %w", err)
	}
	if err := s.importScraperPreferences(ctx, tx, payload.ScraperConfigs, result); err != nil {
		return nil, fmt.Errorf("importing scraper preferences: %w", err)
	}
	if err := s.importSettings(ctx, tx, payload.Settings, result); err != nil {
		return nil, fmt.Errorf("importing settings: %w", err)
	}
	// Users must precede user_preferences and api_tokens so absent owners
	// are recreated from the envelope before the downstream remap lookups
	// run. See importUsers for the "existing users left untouched" policy.
	if err := s.importUsers(ctx, tx, payload.Users, result); err != nil {
		return nil, fmt.Errorf("importing users: %w", err)
	}
	if err := s.importUserPreferences(ctx, tx, payload.UserPreferences, result); err != nil {
		return nil, fmt.Errorf("importing user preferences: %w", err)
	}
	// Libraries must run AFTER connections so the (type, url) -> id remap
	// can resolve to the freshly-imported connection rows. The lookup
	// uses the tx so it observes those uncommitted rows.
	if err := s.importLibraries(ctx, tx, payload.Libraries, result); err != nil {
		return nil, fmt.Errorf("importing libraries: %w", err)
	}
	// API tokens must run AFTER importUsers so the username -> user_id
	// lookup sees the final user set, including any users just recreated
	// from the envelope. The opts parameter controls the admin-fallback
	// opt-in behavior for tokens whose owner is still absent after user import.
	if err := s.importAPITokens(ctx, tx, payload.APITokens, result, opts); err != nil {
		return nil, fmt.Errorf("importing api tokens: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing import tx: %w", err)
	}
	committed = true

	return result, nil
}

// listScraperScopes returns all scope strings present in the scraper_config table.
func (s *Service) listScraperScopes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope FROM scraper_config ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("querying scraper scopes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup
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
// and groups them by user. Password hashes and active session tokens are
// never included in user preferences; this query only touches the preferences table.
//
// Both user_id (UUID) and username are carried per group so a target instance
// can prefer id-based matching (stable across installs from envelope v1.4)
// and fall back to username only when an older envelope is being restored.
func (s *Service) exportUserPreferences(ctx context.Context) ([]UserPrefsExport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, p.key, p.value
		FROM user_preferences p
		JOIN users u ON u.id = p.user_id
		ORDER BY u.username, p.key
	`)
	if err != nil {
		return nil, fmt.Errorf("querying user preferences: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	// Build an ordered list keyed by user_id (or username when id is somehow
	// blank, which should not happen but stays defensive).
	var result []UserPrefsExport
	index := make(map[string]int) // key -> index in result
	for rows.Next() {
		var userID, username, key, value string
		if err := rows.Scan(&userID, &username, &key, &value); err != nil {
			return nil, fmt.Errorf("scanning user preference row: %w", err)
		}
		groupKey := userID
		if groupKey == "" {
			groupKey = username
		}
		idx, ok := index[groupKey]
		if !ok {
			idx = len(result)
			index[groupKey] = idx
			result = append(result, UserPrefsExport{
				UserID:      userID,
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
// Resolution order:
//  1. If the envelope carries user_id and a row with that id exists on the
//     target (recreated by importUsers when the source id was preserved),
//     use it.
//  2. Otherwise fall back to username lookup so pre-1.4 envelopes (which
//     carried only Username) still import cleanly.
//  3. Neither match -> skip with a warning so an empty target users table
//     does not silently drop every preference without trace.
func (s *Service) importUserPreferences(ctx context.Context, db dbExecutor, prefs []UserPrefsExport, result *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, up := range prefs {
		var userID string
		// Try id-based lookup first when the envelope provides one.
		if up.UserID != "" {
			err := db.QueryRowContext(ctx,
				`SELECT id FROM users WHERE id = ?`, up.UserID).Scan(&userID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("looking up user id %q for preferences: %w", up.UserID, err)
			}
		}
		// Fall back to username lookup for pre-1.4 envelopes or when the
		// id-based match missed.
		if userID == "" {
			err := db.QueryRowContext(ctx,
				`SELECT id FROM users WHERE username = ?`, up.Username).Scan(&userID)
			if errors.Is(err, sql.ErrNoRows) {
				slog.Warn("import: skipping preferences for unknown user",
					"username", up.Username, "user_id", up.UserID)
				continue
			} else if err != nil {
				// A real DB error (connection issue, corruption) must fail the
				// import -- silently swallowing it would return success after
				// dropping preference rows.
				return fmt.Errorf("looking up user %q for preferences: %w", up.Username, err)
			}
		}
		for k, v := range up.Preferences {
			_, err := db.ExecContext(ctx,
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
