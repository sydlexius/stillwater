package settingsio

// import_sections.go contains the per-section import helpers extracted from
// ImportWithOptions. Each function handles exactly one payload section and
// increments the relevant counter(s) on result. The orchestrator in export.go
// calls them in dependency order.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// isProviderKeyOwnedSetting reports whether a settings KV key is owned by the
// dedicated provider-key import path (importProviderKeys) rather than the
// generic settings blob. These are exactly the encrypted-at-rest API key rows
// (`provider.<name>.api_key`) and their test-status companions
// (`provider.<name>.key_status`), both written by ImportSetAPIKeyTx.
//
// importProviderKeys re-encrypts the authoritative copy (from the dedicated
// Payload.ProviderKeys section) under the target instance key before
// importSettings runs. Export no longer duplicates these rows into
// Payload.Settings, but legacy (pre-1.6) envelopes did carry them there as
// source-encrypted ciphertext; without this skip, importSettings would
// overwrite the re-encrypted key with that undecryptable source ciphertext (and
// resurrect the stale key_status). The skip is applied unconditionally for all
// envelope versions, since legacy envelopes carry the bad ciphertext in
// Payload.Settings and always also carry the correct value in
// Payload.ProviderKeys.
//
// Other `provider.*` settings (base_url, rate_limit, rate_limit_ceiling,
// field_verbosity.*, websearch.<name>.enabled, priority.*,
// name_similarity_threshold) are plaintext, have no dedicated encrypted import
// path, and must keep round-tripping through the generic settings blob -- none
// of them end in `.api_key` or `.key_status`, so this predicate leaves them
// untouched.
func isProviderKeyOwnedSetting(key string) bool {
	return strings.HasPrefix(key, "provider.") &&
		(strings.HasSuffix(key, ".api_key") || strings.HasSuffix(key, ".key_status"))
}

// importSettings upserts every key-value pair from the exported settings map
// into the settings KV table. The timestamp is fixed for the entire batch so
// that multiple calls within a single import produce a consistent updated_at.
// Accepts a dbExecutor so the orchestrator can hand it a *sql.Tx wrapping
// every s.db-direct import section.
func (s *Service) importSettings(ctx context.Context, db dbExecutor, settings map[string]string, result *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range settings {
		// Provider API keys and their statuses are owned by importProviderKeys,
		// which runs earlier in the transaction and re-encrypts them under the
		// target instance key. Skipping them here prevents the generic blob's
		// source-encrypted (target-undecryptable) ciphertext from clobbering the
		// authoritative copy. result.Settings therefore counts only rows we
		// actually apply.
		if isProviderKeyOwnedSetting(k) {
			continue
		}
		_, err := db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now)
		if err != nil {
			return fmt.Errorf("upserting setting %q: %w", k, err)
		}
		result.Settings++
	}
	return nil
}

// importProviderKeys writes each provider API key via the provider settings
// service, which handles at-rest encryption. An empty key map is a no-op.
func (s *Service) importProviderKeys(ctx context.Context, db dbExecutor, keys map[string]string, result *ImportResult) error {
	for name, key := range keys {
		if err := s.providerSettings.ImportSetAPIKeyTx(ctx, db, provider.ProviderName(name), key); err != nil {
			return fmt.Errorf("setting provider key %q: %w", name, err)
		}
		result.ProviderKeys++
	}
	return nil
}

// importConnections upserts connections by matching on (type, url). When a
// connection with the same (type, url) exists on the target it is updated in
// place; otherwise a new connection is created. The internal connection ID is
// never exported; only the natural key (type, url) crosses the wire.
//
// carryV14Fields signals that the envelope is v1.4 or later, so the four
// v1.4-only fields (FeatureMetadataPush, FeatureTriggerRefresh,
// FeatureManageServerFiles, PreStillwaterConfigJSON) are authoritative.
// When false (a pre-1.4 envelope), those fields decoded as zero values and
// must not be copied onto the target's existing connection row -- doing so
// would silently disable toggles the operator had set.
//
// carryV15Fields signals that the envelope version is recognized as one
// that carries the v1.5-only field (VerifyPathAfterUpdate), in which case
// the field is authoritative. When false (a pre-1.5 envelope OR a future
// envelope version not yet added to envelopeCarriesConnectionV15Fields),
// the field decoded as a zero value and must not be copied onto the
// target's existing connection row for the same reason as V14.
//
// carryV17Fields plays the same role for the v1.7-only PathMappings field: a
// pre-1.7 envelope decoded it as nil, so it must not clobber the target's
// existing Lidarr path mappings.
func (s *Service) importConnections(ctx context.Context, db dbExecutor, conns []ConnectionExport, result *ImportResult, carryV14Fields, carryV15Fields, carryV17Fields bool) error {
	// Index rather than range-value: ConnectionExport is large enough that a
	// per-iteration value copy trips gocritic's rangeValCopy.
	for i := range conns {
		ce := &conns[i]
		existing, err := s.connectionSvc.ImportGetByTypeAndURLTx(ctx, db, ce.Type, ce.URL)
		if err != nil {
			return fmt.Errorf("looking up connection %q: %w", ce.Name, err)
		}
		if existing != nil {
			existing.Name = ce.Name
			existing.APIKey = ce.APIKey
			existing.Enabled = ce.Enabled
			if carryV14Fields {
				existing.FeatureManageServerFiles = ce.FeatureManageServerFiles
				existing.PreStillwaterConfigJSON = ce.PreStillwaterConfigJSON
			}
			// Map the flat envelope's platform-specific fields onto the
			// type-discriminated sub-config (#1686). existing already carries
			// the matching sub-config from the DB scan; platform identity is
			// preserved when already resolved (see applyExportConfig).
			applyExportConfig(existing, *ce, carryV14Fields, carryV15Fields, carryV17Fields)
			if err := s.connectionSvc.ImportUpdateTx(ctx, db, existing); err != nil {
				return fmt.Errorf("updating connection %q: %w", ce.Name, err)
			}
		} else {
			// A fresh row: every envelope field is authoritative (pre-1.4/1.5
			// envelopes simply decoded the newer fields as zero values).
			c := &connection.Connection{
				Name:                     ce.Name,
				Type:                     ce.Type,
				URL:                      ce.URL,
				APIKey:                   ce.APIKey,
				Enabled:                  ce.Enabled,
				FeatureManageServerFiles: ce.FeatureManageServerFiles,
				PreStillwaterConfigJSON:  ce.PreStillwaterConfigJSON,
			}
			applyExportConfig(c, *ce, true, true, true)
			if err := s.connectionSvc.ImportCreateTx(ctx, db, c); err != nil {
				return fmt.Errorf("creating connection %q: %w", ce.Name, err)
			}
		}
		result.Connections++
	}
	return nil
}

