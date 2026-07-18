---
description: Static reference for current Stillwater UI labels -- navigation items, Settings tabs, artist page sections, and image terminology. Use this when writing documentation to match exact wording shown in the interface.
---

<!-- source: internal/i18n/locales/en.json (hand-derived; not generated). Update when labels change. -->

# UI label glossary

When writing documentation that names a control, section, or tab, use the exact label from this table. Mismatches (for example "Settings > Platform" instead of "Settings > General") confuse users who cannot find what the doc describes.

## Sidebar navigation

These are the top-level items in the left-hand sidebar, as rendered to end users.

| Label | Notes |
|---|---|
| Dashboard | Main at-a-glance health view |
| Artists | The artist list and grid |
| Reports | Parent item; expands to Compliance, Duplicates, Unmatched Images |
| Reports > Compliance | Per-artist rule compliance table |
| Reports > Duplicates | Near-duplicate artist detection |
| Reports > Unmatched Images | Images without Stillwater provenance |
| Rules | Top-level nav to the violations / rule-run surface |
| Activity | Full metadata-change activity feed |
| Logs | Live log viewer (sidebar shortcut) |
| Settings | Application configuration |
| Guide | Built-in user guide |
| Help | Context-sensitive help panel |
| Quick Actions | Shortcut panel for common bulk actions |
| Preferences | Per-user appearance and layout drawer |
| Log Out | Sign out of the current session |

## Settings tabs

Settings is divided into eleven tabs. Use these exact names when writing "Settings > X" navigation paths.

| Tab label | Key sections inside |
|---|---|
| General | Platform Profile, Active Profile Details, TLS Status, Base Path, Behavior, Image Cache, Symlinks, Onboarding |
| Providers | Provider API Keys, Web Image Search, Provider Priorities, Metadata Language Preferences, Advanced, Name Similarity, Tag Sources |
| Connections | Server Connections (Emby, Jellyfin, Lidarr) |
| Libraries | Music Libraries |
| Automation | Webhooks, Notification Badges, API Tokens |
| Rules | Rules (by category), Scheduled Evaluation |
| Users | Multi-User Mode, User Accounts, Pending Invites |
| Auth Providers | Local, Emby, Jellyfin, OpenID Connect (OIDC) |
| Maintenance | Confirmation Dialogs, Database Maintenance, Database Backup, Settings Export / Import |
| Logs | Log Settings, Log Viewer |
| Updates | Application Updates |

**Common mistakes to avoid:**

- "Settings > Platform" does not exist. Platform Profile is under **Settings > General**.
- "Settings > Backups" does not exist. Database Backup is under **Settings > Maintenance**.
- "Settings > Appearance" does not exist. Per-user appearance controls live in the **Preferences** drawer (sidebar).
- The Auth tab is "Auth Providers" (capital P), not "Auth providers".

## Artist detail page

Tabs on the artist detail page (accessed by clicking any artist in the list):

| Tab label | Contents |
|---|---|
| Overview | Biography, Tags (Genres/Styles/Moods), Details panel (Name, Sort Name, Type, Born, Formed, etc.) |
| Images | Four image slots: Thumb, Fanart, Logo, Banner |
| Violations | Open rule violations for this artist |
| Providers | Source attributions showing which provider supplied each field |
| Discography | Album entries from the artist.nfo |
| History | Prior values per field (clock icon per field; accessible in edit mode) |
| Debug | Raw platform API payload (only when "Show platform debug info" is enabled in Settings > General) |

Note: "History" is a per-field in-line feature (clock icon), not a dedicated tab. The standalone History tab was removed. See [edit an artist](../how-to/edit-artist.md) for the revert workflow.

## Image types

Stillwater uses four image slots per artist. Each has a Stillwater-internal name, a display label, and platform-specific aliases.

| Stillwater slot | Display label | Kodi | Emby / Jellyfin |
|---|---|---|---|
| thumb | Thumbnail | Folder | Primary |
| fanart | Fanart | Fanart | Backdrop |
| logo | Logo | Logo | Logo |
| banner | Banner | Banner | Banner |

The UI shows the platform-appropriate term when a library is linked to a platform profile. Documentation targeting users (not code) should use the Stillwater display label unless describing platform-specific behavior.

## Key action labels

Common buttons and their exact wording in the UI:

| Action | Label shown |
|---|---|
| Start a metadata refresh for one artist | Refresh Metadata |
| Run rule evaluation for one artist | Run Rules |
| Link an artist to a MusicBrainz entry | Identify Artist |
| Re-link an artist (clear existing IDs) | Re-identify Artist |
| Lock an artist from automated changes | Lock Artist |
| Fetch images from providers (single slot) | Find (on the slot row) |
| Fetch images across many artists | Bulk actions > Fetch images |
| Evaluate rules across many artists | Bulk actions > Run rules |
| Apply all fixable violations at once | Fix All (N) |
| Dismiss a violation without fixing it | Dismiss |
| Undo a recent fix | Undo |
| Export application settings | Export Settings |
| Import application settings | (Settings > Maintenance > Settings Export / Import) |

## Automation modes (rules)

Each rule has an automation mode controlling how violations are handled:

| Mode label | Behavior |
|---|---|
| Manual (notify only) | Finds violations; fixes wait for you to click |
| Auto | Finds violations and applies fixes automatically during evaluation |
