---
description: Connect Stillwater to an Emby server. Push metadata edits, trigger library refreshes, and optionally let Stillwater manage NFO and artwork files on the server.
---

# Connect Emby

About 5 minutes once you know where Emby's API key page is.

Stillwater connects to Emby using an API key generated in Emby's web UI. The connection enables Stillwater to import the artists Emby already knows about, push metadata edits to Emby via its API, trigger library refreshes after edits land, and (optionally) coordinate with Emby's own metadata writer so the two don't fight each other.

## Before you start

You'll need:

- An **Emby server** you can reach over HTTP from the Stillwater host. The Emby server URL plus port (typical examples: `http://192.168.1.100:8096`, `https://emby.example.com`).
- **Administrator access** to the Emby server. API keys are an admin-only feature.
- (Optional, for NFO writeback) **Filesystem access** from the Stillwater host to the same music library directory Emby reads from. NFO writeback is an additional delivery mode and works alongside the API connection; you don't have to pick one.

## Get an Emby API key

API keys are managed in Emby's web UI under the dashboard:

1. Open the Emby dashboard. The URL is usually `http://<your-emby-host>:8096/web/index.html#!/dashboard`. Sign in as administrator.
2. In the left sidebar, scroll down to **Advanced** and click **API Keys**.
3. Click the **+** button at the top of the API Keys page.
4. Enter an application name. `Stillwater` is fine; this is just a label so you can recognize the key later.
5. Click **OK**. Emby generates the key and shows it in the API Keys list.
6. Copy the key. You'll paste it into Stillwater in a moment.

The key has full administrator access on the Emby server. Treat it like a password: don't paste it into chat logs, screenshots, or shared scripts. Stillwater stores the key encrypted at rest in its own database.

!!! tip "API key versus user password"
    Emby supports several authentication methods. Stillwater uses the **API key** flow specifically; an Emby username and password isn't enough. If you don't see an **API Keys** section under **Advanced**, you're probably looking at a user account dashboard rather than the server admin dashboard. Confirm the URL ends with `/dashboard` and the user you're signed in as has the `Allow this user to manage the server` flag.

## Connect Stillwater to Emby

In Stillwater, open **Settings** > **Connections** (or, during first-time setup, the Server Connections wizard step). Three connection cards are pre-shown -- Emby, Jellyfin, Lidarr. On the **Emby** card, click **Configure**.

Fill in three fields:

- **Name.** A label for the connection. Free-form; "Living room Emby" or just "Emby" is fine.
- **URL.** The full URL to the Emby server, including scheme and port. Examples: `http://192.168.1.100:8096`, `https://emby.example.com`. Use the hostname Stillwater can reach from its own network position; if Stillwater is in Docker on a different host than Emby, this is usually the LAN IP, not `localhost`.
- **API key.** Paste the key from the previous section.

Click **Test**. Stillwater verifies the URL is reachable and the API key is valid. On success, it resolves a user ID on the server (used to scope subsequent metadata edits) and saves the connection.

## What the connection enables

Each connection has a set of feature toggles that control how Stillwater interacts with the server. Defaults are sensible; you can adjust them per-connection later under **Settings** > **Connections**.

- **Library import.** Stillwater pulls the list of album artists Emby already knows about. Used to align Stillwater's library with Emby's view; matters most when you want Stillwater to operate on the same artists Emby has already organized. While an import is running -- whether triggered by the initial library setup or by the per-library **Re-sync Artists** button under **Settings** > **Libraries** -- a small progress pill appears at the bottom-right of the page showing the library name and how many of the total artists have been pulled so far, so you can see the import is making progress and isn't stuck. The pill stays visible as you navigate between pages.
- **Metadata push.** Stillwater pushes metadata edits to Emby via its API after you make changes in Stillwater. Without this, edits stay local to Stillwater (and the NFO file, if NFO writeback is enabled). The push payload includes every external ID Stillwater has on the artist (MusicBrainz, TheAudioDb, Discogs, Spotify), so Emby's metadata fetchers and other plugins that key off `ProviderIds` can pick up the work Stillwater has already done. Empty IDs are omitted so a Stillwater-side gap never overwrites an existing Emby-side value with an empty string.
- **Sort name for numeric-prefix artists.** When an artist's name starts with a digit run (12 Pebbles, 3 Doors Down, 38 Special, 311) and Stillwater has no upstream sort name on file, Stillwater derives a zero-padded sort value (`0000000012 Pebbles`) and pushes it as the artist's `ForcedSortName`. This makes the artist sort numerically next to its peers in Emby's library list. Because Emby clears that value on its next metadata refresh otherwise, Stillwater also locks the sort-name field on the Emby side (Stillwater adds `SortName` to the artist's locked-fields list, alongside whatever you already had locked there manually). Artists with an existing sort name from MusicBrainz are pushed verbatim and the lock is NOT applied, so a manual unlock of the sort-name field on the Emby side is preserved for those artists. Stillwater's own local sort-name column is never overwritten by the derivation; the derived value lives only on the Emby side.
- **Image write.** Stillwater pushes artwork (primary, backdrop, banner, logo) to Emby via its API.
- **Trigger refresh.** After pushing edits, Stillwater asks Emby to refresh the affected items so the new metadata appears in Emby's UI without waiting for Emby's own scan schedule.
- **Read MusicBrainz IDs.** Stillwater reads the `ProviderIds` field on Emby's artist records and uses those IDs as starting points for its own provider lookups. Saves a round trip and avoids re-identifying artists Emby already resolved.
- **Lock-state sync from Emby.** A scheduled background job (default cadence: every 30 minutes) walks every artist Stillwater has linked to this Emby connection, reads the platform's `LockData` flag, and updates Stillwater's per-artist lock state to match. Lets you toggle a lock in either Emby's UI or Stillwater's UI and have both views agree on the next sync. Stillwater records `lock_source=platform` on locks pulled this way so the origin is visible in the artist's lock history. Locks toggled in Stillwater within the last 5 minutes are protected from being overwritten by a stale platform snapshot. Configure the cadence via the `lock_sync.interval_minutes` setting (set to 0 to disable).

