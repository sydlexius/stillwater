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
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// importSettings upserts every key-value pair from the exported settings map
// into the settings KV table. The timestamp is fixed for the entire batch so
// that multiple calls within a single import produce a consistent updated_at.
// Accepts a dbExecutor so the orchestrator can hand it a *sql.Tx wrapping
// every s.db-direct import section.
func (s *Service) importSettings(ctx context.Context, db dbExecutor, settings map[string]string, result *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range settings {
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
func (s *Service) importProviderKeys(ctx context.Context, keys map[string]string, result *ImportResult) error {
	for name, key := range keys {
		if err := s.providerSettings.SetAPIKey(ctx, provider.ProviderName(name), key); err != nil {
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
func (s *Service) importConnections(ctx context.Context, conns []ConnectionExport, result *ImportResult, carryV14Fields bool) error {
	for _, ce := range conns {
		existing, err := s.connectionSvc.GetByTypeAndURL(ctx, ce.Type, ce.URL)
		if err != nil {
			return fmt.Errorf("looking up connection %q: %w", ce.Name, err)
		}
		if existing != nil {
			existing.Name = ce.Name
			existing.APIKey = ce.APIKey
			existing.Enabled = ce.Enabled
			existing.FeatureLibraryImport = ce.FeatureLibraryImport
			existing.FeatureNFOWrite = ce.FeatureNFOWrite
			existing.FeatureImageWrite = ce.FeatureImageWrite
			if carryV14Fields {
				existing.FeatureMetadataPush = ce.FeatureMetadataPush
				existing.FeatureTriggerRefresh = ce.FeatureTriggerRefresh
				existing.FeatureManageServerFiles = ce.FeatureManageServerFiles
				existing.PreStillwaterConfigJSON = ce.PreStillwaterConfigJSON
			}
			// platform_user_id and platform_server_id reflect the live peer's
			// identity. Preserve the receiving instance's value if it already
			// has one (a prior connection-test resolved it); fall back to the
			// envelope's value for restore-into-fresh-install path where the
			// receiving row was just created and has empty strings.
			if existing.PlatformUserID == "" {
				existing.PlatformUserID = ce.PlatformUserID
			}
			if existing.PlatformServerID == "" {
				existing.PlatformServerID = ce.PlatformServerID
			}
			if err := s.connectionSvc.Update(ctx, existing); err != nil {
				return fmt.Errorf("updating connection %q: %w", ce.Name, err)
			}
		} else {
			c := &connection.Connection{
				Name:                     ce.Name,
				Type:                     ce.Type,
				URL:                      ce.URL,
				APIKey:                   ce.APIKey,
				Enabled:                  ce.Enabled,
				FeatureLibraryImport:     ce.FeatureLibraryImport,
				FeatureNFOWrite:          ce.FeatureNFOWrite,
				FeatureImageWrite:        ce.FeatureImageWrite,
				FeatureMetadataPush:      ce.FeatureMetadataPush,
				FeatureTriggerRefresh:    ce.FeatureTriggerRefresh,
				FeatureManageServerFiles: ce.FeatureManageServerFiles,
				PreStillwaterConfigJSON:  ce.PreStillwaterConfigJSON,
				PlatformUserID:           ce.PlatformUserID,
				PlatformServerID:         ce.PlatformServerID,
			}
			if err := s.connectionSvc.Create(ctx, c); err != nil {
				return fmt.Errorf("creating connection %q: %w", ce.Name, err)
			}
		}
		result.Connections++
	}
	return nil
}

// importPlatformProfiles upserts platform profiles by name. An existing profile
// with the same name has its ID preserved and its fields updated; absent profiles
// are created with a new ID. IsActive is forced to false on create to prevent
// multiple active profiles from being introduced during import.
func (s *Service) importPlatformProfiles(ctx context.Context, profiles []platform.Profile, result *ImportResult) error {
	for i := range profiles {
		p := &profiles[i]
		existing, err := s.platformSvc.GetByName(ctx, p.Name)
		if err != nil {
			return fmt.Errorf("looking up platform profile %q: %w", p.Name, err)
		}
		if existing != nil {
			p.ID = existing.ID
			if err := s.platformSvc.Update(ctx, p); err != nil {
				return fmt.Errorf("updating platform profile %q: %w", p.Name, err)
			}
		} else {
			p.ID = ""          // Let Create generate a new ID.
			p.IsActive = false // Avoid creating multiple active profiles on import.
			if err := s.platformSvc.Create(ctx, p); err != nil {
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
func (s *Service) importWebhooks(ctx context.Context, webhooks []webhook.Webhook, result *ImportResult) error {
	for i := range webhooks {
		w := &webhooks[i]
		existing, err := s.webhookSvc.GetByNameAndURL(ctx, w.Name, w.URL)
		if err != nil {
			return fmt.Errorf("looking up webhook %q: %w", w.Name, err)
		}
		if existing != nil {
			w.ID = existing.ID
			if err := s.webhookSvc.Update(ctx, w); err != nil {
				return fmt.Errorf("updating webhook %q: %w", w.Name, err)
			}
		} else {
			w.ID = "" // Let Create generate a new ID.
			if err := s.webhookSvc.Create(ctx, w); err != nil {
				return fmt.Errorf("creating webhook %q: %w", w.Name, err)
			}
		}
		result.Webhooks++
	}
	return nil
}

// importProviderPriorities writes the ordered provider list and the disabled
// provider set for each exported field. An empty priorities slice is a no-op.
func (s *Service) importProviderPriorities(ctx context.Context, priorities []PriorityExport, result *ImportResult) error {
	for _, p := range priorities {
		if err := s.providerSettings.SetPriority(ctx, p.Field, p.Providers); err != nil {
			return fmt.Errorf("setting priority for %q: %w", p.Field, err)
		}
		disabled := p.Disabled
		if disabled == nil {
			disabled = []provider.ProviderName{}
		}
		if err := s.providerSettings.SetDisabledProviders(ctx, p.Field, disabled); err != nil {
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
func (s *Service) importRules(ctx context.Context, rules []RuleExport, result *ImportResult) error {
	if s.ruleService == nil {
		return nil
	}
	for i := range rules {
		re := &rules[i]
		if re.ID == "" {
			continue
		}
		existing, err := s.ruleService.GetByID(ctx, re.ID)
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
		if err := s.ruleService.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating rule %q: %w", re.ID, err)
		}
		result.Rules++
	}
	return nil
}

// importScraperPreferences upserts scraper configurations for every scope in
// the exported payload. Each scope is written via SaveConfig which performs an
// ON CONFLICT update internally. Entries with an empty scope are skipped.
// This method is a no-op when scraperService is nil.
func (s *Service) importScraperPreferences(ctx context.Context, configs []ScraperConfigExport, result *ImportResult) error {
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
		if err := s.scraperService.SaveConfig(ctx, sce.Scope, &sce.Config, sce.Overrides); err != nil {
			return fmt.Errorf("saving scraper config for scope %q: %w", sce.Scope, err)
		}
		result.ScraperConfigs++
	}
	return nil
}
