---
description: A guided tour of every section in Settings -- what each section does, where to find specific knobs, and notable behaviors.
---

<!-- code: web/templates/settings_page.templ (settingsPane() composes the sections; each is keyed by @nextSettingsSection("<id>", "<group>")). The single-scroll Settings screen promoted from the next/ lane in #1757 PR-5; the retired tabbed page and its settingsTabs() enumerator are gone. Updates-section fields re-verified 2026-05-01 with W2.E (#1117) landing the enabled toggle, check-interval selector, and the background scheduler that consumes auto-check (auto-apply is split out to #1284); the Base Path field is editable when SW_BASE_PATH is unset (#1005). -->

# Settings, by section

Stillwater's Settings page is a single-scroll screen organized into sections grouped under Essentials, Data, Integrations, and System. This page is a navigational reference -- each section below describes one settings section, the major cards inside it, and where to find specific knobs. For deeper explanation of *what the settings mean*, follow the cross-links into the relevant Core Concepts or How-to pages.

If you already know the name of the control you want, the search box at the top of `/settings` jumps to it directly -- see [find a setting](../how-to/find-a-setting.md).

<!-- BEGIN GENERATED: settings-reference -->

## General  {#tab-general}

### Base Path  {#settings-general-base-path}

URL path prefix for running Stillwater behind a reverse proxy at a sub-path.

### TLS Status  {#settings-general-tls-status}

