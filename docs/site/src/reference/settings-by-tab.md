---
description: A guided tour of every tab in Settings -- what each section does, where to find specific knobs, and notable behaviors.
---

<!-- code: web/templates/settings.templ (settingsTabs() at line 140 enumerates the 11 tabs; each panel keyed by data-tab-panel="..."). Inventory verified 2026-04-30 against main; Updates-tab fields re-verified 2026-05-01 with W2.E (#1117) landing the enabled toggle, check-interval selector, and the background scheduler that consumes auto-check (auto-apply is split out to #1284); General-tab base-path field is editable when SW_BASE_PATH is unset (#1005). -->

# Settings, by tab

Stillwater's Settings page is divided into 11 tabs. This page is a navigational reference -- each section below describes one tab, the major panels inside it, and where to find specific knobs. For deeper explanation of *what the settings mean*, follow the cross-links into the relevant Core Concepts or How-to pages.

## General

The basics of how Stillwater behaves: which platform profile is active, how URLs are routed, and how the image cache is sized.

- **Platform profile** -- pick the active profile (Kodi, Emby, Jellyfin, Plex, or a custom one) and review the per-slot image filenames it expects. Built-in profiles are read-only; custom ones are editable.
- **Active profile configuration** -- the NFO output policy and the four image-naming rows (thumbnail, fanart, logo, banner). Read-only when the profile isn't editable.
- **Symlinks** -- if your filesystem supports symlinks, Stillwater can write the primary image once and link the alternate filenames. The toggle is disabled (with a tooltip) on filesystems that don't.
- **Base path** -- the URL prefix Stillwater serves under. The field is editable when `SW_BASE_PATH` is not set in the environment; saving stores the override in the application database and surfaces a "restart required" banner because the HTTP routes are bound at startup. When `SW_BASE_PATH` is set, the env value wins and the field is read-only with an amber caption.
- **Behavior & debug** -- platform debug toggles for verbose logging.
- **Image cache** -- size dropdown (unlimited, 256 MB, 512 MB, 1 GB, 2 GB, or custom) plus a "clear cache" button. Cache stats load asynchronously and show "X MB used (Y files, Z artists)."

## Providers

Everything that controls how Stillwater talks to external metadata sources.

- **Provider keys** -- one card per provider (MusicBrainz, Fanart.tv, AudioDB, Discogs, Last.fm, Genius, Spotify, ...). Set or update API keys here. Disabled providers are skipped during fetches.
- **Web search providers** -- enable/disable image search adapters (e.g., DuckDuckGo).
- **Provider priorities** -- per-field priority chips, drag-reorderable. The order decides who Stillwater asks first when populating that field.
- **Metadata languages** -- multi-select of preferred languages with autocomplete. Drives the "name matches preferred language" rule and influences which alias providers promote.
- **Advanced settings** -- the name-similarity threshold slider (lower = fuzzier matching, higher = stricter), with inline help.

For the *behavior* (how priority and aggregation actually work), see [providers in core concepts](../core-concepts/providers.md). For the per-provider capability matrix, see [the providers reference](providers.md).

## Connections

External platform integrations.

- **Service connections** -- one card per supported platform: Emby, Jellyfin, Lidarr. Each card carries the URL, credentials, and current status. Connection status is live -- if the platform goes down, the card reflects it.

When two libraries point at the same files (one direct, one via Emby for example), conflict gating kicks in -- see the global "image / NFO writes paused" banner that appears site-wide when the gate is engaged. See [field locks](../core-concepts/field-locks.md#what-about-platforms-pushing-back) for the bigger picture.

## Libraries

The libraries Stillwater scans and writes into.

- **Libraries list** -- name, path, type, source, watch mode, and the per-library "lock NFO" toggle. Each row is editable inline.
- **Add library** -- a hidden form revealed by the "Add" button. Asks for name, path, and type (regular or classical). Type help text updates as you switch.

The "lock NFO" toggle controls the per-library `<lockdata>true</lockdata>` switch (see [NFO files](../core-concepts/nfo-files.md#lockdata)). Default is off; enabling it asks platforms to leave every NFO this library writes alone.

## Automation

Outbound webhooks, notification badges, API tokens, and inbound webhook setup.

- **Webhooks** -- create outbound webhooks. Pick a name, type (generic JSON, Discord, Slack, Gotify), and URL. Useful for piping events into your existing notification stack.
- **Notification badges** -- toggle the on-screen badge plus per-severity filters (errors, warnings, info).
- **API tokens** -- create tokens with scopes (read, write, webhook, admin). Shows created-at and last-used timestamps. Revoked tokens go to an archive section.
- **Inbound webhooks** -- per-platform setup instructions (Lidarr, Emby, Jellyfin) including the supported event types and a copy-friendly URL.

The supported inbound events:

- **Lidarr:** ArtistAdded, Download, Grab, AlbumImport
- **Emby:** library.new, item.updated, library.changed, system.notificationtest
- **Jellyfin:** ItemAdded, ItemUpdated, LibraryChanged

## Rules

The rule engine's per-rule configuration plus its scheduler.

- **Rule rows** grouped by category (NFO, Image, Metadata). Each row has the enable toggle, the manual/auto mode, and links to per-rule config (thresholds, severity, etc.).
- **Conflict-gated chips** -- amber chips appear next to image and NFO rules when the conflict gate is currently blocking writes for that category.
- **Scheduled evaluation** -- a dropdown to enable periodic "Run rules" passes (every 5/15/30 minutes, hourly, every 6 or 12 hours, daily, or disabled).

For the catalogue of every built-in rule and its knobs, see [rules catalogue](rules-catalogue.md). For the modes (disabled / manual / auto), see [rules in core concepts](../core-concepts/rules.md).

## Users

Multi-user mode, the user list, and invitations.

- **Multi-user mode toggle** -- when off, Stillwater is single-admin. When on, the rest of this tab becomes meaningful.
- **Users table** -- avatar (initials), name, role badge (administrator vs operator), status (active vs inactive).
- **Create invite** -- pick a role and an expiry (24 hours, 3, 7, or 30 days), generate the invite link, share it.

Errors loading user or invite data appear as an amber alert at the top of the tab.

## Auth providers

How users log in. One section per provider.

- **Local provider** -- username/password. Always on (the toggle is permanently disabled to prevent accidental lockout).
- **Emby** -- enable, auto-provision users from Emby on first login, and a guard rail (admins only, or any user). Default role for new users is configurable. Server URL is read-only and pulled from the connection in the Connections tab.
- **Jellyfin** -- same shape as Emby.
- **OIDC** -- issuer URL, client ID, client secret, default role, admin-groups list, allowed-groups list, display name (shown on the login button), and an optional logo URL.

## Maintenance

Database upkeep, backups, and the settings export/import flow.

- **Confirmation preferences** -- a one-click button to clear the "do not ask again" choices stored in your browser.
- **Database maintenance** -- "Optimize now" (analyze + index refresh) and "Vacuum" (rebuild the SQLite file). Vacuum prompts for confirmation. Auto-optimize schedule dropdown.
- **Backup** -- "Create backup" button, retention count, and max-age dropdown (never expire, or 7 / 14 / 30 / 60 / 90 days). Below: the backup history with download and delete actions.
- **Settings export / import** -- "Export settings" produces an encrypted JSON file (passphrase prompted, minimum 8 characters). "Import settings" takes a `.json` file plus the matching passphrase.

For the export/import flow end-to-end, see [export and import settings](../how-to/export-import-settings.md).

## Logs

Logging configuration and the live log viewer.

- **Log settings** -- level dropdown (trace / debug / info / warn / error), format (JSON or text), and an "ephemeral level" checkbox that lets you bump the level for the current session without persisting.
- **Log to file** -- toggle for file logging plus the path, max size in MB, max number of files, and max age in days. A "clean up old logs" button appears when rotated files exist.
- **Log viewer** -- live tail with level filter buttons (trace / debug / info / warn / error), a search box, pause and clear buttons, a download button, and a file-picker dropdown when file logging is on. Polls every two seconds.

## Updates

Stillwater's self-updater (when running natively) or version status (when running in Docker).

- **Version info** -- current version (always), latest version (after a check), update-available badge when newer.
- **Last checked** timestamp.
- **Release notes** link when available.
- **Docker notice** -- a banner reading "Updates are managed by your container image" when Stillwater detects it's running in a container.
- **Check now** button -- triggers an immediate version check.
- **Status** display -- "Checking...", "Downloading...", "Applying..." during an update cycle.
- **Restart required** banner (amber) when an applied update is staged and waiting for a restart.
- **Updater enabled** toggle -- top-level kill switch. When off, the background loop is a no-op and the Apply button is disabled.
- **Channel** selector -- choose stable, prerelease, or nightly. Your next check (and any Apply) uses the selected channel.
- **Auto-check** toggle -- when on, the background scheduler polls GitHub at the configured interval. The loop reads its config once per tick, so toggle changes take effect on the next scheduled tick rather than instantly; restart the process for immediate effect on long initial intervals.
- **Check interval** dropdown -- how often the background loop polls GitHub: every hour, 6 hours, 12 hours, 24 hours (default), or 7 days. The minimum accepted by the API is 1 hour. If a custom value (set via direct API call) is persisted, the dropdown shows it as a "(custom)" entry so it isn't lost on Save.
- **Save** button -- persists all of the above in one PUT to `/api/v1/updates/config`.

Auto-apply (automatic install of detected updates on the non-Docker path) isn't yet exposed; that work needs a confirmation flow, last-applied status display, and a skip-this-version affordance to be responsible to ship, and is tracked separately. Today an update is detected by the background check, surfaced in the badge and the version row, and installed by clicking Apply.