## Let Stillwater manage server files

Most setups never need to flip this toggle. Stillwater's conflict detector watches each connection's library options on its own and only surfaces guidance when it finds a real clash. **If the Connections page is clean (no conflict banner), you can leave the toggle alone.** This section explains what changes when the detector does flag something.

Emby has its own metadata writer that can produce NFO files and download artwork into your library directory. When that writer is enabled AND Stillwater is also writing into the same directory (NFO writeback mode), the two writers fight: Emby produces duplicate image files (`backdrop.jpg` and `fanart.jpg` for the same artwork; multiple variants of `artist.nfo`); Emby rewrites Stillwater's NFO content on next library refresh; and the on-disk state churns.

When the detector finds that situation, Stillwater **closes its own write gate** and surfaces a banner ("Image file writes paused" / "NFO file writes paused"). While the gate is closed, image and NFO auto-fixes don't run; rules still evaluate and surface violations so you can see what's pending, but the fix that would write to disk is held back until the gate clears. The banner is the only nag you'll see; in the absence of a real conflict, Stillwater stays out of your way.

To clear the gate when it does close, flip **Let Stillwater manage images and NFO files on this server** ON for the connection cited in the banner. You do not need to log into Emby and change settings yourself. Stillwater handles the change for you:

1. Snapshots Emby's current library options into the connection record so they can be restored later.
2. Updates Emby's library options to disable its NFO saver and image saver. Stillwater clears the library's metadata-saver list (`MetadataSavers`) as well as setting `SaveLocalMetadata=false`. Both matter: with `SaveLocalMetadata=false` but an NFO saver still listed, Emby keeps writing `artist.nfo` and artwork into your library. Clearing the saver list is what actually stops it.
3. Becomes the sole writer for NFO files and artwork in that library on disk.
4. The conflict banner clears on the next detector refresh; image and NFO auto-fixes resume.

Disabling the toggle later (or deleting the connection) restores Emby's previous library options from the snapshot. The change is reversible.

### When the toggle is moot

If your Emby connection is API-push only (no shared filesystem mount), Stillwater isn't writing files to disk for that library in the first place. There's nothing to clobber and the detector won't close the gate even when Emby's writer is on. Stillwater also automatically disables its filesystem-dependent rules in this configuration (e.g. the "NFO exists" rule), so you won't see false violations against artists that don't live on disk. Let Emby do its own NFO and art saving if that's how the rest of your stack works.

## Choosing a delivery mode

Stillwater can deliver metadata to Emby two ways. They're not exclusive; use either, or both.

- **API push only.** Stillwater talks to Emby exclusively over the API; no shared filesystem required. Good when Emby and Stillwater are on different hosts and a shared mount isn't practical. The API surface covers most fields Emby exposes in its own metadata editor.
- **API push plus NFO writeback.** Stillwater also writes `artist.nfo` and image files to your music library directory on disk. Higher fidelity (captures fields Emby's API doesn't surface, like Discography), but requires Stillwater to have read/write access to the same library directory Emby reads from. Configure the library path in **Settings** > **Libraries** (or during first-time setup); the connection here handles the API side.

The two modes complement each other: NFO writeback is the source of truth on disk; the API push tells Emby to refresh and pick up what's already on disk (or pushes the change directly when NFO writeback isn't configured).

## Verify the connection works

After saving the connection, the Connections list in Stillwater should show:

- The connection in the **Connected** state.
- A **Last checked** timestamp updated within the last minute.
- The capability toggles you enabled.

To smoke-test the round-trip, open any artist in Stillwater, make a small edit (a tag, a description tweak), and save. If push and refresh are enabled, switch to Emby's UI and confirm the change appears within a few seconds.

## Troubleshooting

Most connection issues -- auth failures, paused write banners, image-fetcher conflicts -- live in [Platform authentication](../troubleshooting/platform-auth.md). For setup-time follow-ups (empty library imports, NFO overwrites, stale Last-checked timestamps), see [Connections](../troubleshooting/index.md#connections) in the troubleshooting docs.

## Next: connect Jellyfin (optional)

If you also run Jellyfin, the [Connect Jellyfin](connect-jellyfin.md) page is structurally identical; the API key creation flow is the only meaningful difference.