// applyExportConfig maps the flat ConnectionExport platform-specific fields
// onto the type-discriminated sub-config of conn (#1686). The flat envelope
// shape is retained for backward compatibility with older Stillwater versions;
// this is the single place the import path translates it into the sub-structs.
//
// gateV14/gateV15 mirror the version gating in importConnections: when false,
// the corresponding fields are not authoritative in this envelope and must not
// overwrite values already on conn. Platform identity (user/server ID) reflects
// the live peer and is only seeded from the envelope when conn does not already
// have one resolved - so a fresh row (empty) takes the envelope value while an
// existing row keeps its own.
func applyExportConfig(conn *connection.Connection, ce ConnectionExport, gateV14, gateV15, gateV17 bool) {
	switch conn.Type {
	case connection.TypeLidarr:
		if conn.Lidarr == nil {
			conn.Lidarr = &connection.LidarrConfig{}
		}
		if gateV15 {
			conn.Lidarr.VerifyPathAfterUpdate = ce.VerifyPathAfterUpdate
		}
		if gateV17 {
			conn.Lidarr.PathMappings = ce.PathMappings
		}
	case connection.TypeEmby:
		if conn.Emby == nil {
			conn.Emby = &connection.EmbyConfig{}
		}
		conn.Emby.FeatureLibraryImport = ce.FeatureLibraryImport
		conn.Emby.FeatureNFOWrite = ce.FeatureNFOWrite
		conn.Emby.FeatureImageWrite = ce.FeatureImageWrite
		if gateV14 {
			conn.Emby.FeatureMetadataPush = ce.FeatureMetadataPush
			conn.Emby.FeatureTriggerRefresh = ce.FeatureTriggerRefresh
		}
		if conn.Emby.PlatformUserID == "" {
			conn.Emby.PlatformUserID = ce.PlatformUserID
		}
		if conn.Emby.PlatformServerID == "" {
			conn.Emby.PlatformServerID = ce.PlatformServerID
		}
	case connection.TypeJellyfin:
		if conn.Jellyfin == nil {
			conn.Jellyfin = &connection.JellyfinConfig{}
		}
		conn.Jellyfin.FeatureLibraryImport = ce.FeatureLibraryImport
		conn.Jellyfin.FeatureNFOWrite = ce.FeatureNFOWrite
		conn.Jellyfin.FeatureImageWrite = ce.FeatureImageWrite
		if gateV14 {
			conn.Jellyfin.FeatureMetadataPush = ce.FeatureMetadataPush
			conn.Jellyfin.FeatureTriggerRefresh = ce.FeatureTriggerRefresh
		}
		if conn.Jellyfin.PlatformUserID == "" {
			conn.Jellyfin.PlatformUserID = ce.PlatformUserID
		}
		if conn.Jellyfin.PlatformServerID == "" {
			conn.Jellyfin.PlatformServerID = ce.PlatformServerID
		}
	}
}

