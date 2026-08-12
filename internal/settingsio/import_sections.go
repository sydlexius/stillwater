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
	"github.com/sydlexius/stillwater/internal/settingsvalidate"
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

// maxReportedRejectedKeys caps ImportResult.SettingsRejectedKeys. The count
// stays exact; only the name list truncates, so an envelope carrying thousands
// of bad keys cannot inflate the response.
const maxReportedRejectedKeys = 100

// renamedSettingKeys maps a settings key that was RENAMED to its current name,
// so an envelope exported before the rename restores the operator's configured
// value instead of stranding it under a name nothing reads (#3008).
//
// Import is where this has to be handled, not boot. A boot-time migration
// cannot help: import runs long after boot, and an envelope is not bound to a
// release, so a pre-rename dev or nightly instance can hand a current build the
// old key at any time. The old row is dropped rather than kept, so a later
// export does not carry the dead name forward.
//
// Only add an entry here for a key whose MEANING and VALUE DOMAIN are
// unchanged. A rename that also changes units or scale needs a conversion, not
// an alias.
var renamedSettingKeys = map[string]string{
	// #3004: disambiguated from the differently-scoped
	// provider.name_similarity_threshold. Same 0-100 percent domain.
	"mbid_revalidate.name_similarity": "mbid_revalidate.name_similarity_threshold",
}