Read-only summary of the HTTPS listener. Configure TLS via SW_TLS_CERT_FILE / SW_TLS_KEY_FILE for a bring-your-own certificate, or SW_ACME_DOMAIN for an automatic ACME-managed certificate (Let's Encrypt by default). Enable HTTP/3 (QUIC) with SW_HTTP3_ENABLED=true.

- **Active (BYO certificate)**
{: #settings-general-tls-status-active-byo }
- **Active (ACME, %s)** -- Status shown when ACME auto-cert is active and SW_ACME_DOMAIN is set. The domain name is substituted into the display label at runtime.
{: #settings-general-tls-status-active-acme-with-domain }
- **Active (ACME)**
{: #settings-general-tls-status-active-acme }
- **Experimental** -- Small chip rendered next to the Active (ACME) status when ACME auto-cert is in use. Signals that the ACME path (Let's Encrypt today; future providers such as ZeroSSL) is unverified and should be treated as preview-only until validated against a real deployment.
{: #settings-general-tls-status-acme-experimental-badge }
- **Inactive**
{: #settings-general-tls-status-inactive }
- **Listening:**
{: #settings-general-tls-status-listening-label }
- **HTTP on :%d** -- Plain-HTTP listener. The port number is substituted at runtime.
{: #settings-general-tls-status-listener-http }
- **HTTPS on :%d** -- HTTPS listener. The port number is substituted at runtime.
{: #settings-general-tls-status-listener-https }
- **HTTP redirect on :%d** -- HTTP redirect listener: every plain-HTTP request on this port receives a 301 redirect to the HTTPS port. *Visibility:* Only present when SW_HTTP_REDIRECT_PORT is configured.
{: #settings-general-tls-status-listener-redirect }
- **HTTP/3 on :%d/UDP** -- HTTP/3 (QUIC) listener over UDP. The port number is substituted at runtime. *Visibility:* Only present when SW_HTTP3_ENABLED=true.
{: #settings-general-tls-status-listener-http3 }

### Confirmation Dialogs  {#settings-general-confirm-dialogs}

Destructive actions in Stillwater (delete an artist, clear a cache, revoke a token) prompt you to confirm before going through. Each dialog has a Don't ask again checkbox; this section lists every dialog you have suppressed so you can re-enable the prompt if you change your mind.

## Music libraries  {#tab-libraries}

### Music Libraries  {#settings-libraries-libraries}

A library is a top-level directory containing one folder per artist; that is the layout Emby, Jellyfin, and Kodi all expect. Add a library entry for each such directory you want Stillwater to scan and write into. Filesystem watch mode is configured per entry below.

- **Connection**
{: #settings-libraries-libraries-connection-badge }
- **Lock NFOs** -- When on, Stillwater stamps a lockdata flag into every NFO it writes for this library. This tells Emby and Jellyfin to refuse metadata refreshes for those artists so Stillwater's curated values are not overwritten by the platform's own scrapers. Off by default. Artists whose NFO already contains a lockdata flag (set by Stillwater or another tool) are automatically marked as locked at the artist level.
{: #settings-libraries-libraries-lock-nfo-label }
- **Filesystem monitoring mode** -- How Stillwater detects new or changed files in this library. Watching subscribes to native filesystem events; polling re-scans on a fixed interval.
{: #settings-libraries-libraries-fs-mode-title }
- **Re-sync Artists**
{: #settings-libraries-libraries-resync }
- **Scan Library**
{: #settings-libraries-libraries-scan }
- **Off** -- Stillwater does not monitor this library's filesystem. New files are picked up only by manual scans.
{: #settings-libraries-libraries-fs-off }
- **Watch** -- Subscribe to native filesystem events so changes are picked up immediately. Recommended for local disks.
{: #settings-libraries-libraries-fs-watch }
- **Poll** -- Periodically re-scan the library for changes. Works on every filesystem but adds some delay between a change and Stillwater noticing it.
{: #settings-libraries-libraries-fs-poll }
- **Watch + Poll** -- Combine native filesystem events with periodic polling. Useful when watching alone misses some changes (for example, on certain network shares).
{: #settings-libraries-libraries-fs-both }
- **Poll interval**
{: #settings-libraries-libraries-poll-interval-title }
- **Add Library**
{: #settings-libraries-libraries-add }
- **Library Name**
{: #settings-libraries-libraries-name }
- **Library Path**
{: #settings-libraries-libraries-path }

## Platform profile  {#tab-platform}

### Platform Profile  {#settings-platform-platform-profile}

A platform profile bundles the NFO format and image filename conventions for one media platform (Emby, Jellyfin, or Kodi), since each platform expects metadata laid out differently on disk. The active profile is the one Stillwater uses when writing files; change it here to retarget output for a different platform.

### Active Profile Details  {#settings-platform-active-profile}

A platform profile bundles the NFO format and image filename conventions for one media platform (Emby, Jellyfin, or Kodi). This card shows what the active profile will write and lets you override the filename used for each image type. Edits are saved back to the profile.

- **NFO Output** -- NFO files are XML metadata sidecars that Emby, Jellyfin, and Kodi read alongside artist folders to display biography, MusicBrainz ID, and other curated values. This row reflects the active platform profile's NFO output choice: whether Stillwater writes them at all, and in which format. Edit the profile to change.
{: #settings-platform-active-profile-nfo-output }
- **Save Filenames** -- Image filenames are how each platform discovers cover art on disk (folder.jpg, fanart.jpg, banner.jpg, and so on). Saves the per-image-type filenames you edited above into the active profile so future image writes land at those paths.
{: #settings-platform-active-profile-save-filenames }

### Profile naming  {#settings-platform-profile-naming}

- **Enter filename (e.g. folder.jpg):**
{: #settings-platform-profile-naming-prompt-filename }

### Use symlinks for duplicates  {#settings-platform-symlinks}

## Metadata providers  {#tab-providers}

### Provider API Keys  {#settings-providers-provider-keys}

Most metadata providers (Fanart.tv, Discogs, TheAudioDB, and others) require a free or paid API key before they will return data. Paste each key here to enable that provider; keys are encrypted at rest. Providers without a key are skipped during metadata lookups.

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

### Provider config  {#settings-providers-provider-config}

- **Update API key**
{: #settings-providers-provider-config-update-key }
- **Add API key**
{: #settings-providers-provider-config-add-key }
- **Client ID** -- The OAuth application identifier issued by the provider when you registered an app. Paste it here so Stillwater can authenticate against the provider's API.
{: #settings-providers-provider-config-client-id }
- **Client Secret** -- The OAuth application secret issued alongside the Client ID. Stored encrypted at rest. Treat it like a password and do not share it.
{: #settings-providers-provider-config-client-secret }
- **Client Access Token**
{: #settings-providers-provider-config-client-access-token }
- **API Key**
{: #settings-providers-provider-config-api-key }
- **Server** -- Which MusicBrainz endpoint Stillwater queries. Pick a preset or supply a custom mirror URL.
{: #settings-providers-provider-config-server }
- **Official** -- Send requests to musicbrainz.org. The public service is rate-limited to roughly one request per second.
{: #settings-providers-provider-config-official }
- **Beta** -- Send requests to the MusicBrainz beta server. The data set matches the official server, but the deployment runs preview code. Subject to the same one-request-per-second public rate limit.
{: #settings-providers-provider-config-beta }
- **Custom mirror** -- Point Stillwater at a self-hosted MusicBrainz mirror. Requests bypass the public rate limit so you can raise throughput, subject to whatever your mirror can sustain. If the mirror returns an invalid response (for example an HTML error page instead of JSON), Stillwater logs a warning and automatically retries that request against the official MusicBrainz API, so a misconfigured mirror degrades gracefully instead of silently losing metadata.
{: #settings-providers-provider-config-custom-mirror }
- **Base URL**
{: #settings-providers-provider-config-base-url }
- **Rate (req/s)**
{: #settings-providers-provider-config-rate-limit }
- **OAuth Credentials** -- Sub-section for OAuth application credentials used when a provider requires authenticated access. Fill in the Client ID and Client Secret issued by the provider.
{: #settings-providers-provider-config-oauth-credentials }
- **Field verbosity** -- Controls the level of detail fetched from the provider for specific metadata fields.
{: #settings-providers-provider-config-verbosity-section }

### Web Image Search  {#settings-providers-web-search}

Authoritative sources like Fanart.tv and TheAudioDB curate a fixed catalogue of images per artist; for obscure or local artists they often have nothing. Web image search (DuckDuckGo) crawls the open web for matches, giving Stillwater far more candidates at the cost of mixed quality. No API key required.

### Provider Priorities  {#settings-providers-priorities}

For each metadata field (biography, genres, image URLs, and so on) Stillwater queries providers in a specific order and uses the first non-empty answer. This list is that order: drag to rearrange, click the checkmark or X to include or skip a provider for a given field. Only providers you have configured appear here.

- **Restore defaults**
{: #settings-providers-priorities-restore-defaults }
- **Drag to reorder. Click to enable/disable.**
{: #settings-providers-priorities-instructions }
- **Disable this provider**
{: #settings-providers-priorities-disable-provider }
- **Enable this provider**
{: #settings-providers-priorities-enable-provider }
- **No configured providers for this field.**
{: #settings-providers-priorities-no-providers }

### Tag Sources  {#settings-providers-tag-sources}

Filter genre, style, and mood tags written to artist metadata. Exclude patterns drop matching tags before they are saved; count caps limit how many tags of each type are kept.

- **Exclude Patterns** -- One pattern per line, case-insensitive. An asterisk (*) is a wildcard that matches any run of characters. Blank lines are ignored.
{: #settings-providers-tag-sources-exclude }
- **Tag Count Caps** -- Set to 0 to allow an unlimited number of tags. When a cap is set, the highest-priority provider's tags fill the slots first.
{: #settings-providers-tag-sources-caps }
- **Max Genres**
{: #settings-providers-tag-sources-caps-max-genres }
- **Max Styles**
{: #settings-providers-tag-sources-caps-max-styles }
- **Max Moods**
{: #settings-providers-tag-sources-caps-max-moods }

## Languages  {#tab-languages}

### Metadata Language Preferences  {#settings-languages-metadata-languages}

Set your preferred languages for artist names, biographies, aliases, and genre/style/mood tags. Search by language name and pick from the autocomplete to add pills. Order matters: pills are an ordered priority list, left to right. When a provider has the same field in multiple languages, Stillwater walks the list and uses the first language the provider offers; everything to the right is a fallback. Drag pills or focus one and use the arrow keys to reorder; click the X or press Backspace on a focused pill to remove. Clearing all pills falls back to English silently.

- **Search languages**
{: #settings-languages-metadata-languages-input-label }
- **Remove**
{: #settings-languages-metadata-languages-remove }
- **Use MB sort-name as a fallback for display names** -- When your top metadata language is Latin-script (English, German, French, etc.) and MusicBrainz does not have a tagged alias in that language, Stillwater can use MB's sort-name as the display name (e.g. 青木達之 becomes Tatsuyuki Aoki). Disable this if you want to see only the canonical name or curator-tagged aliases.
{: #settings-languages-metadata-languages-romanization-fallback }

## Rules & severity  {#tab-rules}

### Rules  {#settings-rules-rules}

- **paused: conflict gating** -- A conflict gate is Stillwater's safeguard that pauses its own writes when a connected media server has its own NFO or image saver enabled, since whichever wrote second wins and edits would otherwise round-trip endlessly. This chip appears on the NFO or Image rule category header while a gate is active for that surface; auto-fix stays paused until you turn off the platform-side saver and dismiss the banner.
{: #settings-rules-rules-conflict-gated-chip }
- **Requires local library** -- Some rules check or rewrite files on disk, which only works for libraries whose path Stillwater can read directly (not API-only libraries imported through Emby or Jellyfin). This badge marks a rule that depends on local-filesystem access; the rule stays disabled until you add at least one library with a real filesystem path on the Libraries tab.
{: #settings-rules-rules-requires-local }
- **Auto-fix** -- When this rule is enabled, the dropdown picks how Stillwater handles a failed check: manual records the violation on the Reports page so you can review and apply fixes yourself, while auto applies the fix during the same scan without waiting for review. Disabling the rule is a separate toggle.
{: #settings-rules-rules-auto-fix }
- **Manual (notify only)** -- When this rule is enabled, the dropdown picks how Stillwater handles a failed check: manual records the violation on the Reports page so you can review and apply fixes yourself, while auto applies the fix during the same scan. Disabling the rule is a separate toggle.
{: #settings-rules-rules-manual }
- **Resolution preset**
{: #settings-rules-rules-resolution-preset }
- **-- select preset --**
{: #settings-rules-rules-select-preset }
- **Min width (px)**
{: #settings-rules-rules-min-width }
- **Min height (px)**
{: #settings-rules-rules-min-height }
- **Aspect ratio preset**
{: #settings-rules-rules-aspect-preset }
- **Aspect ratio (decimal)**
{: #settings-rules-rules-aspect-ratio }
- **Tolerance (e.g. 0.1)**
{: #settings-rules-rules-tolerance }
- **Min length (chars)**
{: #settings-rules-rules-min-length }
- **Threshold % (total area)**
{: #settings-rules-rules-threshold-percent }
- **Trim margin (px)**
{: #settings-rules-rules-trim-margin }
- **Severity**
{: #settings-rules-rules-severity-label }

### Preset  {#settings-rules-preset}

- **720p HD (1280x720)**
{: #settings-rules-preset-fanart-720p }
- **1080p Full HD (1920x1080)**
{: #settings-rules-preset-fanart-1080p }
- **1440p QHD (2560x1440)**
{: #settings-rules-preset-fanart-1440p }
- **4K UHD (3840x2160)**
{: #settings-rules-preset-fanart-4k }
- **Standard (500x500)**
{: #settings-rules-preset-thumb-standard }
- **HD (1000x1000)**
{: #settings-rules-preset-thumb-hd }
- **Ultra (2000x2000)**
{: #settings-rules-preset-thumb-ultra }
- **Kodi standard (758x140)**
{: #settings-rules-preset-banner-kodi }
- **Wide (1000x185)**
{: #settings-rules-preset-banner-wide }
- **Standard (400px)**
{: #settings-rules-preset-logo-standard }
- **Large (800px)**
{: #settings-rules-preset-logo-large }
- **16:9 widescreen (1.778)**
{: #settings-rules-preset-aspect-16-9 }
- **16:10 (1.6)**
{: #settings-rules-preset-aspect-16-10 }
- **2:1 ultra-wide (2.0)**
{: #settings-rules-preset-aspect-2-1 }
- **1:1 square (1.0)**
{: #settings-rules-preset-aspect-1-1 }

## Schedule  {#tab-schedule}

### Scheduled Evaluation  {#settings-schedule-rule-schedule}

A rule is a check that compares the actual state of an artist's NFO or images against the value Stillwater believes is correct. Scheduling makes Stillwater run every enabled rule across the whole library on a fixed cadence (in addition to triggering them on changes). Requires a container restart after changing.

### Schedule  {#settings-schedule-schedule}

- **Every 5 minutes**
{: #settings-schedule-schedule-every-5m }
- **Every 15 minutes**
{: #settings-schedule-schedule-every-15m }
- **Every 30 minutes**
{: #settings-schedule-schedule-every-30m }
- **Every hour**
{: #settings-schedule-schedule-every-hour }
- **Every 6 hours**
{: #settings-schedule-schedule-every-6h }
- **Every 12 hours**
{: #settings-schedule-schedule-every-12h }
- **Daily (24h)**
{: #settings-schedule-schedule-daily }

## Servers (Emby, Jellyfin, Lidarr)  {#tab-connections}

### Server Connections  {#settings-connections-connections}

A connection is a credentialed link to an external media server (Emby, Jellyfin, or Lidarr) that lets Stillwater both read its library structure and write NFO files and images back into the artist folders that server manages. Add one connection per server you want Stillwater to integrate with.

- **Discover Libraries**
{: #settings-connections-connections-discover }
- **Feature toggles**
{: #settings-connections-connections-feature-toggles }
- **What Stillwater sends to this connection**
{: #settings-connections-connections-sends-heading }
- **Image download/write** -- When on, Stillwater downloads images from providers and writes them to artist folders that this server's libraries cover. Writes can still be paused by the conflict banner shown at the top of the page when a round-trip with the platform's own image saver would otherwise overwrite Stillwater's edits.
{: #settings-connections-connections-feature-image-write }
- **Let Stillwater manage images and NFO files on this server**
{: #settings-connections-connections-manage-title }
- **Verify path after rename (Lidarr only)**
{: #settings-connections-connections-verify-path-title }
- **Server name**
{: #settings-connections-connections-server-name }
- **Server URL**
{: #settings-connections-connections-base-url }
- **API Key**
{: #settings-connections-connections-api-key }
- **Not configured**
{: #settings-connections-connections-not-configured }
- **Add server**
{: #settings-connections-connections-add-server }
- **Choose a server type**
{: #settings-connections-connections-pick-type }

## Webhooks & notifications  {#tab-webhooks}

### Webhooks  {#settings-webhooks-webhooks}

A webhook is an HTTP POST Stillwater sends to a URL you provide whenever a chosen event happens (a rule fixes a violation, an artist is added, and so on). Configure as many webhooks as you need to forward Stillwater events into Slack, Discord, your home automation, or any service that can accept JSON over HTTP.

- **Add Webhook**
{: #settings-webhooks-webhooks-add }
- **Webhook name**
{: #settings-webhooks-webhooks-name }
- **Webhook type**
{: #settings-webhooks-webhooks-type }
- **Select type...**
{: #settings-webhooks-webhooks-select-type }
- **Generic (JSON)** -- Send a generic JSON payload describing the event. Use this for in-house tooling or services without a dedicated formatter.
{: #settings-webhooks-webhooks-type-generic }
- **Webhook URL**
{: #settings-webhooks-webhooks-url }

### Notification Badges  {#settings-webhooks-notif-badges}

A violation is what Stillwater records when an enabled rule disagrees with what the artist's NFO or images contain on disk. The sidebar badge surfaces the count of active violations directly in the navigation so you do not have to open the Reports page to know whether anything needs attention.

- **Enable badge** -- When on, the Reports link in the sidebar carries a small numeric badge whose count is the number of active violations matching the severity filters below. Turn it off if you would rather not see the count at a glance.
{: #settings-webhooks-notif-badges-enable-badge }
- **Count violations by severity** -- Each violation has a severity (info, warning, error). These toggles decide which severities the sidebar badge tallies; disabling a severity hides it from the count without removing those violations from the Reports page itself.
{: #settings-webhooks-notif-badges-count-by-severity }

## API tokens  {#tab-tokens}

### API Tokens  {#settings-tokens-api-tokens}

API tokens are long-lived credentials that let scripts and external tools call the Stillwater REST API without a browser session. Each token is scoped (read, write, webhook, or admin) so you can grant exactly the access an integration needs and revoke it independently.

- **Revoked**
{: #settings-tokens-api-tokens-revoked }
- **Read** -- Read-only access to artists, libraries, rules, and settings. Safe for dashboards and one-way integrations that only fetch data.
{: #settings-tokens-api-tokens-scope-read }
- **Write** -- Create and modify artists, run rules, and queue background work. Use this for automation scripts that need to make changes but should not touch user accounts.
{: #settings-tokens-api-tokens-scope-write }
- **Webhook** -- Lets the token receive inbound webhook deliveries from external systems. Pair with a single integration so an exposed token does not also grant read or write access.
{: #settings-tokens-api-tokens-scope-webhook }
- **Admin** -- Grants every API scope (read, write, and webhook) on top of the routes the owning user's role allows. Routes that are gated by the administrator role still require the owning user to be an administrator; revocation is always limited to the owning user's own tokens.
{: #settings-tokens-api-tokens-scope-admin }

## Users  {#tab-users}

### Users  {#settings-users-users}

User accounts and pending invites both live here. An account is someone who can already sign in; an invite is a single-use link that creates an account when the recipient redeems it. Use this tab to issue invites, change roles, and revoke access.

- **never**
{: #settings-users-users-last-login-never }
- **just now**
{: #settings-users-users-last-login-just-now }

#### Multi-User Mode  {#settings-users-users-multi-user-mode}

Single-user mode is the default and assumes one administrator owns the whole instance; multi-user mode unlocks invites, per-user roles, and per-account preferences so several people can share the same Stillwater. Turn it on before you create the first invite for someone other than yourself.

- **Multi-User Mode**
{: #settings-users-users-enable-multi-user }
- **Create Invite**
{: #settings-users-users-create-invite }
- **Role** -- The permission level granted to the new account when this invite is redeemed. Admins manage settings; standard users browse and tag their own library.
{: #settings-users-users-role }
- **Invite Role** -- Accessible label for the role selector when creating an invite. Mirrors the visible Role control.
{: #settings-users-users-role-for-invite }
- **Invite Expiry** -- How long the new invite link remains usable. After this period the link expires and can no longer redeem an account.
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
- **Inactive only**
{: #settings-users-users-inactive-only-label }
- **Delete selected**
{: #settings-users-users-bulk-delete }
- **User accounts** -- An account is anyone who has signed in successfully and exists in Stillwater's user table, regardless of which auth provider verified them. This table lists every active account; use the row controls to promote or demote a user's role or deactivate them.
{: #settings-users-users-user-accounts }
- **Select all inactive users**
{: #settings-users-users-bulk-select-all }
- **User**
{: #settings-users-users-user }
- **Auth Provider**
{: #settings-users-users-auth-provider }
- **Last Login**
{: #settings-users-users-last-login-column }
- **Actions**
{: #settings-users-users-actions }
- **Select %s for bulk delete**
{: #settings-users-users-bulk-select-user }
- **Delete**
{: #settings-users-users-delete }
- **Permanently delete {name}?**
{: #settings-users-users-delete-prompt-single }
- **Permanently delete {count} selected users?**
{: #settings-users-users-bulk-delete-prompt-other }
- **{count} users deleted**
{: #settings-users-users-bulk-delete-success-other }
- **{count} selected**
{: #settings-users-users-bulk-selected-count-other }
- **Delete user account**
{: #settings-users-users-delete-dialog-title }
- **This permanently removes the account and cannot be undone.**
{: #settings-users-users-delete-dialog-irreversible }
- **Reason (optional, recorded in the audit log)**
{: #settings-users-users-delete-dialog-reason-label }
- **Cancel**
{: #settings-users-users-delete-dialog-cancel }

#### Pending Invites  {#settings-users-users-pending-invites}

An invite is a one-time link Stillwater issues to a prospective user; redeeming it creates an account at the role baked into the link. This list shows invites that have been issued but not yet redeemed, so you can revoke or copy each link before it expires.

- **Role:** -- Marks the role badge shown next to a pending invite in the list below. The redeemed account will be created with this role.
{: #settings-users-users-role-label }
- **Expires:** -- Marks the expiry timestamp shown next to a pending invite in the list below. The invite stops working after this time.
{: #settings-users-users-expires-label }
- **Revoke**
{: #settings-users-users-revoke }

## Auth providers  {#tab-auth}

### Authentication Providers  {#settings-auth-auth}

An authentication provider is the system Stillwater asks to verify a user's password before letting them in: a local username/password, an Emby or Jellyfin server, or an OIDC identity provider. Enable as many as you want; users see a sign-in button per enabled provider.

- **Local** -- Local accounts live in Stillwater's own user table: username, role, and a password hash that Stillwater verifies itself with no external service involved. This is the simplest provider to enable and is on by default for the first admin account.
{: #settings-auth-auth-local }
- **Emby** -- When this provider is on, the sign-in form asks for the username and password of an account on the linked Emby server and verifies them against Emby's API rather than against Stillwater's own user table. Reuses whichever Emby connection you have already configured.
{: #settings-auth-auth-emby }
- **Enable Emby Auth**
{: #settings-auth-auth-enable-emby }
- **Server URL**
{: #settings-auth-auth-server-url }
- **Sourced from your Emby connection**
{: #settings-auth-auth-sourced-from-emby }
- **Auto-Provision** -- Auto-provisioning means Stillwater creates a local account on the fly the first time someone signs in through the upstream provider. With this on, anyone who can authenticate against the linked Emby server gets a Stillwater account without an admin issuing an invite first; the guard rail below decides who actually qualifies.
{: #settings-auth-auth-auto-provision-emby }
- **Emby Auto-Provision**
{: #settings-auth-auth-enable-auto-provision-emby }
- **Guard Rail** -- When auto-provisioning is on, the guard rail narrows who actually gets an account created. Pick admins-only to limit it to users with admin rights on the upstream Emby or Jellyfin server, or any user to provision everyone the provider authenticates.
{: #settings-auth-auth-guard-rail }
- **Emby guard rail setting**
{: #settings-auth-auth-emby-guard-rail }
- **Admins only** -- Only users with an existing admin account in the upstream provider are allowed to auto-provision. Other users can still sign in but will not have a Stillwater account created for them.
{: #settings-auth-auth-admins-only }
- **Any user** -- Every user the upstream provider authenticates is auto-provisioned a Stillwater account at the configured default role.
{: #settings-auth-auth-any-user }
- **Default Role** -- Stillwater accounts have a role (Administrator or User) that decides what they can change. This setting picks the role assigned to brand-new accounts created by auto-provisioning; an admin can promote or demote them later.
{: #settings-auth-auth-default-role }
- **Default role for Emby users**
{: #settings-auth-auth-default-role-emby }
- **Jellyfin** -- When this provider is on, the sign-in form asks for the username and password of an account on the linked Jellyfin server and verifies them against Jellyfin's API rather than against Stillwater's own user table. Requires an active Jellyfin connection.
{: #settings-auth-auth-jellyfin }
- **Enable Jellyfin Auth**
{: #settings-auth-auth-enable-jellyfin }
- **Sourced from your Jellyfin connection**
{: #settings-auth-auth-sourced-from-jellyfin }
- **Auto-Provision** -- Auto-provisioning means Stillwater creates a local account on the fly the first time someone signs in through the upstream provider. With this on, anyone who can authenticate against the linked Jellyfin server gets a Stillwater account without an admin issuing an invite first; the guard rail below decides who actually qualifies.
{: #settings-auth-auth-auto-provision-jellyfin }
- **Jellyfin Auto-Provision**
{: #settings-auth-auth-enable-auto-provision-jellyfin }
- **Jellyfin guard rail setting**
{: #settings-auth-auth-jellyfin-guard-rail }
- **Default role for Jellyfin users**
{: #settings-auth-auth-default-role-jellyfin }
- **OpenID Connect (OIDC)** -- OpenID Connect (OIDC) is a standard protocol that lets Stillwater redirect sign-in to an existing identity provider so users authenticate there once and reach every connected app without re-entering credentials. Works with Authentik, Keycloak, Authelia, Auth0, or any OIDC-compliant provider.
{: #settings-auth-auth-oidc }
- **Enable OIDC Auth**
{: #settings-auth-auth-enable-oidc }
- **Issuer URL** -- Base URL of your OIDC provider. Stillwater discovers the rest of the endpoints automatically through the provider's well-known configuration document.
{: #settings-auth-auth-issuer-url }
- **OIDC Issuer URL**
{: #settings-auth-auth-oidc-issuer }
- **OIDC Client ID** -- Public identifier registered for Stillwater in your OIDC provider.
{: #settings-auth-auth-client-id }
- **OIDC Client Secret** -- Confidential credential issued by your OIDC provider. Leave blank when editing other fields to keep the existing secret.
{: #settings-auth-auth-client-secret }
- **Default role for OIDC users not in an admin group**
{: #settings-auth-auth-default-role-oidc }
- **OIDC Admin Groups** -- Comma-separated list of OIDC groups whose members are granted the Administrator role. Stillwater reads the values from the groups claim on the ID token.
{: #settings-auth-auth-admin-groups }
- **OIDC Allowed Groups** -- Comma-separated list of OIDC groups allowed to log in. Leave empty to allow every authenticated user from this provider.
{: #settings-auth-auth-allowed-groups }
- **Display Name** -- Provider name shown on the sign-in button. Falls back to a generic OIDC label when blank.
{: #settings-auth-auth-oidc-display-name }
- **Logo URL** -- Optional image shown next to the OIDC sign-in button. A key icon is used when blank.
{: #settings-auth-auth-oidc-logo-url }
- **Auto-Provision** -- Auto-provisioning means Stillwater creates a local account on the fly the first time someone signs in. With this on, every user the OIDC provider authenticates gets a Stillwater account without an admin issuing an invite first; the allowed groups list below restricts who qualifies.
{: #settings-auth-auth-auto-provision-oidc }
- **Enable auto-provisioning for OIDC users**
{: #settings-auth-auth-enable-auto-provision-oidc }
- **OIDC Auto-Provision**
{: #settings-auth-auth-oidc-auto-provision }

## Configuration file  {#tab-config-file}

### Performance & Scanning  {#settings-config-file-operations}

Operational tuning knobs for scanning and rule evaluation. These were previously available only as environment variables.

- **Advanced: only raise this for self-hosted or mirrored providers**
{: #settings-config-file-operations-workers-caution-title }
- **Raising this only helps if your metadata providers permit higher request throughput, such as a self-hosted or mirrored MusicBrainz. The shared per-provider rate limiter still caps requests to the public providers, so on a default install a high value mostly adds database write contention with no speedup. Most deployments should leave this at the default.**
{: #settings-config-file-operations-workers-caution-body }
- **Concurrent Artists** -- How many artists the rule engine evaluates concurrently during a Run Rules pass. Default is 2. Applies on the next pass.
{: #settings-config-file-operations-workers }
- **Set via the %s environment variable. Edit that variable and restart to change it.**
{: #settings-config-file-operations-env-managed }
- **Scanner Exclusions** -- Comma-separated artist directory names the scanner skips (for example: Various Artists, Soundtrack). Whitespace around each name is trimmed. Takes effect on the next scan.
{: #settings-config-file-operations-exclusions }
- **Scanner mtime Fast Path** -- When on, the scanner reuses cached image flags for artist directories whose modification time has not changed since the previous scan, skipping a re-probe. Turn off on filesystems with unreliable modification times (some network shares, FUSE mounts, restored backups).
{: #settings-config-file-operations-mtime }

### Settings Export / Import  {#settings-config-file-export-import}

- **Export passphrase** -- Pick a passphrase used to encrypt the exported file. You will need the same passphrase to import the file later.
{: #settings-config-file-export-import-export-passphrase }
- **Import settings file** -- Pick the encrypted .json file produced by a previous export.
{: #settings-config-file-export-import-import-file-label }
- **Import passphrase** -- Enter the same passphrase used when the file was exported.
{: #settings-config-file-export-import-import-passphrase }

### Advanced  {#settings-config-file-advanced}

Provider matching is how Stillwater decides whether a search result returned by MusicBrainz, Discogs, or another source actually refers to the artist you asked about. These controls adjust the thresholds and tie-breakers used during that match.

### Name similarity  {#settings-config-file-name-similarity}

Minimum similarity score (0-100) required when matching artist names from search results. Set to 0 to disable name validation and accept any result. Default is 60.

- **Name Similarity Threshold**
{: #settings-config-file-name-similarity-label }

## Maintenance  {#tab-maintenance}

The Maintenance tab groups five sections that keep the database and configuration healthy. Database Maintenance and Database Backup each have an auto-run option; the Schedule section supplies the shared interval choices (every 6 hours through weekly) that both use. Confirmation Dialogs lists every destructive-action prompt you have suppressed with Don't ask again, so you can re-enable any of them. Settings Export / Import lets you snapshot your full configuration as an encrypted file and restore it on the same or a different instance.

### Database Maintenance  {#settings-maintenance-db-maintenance}

SQLite databases accumulate fragmentation over time, slowing queries and leaving freed pages on disk. Maintenance runs VACUUM and ANALYZE to compact the database file and refresh query planner statistics; trigger it manually or schedule it to run on its own.

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

A backup is a snapshot of Stillwater's SQLite database, which holds every artist record, library configuration, rule, and setting on this instance. Use this section to take an on-demand backup, download an existing one for off-instance storage, or restore a snapshot if something goes wrong.

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

### Backup Schedule  {#settings-maintenance-backup-schedule}

How often Stillwater takes an automatic database backup.

- **Backup Interval** -- Number of hours between automatic backups. Must be a positive whole number.
{: #settings-maintenance-backup-schedule-interval }
- **hours**
{: #settings-maintenance-backup-schedule-interval-unit }

### Operations  {#settings-maintenance-operations}

- **Set via the %s environment variable. Edit that variable and restart to change it.**
{: #settings-maintenance-operations-env-managed }

### Image Cache  {#settings-maintenance-image-cache}

When an artist comes from a connected media server but Stillwater cannot resolve the artist's folder on disk, downloaded cover art is stored in a local image cache instead so the UI can still render thumbnails. This section shows the cache size, lets you cap how big it can grow, and lets you clear it.

- **Maximum size** -- Cap the total disk space the image cache may use. When the cache reaches this size, the oldest entries are evicted first. Pick from 256 MB, 512 MB, 1 GB, 2 GB, or Unlimited.
{: #settings-maintenance-image-cache-max-size }
- **Unlimited** -- When the maximum size is set to Unlimited, Stillwater never evicts cached images automatically. Disk usage grows with each new image fetched.
{: #settings-maintenance-image-cache-unlimited }
- **Clear Cache** -- Remove every image currently held in the local cache. Cached images are re-fetched from providers the next time an artist screen is opened.
{: #settings-maintenance-image-cache-clear }

## Updates  {#tab-updates}

### Application Updates  {#settings-updates-updates}

Stillwater can update its own binary in place by downloading the latest release from GitHub and swapping the executable on next restart. Settings here decide whether to check, how often, and which release channel to follow. (Docker installs ignore these settings; update by pulling a new image instead.)

- **Skip %s**
{: #settings-updates-updates-skip-version }
- **Last auto-applied**
{: #settings-updates-updates-last-auto-applied }
- **Skipped versions**
{: #settings-updates-updates-skip-version-list-label }
- **Update channel and schedule** -- These settings govern the auto-update workflow: which release channel Stillwater watches (stable, prerelease, or nightly), how often it polls GitHub, and whether it installs new builds automatically or just notifies you.
{: #settings-updates-updates-config }
- **Updater enabled** -- The master switch for everything in this section: when off, Stillwater never polls GitHub, never shows update banners, and the Apply Update button is grayed out regardless of the channel and auto-check settings below. Turn it off in environments where the binary is managed by an external system.
{: #settings-updates-updates-enabled }
- **Release channel** -- Pick which kinds of releases to receive. Stable is the recommended default and only delivers fully tested versions. Prerelease also includes release candidates that are still being polished. Nightly delivers in-progress builds from active development; expect rough edges.
{: #settings-updates-updates-channel }
- **Automatic update checks** -- When on, a background task polls GitHub at the configured interval and notes whether a newer release is available, surfacing a banner in the UI when one appears. Checking is read-only; it never installs anything on its own.
{: #settings-updates-updates-auto-check }
- **Automatically install updates** -- When on, Stillwater installs new releases automatically as they become available. This setting has no effect on Docker installs; Docker users update by pulling a new image and recreating the container.
{: #settings-updates-updates-auto-update }
- **Check interval** -- How often the background task polls the GitHub releases API to look for newer builds. Shorter intervals find releases sooner; longer intervals are gentler on GitHub's rate limit. The minimum is once per hour.
{: #settings-updates-updates-check-interval }
<!-- END GENERATED: settings-reference -->