// importPlatformProfiles upserts platform profiles by name. An existing profile
// with the same name has its ID preserved and its fields updated; absent profiles
// are created with a new ID. IsActive is forced to false on create to prevent
// multiple active profiles from being introduced during import.
func (s *Service) importPlatformProfiles(ctx context.Context, db dbExecutor, profiles []platform.Profile, result *ImportResult) error {
	for i := range profiles {
		p := &profiles[i]
		existing, err := s.platformSvc.ImportGetByNameTx(ctx, db, p.Name)
		if err != nil {
			return fmt.Errorf("looking up platform profile %q: %w", p.Name, err)
		}
		if existing != nil {
			p.ID = existing.ID
			if err := s.platformSvc.ImportUpdateTx(ctx, db, p); err != nil {
				return fmt.Errorf("updating platform profile %q: %w", p.Name, err)
			}
		} else {
			p.ID = ""          // Let Create generate a new ID.
			p.IsActive = false // Avoid creating multiple active profiles on import.
			if err := s.platformSvc.ImportCreateTx(ctx, db, p); err != nil {
				return fmt.Errorf("creating platform profile %q: %w", p.Name, err)
			}
		}
		result.Profiles++
	}
	return nil
}

// importWebhooks upserts webhooks by matching on (name, url). An existing
// webhook is updated in place with its ID preserved; absent webhooks are
// created with a new ID.
func (s *Service) importWebhooks(ctx context.Context, db dbExecutor, webhooks []webhook.Webhook, result *ImportResult) error {
	for i := range webhooks {
		w := &webhooks[i]
		existing, err := s.webhookSvc.ImportGetByNameAndURLTx(ctx, db, w.Name, w.URL)
		if err != nil {
			return fmt.Errorf("looking up webhook %q: %w", w.Name, err)
		}
		if existing != nil {
			w.ID = existing.ID
			if err := s.webhookSvc.ImportUpdateTx(ctx, db, w); err != nil {
				return fmt.Errorf("updating webhook %q: %w", w.Name, err)
			}
		} else {
			w.ID = "" // Let Create generate a new ID.
			if err := s.webhookSvc.ImportCreateTx(ctx, db, w); err != nil {
				return fmt.Errorf("creating webhook %q: %w", w.Name, err)
			}
		}
		result.Webhooks++
	}
	return nil
}

// importProviderPriorities writes the ordered provider list and the disabled
// provider set for each exported field. An empty priorities slice is a no-op.
func (s *Service) importProviderPriorities(ctx context.Context, db dbExecutor, priorities []PriorityExport, result *ImportResult) error {
	for _, p := range priorities {
		if err := s.providerSettings.ImportSetPriorityTx(ctx, db, p.Field, p.Providers); err != nil {
			return fmt.Errorf("setting priority for %q: %w", p.Field, err)
		}
		disabled := p.Disabled
		if disabled == nil {
			disabled = []provider.ProviderName{}
		}
		if err := s.providerSettings.ImportSetDisabledProvidersTx(ctx, db, p.Field, disabled); err != nil {
			return fmt.Errorf("setting disabled providers for %q: %w", p.Field, err)
		}
		result.Priorities++
	}
	return nil
}

// importRules applies exported rule configuration (enabled, automation_mode,
// config) to the matching local rules. Rules are matched by ID. Unknown IDs
// (exported by a newer binary that this instance does not have) are silently
// skipped so cross-version imports do not abort. Entries with an empty ID or
// an unrecognized automation_mode are also skipped with a warning. This method
// is a no-op when ruleService is nil.
func (s *Service) importRules(ctx context.Context, db dbExecutor, rules []RuleExport, result *ImportResult) error {
	if s.ruleService == nil {
		return nil
	}
	for i := range rules {
		re := &rules[i]
		if re.ID == "" {
			continue
		}
		existing, err := s.ruleService.ImportGetByIDTx(ctx, db, re.ID)
		if err != nil {
			// Unknown rule IDs (newer export, older binary) are expected -- skip.
			// Other errors (DB connection, corruption) must surface.
			if errors.Is(err, rule.ErrNotFound) {
				continue
			}
			return fmt.Errorf("looking up rule %q: %w", re.ID, err)
		}
		// Validate automation_mode before writing. A tampered or stale payload
		// could carry an unrecognized value. Only the two constants defined in
		// the rule package are valid; "disabled" is not a valid automation_mode
		// -- use enabled=false to disable a rule.
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
		if err := s.ruleService.ImportUpdateTx(ctx, db, existing); err != nil {
			return fmt.Errorf("updating rule %q: %w", re.ID, err)
		}
		result.Rules++
	}
	return nil
}

// importScraperPreferences upserts scraper configurations for every scope in
// the exported payload. Each scope is written via the tx-aware import helper
// so a mid-import failure rolls back every prior section's writes. Entries
// with an empty scope are skipped. This method is a no-op when
// scraperService is nil.
func (s *Service) importScraperPreferences(ctx context.Context, db dbExecutor, configs []ScraperConfigExport, result *ImportResult) error {
	if s.scraperService == nil {
		return nil
	}
	for i := range configs {
		sce := &configs[i]
		if sce.Scope == "" {
			continue
		}
		// Clear the ID so SaveConfig resolves it from the DB, avoiding ID
		// collisions when importing across instances.
		sce.Config.ID = ""
		if err := s.scraperService.ImportSaveConfigTx(ctx, db, sce.Scope, &sce.Config, sce.Overrides); err != nil {
			return fmt.Errorf("saving scraper config for scope %q: %w", sce.Scope, err)
		}
		result.ScraperConfigs++
	}
	return nil
}
