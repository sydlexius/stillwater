---
description: Walk through Stillwater's first-time setup wizard. Create the admin account, add libraries, connect a media server, and discover your artists.
---

# First-time setup

The first time you open Stillwater in a browser, it has no admin user, no library configuration, and nothing connected. The setup flow walks you through everything you need to get useful.

There are two pieces:

1. **Create the admin account.** A short form that runs once. After this, the home page is gated by login.
2. **The setup wizard.** A multi-step guided tour for libraries, language preferences, the platform profile, provider API keys, server connections, and an initial artist discovery.

You can skip individual wizard steps and come back to them later under **Settings**. The wizard is a guide, not a gatekeeper. The only step that truly blocks you is creating the admin account.

## Create the admin account

Open Stillwater (default `http://localhost:1973`). With no users in the database, the home page renders a setup screen instead of the normal login form.

Before picking an authentication method, the setup screen offers two cards:

- **Start fresh.** The path described below: create a new admin account, then run the setup wizard. Choose this for a brand-new install.
- **Restore from backup.** Upload a previously-exported settings bundle (`.json`) and enter its passphrase. The restore replaces this fresh install's empty database with the bundle's contents, including the original admin accounts. After it completes you sign in with your *original* credentials, with no throwaway admin to clean up. See [Export and import settings](../how-to/export-import-settings.md) for how to produce the bundle on the source instance.

If you picked Start fresh, pick an authentication method:

- **Local username and password.** Stillwater stores the password hashed in its own database. Suitable when Stillwater is the only thing handling auth.
- **Emby or Jellyfin.** Sign in with an existing account on your media server. Stillwater redirects to that server for authentication and creates a federated user record. Suitable when you'd rather not manage another username/password.
- **OIDC** (only when this Stillwater instance was started with OIDC pre-configured). Sign in with an external identity provider. OIDC for the first user requires the issuer URL and client ID to be present in the settings store before first run.

Whichever method you pick, **the first user created is always an administrator**, regardless of role mappings the auth method might otherwise apply.

After the admin user is created, Stillwater redirects you to the setup wizard at `/setup/wizard`.

## Welcome (intro)

A brief overview of what Stillwater does (metadata curation, image management, multi-provider fallback, multi-platform delivery, rule engine, scanner). No fields to fill in. Read or skip; click **Next**.

This intro page sits outside the wizard's numbered progress bar; the numbered steps below start at the Music Libraries step.

## Wizard step 1: Music Libraries

Add one or more music library directories. For each library, you'll provide:

- **Name.** A label for the library. Free-form; "Music," "Classical," "Live shows" are all fine.
- **Path.** The directory on the Stillwater host (or container) containing the library. Stillwater needs read access; for NFO writeback it also needs write access.
- **Type.** `Regular`. Top-level directories are album artists: `/music/Pink Floyd/`, `/music/Radiohead/`, `/music/London Symphony Orchestra/`. The same convention covers pop, rock, jazz, classical, and everything else: orchestras, ensembles, and conductors all have their own MusicBrainz IDs and are treated the same as any other artist.

Stillwater validates that the path exists and is writable before letting you save.

