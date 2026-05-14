---
description: A guided tour of every tab in Settings -- what each section does, where to find specific knobs, and notable behaviors.
---

<!-- code: web/templates/settings.templ (settingsTabs() at line 140 enumerates the 11 tabs; each panel keyed by data-tab-panel="..."). Inventory verified 2026-04-30 against main; Updates-tab fields re-verified 2026-05-01 with W2.E (#1117) landing the enabled toggle, check-interval selector, and the background scheduler that consumes auto-check (auto-apply is split out to #1284); General-tab base-path field is editable when SW_BASE_PATH is unset (#1005). -->

# Settings, by tab

Stillwater's Settings page is divided into 11 tabs. This page is a navigational reference -- each section below describes one tab, the major panels inside it, and where to find specific knobs. For deeper explanation of *what the settings mean*, follow the cross-links into the relevant Core Concepts or How-to pages.

<!-- BEGIN GENERATED: settings-reference -->

## General  {#tab-general}

### Platform Profile  {#settings-general-platform-profile}

A platform profile bundles the NFO format and image filename conventions for one media platform (Emby, Jellyfin, or Kodi), since each platform expects metadata laid out differently on disk. The active profile is the one Stillwater uses when writing files; change it here to retarget output for a different platform.

### Active Profile Details  {#settings-general-active-profile}

A platform profile bundles the NFO format and image filename conventions for one media platform (Emby, Jellyfin, or Kodi). This card shows what the active profile will write and lets you override the filename used for each image type. Edits are saved back to the profile.

- **NFO Output** -- NFO files are XML metadata sidecars that Emby, Jellyfin, and Kodi read alongside artist folders to display biography, MusicBrainz ID, and other curated values. This row reflects the active platform profile's NFO output choice: whether Stillwater writes them at all, and in which format. Edit the profile to change.
{: #settings-general-active-profile-nfo-output }
- **Save Filenames** -- Image filenames are how each platform discovers cover art on disk (folder.jpg, fanart.jpg, banner.jpg, and so on). Saves the per-image-type filenames you edited above into the active profile so future image writes land at those paths.
{: #settings-general-active-profile-save-filenames }

### Use symlinks for duplicates  {#settings-general-symlinks}

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

### Base Path  {#settings-general-base-path}

URL path prefix for running Stillwater behind a reverse proxy at a sub-path.

### Behavior  {#settings-general-behavior}

These switches set the defaults Stillwater applies during metadata workflows: whether NFO writes use symlinks for duplicate filenames, and whether opening an artist's image page kicks off a provider search automatically. They affect every artist on this instance, not just the one you are looking at.

### Show platform debug info on artist pages  {#settings-general-platform-debug}

Each artist who came in from a connected media server has a raw payload Stillwater received from the platform's API (the source of truth for IDs, image URLs, and library paths). Turning this on adds a Debug tab to the artist detail page that displays that payload, which is useful when tracing why a field looks wrong.

### Image Cache  {#settings-general-image-cache}

When an artist comes from a connected media server but Stillwater cannot resolve the artist's folder on disk, downloaded cover art is stored in a local image cache instead so the UI can still render thumbnails. This section shows the cache size, lets you cap how big it can grow, and lets you clear it.

- **Maximum size** -- Cap the total disk space the image cache may use. When the cache reaches this size, the oldest entries are evicted first. Pick from 256 MB, 512 MB, 1 GB, 2 GB, or Unlimited.
{: #settings-general-image-cache-max-size }
- **Unlimited** -- When the maximum size is set to Unlimited, Stillwater never evicts cached images automatically. Disk usage grows with each new image fetched.
{: #settings-general-image-cache-unlimited }
- **Clear Cache** -- Remove every image currently held in the local cache. Cached images are re-fetched from providers the next time an artist screen is opened.
{: #settings-general-image-cache-clear }

## Providers  {#tab-providers}

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

### Web Image Search  {#settings-providers-web-search}

Authoritative sources like Fanart.tv and TheAudioDB curate a fixed catalogue of images per artist; for obscure or local artists they often have nothing. Web search providers (Bing, Brave, and similar) crawl the open web for matches, giving Stillwater far more candidates at the cost of mixed quality. No API key required.

### Provider Priorities  {#settings-providers-priorities}

For each metadata field (biography, genres, image URLs, and so on) Stillwater queries providers in a specific order and uses the first non-empty answer. This list is that order: drag to rearrange, click the checkmark or X to include or skip a provider for a given field. Only providers you have configured appear here.

- **Restore defaults**
{: #settings-providers-priorities-restore-defaults }

### Metadata Language Preferences  {#settings-providers-metadata-languages}

Set your preferred languages for artist names, biographies, and aliases. Providers will return content in the first available language from your list. Drag to reorder priority.

- **Remove**
{: #settings-providers-metadata-languages-remove }
- **Search languages**
{: #settings-providers-metadata-languages-input-label }

### Advanced  {#settings-providers-advanced}

Provider matching is how Stillwater decides whether a search result returned by MusicBrainz, Discogs, or another source actually refers to the artist you asked about. These controls adjust the thresholds and tie-breakers used during that match.

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
- **Official** -- Send requests to musicbrainz.org. The public service is rate-limited to roughly one request per second.
{: #settings-providers-provider-config-official }
- **Beta** -- Send requests to the MusicBrainz beta server. The data set matches the official server, but the deployment runs preview code. Subject to the same one-request-per-second public rate limit.
{: #settings-providers-provider-config-beta }
- **Custom mirror** -- Point Stillwater at a self-hosted MusicBrainz mirror. Requests bypass the public rate limit so you can raise throughput, subject to whatever your mirror can sustain.
{: #settings-providers-provider-config-custom-mirror }
- **OAuth Credentials** -- Sub-section for OAuth application credentials used when a provider requires authenticated access. Fill in the Client ID and Client Secret issued by the provider.
{: #settings-providers-provider-config-oauth-credentials }

## Connections  {#tab-connections}

### Server Connections  {#settings-connections-connections}

A connection is a credentialed link to an external media server (Emby, Jellyfin, or Lidarr) that lets Stillwater both read its library structure and write NFO files and images back into the artist folders that server manages. Add one connection per server you want Stillwater to integrate with.

- **Feature toggles**
{: #settings-connections-connections-feature-toggles }
- **What Stillwater sends to this connection**
{: #settings-connections-connections-sends-heading }
- **Library import** -- When on, Stillwater imports library and artist metadata from this server during scans.
{: #settings-connections-connections-feature-library-import }
- **NFO write** -- When on, Stillwater writes artist.nfo files into folders this server's libraries cover. Writes can still be paused by the conflict banner shown at the top of the page when a round-trip with the platform's own NFO saver would otherwise overwrite Stillwater's edits.
{: #settings-connections-connections-feature-nfo-write }
- **Image download/write** -- When on, Stillwater downloads images from providers and writes them to artist folders that this server's libraries cover. Writes can still be paused by the conflict banner shown at the top of the page when a round-trip with the platform's own image saver would otherwise overwrite Stillwater's edits.
{: #settings-connections-connections-feature-image-write }
- **Let Stillwater manage artwork and NFO files on this server**
{: #settings-connections-connections-manage-title }
- **Not configured**
{: #settings-connections-connections-not-configured }
- **Server URL**
{: #settings-connections-connections-base-url }
- **API Key**
{: #settings-connections-connections-api-key }

## Libraries  {#tab-libraries}

### Music Libraries  {#settings-libraries-libraries}

A library is a top-level directory containing one folder per artist; that is the layout Emby, Jellyfin, and Kodi all expect. Add a library entry for each such directory you want Stillwater to scan and write into. The library type (regular or classical) and filesystem watch mode are configured per entry below.

- **Connection**
{: #settings-libraries-libraries-connection-badge }
- **Lock NFOs** -- When on, Stillwater stamps a lockdata flag into every NFO it writes for this library. This tells Emby and Jellyfin to refuse metadata refreshes for those artists so Stillwater's curated values are not overwritten by the platform's own scrapers. Off by default. Artists whose NFO already contains a lockdata flag (set by Stillwater or another tool) are automatically marked as locked at the artist level.
{: #settings-libraries-libraries-lock-nfo-label }
- **Add Library**
{: #settings-libraries-libraries-add }
- **Library Name**
{: #settings-libraries-libraries-name }
- **Library Path**
{: #settings-libraries-libraries-path }
- **Library Type**
{: #settings-libraries-libraries-type }
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
- **Poll interval**
{: #settings-libraries-libraries-poll-interval-title }
- **Re-sync Artists**
{: #settings-libraries-libraries-resync }
- **Scan Library**
{: #settings-libraries-libraries-scan }

## Automation  {#tab-automation}

### Webhooks  {#settings-automation-webhooks}

A webhook is an HTTP POST Stillwater sends to a URL you provide whenever a chosen event happens (a rule fixes a violation, an artist is added, and so on). Configure as many webhooks as you need to forward Stillwater events into Slack, Discord, your home automation, or any service that can accept JSON over HTTP.

- **Add Webhook**
{: #settings-automation-webhooks-add }
- **Webhook name**
{: #settings-automation-webhooks-name }
- **Webhook type**
{: #settings-automation-webhooks-type }
- **Select type...**
{: #settings-automation-webhooks-select-type }
- **Generic (JSON)** -- Send a generic JSON payload describing the event. Use this for in-house tooling or services without a dedicated formatter.
{: #settings-automation-webhooks-type-generic }
- **Webhook URL**
{: #settings-automation-webhooks-url }

### Notification Badges  {#settings-automation-notif-badges}

A violation is what Stillwater records when an enabled rule disagrees with what the artist's NFO or images contain on disk. The sidebar badge surfaces the count of active violations directly in the navigation so you do not have to open the Reports page to know whether anything needs attention.

- **Enable badge** -- When on, the Reports link in the sidebar carries a small numeric badge whose count is the number of active violations matching the severity filters below. Turn it off if you would rather not see the count at a glance.
{: #settings-automation-notif-badges-enable-badge }
- **Count violations by severity** -- Each violation has a severity (info, warning, error). These toggles decide which severities the sidebar badge tallies; disabling a severity hides it from the count without removing those violations from the Reports page itself.
{: #settings-automation-notif-badges-count-by-severity }

### API Tokens  {#settings-automation-api-tokens}

API tokens are long-lived credentials that let scripts and external tools call the Stillwater REST API without a browser session. Each token is scoped (read, write, webhook, or admin) so you can grant exactly the access an integration needs and revoke it independently.

- **Revoked**
{: #settings-automation-api-tokens-revoked }
- **Read** -- Read-only access to artists, libraries, rules, and settings. Safe for dashboards and one-way integrations that only fetch data.
{: #settings-automation-api-tokens-scope-read }
- **Write** -- Create and modify artists, run rules, and queue background work. Use this for automation scripts that need to make changes but should not touch user accounts.
{: #settings-automation-api-tokens-scope-write }
- **Webhook** -- Lets the token receive inbound webhook deliveries from external systems. Pair with a single integration so an exposed token does not also grant read or write access.
{: #settings-automation-api-tokens-scope-webhook }
- **Admin** -- Grants every API scope (read, write, and webhook) on top of the routes the owning user's role allows. Routes that are gated by the administrator role still require the owning user to be an administrator; revocation is always limited to the owning user's own tokens.
{: #settings-automation-api-tokens-scope-admin }

## Rules  {#tab-rules}

### Rules  {#settings-rules-rules}

- **paused: conflict gating** -- A conflict gate is Stillwater's safeguard that pauses its own writes when a connected media server has its own NFO or image saver enabled, since whichever wrote second wins and edits would otherwise round-trip endlessly. This chip appears on the NFO or Image rule category header while a gate is active for that surface; auto-fix stays paused until you turn off the platform-side saver and dismiss the banner.
{: #settings-rules-rules-conflict-gated-chip }
- **Requires local library** -- Some rules check or rewrite files on disk, which only works for libraries whose path Stillwater can read directly (not API-only libraries imported through Emby or Jellyfin). This badge marks a rule that depends on local-filesystem access; the rule stays disabled until you add at least one library with a real filesystem path on the Libraries tab.
{: #settings-rules-rules-requires-local }
- **Auto-fix** -- When this rule is enabled, the dropdown picks how Stillwater handles a failed check: manual records the violation on the Reports page so you can review and apply fixes yourself, while auto applies the fix during the same scan without waiting for review. Disabling the rule is a separate toggle.
{: #settings-rules-rules-auto-fix }
- **Manual (notify only)** -- When this rule is enabled, the dropdown picks how Stillwater handles a failed check: manual records the violation on the Reports page so you can review and apply fixes yourself, while auto applies the fix during the same scan. Disabling the rule is a separate toggle.
{: #settings-rules-rules-manual }

### Scheduled Evaluation  {#settings-rules-rule-schedule}

A rule is a check that compares the actual state of an artist's NFO or images against the value Stillwater believes is correct. Scheduling makes Stillwater run every enabled rule across the whole library on a fixed cadence (in addition to triggering them on changes). Requires a container restart after changing.

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

User accounts and pending invites both live here. An account is someone who can already sign in; an invite is a single-use link that creates an account when the recipient redeems it. Use this tab to issue invites, change roles, and revoke access.

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
- **User accounts** -- An account is anyone who has signed in successfully and exists in Stillwater's user table, regardless of which auth provider verified them. This table lists every active account; use the row controls to promote or demote a user's role or deactivate them.
{: #settings-users-users-user-accounts }
- **User**
{: #settings-users-users-user }
- **Auth Provider**
{: #settings-users-users-auth-provider }
- **Actions**
{: #settings-users-users-actions }

#### Pending Invites  {#settings-users-users-pending-invites}

An invite is a one-time link Stillwater issues to a prospective user; redeeming it creates an account at the role baked into the link. This list shows invites that have been issued but not yet redeemed, so you can revoke or copy each link before it expires.

- **Role:** -- Marks the role badge shown next to a pending invite in the list below. The redeemed account will be created with this role.
{: #settings-users-users-role-label }
- **Expires:** -- Marks the expiry timestamp shown next to a pending invite in the list below. The invite stops working after this time.
{: #settings-users-users-expires-label }
- **Revoke**
{: #settings-users-users-revoke }

## Auth Providers  {#tab-auth-providers}

### Authentication Providers  {#settings-auth-providers-auth}

An authentication provider is the system Stillwater asks to verify a user's password before letting them in: a local username/password, an Emby or Jellyfin server, or an OIDC identity provider. Enable as many as you want; users see a sign-in button per enabled provider.

- **Local** -- Local accounts live in Stillwater's own user table: username, role, and a password hash that Stillwater verifies itself with no external service involved. This is the simplest provider to enable and is on by default for the first admin account.
{: #settings-auth-providers-auth-local }
- **Emby** -- When this provider is on, the sign-in form asks for the username and password of an account on the linked Emby server and verifies them against Emby's API rather than against Stillwater's own user table. Reuses whichever Emby connection you have already configured.
{: #settings-auth-providers-auth-emby }
- **Enable Emby Auth**
{: #settings-auth-providers-auth-enable-emby }
- **Server URL**
{: #settings-auth-providers-auth-server-url }
- **Sourced from your Emby connection**
{: #settings-auth-providers-auth-sourced-from-emby }
- **Auto-Provision** -- Auto-provisioning means Stillwater creates a local account on the fly the first time someone signs in through the upstream provider. With this on, anyone who can authenticate against the linked Emby server gets a Stillwater account without an admin issuing an invite first; the guard rail below decides who actually qualifies.
{: #settings-auth-providers-auth-auto-provision-emby }
- **Emby Auto-Provision**
{: #settings-auth-providers-auth-enable-auto-provision-emby }
- **Guard Rail** -- When auto-provisioning is on, the guard rail narrows who actually gets an account created. Pick admins-only to limit it to users with admin rights on the upstream Emby or Jellyfin server, or any user to provision everyone the provider authenticates.
{: #settings-auth-providers-auth-guard-rail }
- **Emby guard rail setting**
{: #settings-auth-providers-auth-emby-guard-rail }
- **Admins only** -- Only users with an existing admin account in the upstream provider are allowed to auto-provision. Other users can still sign in but will not have a Stillwater account created for them.
{: #settings-auth-providers-auth-admins-only }
- **Any user** -- Every user the upstream provider authenticates is auto-provisioned a Stillwater account at the configured default role.
{: #settings-auth-providers-auth-any-user }
- **Default Role** -- Stillwater accounts have a role (Administrator or User) that decides what they can change. This setting picks the role assigned to brand-new accounts created by auto-provisioning; an admin can promote or demote them later.
{: #settings-auth-providers-auth-default-role }
- **Default role for Emby users**
{: #settings-auth-providers-auth-default-role-emby }
- **Jellyfin** -- When this provider is on, the sign-in form asks for the username and password of an account on the linked Jellyfin server and verifies them against Jellyfin's API rather than against Stillwater's own user table. Requires an active Jellyfin connection.
{: #settings-auth-providers-auth-jellyfin }
- **Enable Jellyfin Auth**
{: #settings-auth-providers-auth-enable-jellyfin }
- **Sourced from your Jellyfin connection**
{: #settings-auth-providers-auth-sourced-from-jellyfin }
- **Auto-Provision** -- Auto-provisioning means Stillwater creates a local account on the fly the first time someone signs in through the upstream provider. With this on, anyone who can authenticate against the linked Jellyfin server gets a Stillwater account without an admin issuing an invite first; the guard rail below decides who actually qualifies.
{: #settings-auth-providers-auth-auto-provision-jellyfin }
- **Jellyfin Auto-Provision**
{: #settings-auth-providers-auth-enable-auto-provision-jellyfin }
- **Jellyfin guard rail setting**
{: #settings-auth-providers-auth-jellyfin-guard-rail }
- **Default role for Jellyfin users**
{: #settings-auth-providers-auth-default-role-jellyfin }
- **OpenID Connect (OIDC)** -- OpenID Connect (OIDC) is a standard protocol that lets Stillwater redirect sign-in to an existing identity provider so users authenticate there once and reach every connected app without re-entering credentials. Works with Authentik, Keycloak, Authelia, Auth0, or any OIDC-compliant provider.
{: #settings-auth-providers-auth-oidc }
- **Enable OIDC Auth**
{: #settings-auth-providers-auth-enable-oidc }
- **Issuer URL** -- Base URL of your OIDC provider. Stillwater discovers the rest of the endpoints automatically through the provider's well-known configuration document.
{: #settings-auth-providers-auth-issuer-url }
- **OIDC Issuer URL**
{: #settings-auth-providers-auth-oidc-issuer }
- **OIDC Client ID** -- Public identifier registered for Stillwater in your OIDC provider.
{: #settings-auth-providers-auth-client-id }
- **OIDC Client Secret** -- Confidential credential issued by your OIDC provider. Leave blank when editing other fields to keep the existing secret.
{: #settings-auth-providers-auth-client-secret }
- **Default role for OIDC users not in an admin group**
{: #settings-auth-providers-auth-default-role-oidc }
- **OIDC Admin Groups** -- Comma-separated list of OIDC groups whose members are granted the Administrator role. Stillwater reads the values from the groups claim on the ID token.
{: #settings-auth-providers-auth-admin-groups }
- **OIDC Allowed Groups** -- Comma-separated list of OIDC groups allowed to log in. Leave empty to allow every authenticated user from this provider.
{: #settings-auth-providers-auth-allowed-groups }
- **Display Name** -- Provider name shown on the sign-in button. Falls back to a generic OIDC label when blank.
{: #settings-auth-providers-auth-oidc-display-name }
- **Logo URL** -- Optional image shown next to the OIDC sign-in button. A key icon is used when blank.
{: #settings-auth-providers-auth-oidc-logo-url }
- **Auto-Provision** -- Auto-provisioning means Stillwater creates a local account on the fly the first time someone signs in. With this on, every user the OIDC provider authenticates gets a Stillwater account without an admin issuing an invite first; the allowed groups list below restricts who qualifies.
{: #settings-auth-providers-auth-auto-provision-oidc }
- **Enable auto-provisioning for OIDC users**
{: #settings-auth-providers-auth-enable-auto-provision-oidc }
- **OIDC Auto-Provision**
{: #settings-auth-providers-auth-oidc-auto-provision }

## Maintenance  {#tab-maintenance}

The Maintenance tab groups five sections that keep the database and configuration healthy. Database Maintenance and Database Backup each have an auto-run option; the Schedule section supplies the shared interval choices (every 6 hours through weekly) that both use. Confirmation Dialogs lists every destructive-action prompt you have suppressed with Don't ask again, so you can re-enable any of them. Settings Export / Import lets you snapshot your full configuration as an encrypted file and restore it on the same or a different instance.

### Confirmation Dialogs  {#settings-maintenance-confirm-dialogs}

Destructive actions in Stillwater (delete an artist, clear a cache, revoke a token) prompt you to confirm before going through. Each dialog has a Don't ask again checkbox; this section lists every dialog you have suppressed so you can re-enable the prompt if you change your mind.

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

### Settings Export / Import  {#settings-maintenance-export-import}

- **Export passphrase** -- Pick a passphrase used to encrypt the exported file. You will need the same passphrase to import the file later.
{: #settings-maintenance-export-import-export-passphrase }
- **Import settings file** -- Pick the encrypted .json file produced by a previous export.
{: #settings-maintenance-export-import-import-file-label }
- **Import passphrase** -- Enter the same passphrase used when the file was exported.
{: #settings-maintenance-export-import-import-passphrase }

## Logs  {#tab-logs}

The Logs tab is divided into two sections. Log Settings controls what Stillwater captures: the minimum severity level, the on-disk format, and the rotation policy that determines how many files are kept and for how long. Log Viewer reads back what Log Settings retained, streaming the in-memory ring buffer live and letting you page through older rotated files; it can only show entries that the retention limits above have not already pruned.

### Log Settings  {#settings-logs-log-settings}

Stillwater emits structured log lines for every meaningful event (scans, rule fixes, API calls, errors). These settings decide how chatty those logs are, what format they use, and whether they are kept on disk in addition to stdout. Changes take effect immediately.

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

Stillwater keeps the most recent log lines in an in-memory ring buffer in addition to writing them to disk. The viewer streams that buffer live so you can watch what the app is doing right now, filter by severity, and grep across messages without leaving the browser.

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

Stillwater can update its own binary in place by downloading the latest release from GitHub and swapping the executable on next restart. Settings here decide whether to check, how often, and which release channel to follow. (Docker installs ignore these settings; update by pulling a new image instead.)

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
- **Last auto-applied**
{: #settings-updates-updates-last-auto-applied }
- **Skip %s**
{: #settings-updates-updates-skip-version }
- **Skipped versions**
{: #settings-updates-updates-skip-version-list-label }
- **Check interval** -- How often the background task polls the GitHub releases API to look for newer builds. Shorter intervals find releases sooner; longer intervals are gentler on GitHub's rate limit. The minimum is once per hour.
{: #settings-updates-updates-check-interval }
<!-- END GENERATED: settings-reference -->
