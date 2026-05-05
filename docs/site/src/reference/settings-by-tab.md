---
description: A guided tour of every tab in Settings -- what each section does, where to find specific knobs, and notable behaviors.
---

<!-- code: web/templates/settings.templ (settingsTabs() at line 140 enumerates the 11 tabs; each panel keyed by data-tab-panel="..."). Inventory verified 2026-04-30 against main; Updates-tab fields re-verified 2026-05-01 with W2.E (#1117) landing the enabled toggle, check-interval selector, and the background scheduler that consumes auto-check (auto-apply is split out to #1284); General-tab base-path field is editable when SW_BASE_PATH is unset (#1005). -->

# Settings, by tab

Stillwater's Settings page is divided into 11 tabs. This page is a navigational reference -- each section below describes one tab, the major panels inside it, and where to find specific knobs. For deeper explanation of *what the settings mean*, follow the cross-links into the relevant Core Concepts or How-to pages.

<!-- BEGIN GENERATED: settings-reference -->

## General  {#tab-general}

### Platform Profile  {#settings-platform-profile}

Select the target platform to control NFO output and image naming conventions.

### Active Profile Details  {#settings-active-profile}

Edit filenames for each image type. Changes are saved to the profile.

- **NFO Output** {#settings-active-profile-nfo-output}
- **Save Filenames** {#settings-active-profile-save-filenames}

### Use symlinks for duplicate copies  {#settings-symlinks}

- **Symlinks are not supported on this filesystem. The library path does not support symbolic links.** {#settings-symlinks-unsupported-description}

### Base Path  {#settings-base-path}

URL path prefix for running Stillwater behind a reverse proxy at a sub-path.

### Behavior  {#settings-behavior}

Configure default behaviors for metadata workflows.

### Show platform debug info on artist pages  {#settings-platform-debug}

When enabled, a Debug tab appears on artist detail pages for platform-connected artists

### Image Cache  {#settings-image-cache}

Manage cached images for artists without filesystem paths.

- **Maximum size** {#settings-image-cache-max-size}
- **Unlimited** {#settings-image-cache-unlimited}
- **Clear Cache** {#settings-image-cache-clear}


## Providers  {#tab-providers}

### Provider API Keys  {#settings-provider-keys}

Configure API keys for metadata providers. Providers without a configured key will be skipped during metadata lookups.

- **No API key required** {#settings-provider-keys-no-key-required}
- **Premium key configured** {#settings-provider-keys-premium-configured}
- **Free tier (optional premium upgrade)** {#settings-provider-keys-free-tier}
- **Key configured** {#settings-provider-keys-key-configured}
- **API key required** {#settings-provider-keys-key-required}
- **Requires a Spotify Premium subscription and a registered** {#settings-provider-keys-spotify-note-prefix}
- **app.** {#settings-provider-keys-spotify-note-suffix}

### Web Image Search  {#settings-web-search}

Enable web search providers to find additional artist images beyond authoritative sources. No API key required.

### Provider Priorities  {#settings-priorities}

Set the preferred provider order for each metadata field. Drag to reorder. Click the checkmark/X to enable or disable. Only configured providers are shown.

### Metadata Language Preferences  {#settings-metadata-languages}

Set your preferred languages for artist names, biographies, and aliases. Providers will return content in the first available language from your list. Drag to reorder priority.

- **Remove** {#settings-metadata-languages-remove}
- **Search languages** {#settings-metadata-languages-input-label}

### Advanced  {#settings-advanced}

Fine-tune provider matching behavior.

### Name similarity  {#settings-name-similarity}

Minimum similarity score (0-100) required when matching artist names from search results. Set to 0 to disable name validation and accept any result. Default is 60.

- **Name Similarity Threshold** {#settings-name-similarity-label}

### Provider config  {#settings-provider-config}

- **Client ID** {#settings-provider-config-client-id}
- **Client Secret** {#settings-provider-config-client-secret}
- **Server** {#settings-provider-config-server}
- **Official** {#settings-provider-config-official}
- **Beta** {#settings-provider-config-beta}
- **Custom mirror** {#settings-provider-config-custom-mirror}
- **Self-hosted mirrors can often handle higher rates. Default: 10 req/s.** {#settings-provider-config-custom-help}
- **OAuth Credentials** {#settings-provider-config-oauth-credentials}
- **Required for submitting edits to MusicBrainz.** {#settings-provider-config-oauth-note}


## Connections  {#tab-connections}

### Server Connections  {#settings-connections}

Connect to Emby, Jellyfin, or Lidarr servers for library sync and metadata push.

- **Feature toggles** {#settings-connections-feature-toggles}
- **What Stillwater sends to this connection** {#settings-connections-sends-heading}
- **Library import** {#settings-connections-feature-library-import}
- **When on, Stillwater imports library metadata from this server.** {#settings-connections-feature-library-import-tooltip}
- **NFO write** {#settings-connections-feature-nfo-write}
- **When on, Stillwater writes artist.nfo files for artists in this server's libraries. Writes can still be gated while conflict gating is active (write-back or round-trip overlap) -- see the top banner for details.** {#settings-connections-feature-nfo-write-tooltip}
- **Image download/write** {#settings-connections-feature-image-write}
- **When on, Stillwater writes image files for artists in this server's libraries. Writes can still be gated while conflict gating is active (write-back or round-trip overlap) -- see the top banner for details.** {#settings-connections-feature-image-write-tooltip}
- **Let Stillwater manage artwork and NFO files on this server** {#settings-connections-manage-title}
- **When on, Stillwater watches this server and turns off its artwork and NFO savers whenever they get re-enabled. Your previous settings are saved and restored if you turn this off or remove the connection.** {#settings-connections-manage-description}
- **Not configured** {#settings-connections-not-configured}


## Libraries  {#tab-libraries}

### Music Libraries  {#settings-libraries}

Manage your music library paths. Each library maps to a directory containing artist folders.

- **Connection** {#settings-libraries-connection-badge}
- **No libraries configured.** {#settings-libraries-empty}
- **Lock NFOs** {#settings-libraries-lock-nfo-label}
- **When on, Stillwater stamps <lockdata>true</lockdata> into every NFO it writes for this library. This tells Emby and Jellyfin to refuse metadata refreshes for those artists, preserving Stillwater's curated values from being overwritten by the platform's own scrapers. Off by default. Tip: artists whose NFO already contains <lockdata>true</lockdata> (set by Stillwater or another tool) are automatically marked as locked at the artist level.** {#settings-libraries-lock-nfo-title}
- **Add Library** {#settings-libraries-add}
- **Regular** {#settings-libraries-type-regular}
- **Classical** {#settings-libraries-type-classical}
- **Filesystem monitoring mode** {#settings-libraries-fs-mode-title}
- **Off** {#settings-libraries-fs-off}
- **Watch** {#settings-libraries-fs-watch}
- **Poll** {#settings-libraries-fs-poll}
- **Watch + Poll** {#settings-libraries-fs-both}
- **Re-sync Artists** {#settings-libraries-resync}
- **Scan Library** {#settings-libraries-scan}


## Automation  {#tab-automation}

### Webhooks  {#settings-webhooks}

Send notifications to external services when events occur.

- **No webhooks configured.** {#settings-webhooks-empty}
- **Add Webhook** {#settings-webhooks-add}
- **Select type...** {#settings-webhooks-select-type}
- **Generic (JSON)** {#settings-webhooks-type-generic}

### Notification Badges  {#settings-notif-badges}

Show a counter badge on the Open Violations link in the sidebar indicating active violations.

- **Enable badge** {#settings-notif-badges-enable-badge}
- **Count violations by severity** {#settings-notif-badges-count-by-severity}

### API Tokens  {#settings-api-tokens}

Generate tokens for external applications to access the Stillwater API.

- **No API tokens created.** {#settings-api-tokens-empty}
- **Revoked** {#settings-api-tokens-revoked}
- **Read** {#settings-api-tokens-scope-read}
- **Write** {#settings-api-tokens-scope-write}
- **Webhook** {#settings-api-tokens-scope-webhook}
- **Admin** {#settings-api-tokens-scope-admin}

### Inbound Webhooks  {#settings-inbound-webhooks}

Receive events from external applications to trigger actions in Stillwater.

- **Webhook URL** {#settings-inbound-webhooks-url-label}
- **Supported events** {#settings-inbound-webhooks-supported-events}
- **Supported events (Emby internal names)** {#settings-inbound-webhooks-supported-events-emby}


## Rules  {#tab-rules}

### Rules  {#settings-rules}

- **Image-category rules are paused while conflict gating is active. Resolve the active write-back or round-trip conflict in the top banner to resume auto-fix.** {#settings-rules-conflict-gated-image-tooltip}
- **paused: conflict gating** {#settings-rules-conflict-gated-chip}
- **NFO-category rules are paused while conflict gating is active. Resolve the active write-back or round-trip conflict in the top banner to resume auto-fix.** {#settings-rules-conflict-gated-nfo-tooltip}
- **No rules found.** {#settings-rules-empty}
- **This rule requires a local library with a filesystem path. Add a library with a path to enable it.** {#settings-rules-requires-local-tooltip}
- **Requires a local library with a filesystem path** {#settings-rules-requires-local-tooltip-short}
- **Requires local library** {#settings-rules-requires-local}
- **Auto-fix** {#settings-rules-auto-fix}
- **Manual (notify only)** {#settings-rules-manual}
- **Cannot enable: no local library configured** {#settings-rules-cannot-enable-tooltip}

### Scheduled Evaluation  {#settings-rule-schedule}

Run all enabled rules on a recurring schedule. Requires a container restart after changing.

- **Evaluates all enabled rules against every artist on the selected interval. Changes take effect after container restart.** {#settings-rule-schedule-note}

### Schedule  {#settings-schedule}

- **Every 5 minutes** {#settings-schedule-every-5m}
- **Every 15 minutes** {#settings-schedule-every-15m}
- **Every 30 minutes** {#settings-schedule-every-30m}
- **Every hour** {#settings-schedule-every-hour}
- **Every 6 hours** {#settings-schedule-every-6h}
- **Every 12 hours** {#settings-schedule-every-12h}
- **Daily (24h)** {#settings-schedule-daily}


## Users  {#tab-users}

### Users  {#settings-users}

- **Multi-User Mode** {#settings-users-multi-user-mode}
- **Multi user** {#settings-users-multi-user} -- Allow multiple users to access this Stillwater instance with separate accounts and roles.
- **Enable multi-user mode** {#settings-users-enable-multi-user}
- **Manage** {#settings-users-manage} -- Manage who has access to this instance.
- **Create Invite** {#settings-users-create-invite}
- **Role** {#settings-users-role}
- **Role for invited user** {#settings-users-role-for-invite}
- **Expires In** {#settings-users-expires-in}
- **Invite expiry duration** {#settings-users-invite-expiry}
- **24 hours** {#settings-users-24-hours}
- **3 days** {#settings-users-3-days}
- **7 days** {#settings-users-7-days}
- **30 days** {#settings-users-30-days}
- **Invite Link** {#settings-users-invite-link}
- **Generated invite link** {#settings-users-generated-invite-link}
- **Copy invite link to clipboard** {#settings-users-copy-invite}
- **Invite link copied** {#settings-users-invite-link-copied}
- **Copy Link** {#settings-users-copy-link}
- **This link can only be used once.** {#settings-users-link-single-use}
- **User accounts** {#settings-users-user-accounts}
- **User** {#settings-users-user}
- **Auth Provider** {#settings-users-auth-provider}
- **Actions** {#settings-users-actions}
- **Loading users...** {#settings-users-loading}
- **Pending Invites** {#settings-users-pending-invites} -- Invite links that have not been redeemed yet.
- **Loading invites...** {#settings-users-loading-invites}
- **Role:** {#settings-users-role-label}
- **Expires:** {#settings-users-expires-label}
- **Revoke this invite? It will no longer be usable.** {#settings-users-revoke-confirm}
- **Invite revoked** {#settings-users-invite-revoked}
- **Revoke** {#settings-users-revoke}


## Auth Providers  {#tab-auth-providers}

### Authentication Providers  {#settings-auth}

Configure how users can authenticate with this instance.

- **Local** {#settings-auth-local} -- Username and password authentication managed by Stillwater.
- **Local authentication cannot be disabled. It provides break-glass access if all other providers are misconfigured.** {#settings-auth-local-always-on}
- **Enable Emby authentication** {#settings-auth-enable-emby}
- **Emby** {#settings-auth-emby} -- Authenticate using an Emby server account. Uses the existing Emby connection.
- **Server URL** {#settings-auth-server-url}
- **Sourced from your Emby connection** {#settings-auth-sourced-from-emby}
- **Auto-Provision** {#settings-auth-auto-provision}
- **Auto provision emby** {#settings-auth-auto-provision-emby} -- Automatically create accounts for valid Emby users
- **Enable auto-provisioning for Emby users** {#settings-auth-enable-auto-provision-emby}
- **Guard Rail** {#settings-auth-guard-rail} -- Who can auto-provision when enabled
- **Emby guard rail setting** {#settings-auth-emby-guard-rail}
- **Admins only** {#settings-auth-admins-only}
- **Any user** {#settings-auth-any-user}
- **Default Role** {#settings-auth-default-role} -- Role assigned to auto-provisioned users
- **Default role for Emby users** {#settings-auth-default-role-emby}
- **Enable Jellyfin authentication** {#settings-auth-enable-jellyfin}
- **Jellyfin** {#settings-auth-jellyfin} -- Authenticate using a Jellyfin server account. Requires an active Jellyfin connection.
- **Sourced from your Jellyfin connection** {#settings-auth-sourced-from-jellyfin}
- **Auto provision jellyfin** {#settings-auth-auto-provision-jellyfin} -- Automatically create accounts for valid Jellyfin users
- **Enable auto-provisioning for Jellyfin users** {#settings-auth-enable-auto-provision-jellyfin}
- **Jellyfin guard rail setting** {#settings-auth-jellyfin-guard-rail}
- **Default role for Jellyfin users** {#settings-auth-default-role-jellyfin}
- **Enable OpenID Connect authentication** {#settings-auth-enable-oidc}
- **Oidc** {#settings-auth-oidc} -- Single sign-on via Authentik, Keycloak, Authelia, or any OIDC-compliant provider.
- **Issuer URL** {#settings-auth-issuer-url}
- **Client ID** {#settings-auth-client-id}
- **Client Secret** {#settings-auth-client-secret}
- **Default role for OIDC users not in an admin group** {#settings-auth-default-role-oidc}
- **Administrator Groups** {#settings-auth-admin-groups}
- **Allowed Groups** {#settings-auth-allowed-groups}
- **Display Name** {#settings-auth-oidc-display-name}
- **Logo URL** {#settings-auth-oidc-logo-url}
- **Enable auto-provisioning for OIDC users** {#settings-auth-enable-auto-provision-oidc}
- **Auto provision oidc** {#settings-auth-auto-provision-oidc} -- Create accounts for authenticated OIDC users


## Maintenance  {#tab-maintenance}

### Confirmation Dialogs  {#settings-confirm-dialogs}

Manage "Don't ask again" preferences for confirmation dialogs throughout the app.

- **If you previously checked "Don't ask again" on a confirmation dialog, you can reset all preferences here to restore those prompts.** {#settings-confirm-dialogs-reset-info}
- **Preferences reset.** {#settings-confirm-dialogs-reset-success}

### Database Maintenance  {#settings-db-maintenance}

Optimize database performance and reclaim disk space.

- **Auto-optimize schedule** {#settings-db-maintenance-auto-schedule}
- **Runs PRAGMA optimize and WAL checkpoint on the selected interval. Requires restart to apply schedule changes.** {#settings-db-maintenance-schedule-note}

### Schedule  {#settings-schedule}

- **Every 6 hours** {#settings-schedule-every-6h}
- **Every 12 hours** {#settings-schedule-every-12h}
- **Daily (24h)** {#settings-schedule-daily}
- **Weekly** {#settings-schedule-weekly}

### Database Backup  {#settings-backup}

Create, download, and manage database backups.

- **Retention** {#settings-backup-retention}
- **Keep** {#settings-backup-keep}
- **backups** {#settings-backup-backups-unit}
- **Max age** {#settings-backup-max-age}
- **7 days** {#settings-backup-days-7}
- **14 days** {#settings-backup-days-14}
- **30 days** {#settings-backup-days-30}
- **60 days** {#settings-backup-days-60}
- **90 days** {#settings-backup-days-90}
- **Oldest backups are pruned after each automatic backup when they exceed the configured retention count or maximum age.** {#settings-backup-retention-note}
- **Loading backup history...** {#settings-backup-loading-history}

### Settings Export / Import  {#settings-export-import}

- **Export all settings (provider keys, connections, profiles, webhooks) as an encrypted file.** {#settings-export-import-description-line1}
- **A passphrase you choose protects the file, so it can be imported on any Stillwater instance.** {#settings-export-import-description-line2}
- **Export passphrase** {#settings-export-import-export-passphrase}
- **Import settings file** {#settings-export-import-import-file-label}
- **Import passphrase** {#settings-export-import-import-passphrase}
- **The export file is encrypted with your passphrase using PBKDF2 + AES-256-GCM.** {#settings-export-import-encryption-note-line1}
- **You will need the same passphrase to import the file on any instance.** {#settings-export-import-encryption-note-line2}


## Logs  {#tab-logs}

### Log Settings  {#settings-log-settings}

Configure log level, format, and file output with rotation. Changes take effect immediately.

- **Level** {#settings-log-settings-level}
- **Trace** {#settings-log-settings-level-trace}
- **Debug** {#settings-log-settings-level-debug}
- **Format** {#settings-log-settings-format}
- **JSON** {#settings-log-settings-format-json}
- **Text** {#settings-log-settings-format-text}
- **Revert log level on restart** {#settings-log-settings-revert-on-restart}
- **Revert to** {#settings-log-settings-revert-to}
- **on restart** {#settings-log-settings-on-restart}
- **Log to file** {#settings-log-settings-log-to-file} -- Write logs to a rotating file in addition to stdout.
- **Log file path** {#settings-log-settings-file-path}
- **Logs are always written to stdout. This enables an additional rotating file.** {#settings-log-settings-file-path-note}
- **Max size (MB)** {#settings-log-settings-max-size}
- **Files to keep** {#settings-log-settings-files-to-keep}
- **Max age (days)** {#settings-log-settings-max-age}

### Log Viewer  {#settings-log-viewer}

View application logs in real time with level filtering and search.

- **Log level filter** {#settings-log-viewer-level-filter}
- **File** {#settings-log-viewer-file-label}
- **Select log file to view** {#settings-log-viewer-select-file}
- **Live (current)** {#settings-log-viewer-live-current}
- **Showing up to 200 most recent entries. Live view polls in real time; historical files are loaded on demand.** {#settings-log-viewer-footer-note}


## Updates  {#tab-updates}

### Application Updates  {#settings-updates}

Check for new Stillwater releases and apply binary updates.

- **Config** {#settings-updates-config} -- Control how the updater discovers and applies new releases.
- **Updater enabled** {#settings-updates-enabled} -- Top-level kill switch. When off, both the background loop and the Apply button are disabled.
- **Release channel** {#settings-updates-channel} -- Stable tracks only non-prerelease versions. Prerelease includes release candidates. Nightly tracks date-stamped builds from the default branch.
- **Automatic update checks** {#settings-updates-auto-check} -- Periodically check for new releases in the background at the configured interval.
- **Check interval** {#settings-updates-check-interval} -- How often the background loop polls GitHub for new releases. Minimum is 1 hour.
<!-- END GENERATED: settings-reference -->