!!! tip "Library structure expectations"
    Stillwater expects **one album artist per top-level directory**: `/music/Pink Floyd/`, `/music/Radiohead/`. Compilations (`Various Artists`, `Various`, `VA`, `Soundtrack`, `OST`) are excluded by default, configurable under Settings later.

    If your library is laid out differently (per-album folders flattened to root, single-folder dump, etc.), Stillwater's scanner won't find your artists cleanly. Tools like [MusicBrainz Picard](https://picard.musicbrainz.org/), [Beets](https://beets.io/), and [Lidarr](https://lidarr.audio/) reorganize libraries into the expected layout.

You can add more libraries later under **Settings** > **Libraries**.

## Wizard step 2: Language preferences

Choose the languages Stillwater prefers when fetching metadata from providers. Search for a language by name and pick from the autocomplete to add a pill. Pills accumulate left-to-right in priority order: drag to reorder, or focus a pill and use arrow keys.

**Order matters.** When a provider returns content for an artist in several languages, Stillwater walks the list from left to right and uses the first language the provider has. So `English, French, Japanese` means "give me English if you have it, otherwise French, otherwise Japanese, otherwise whatever the provider considers its default." Languages later in the list are fallbacks, not co-equals.

You can skip this step and use the default (English-only). Language preferences can be changed later under **Settings** > **Providers** > **Metadata Language Preferences**.

## Wizard step 3: Platform Profile

Pick the platform you primarily target: Emby, Jellyfin, Kodi, Plex, or another supported profile. The profile controls:

- Which NFO fields Stillwater writes (each platform reads a slightly different XML dialect).
- Default rules and validations geared to that platform's behavior.
- Image-format and resolution preferences.

You can run Stillwater against multiple platforms at once by combining the profile choice here with the per-server connections in step 5. The profile sets the *primary* dialect.

If you signed in with Emby or Jellyfin during admin setup, the wizard pre-selects the matching profile.

## Wizard step 4: Provider API keys

Stillwater queries up to ten metadata providers in a per-field fallback chain. Some providers work without an API key (MusicBrainz, Wikipedia, Wikidata, AudioDB's free tier). Others require keys you create on each provider's developer portal:

- **Discogs**, **Last.fm**, **Genius**: free developer keys, each provider has its own signup flow.
- **Fanart.tv**: free key after creating an account.
- **Spotify**: requires a paid account (Spotify Premium) for developer API access.

The wizard shows a card per provider with a status badge (Configured, Not configured, Error) and a link to where you create the key. Skipping the step entirely is fine; Stillwater will run on the keyless providers.

There's also a separate **Web image search** subsection at the bottom of this step, with toggles for each web-search-capable provider. Web image search is opt-in and can be turned on later.

You can add or update keys later under **Settings** > **Providers**.

## Wizard step 5: Server connections

Connect Stillwater to your media server(s) and (optionally) Lidarr. The wizard shows a card per supported connection type:

- **Emby.** Stillwater imports artists from Emby's library, pushes metadata edits via the Emby API, can trigger Emby refreshes, and reads MusicBrainz IDs that Emby has already resolved.
- **Jellyfin.** Same set of capabilities as Emby, against a Jellyfin server.
- **Lidarr.** Read-only. Used for detecting NFO settings and reading platform-level metadata profiles. Lidarr does not provide a library to scan; that comes from your library directories in step 1.

For each connection you'll need:

- **Server URL.** Including scheme and port (`http://192.168.1.100:8096`).
- **API key or auth token.** Each platform documents how to generate one in its admin UI. The dedicated [Connect Emby](connect-emby.md) and [Connect Jellyfin](connect-jellyfin.md) pages walk through the API-key creation step in detail.

After you save a connection, the wizard runs a "clobber check" against it to look for configuration on the server side that would silently overwrite Stillwater's writes. If it finds anything, it surfaces a warning here.

You can skip this step if you only use NFO writeback. You can also add or remove connections later under **Settings** > **Connections**.

## Wizard step 6: Conflict pre-flight (conditional)

This step only appears when you have **both** at least one library configured (step 1) **and** at least one enabled Emby, Jellyfin, or Lidarr connection (step 5). When it appears, Stillwater runs a synchronous probe to detect potential write-back and round-trip conflicts: configurations where a connected server might overwrite the files Stillwater is about to write, or where Stillwater would overwrite content the server is authoritative on. Pre-existing NFO files on disk are surfaced as part of the same check.

Three outcomes:

- **Green (no conflicts):** the wizard lets you continue.
- **Yellow (recoverable):** Stillwater detected pre-existing NFO files but believes its writes will be safe. You can continue.
- **Red (blocking):** Stillwater detected configuration that would cause its writes to be overwritten by your media server, or that would overwrite NFO content the server appears to be authoritative on. The Continue button is disabled until you resolve the issue. The page suggests specific fixes (typically toggling a server-side setting or enabling the library-wide NFO `lockdata` switch).

If the probe fails to reach the server, you'll see a retry button. Failed probes do not block the wizard; you can continue and revisit Settings later.

## Wizard step 7: Artist discovery

A two-phase final step.

**Opt-in phase.** Stillwater shows the count of unidentified artists in your library (artists Stillwater can see but hasn't yet linked to a MusicBrainz ID). Click **Start discovery** to scan and resolve them, or skip and run discovery later from the Artists view.

**Progress phase.** Discovery runs in the background. The page shows live progress, which step is active (scanning, identifying, fetching metadata, fetching images), and a per-artist log. The Back button is hidden during this phase to prevent accidental cancels.

**Review phase.** When discovery finishes, the wizard presents a review of identified vs. ambiguous vs. unresolved artists. Ambiguous artists go to a re-identify queue you can work through after the wizard. Unresolved artists stay in the library and can be manually linked later.

Click **Finish** to leave the wizard. You land on the **Artists** view with your discovered library.

## Foreign-file baseline

When Stillwater's scanner has already found pre-existing artwork files in your library, **Step 7** (Artist discovery) shows a baseline sub-section. You'll be asked whether to record those files as the library's *starting baseline*.

**Why it matters:** without baselining, onboarding an existing music library against a new Stillwater install would surface hundreds of "foreign file" alerts on day one -- every piece of pre-existing artwork, since none of it carries Stillwater's provenance tag yet. That noise trains operators to ignore the foreign-file surface before it has shown them anything useful.

The sub-section defaults to **Yes -- record as baseline**. You can switch to **No** if you want Stillwater to flag every pre-existing foreign file from the first scan. Most installs should leave this at Yes.

After the baseline scan completes:

- Pre-existing artwork is recorded in the content-hash allowlist and **does not appear as an incident**.
- Every subsequent scan diffs against the baseline. Only files added *after* the baseline are flagged as foreign.

The baseline is a per-instance, one-time event. If you ever want a fresh look at the foreign-file surface (for example, after migrating to a new library structure), use **Settings** > **Maintenance** to clear the allowlist and the next scan will re-baseline.

## After the wizard

Everything the wizard touched lives under the **Settings** menu, organized by area:

- **Settings** > **Libraries** to add, remove, or reconfigure libraries.
- **Settings** > **Providers** > **Metadata Language Preferences** for language preferences, and **Settings** > **General** for other account-level defaults.
- **Settings** > **General** to switch platform profiles.
- **Settings** > **Providers** for API keys and per-provider configuration.
- **Settings** > **Connections** for media server and Lidarr connections.
- **Settings** > **Rules** for the rule engine.
- **Settings** > **Maintenance** > **Backup** for scheduled and manual backups.

Settings can be edited freely from this point forward. If you want to walk through the wizard again, administrators can re-launch it from **Settings** > **General** > **Onboarding**.

## Troubleshooting

See [First-run wizard](../troubleshooting/index.md#first-run-wizard) in the troubleshooting docs.

---

When you're done with the wizard, the next thing most users do is fine-tune connections.

[Continue to Connect Emby](connect-emby.md){ .md-button .md-button--primary }
[Connect Jellyfin](connect-jellyfin.md){ .md-button }