// importSettings upserts every key-value pair from the exported settings map
// into the settings KV table. The timestamp is fixed for the entire batch so
// that multiple calls within a single import produce a consistent updated_at.
// Accepts a dbExecutor so the orchestrator can hand it a *sql.Tx wrapping
// every s.db-direct import section.
func (s *Service) importSettings(ctx context.Context, db dbExecutor, settings map[string]string, result *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range settings {
		// Apply a rename BEFORE validation, so the value is checked against
		// the current key's rules. The envelope may legitimately carry both
		// names (an operator who set the value, upgraded, and set it again);
		// the CURRENT name wins, because it is the one the running build
		// reads and the one a later export will carry.
		if current, renamed := renamedSettingKeys[k]; renamed {
			if _, hasCurrent := settings[current]; hasCurrent {
				slog.Warn("import: dropping renamed setting key, the current name is also present",
					"old_key", k, "current_key", current)
				result.SettingsRenamedDropped++
				continue
			}
			slog.Info("import: migrating renamed setting key",
				"old_key", k, "current_key", current)
			result.SettingsRenamed++
			k = current
		}
		// Provider API keys and their statuses are owned by importProviderKeys,
		// which runs earlier in the transaction and re-encrypts them under the
		// target instance key. Skipping them here prevents the generic blob's
		// source-encrypted (target-undecryptable) ciphertext from clobbering the
		// authoritative copy. result.Settings therefore counts only rows we
		// actually apply.
		if isProviderKeyOwnedSetting(k) {
			continue
		}
		// Validate through the SAME registry PUT /api/v1/settings uses
		// (#3008). Import is the second write path into this table and was
		// the unguarded one: an envelope is a file that gets copied between
		// machines and hand-edited, so a bad value reaches the boot readers
		// exactly as it would through the API -- and getDBIntSetting parses
		// with fmt.Sscanf("%d"), which stops at the first non-digit and
		// reports success, turning a stored "0.5" into a valid, in-range 0.
		//
		// ok=false means no validator is registered for this key, which is
		// the established pass-through: the PUT path stores such keys
		// unvalidated too, and diverging here would make a restore refuse
		// keys the API accepts. Whether that default is right at all is
		// #3005; this change deliberately matches it rather than settling it.
		// ValidateStoredState, not Validate: an import RE-ESTABLISHES stored
		// state rather than accepting an operator action. It applies every
		// data-validity rule and relaxes only the write-time POLICY half --
		// auth.providers.local.enabled refuses any falsy value so local auth
		// cannot be switched off through the API, which is a rule about WHO IS
		// WRITING, not about whether the value is usable. Enforcing that during
		// a restore either silently changes a security posture or makes a
		// legitimate backup unrestorable (#2534).
		//
		// The value is still held to the data rule and still canonicalised: an
		// earlier revision of this code relaxed the whole validator and a
		// review proved it then stored "banana" for a boolean key.
		canonical, ok, relaxed, verr := settingsvalidate.ValidateStoredState(k, v)
		if relaxed {
			reason, _ := settingsvalidate.IsPolicy(k)
			slog.Info("import: applying setting that a write-time policy would refuse",
				"key", k, "policy", reason)
		}
		if ok && verr != nil {
			// Skip the row, do not abort the restore. An envelope is a batch
			// restore and one bad legacy row must not make a backup
			// unrestorable (#2534). Recorded in ImportResult so a partial
			// restore cannot pass for a complete one.
			slog.Warn("import: skipping setting that failed validation",
				"key", k, "error", verr)
			result.SettingsRejected++
			if len(result.SettingsRejectedKeys) < maxReportedRejectedKeys {
				result.SettingsRejectedKeys = append(result.SettingsRejectedKeys, k)
			}
			continue
		}
		// Store the canonical form, not the raw envelope value: validateBool
		// rewrites "TRUE" to "true", and getDBBoolSetting tests
		// v == "true" || v == "1", so an un-canonicalised value would read
		// back as DISABLED -- an operator restoring a backup and silently
		// losing a setting they had switched on.
		_, err := db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, canonical, now)
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
// carryV17Fields plays the same role for the v1.7-only PathMappings field: a
// pre-1.7 envelope decoded it as nil, so it must not clobber the target's
// existing Lidarr path mappings.
// countIgnoredFeatureToggles reports how many per-feature write toggles the
// envelope entry sets that its connection type does not have, warning once for
// the entry when there are any (#2579).
//
// applyExportConfig simply has no assignment for these outside its
// Emby/Jellyfin arms, so such a value was dropped with no error, no warning and
// no log line while the import reported a plain success. The connection still
// imports -- an envelope is a batch restore, and refusing one over an
// inapplicable field would block an operator recovering a backup, a worse
// outcome than the one being fixed -- but the drop is no longer silent.
//
// Called once per entry, BEFORE the create/update branches: those build the
// connection differently and share only applyExportConfig, so a counter wired
// into one branch would leave the other silent.
//
// Keyed on the value being TRUE rather than merely present: ConnectionExport
// carries plain bools, so an omitted field and an explicit false are
// indistinguishable on the wire, and export emits false for all three on every
// unsupported-type row. Keying on presence would report an ignored field for
// every ordinary Lidarr envelope.
func countIgnoredFeatureToggles(ce *ConnectionExport) int {
	if connection.SupportsFeatureToggles(ce.Type) {
		return 0
	}
	ignored := 0
	for _, on := range []bool{ce.FeatureImageWrite, ce.FeatureMetadataPush, ce.FeatureTriggerRefresh} {
		if on {
			ignored++
		}
	}
	if ignored > 0 {
		slog.Warn("import: ignoring connection feature toggles not supported by this connection type",
			"connection_name", ce.Name, "connection_type", ce.Type, "ignored_count", ignored)
	}
	return ignored
}

func (s *Service) importConnections(ctx context.Context, db dbExecutor, conns []ConnectionExport, result *ImportResult, carryV14Fields, carryV17Fields bool) error {
	// Index rather than range-value: ConnectionExport is large enough that a
	// per-iteration value copy trips gocritic's rangeValCopy.
	for i := range conns {
		ce := &conns[i]
		// Count feature toggles this connection type does not have BEFORE
		// either branch runs (#2579). applyExportConfig simply has no
		// assignment for them outside the Emby/Jellyfin arms, so the value was
		// dropped with no error, no warning, and no log line while the import
		// reported a plain success. The connection still imports -- see
		// ImportResult.ConnectionFeaturesIgnored for why refusing would be
		// worse -- but the drop is now visible to the operator.
		//
		// Counted once per envelope entry rather than inside the create/update
		// branches: those build the connection differently and share only
		// applyExportConfig, so a counter wired into one would leave the other
		// silent.
		//
		// Keyed on the value being TRUE, not merely present: ConnectionExport
		// carries plain bools, so an omitted field and an explicit false are
		// indistinguishable on the wire, and export emits false for all three
		// on every unsupported-type row. Keying on presence would report an
		// ignored field for every ordinary Lidarr envelope.
		result.ConnectionFeaturesIgnored += countIgnoredFeatureToggles(ce)
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
				// snapshotCopyAllowed is a POSITIVE allow-list of when it is safe
				// to write the incoming envelope's snapshot over the stored one
				// (#2439). It is deliberately NOT a negated guard ("skip unless
				// bad") -- a predicate that is safe to consult when deciding
				// whether to WRITE becomes destructive once inverted to decide
				// whether to CLEAR, because "unknown"/false must mean "leave the
				// stored value alone", never "go ahead and act". The two allowed
				// cases are: the envelope actually carries a snapshot (it is
				// authoritative and always wins), or the target has no snapshot
				// to lose (nothing to protect). The one disallowed case is an
				// empty incoming value about to overwrite a non-empty stored one
				// -- pre_stillwater_config_json is the ONLY copy of the
				// operator's original peer saver configuration, so an envelope
				// exported before Stillwater-managed mode was ever enabled (or
				// after a since-superseded disable) must not erase a snapshot
				// that a later, unrelated managed-mode session captured on the
				// target. FeatureManageServerFiles is still copied verbatim
				// above: if that leaves managed=true paired with the (now
				// preserved) non-empty snapshot, that pairing is consistent and
				// fine; if it leaves managed=true paired with an empty snapshot,
				// normalizeImportedManagedInvariant below still coerces it, so no
				// new inconsistent state can result from this guard alone.
				snapshotCopyAllowed := ce.PreStillwaterConfigJSON != "" || existing.PreStillwaterConfigJSON == ""
				if snapshotCopyAllowed {
					existing.PreStillwaterConfigJSON = ce.PreStillwaterConfigJSON
				} else {
					slog.Warn("import: preserving existing pre_stillwater_config_json; refusing to overwrite with empty snapshot from envelope",
						"connection_id", existing.ID, "connection_name", existing.Name,
						"reason", "incoming snapshot is empty but a non-empty snapshot is already on file")
				}
			}
			// Map the flat envelope's platform-specific fields onto the
			// type-discriminated sub-config (#1686). existing already carries
			// the matching sub-config from the DB scan; platform identity is
			// preserved when already resolved (see applyExportConfig).
			applyExportConfig(existing, *ce, carryV14Fields, carryV17Fields)
			if err := s.connectionSvc.ImportUpdateTx(ctx, db, existing); err != nil {
				return fmt.Errorf("updating connection %q: %w", ce.Name, err)
			}
		} else {
			// A fresh row: every envelope field is authoritative (pre-1.4
			// envelopes simply decoded the newer fields as zero values).
			// The #2439 snapshot-preservation guard above does not apply here:
			// there is no existing target row, so there is no stored snapshot
			// that an empty incoming value could destroy. Whatever the envelope
			// carries (empty or not) is simply the connection's starting state.
			c := &connection.Connection{
				Name:                     ce.Name,
				Type:                     ce.Type,
				URL:                      ce.URL,
				APIKey:                   ce.APIKey,
				Enabled:                  ce.Enabled,
				FeatureManageServerFiles: ce.FeatureManageServerFiles,
				PreStillwaterConfigJSON:  ce.PreStillwaterConfigJSON,
			}
			applyExportConfig(c, *ce, true, true)
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
// gateV14/gateV17 mirror the version gating in importConnections: when false,
// the corresponding fields are not authoritative in this envelope and must not
// overwrite values already on conn. Platform identity (user/server ID) reflects
// the live peer and is only seeded from the envelope when conn does not already
// have one resolved - so a fresh row (empty) takes the envelope value while an
// existing row keeps its own.
func applyExportConfig(conn *connection.Connection, ce ConnectionExport, gateV14, gateV17 bool) {
	// PathMappings is connection-level for EVERY type since #2380, so it is
	// applied outside the type switch. A pre-1.7 envelope (gateV17 false)
	// decoded it as nil and must not clobber the target's existing mappings.
	if gateV17 {
		conn.SetPathMappings(ce.PathMappings)
	}
	switch conn.Type {
	case connection.TypeLidarr:
		// The Lidarr sub-config carries no envelope-sourced fields since the
		// verify-path-after-update toggle was retired (#2563), but it is still
		// materialized so a Lidarr row always has a non-nil sub-config.
		if conn.Lidarr == nil {
			conn.Lidarr = &connection.LidarrConfig{}
		}
	case connection.TypeEmby:
		if conn.Emby == nil {
			conn.Emby = &connection.EmbyConfig{}
		}
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
