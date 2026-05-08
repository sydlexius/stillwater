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

- **No API key required** -- Shown next to a provider that works without any API key. Nothing to configure here.
{: #settings-providers-provider-keys-no-key-required }
- **Premium key configured** -- Shown next to a provider with optional premium tier when a premium key has been saved. The provider is queried using the upgraded access.
{: #settings-providers-provider-keys-premium-configured }
- **Free tier (optional premium upgrade)** -- Shown next to a provider that works without a key but offers an optional premium key for higher rate limits or extra features. Add a key here to upgrade.
{: #settings-providers-provider-keys-free-tier }
- **Key configured** -- Shown next to a provider that requires an API key once a valid key has been saved. Stillwater will include this provider in metadata lookups.
{: #settings-providers-provider-keys-key-configured }
- **API key required** -- Shown next to a provider that needs an API key but does not have one saved yet. Stillwater skips this provider during lookups until a key is added.
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

- **Client ID** -- The OAuth application identifier issued by the provider when you registered an app. Paste it here so Stillwater can authenticate against the provider's API.
{: #settings-providers-provider-config-client-id }
- **Client Secret** -- The OAuth application secret issued alongside the Client ID. Stored encrypted at rest. Treat it like a password and do not share it.
{: #settings-providers-provider-config-client-secret }
- **Server** -- Which MusicBrainz endpoint Stillwater queries. Pick a preset or supply a custom mirror URL.
{: #settings-providers-provider-config-server }
- **Official** -- Send requests to musicbrainz.org. The public service is rate limited to roughly one request per second.
{: #settings-providers-provider-config-official }
- **Beta** -- Send requests to the MusicBrainz beta server. The data set matches the official server, but the deployment runs preview code. Subject to the same one-request-per-second public rate limit.
{: #settings-providers-provider-config-beta }
- **Custom mirror** -- Point Stillwater at a self-hosted MusicBrainz mirror. Requests bypass the public rate limit so you can raise throughput, subject to whatever your mirror can sustain.
{: #settings-providers-provider-config-custom-mirror }
- **OAuth Credentials** -- Sub-section for OAuth application credentials used when a provider requires authenticated access. Fill in the Client ID and Client Secret issued by the provider.
{: #settings-providers-provider-config-oauth-credentials }

## Connections  {#tab-connections}

### Server Connections  {#settings-connections-connections}

Connect to Emby, Jellyfin, or Lidarr servers for library sync and metadata push.

- **Feature toggles**
{: #settings-connections-connections-feature-toggles }
- **What Stillwater sends to this connection**
{: #settings-connections-connections-sends-heading }
- **Library import** -- When on, Stillwater imports library and artist metadata from this server during scans.
{: #settings-connections-connections-feature-library-import }
- **NFO write** -- When on, Stillwater writes artist.nfo files into folders this server's libraries cover. Writes can still be paused while a conflict gate is active.
{: #settings-connections-connections-feature-nfo-write }
- **Image download/write** -- When on, Stillwater downloads images from providers and writes them to artist folders that this server's libraries cover. Writes can still be paused while a conflict gate is active.
{: #settings-connections-connections-feature-image-write }
- **Let Stillwater manage artwork and NFO files on this server**
{: #settings-connections-connections-manage-title }
- **Not configured**
{: #settings-connections-connections-not-configured }

## Libraries  {#tab-libraries}

### Music Libraries  {#settings-libraries-libraries}

Manage your music library paths. Each library maps to a directory containing artist folders.

- **Connection**
{: #settings-libraries-libraries-connection-badge }
- **Lock NFOs** -- When on, Stillwater stamps a lockdata flag into every NFO it writes for this library. This tells Emby and Jellyfin to refuse metadata refreshes for those artists so Stillwater's curated values are not overwritten by the platform's own scrapers. Off by default. Artists whose NFO already contains a lockdata flag (set by Stillwater or another tool) are automatically marked as locked at the artist level.
{: #settings-libraries-libraries-lock-nfo-label }
- **Add Library**
{: #settings-libraries-libraries-add }
- **Regular** -- Standard music library where the artist folder maps to the performing artist.
{: #settings-libraries-libraries-type-regular }
- **Classical** -- Treat the library as a classical collection. Stillwater applies composer-aware naming and folder conventions during scans and writes.
{: #settings-libraries-libraries-type-classical }
- **Filesystem monitoring mode** -- How Stillwater detects new or changed files in this library. Watching subscribes to native filesystem events; polling re-scans on a fixed interval.
{: #settings-libraries-libraries-fs-mode-title }
- **Off** -- Stillwater does not monitor this library's filesystem. New files are picked up only by manual scans.
{: #settings-libraries-libraries-fs-off }
- **Watch** -- Subscribe to native filesystem events so changes are picked up immediately. Recommended for local disks.
{: #settings-libraries-libraries-fs-watch }
- **Poll** -- Periodically re-scan the library for changes. Works on every filesystem but adds some delay between a change and Stillwater noticing it.
{: #settings-libraries-libraries-fs-poll }
- **Watch + Poll** -- Combine native filesystem events with periodic polling. Useful when watching alone misses some changes (for example, on certain network shares).
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
- **Generic (JSON)** -- Send a generic JSON payload describing the event. Use this for in-house tooling or services without a dedicated formatter.
{: #settings-automation-webhooks-type-generic }

### Notification Badges  {#settings-automation-notif-badges}

Show a counter badge on the Open Violations link in the sidebar indicating active violations.

- **Enable badge** -- Show a counter next to the Reports link in the sidebar with the number of active violations.
{: #settings-automation-notif-badges-enable-badge }
- **Count violations by severity** -- Choose which severity levels contribute to the sidebar badge count. Disable a severity to ignore those violations in the badge while still surfacing them on the Reports page.
{: #settings-automation-notif-badges-count-by-severity }

### API Tokens  {#settings-automation-api-tokens}

Generate tokens for external applications to access the Stillwater API.

- **Revoked**
{: #settings-automation-api-tokens-revoked }
- **Read** -- Read-only access to artists, libraries, rules, and settings. Safe for dashboards and one-way integrations that only fetch data.
{: #settings-automation-api-tokens-scope-read }
- **Write** -- Create and modify artists, run rules, and queue background work. Use this for automation scripts that need to make changes but should not touch user accounts.
{: #settings-automation-api-tokens-scope-write }
- **Webhook** -- Lets the token receive inbound webhook deliveries from external systems. Pair with a single integration so an exposed token does not also grant read or write access.
{: #settings-automation-api-tokens-scope-webhook }
- **Admin** -- Full administrative access. Treat tokens with this scope like a root password: they can change settings, manage users, and revoke other tokens.
{: #settings-automation-api-tokens-scope-admin }

## Rules  {#tab-rules}

### Rules  {#settings-rules-rules}

- **paused: conflict gating** -- Shown on the NFO or Image rule category header when a write-back or round-trip conflict is active in the top banner. Auto-fix is paused for that category until you resolve the conflict.
{: #settings-rules-rules-conflict-gated-chip }
- **Requires local library** -- Shown next to a rule that needs a library with a filesystem path Stillwater can read. The rule is disabled until you add at least one local library on the Libraries tab.
{: #settings-rules-rules-requires-local }
- **Auto-fix** -- When the rule finds a violation, Stillwater corrects it automatically during scans without prompting.
{: #settings-rules-rules-auto-fix }
- **Manual (notify only)** -- Stillwater records violations and surfaces them on the Reports page, but does not modify any files. You apply each fix manually.
{: #settings-rules-rules-manual }

### Scheduled Evaluation  {#settings-rules-rule-schedule}

Run all enabled rules on a recurring schedule. Requires a container restart after changing.

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
- **Role** -- The permission level granted to the new account when this invite is redeemed. Admins manage settings; standard users browse and tag their own library.
{: #settings-users-users-role }
- **Role for invited user** -- Accessible label for the role selector when creating an invite. Mirrors the visible Role control.
{: #settings-users-users-role-for-invite }
- **Expires In** -- How long the new invite link remains usable. After this period the link expires and can no longer redeem an account.
{: #settings-users-users-expires-in }
- **Invite expiry duration** -- Accessible label for the expiry-duration selector when creating an invite. Mirrors the visible Expires In control.
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
- **User accounts** -- Table of every account that exists on this instance. Use the row controls to change a user's role or deactivate them.
{: #settings-users-users-user-accounts }
- **User**
{: #settings-users-users-user }
- **Auth Provider**
{: #settings-users-users-auth-provider }
- **Actions**
{: #settings-users-users-actions }
- **Pending Invites** -- Invite links that have not been redeemed yet.
{: #settings-users-users-pending-invites }
- **Role:** -- Marks the role badge shown next to a pending invite in the list below. The redeemed account will be created with this role.
{: #settings-users-users-role-label }
- **Expires:** -- Marks the expiry timestamp shown next to a pending invite in the list below. The invite stops working after this time.
{: #settings-users-users-expires-label }
- **Revoke**
{: #settings-users-users-revoke }

## Auth Providers  {#tab-auth-providers}

### Authentication Providers  {#settings-auth-providers-auth}

Configure how users can authenticate with this instance.

- **Local** -- Username and password authentication managed by Stillwater.
{: #settings-auth-providers-auth-local }
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
- **Admins only** -- Only users who already have an admin account in the upstream provider are allowed to auto-provision. Other users can still sign in but will not have a Stillwater account created for them.
{: #settings-auth-providers-auth-admins-only }
- **Any user** -- Every user the upstream provider authenticates is auto-provisioned a Stillwater account at the configured default role.
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
- **Issuer URL** -- Base URL of your OIDC provider. Stillwater discovers the rest of the endpoints automatically through the provider's well-known configuration document.
{: #settings-auth-providers-auth-issuer-url }
- **Client ID** -- Public identifier registered for Stillwater in your OIDC provider.
{: #settings-auth-providers-auth-client-id }
- **Client Secret** -- Confidential credential issued by your OIDC provider. Leave blank when editing other fields to keep the existing secret.
{: #settings-auth-providers-auth-client-secret }
- **Default role for OIDC users not in an admin group**
{: #settings-auth-providers-auth-default-role-oidc }
- **Administrator Groups** -- Comma-separated list of OIDC groups whose members are granted the Administrator role. Stillwater reads the values from the groups claim on the ID token.
{: #settings-auth-providers-auth-admin-groups }
- **Allowed Groups** -- Comma-separated list of OIDC groups allowed to log in. Leave empty to allow every authenticated user from this provider.
{: #settings-auth-providers-auth-allowed-groups }
- **Display Name** -- Provider name shown on the sign-in button. Falls back to a generic OIDC label when blank.
{: #settings-auth-providers-auth-oidc-display-name }
- **Logo URL** -- Optional image shown next to the OIDC sign-in button. A key icon is used when blank.
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

- **Auto-optimize schedule** -- How often Stillwater runs the optimize task in the background. The schedule applies after a restart.
{: #settings-maintenance-db-maintenance-auto-schedule }

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

- **Retention** -- Controls how long Stillwater keeps automatic backups before pruning them.
{: #settings-maintenance-backup-retention }
- **Keep** -- Maximum number of backups to retain. Older backups are pruned after each automatic backup once this count is exceeded.
{: #settings-maintenance-backup-keep }
- **backups**
{: #settings-maintenance-backup-backups-unit }
- **Max age** -- Discard automatic backups older than the selected age. Set to Never to keep backups indefinitely; the count limit still applies.
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

### Settings Export / Import  {#settings-maintenance-export-import}

- **Export passphrase** -- Pick a passphrase used to encrypt the exported file. You will need the same passphrase to import the file later.
{: #settings-maintenance-export-import-export-passphrase }
- **Import settings file** -- Pick the encrypted .json file produced by a previous export.
{: #settings-maintenance-export-import-import-file-label }
- **Import passphrase** -- Enter the same passphrase used when the file was exported.
{: #settings-maintenance-export-import-import-passphrase }

## Logs  {#tab-logs}

### Log Settings  {#settings-logs-log-settings}

Configure log level, format, and file output with rotation. Changes take effect immediately.

- **Level** -- Minimum severity to record. Trace and Debug are verbose and intended for troubleshooting; Info is a good default for normal operation.
{: #settings-logs-log-settings-level }
- **Trace**
{: #settings-logs-log-settings-level-trace }
- **Debug**
{: #settings-logs-log-settings-level-debug }
- **Format** -- JSON is easier to ingest into log shippers and search tools. Text is more readable when you are reading the file directly.
{: #settings-logs-log-settings-format }
- **JSON**
{: #settings-logs-log-settings-format-json }
- **Text**
{: #settings-logs-log-settings-format-text }
- **Revert log level on restart** -- Apply the new log level only for the current process. The persisted level is restored when Stillwater restarts. Useful for temporary debug sessions.
{: #settings-logs-log-settings-revert-on-restart }
- **Revert to**
{: #settings-logs-log-settings-revert-to }
- **on restart**
{: #settings-logs-log-settings-on-restart }
- **Log to file** -- Write logs to a rotating file in addition to stdout.
{: #settings-logs-log-settings-log-to-file }
- **Log file path** -- Where Stillwater writes the rotating log file. Use a path inside your config directory so the log persists across container restarts.
{: #settings-logs-log-settings-file-path }
- **Max size (MB)** -- Rotate the log file once it grows past this size. The active file becomes a numbered archive and a new active file is started.
{: #settings-logs-log-settings-max-size }
- **Files to keep** -- Number of rotated log files to retain alongside the active one. Older files are removed during rotation.
{: #settings-logs-log-settings-files-to-keep }
- **Max age (days)** -- Discard rotated log files older than this many days, regardless of how many files exist.
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
