---
description: A guided tour of every tab in Settings -- what each section does, where to find specific knobs, and notable behaviors.
---

<!-- code: web/templates/settings.templ (settingsTabs() at line 140 enumerates the 11 tabs; each panel keyed by data-tab-panel="..."). Inventory verified 2026-04-30 against main; Updates-tab fields re-verified 2026-05-01 with W2.E (#1117) landing the enabled toggle, check-interval selector, and the background scheduler that consumes auto-check (auto-apply is split out to #1284); General-tab base-path field is editable when SW_BASE_PATH is unset (#1005). -->

# Settings, by tab

Stillwater's Settings page is divided into 11 tabs. This page is a navigational reference -- each section below describes one tab, the major panels inside it, and where to find specific knobs. For deeper explanation of *what the settings mean*, follow the cross-links into the relevant Core Concepts or How-to pages.

<!-- BEGIN GENERATED: settings-reference -->

## General  {#tab-general}

### Platform Profile  {#settings-general-platform-profile}

Select the target platform to control NFO output and image naming conventions.

### Active Profile Details  {#settings-general-active-profile}

Edit filenames for each image type. Changes are saved to the profile.

- **NFO Output** -- Whether the active platform profile is configured to write NFO files. The profile editor controls the on/off state and the NFO format (Emby, Jellyfin, Kodi).
{: #settings-general-active-profile-nfo-output }
- **Save Filenames** -- Save the filename overrides above to the active platform profile so future writes use them.
{: #settings-general-active-profile-save-filenames }

### Use symlinks for duplicates  {#settings-general-symlinks}

### TLS Status  {#settings-general-tls-status}

Read-only summary of the HTTPS listener. Configure TLS via SW_TLS_CERT_FILE / SW_TLS_KEY_FILE or the [server.tls] block in config.toml; see the Direct TLS Setup how-to.

- **Active (BYO certificate)**
{: #settings-general-tls-status-active-byo }
- **Active (ACME)**
{: #settings-general-tls-status-active-acme }
- **Inactive**
{: #settings-general-tls-status-inactive }
- **Listening:**
{: #settings-general-tls-status-listening-label }

### Base Path  {#settings-general-base-path}

URL path prefix for running Stillwater behind a reverse proxy at a sub-path.

### Behavior  {#settings-general-behavior}

Configure default behaviors for metadata workflows.

### Show platform debug info on artist pages  {#settings-general-platform-debug}

When enabled, a Debug tab appears on artist detail pages for platform-connected artists

### Image Cache  {#settings-general-image-cache}

Manage cached images for artists without filesystem paths.

- **Maximum size** -- Cap the total disk space the image cache may use. When the cache reaches this size, the oldest entries are evicted first. Pick from 256 MB, 512 MB, 1 GB, 2 GB, or Unlimited.
{: #settings-general-image-cache-max-size }
- **Unlimited** -- When the maximum size is set to Unlimited, Stillwater never evicts cached images automatically. Disk usage grows with each new image fetched.
{: #settings-general-image-cache-unlimited }
- **Clear Cache** -- Remove every image currently held in the local cache. Cached images are re-fetched from providers the next time an artist screen is opened.
{: #settings-general-image-cache-clear }

## Providers  {#tab-providers}

### Provider API Keys  {#settings-providers-provider-keys}

Configure API keys for metadata providers. Providers without a configured key will be skipped during metadata lookups.

- **No API key required**
{: #settings-providers-provider-keys-no-key-required }
- **Premium key configured**
{: #settings-providers-provider-keys-premium-configured }
- **Free tier (optional premium upgrade)**
{: #settings-providers-provider-keys-free-tier }
- **Key configured**
{: #settings-providers-provider-keys-key-configured }
- **API key required**
{: #settings-providers-provider-keys-key-required }

### Web Image Search  {#settings-providers-web-search}

Enable web search providers to find additional artist images beyond authoritative sources. No API key required.

### Provider Priorities  {#settings-providers-priorities}

Set the preferred provider order for each metadata field. Drag to reorder. Click the checkmark/X to enable or disable. Only configured providers are shown.

- **Restore defaults**
{: #settings-providers-priorities-restore-defaults }

### Metadata Language Preferences  {#settings-providers-metadata-languages}

Set your preferred languages for artist names, biographies, and aliases. Providers will return content in the first available language from your list. Drag to reorder priority.

- **Remove**
{: #settings-providers-metadata-languages-remove }
- **Search languages**
{: #settings-providers-metadata-languages-input-label }

### Advanced  {#settings-providers-advanced}

Fine-tune provider matching behavior.

### Name similarity  {#settings-providers-name-similarity}

Minimum similarity score (0-100) required when matching artist names from search results. Set to 0 to disable name validation and accept any result. Default is 60.

- **Name Similarity Threshold**
{: #settings-providers-name-similarity-label }

### Provider config  {#settings-providers-provider-config}

- **Client ID**
{: #settings-providers-provider-config-client-id }
- **Client Secret**
{: #settings-providers-provider-config-client-secret }
- **Server**
{: #settings-providers-provider-config-server }
- **Official**
{: #settings-providers-provider-config-official }
- **Beta**
{: #settings-providers-provider-config-beta }
- **Custom mirror**
{: #settings-providers-provider-config-custom-mirror }
- **Self-hosted mirrors can often handle higher rates. Default: 10 req/s.**
{: #settings-providers-provider-config-custom-help }
- **OAuth Credentials**
{: #settings-providers-provider-config-oauth-credentials }
- **Required for submitting edits to MusicBrainz.**
{: #settings-providers-provider-config-oauth-note }

## Connections  {#tab-connections}

### Server Connections  {#settings-connections-connections}

Connect to Emby, Jellyfin, or Lidarr servers for library sync and metadata push.

- **Feature toggles**
{: #settings-connections-connections-feature-toggles }
- **What Stillwater sends to this connection**
{: #settings-connections-connections-sends-heading }
- **Library import**
{: #settings-connections-connections-feature-library-import }
- **When on, Stillwater imports library metadata from this server.**
{: #settings-connections-connections-feature-library-import-tooltip }
- **NFO write**
{: #settings-connections-connections-feature-nfo-write }
- **When on, Stillwater writes artist.nfo files for artists in this server's libraries. Writes can still be gated while conflict gating is active (write-back or round-trip overlap) -- see the top banner for details.**
{: #settings-connections-connections-feature-nfo-write-tooltip }
- **Image download/write**
{: #settings-connections-connections-feature-image-write }
- **When on, Stillwater writes image files for artists in this server's libraries. Writes can still be gated while conflict gating is active (write-back or round-trip overlap) -- see the top banner for details.**
{: #settings-connections-connections-feature-image-write-tooltip }
- **Let Stillwater manage artwork and NFO files on this server**
{: #settings-connections-connections-manage-title }
- **Not configured**
{: #settings-connections-connections-not-configured }

## Libraries  {#tab-libraries}

### Music Libraries  {#settings-libraries-libraries}

Manage your music library paths. Each library maps to a directory containing artist folders.

- **Connection**
{: #settings-libraries-libraries-connection-badge }
- **Lock NFOs**
{: #settings-libraries-libraries-lock-nfo-label }
- **When on, Stillwater stamps &lt;lockdata&gt;true&lt;/lockdata&gt; into every NFO it writes for this library. This tells Emby and Jellyfin to refuse metadata refreshes for those artists, preserving Stillwater's curated values from being overwritten by the platform's own scrapers. Off by default. Tip: artists whose NFO already contains &lt;lockdata&gt;true&lt;/lockdata&gt; (set by Stillwater or another tool) are automatically marked as locked at the artist level.**
{: #settings-libraries-libraries-lock-nfo-title }
- **Add Library**
{: #settings-libraries-libraries-add }
- **Regular**
{: #settings-libraries-libraries-type-regular }
- **Classical**
{: #settings-libraries-libraries-type-classical }
- **Filesystem monitoring mode**
{: #settings-libraries-libraries-fs-mode-title }
- **Off**
{: #settings-libraries-libraries-fs-off }
- **Watch**
{: #settings-libraries-libraries-fs-watch }
- **Poll**
{: #settings-libraries-libraries-fs-poll }
- **Watch + Poll**
{: #settings-libraries-libraries-fs-both }
- **Re-sync Artists**
{: #settings-libraries-libraries-resync }
- **Scan Library**
{: #settings-libraries-libraries-scan }

## Automation  {#tab-automation}

### Webhooks  {#settings-automation-webhooks}

Send notifications to external services when events occur.

- **Add Webhook**
{: #settings-automation-webhooks-add }
- **Select type...**
{: #settings-automation-webhooks-select-type }
- **Generic (JSON)**
{: #settings-automation-webhooks-type-generic }

### Notification Badges  {#settings-automation-notif-badges}

Show a counter badge on the Open Violations link in the sidebar indicating active violations.

- **Enable badge**
{: #settings-automation-notif-badges-enable-badge }
- **Count violations by severity**
{: #settings-automation-notif-badges-count-by-severity }

### API Tokens  {#settings-automation-api-tokens}

Generate tokens for external applications to access the Stillwater API.

- **Revoked**
{: #settings-automation-api-tokens-revoked }
- **Read**
{: #settings-automation-api-tokens-scope-read }
- **Write**
{: #settings-automation-api-tokens-scope-write }
- **Webhook**
{: #settings-automation-api-tokens-scope-webhook }
- **Admin**
{: #settings-automation-api-tokens-scope-admin }

### Inbound Webhooks  {#settings-automation-inbound-webhooks}

Receive events from external applications to trigger actions in Stillwater.

- **Webhook URL**
{: #settings-automation-inbound-webhooks-url-label }
- **Supported events**
{: #settings-automation-inbound-webhooks-supported-events }
- **Supported events (Emby internal names)**
{: #settings-automation-inbound-webhooks-supported-events-emby }

## Rules  {#tab-rules}

### Rules  {#settings-rules-rules}

- **Image-category rules are paused while conflict gating is active. Resolve the active write-back or round-trip conflict in the top banner to resume auto-fix.**
{: #settings-rules-rules-conflict-gated-image-tooltip }
- **paused: conflict gating**
{: #settings-rules-rules-conflict-gated-chip }
- **NFO-category rules are paused while conflict gating is active. Resolve the active write-back or round-trip conflict in the top banner to resume auto-fix.**
{: #settings-rules-rules-conflict-gated-nfo-tooltip }
- **This rule requires a local library with a filesystem path. Add a library with a path to enable it.**
{: #settings-rules-rules-requires-local-tooltip }
- **Requires a local library with a filesystem path**
{: #settings-rules-rules-requires-local-tooltip-short }
- **Requires local library**
{: #settings-rules-rules-requires-local }
- **Auto-fix**
{: #settings-rules-rules-auto-fix }
- **Manual (notify only)**
{: #settings-rules-rules-manual }
- **Cannot enable: no local library configured**
{: #settings-rules-rules-cannot-enable-tooltip }

### Scheduled Evaluation  {#settings-rules-rule-schedule}

Run all enabled rules on a recurring schedule. Requires a container restart after changing.

- **Evaluates all enabled rules against every artist on the selected interval. Changes take effect after container restart.**
{: #settings-rules-rule-schedule-note }

### Schedule  {#settings-rules-schedule}

- **Every 5 minutes**
{: #settings-rules-schedule-every-5m }
- **Every 15 minutes**
{: #settings-rules-schedule-every-15m }
- **Every 30 minutes**
{: #settings-rules-schedule-every-30m }
- **Every hour**
{: #settings-rules-schedule-every-hour }
- **Every 6 hours**
{: #settings-rules-schedule-every-6h }
- **Every 12 hours**
{: #settings-rules-schedule-every-12h }
- **Daily (24h)**
{: #settings-rules-schedule-daily }

## Users  {#tab-users}

### Users  {#settings-users-users}

Manage who has access to this instance.

- **Multi-User Mode** -- Allow multiple users to access this Stillwater instance with separate accounts and roles.
{: #settings-users-users-multi-user-mode }
- **Enable multi-user mode**
{: #settings-users-users-enable-multi-user }
- **Create Invite**
{: #settings-users-users-create-invite }
- **Role**
{: #settings-users-users-role }
- **Role for invited user**
{: #settings-users-users-role-for-invite }
- **Expires In**
{: #settings-users-users-expires-in }
- **Invite expiry duration**
{: #settings-users-users-invite-expiry }
- **24 hours**
{: #settings-users-users-24-hours }
- **3 days**
{: #settings-users-users-3-days }
- **7 days**
{: #settings-users-users-7-days }
- **30 days**
{: #settings-users-users-30-days }
- **Copy invite link to clipboard**
{: #settings-users-users-copy-invite }
- **This link can only be used once.**
{: #settings-users-users-link-single-use }
- **User accounts**
{: #settings-users-users-user-accounts }
- **User**
{: #settings-users-users-user }
- **Auth Provider**
{: #settings-users-users-auth-provider }
- **Actions**
{: #settings-users-users-actions }
- **Pending Invites** -- Invite links that have not been redeemed yet.
{: #settings-users-users-pending-invites }
- **Role:**
{: #settings-users-users-role-label }
- **Expires:**
{: #settings-users-users-expires-label }
- **Revoke this invite? It will no longer be usable.**
{: #settings-users-users-revoke-confirm }
- **Invite revoked**
{: #settings-users-users-invite-revoked }
- **Revoke**
{: #settings-users-users-revoke }

## Auth Providers  {#tab-auth-providers}

### Authentication Providers  {#settings-auth-providers-auth}

Configure how users can authenticate with this instance.

- **Local** -- Username and password authentication managed by Stillwater.
{: #settings-auth-providers-auth-local }
- **Local authentication cannot be disabled. It provides break-glass access if all other providers are misconfigured.**
{: #settings-auth-providers-auth-local-always-on }
- **Emby** -- Authenticate using an Emby server account. Uses the existing Emby connection.
{: #settings-auth-providers-auth-emby }
- **Enable Emby authentication**
{: #settings-auth-providers-auth-enable-emby }
- **Server URL**
{: #settings-auth-providers-auth-server-url }
- **Sourced from your Emby connection**
{: #settings-auth-providers-auth-sourced-from-emby }
- **Auto-Provision** -- Automatically create accounts for valid Emby users
{: #settings-auth-providers-auth-auto-provision-emby }
- **Enable auto-provisioning for Emby users**
{: #settings-auth-providers-auth-enable-auto-provision-emby }
- **Guard Rail** -- Who can auto-provision when enabled
{: #settings-auth-providers-auth-guard-rail }
- **Emby guard rail setting**
{: #settings-auth-providers-auth-emby-guard-rail }
- **Admins only**
{: #settings-auth-providers-auth-admins-only }
- **Any user**
{: #settings-auth-providers-auth-any-user }
- **Default Role** -- Role assigned to auto-provisioned users
{: #settings-auth-providers-auth-default-role }
- **Default role for Emby users**
{: #settings-auth-providers-auth-default-role-emby }
- **Jellyfin** -- Authenticate using a Jellyfin server account. Requires an active Jellyfin connection.
{: #settings-auth-providers-auth-jellyfin }
- **Enable Jellyfin authentication**
{: #settings-auth-providers-auth-enable-jellyfin }
- **Sourced from your Jellyfin connection**
{: #settings-auth-providers-auth-sourced-from-jellyfin }
- **Auto-Provision** -- Automatically create accounts for valid Jellyfin users
{: #settings-auth-providers-auth-auto-provision-jellyfin }
- **Enable auto-provisioning for Jellyfin users**
{: #settings-auth-providers-auth-enable-auto-provision-jellyfin }
- **Jellyfin guard rail setting**
{: #settings-auth-providers-auth-jellyfin-guard-rail }
- **Default role for Jellyfin users**
{: #settings-auth-providers-auth-default-role-jellyfin }
- **OpenID Connect (OIDC)** -- Single sign-on via Authentik, Keycloak, Authelia, or any OIDC-compliant provider.
{: #settings-auth-providers-auth-oidc }
- **Enable OpenID Connect authentication**
{: #settings-auth-providers-auth-enable-oidc }
- **Issuer URL**
{: #settings-auth-providers-auth-issuer-url }
- **Client ID**
{: #settings-auth-providers-auth-client-id }
- **Client Secret**
{: #settings-auth-providers-auth-client-secret }
- **Default role for OIDC users not in an admin group**
{: #settings-auth-providers-auth-default-role-oidc }
- **Administrator Groups**
{: #settings-auth-providers-auth-admin-groups }
- **Allowed Groups**
{: #settings-auth-providers-auth-allowed-groups }
- **Display Name**
{: #settings-auth-providers-auth-oidc-display-name }
- **Logo URL**
{: #settings-auth-providers-auth-oidc-logo-url }
- **Auto-Provision** -- Create accounts for authenticated OIDC users
{: #settings-auth-providers-auth-auto-provision-oidc }
- **Enable auto-provisioning for OIDC users**
{: #settings-auth-providers-auth-enable-auto-provision-oidc }

## Maintenance  {#tab-maintenance}

### Confirmation Dialogs  {#settings-maintenance-confirm-dialogs}

Manage "Don't ask again" preferences for confirmation dialogs throughout the app.

### Database Maintenance  {#settings-maintenance-db-maintenance}

Optimize database performance and reclaim disk space.

- **Auto-optimize schedule**
{: #settings-maintenance-db-maintenance-auto-schedule }
- **Runs PRAGMA optimize and WAL checkpoint on the selected interval. Requires restart to apply schedule changes.**
{: #settings-maintenance-db-maintenance-schedule-note }

### Schedule  {#settings-maintenance-schedule}

- **Every 6 hours**
{: #settings-maintenance-schedule-every-6h }
- **Every 12 hours**
{: #settings-maintenance-schedule-every-12h }
- **Daily (24h)**
{: #settings-maintenance-schedule-daily }
- **Weekly**
{: #settings-maintenance-schedule-weekly }

### Database Backup  {#settings-maintenance-backup}

Create, download, and manage database backups.

- **Retention**
{: #settings-maintenance-backup-retention }
- **Keep**
{: #settings-maintenance-backup-keep }
- **backups**
{: #settings-maintenance-backup-backups-unit }
- **Max age**
{: #settings-maintenance-backup-max-age }
- **7 days**
{: #settings-maintenance-backup-days-7 }
- **14 days**
{: #settings-maintenance-backup-days-14 }
- **30 days**
{: #settings-maintenance-backup-days-30 }
- **60 days**
{: #settings-maintenance-backup-days-60 }
- **90 days**
{: #settings-maintenance-backup-days-90 }
- **Oldest backups are pruned after each automatic backup when they exceed the configured retention count or maximum age.**
{: #settings-maintenance-backup-retention-note }

### Settings Export / Import  {#settings-maintenance-export-import}

- **Export passphrase**
{: #settings-maintenance-export-import-export-passphrase }
- **Import settings file**
{: #settings-maintenance-export-import-import-file-label }
- **Import passphrase**
{: #settings-maintenance-export-import-import-passphrase }
- **The export file is encrypted with your passphrase using PBKDF2 + AES-256-GCM.**
{: #settings-maintenance-export-import-encryption-note-line1 }
- **You will need the same passphrase to import the file on any instance.**
{: #settings-maintenance-export-import-encryption-note-line2 }

## Logs  {#tab-logs}

### Log Settings  {#settings-logs-log-settings}

Configure log level, format, and file output with rotation. Changes take effect immediately.

- **Level**
{: #settings-logs-log-settings-level }
- **Trace**
{: #settings-logs-log-settings-level-trace }
- **Debug**
{: #settings-logs-log-settings-level-debug }
- **Format**
{: #settings-logs-log-settings-format }
- **JSON**
{: #settings-logs-log-settings-format-json }
- **Text**
{: #settings-logs-log-settings-format-text }
- **Revert log level on restart**
{: #settings-logs-log-settings-revert-on-restart }
- **Revert to**
{: #settings-logs-log-settings-revert-to }
- **on restart**
{: #settings-logs-log-settings-on-restart }
- **Log to file** -- Write logs to a rotating file in addition to stdout.
{: #settings-logs-log-settings-log-to-file }
- **Log file path**
{: #settings-logs-log-settings-file-path }
- **Logs are always written to stdout. This enables an additional rotating file.**
{: #settings-logs-log-settings-file-path-note }
- **Max size (MB)**
{: #settings-logs-log-settings-max-size }
- **Files to keep**
{: #settings-logs-log-settings-files-to-keep }
- **Max age (days)**
{: #settings-logs-log-settings-max-age }

### Log Viewer  {#settings-logs-log-viewer}

View application logs in real time with level filtering and search.

- **Log level filter**
{: #settings-logs-log-viewer-level-filter }
- **File**
{: #settings-logs-log-viewer-file-label }
- **Select log file to view**
{: #settings-logs-log-viewer-select-file }
- **Live (current)**
{: #settings-logs-log-viewer-live-current }
- **Showing up to 200 most recent entries. Live view polls in real time; historical files are loaded on demand.**
{: #settings-logs-log-viewer-footer-note }

## Updates  {#tab-updates}

### Application Updates  {#settings-updates-updates}

Check for new Stillwater releases and apply binary updates.

- **Update channel and schedule** -- Control how the updater discovers and applies new releases.
{: #settings-updates-updates-config }
- **Updater enabled** -- Master toggle for the updater. When off, Stillwater stops checking for new releases and the Apply Update button is disabled.
{: #settings-updates-updates-enabled }
- **Release channel** -- Pick which kinds of releases to receive. Stable is the recommended default and only delivers fully tested versions. Prerelease also includes release candidates that are still being polished. Nightly delivers in-progress builds from active development; expect rough edges.
{: #settings-updates-updates-channel }
- **Automatic update checks** -- Periodically check for new releases in the background at the configured interval.
{: #settings-updates-updates-auto-check }
- **Automatically install updates** -- When on, Stillwater installs new releases automatically as they become available. This setting has no effect on Docker installs; Docker users update by pulling a new image and recreating the container.
{: #settings-updates-updates-auto-update }
- **Last auto-applied**
{: #settings-updates-updates-last-auto-applied }
- **Skipped versions**
{: #settings-updates-updates-skip-version-list-label }
- **Check interval** -- How often Stillwater checks GitHub for new releases. Minimum is once per hour.
{: #settings-updates-updates-check-interval }
<!-- END GENERATED: settings-reference -->
