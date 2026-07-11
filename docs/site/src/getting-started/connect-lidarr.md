---
description: Connect Stillwater to Lidarr, and let Stillwater auto-infer the path mapping between the two when they mount the library under different roots.
---

<!-- code: internal/connection/model.go (LidarrConfig, PathMapping), internal/connection/pathinfer.go (InferPathMappings, DefaultPathInferConsensus), internal/api/lidarr_pathinfer.go (applyInferredPathMappingsIfEmpty), internal/publish/lidarr_selfheal.go (selfHealLidarrLinks), web/templates/settings_sections_next.templ (connectionPathMappings). -->

# Connect Lidarr

About 5 minutes once you know where Lidarr's API key page is.

Stillwater connects to Lidarr using an API key generated in Lidarr's web UI. Unlike the Emby and Jellyfin connections, a Lidarr connection doesn't push metadata or artwork -- Lidarr's own library import stays in charge of that. What the connection gives you is path-aware coordination: Stillwater can keep Lidarr's directory reference to an artist correct across renames and merges, and can trigger a Lidarr library refresh so it notices the change.

## Before you start

You'll need:

- A **Lidarr instance** you can reach over HTTP from the Stillwater host. Lidarr's default port is `8686` (examples: `http://192.168.1.100:8686`, `https://lidarr.example.com`).
- **Administrator access** to Lidarr. API keys are exposed under Lidarr's settings.

## Get a Lidarr API key

1. Open Lidarr's web UI and go to **Settings > General**.
2. Under **Security**, find the **API Key** field. Lidarr generates one automatically the first time it starts; you don't need to create it.
3. Copy the key.

The key has full access to your Lidarr instance. Treat it like a password: don't paste it into chat logs, screenshots, or shared scripts. Stillwater stores it encrypted at rest in its own database.

## Connect Stillwater to Lidarr

In Stillwater, open **Settings > Connections** and click **Add server**, then **Lidarr**.

Fill in three fields:

- **Server name.** A label for the connection. Free-form; "Lidarr" is fine.
- **Server URL.** The full URL to the Lidarr instance, including scheme and port. Example: `http://192.168.1.100:8686`. Use the hostname Stillwater can reach from its own network position; if Stillwater is in Docker on a different host than Lidarr, this is usually the LAN IP, not `localhost`.
- **API key.** Paste the key from the previous section.

Click **Test** to verify the URL is reachable and the API key is valid, then save.

## Path mapping: when Stillwater and Lidarr disagree on paths

If Stillwater and Lidarr both mount the same music library, but under different root paths (Stillwater sees `/music/Artist Name`, Lidarr sees `/data/media/Artist Name`), Stillwater needs to know how to translate one into the other before it can tell Lidarr about a renamed or merged artist directory. That translation is the **path mapping**: one or more host-prefix -> Lidarr-prefix pairs (for example, `/music` -> `/data/media`).

If both sides see the exact same path, leave this empty -- paths are sent to Lidarr verbatim.

### Stillwater infers it for you, when it can

The first time you save or enable a Lidarr connection (and any time you click **Re-infer**), Stillwater tries to work out the mapping automatically:

1. It looks at every one of its own artists that has both a MusicBrainz ID and a directory on disk.
2. It matches those against Lidarr's artist list by MusicBrainz ID.
3. For each matched pair, it compares the two paths' directory prefixes (after stripping the common artist-folder name at the end).
4. A mapping is only applied when at least two independently-matched artists agree on the same prefix pair -- a single coincidental match isn't enough to guess from.

If inference finds a consistent mapping, it's applied automatically and the connection panel reports how many mappings came from how many matched artists. If you already have mappings entered, inference never overwrites them -- it only fills in the gap when the list is empty. If your paths already match, or too few artists corroborate the same prefix, you'll see a "no mappings inferred" message and can type the host/Lidarr prefix pairs in yourself.

### Verify path after rename

A related toggle, **Verify path after rename (Lidarr only)**, catches the rare case where Lidarr silently coerces the path Stillwater submitted against its own Root Folder list instead of accepting it as given. It costs one extra request per rename and is worth leaving on if you're not fully confident your path mapping (or Lidarr's Root Folders) is correct yet -- it's the detection half; the path mapping is the prevention half.

## What the connection enables

- **Path sync on rename or merge.** When an artist's directory is renamed, or two duplicate artists are [merged](../how-to/merge-duplicate-artists.md), Stillwater rewrites the path (through the mapping above) and tells Lidarr, so Lidarr's reference to the artist doesn't go stale.
- **Automatic Lidarr link discovery.** Most Stillwater libraries start out with no Lidarr link recorded for most artists. Rather than requiring you to link every artist to Lidarr by hand first, Stillwater resolves the link automatically at the moment it's needed -- during a rename or a merge -- by matching the artist's MusicBrainz ID against Lidarr's artist list. This is best-effort: if Lidarr is unreachable or no match is found, the rename or merge still completes, just without notifying Lidarr.
- **Post-merge refresh.** After a [merge](../how-to/merge-duplicate-artists.md), Stillwater triggers a refresh on Lidarr (along with any other connected server) so it picks up the surviving artist's new location. Lidarr doesn't drop its own stale entry for the removed (loser) artist automatically the way Emby/Jellyfin do -- remove that manually if it lingers.
- **Let Stillwater manage images and NFO files.** Same toggle as Emby/Jellyfin: if Lidarr's own metadata writer is enabled and clashing with Stillwater's writes to the same directory, Stillwater's conflict detector surfaces a banner and you can flip this on to make Stillwater the sole writer.

Lidarr does not get a metadata-push or image-write toggle -- those are Emby/Jellyfin-specific, since Lidarr's own library import is what tracks artist metadata for the Lidarr side.

## Verify the connection works

After saving, the Connections list should show the connection in the **Connected** state with a recent **Last checked** timestamp. To confirm path mapping is working, rename an artist directory (or merge a duplicate) and check that Lidarr's reference to the artist still resolves afterward, rather than showing a missing-files warning.

## Troubleshooting

Connection and authentication failures are covered in [Platform authentication](../troubleshooting/platform-auth.md). If Lidarr keeps flagging an artist as missing after a rename despite a configured path mapping, double-check the mapping's host prefix against the *exact* path Stillwater shows for that artist -- a mismatch there is the most common cause.
