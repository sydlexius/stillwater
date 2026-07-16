---
description: Find and merge duplicate artist records -- how the survivor is chosen, what the dry-run preview shows, and what happens on disk.
---

<!-- code: internal/artist/merge_artists.go (MergeAndReconcile, ChooseSurvivor, executeLoserMerge, commitMergeDB), internal/artist/duplicates.go (DetectDuplicates, markDisambiguationConflicts), internal/api/handlers_artist_duplicates.go (POST /api/v1/artists/merge, /duplicates/ignore, DELETE /duplicates/ignored/{id}), web/templates/artist_duplicates.templ (ArtistMergeModal, dry-run preview, disambiguation override gate). -->

# Merge Duplicate Artists

Stillwater flags artists that appear to be the same person or group -- usually the result of importing the same artist from two sources with slightly different directory names. Merging consolidates them into one record and one directory.

## Find suspected duplicates

Open **Possible Duplicate Artists** (sidebar, or `/reports/duplicates` directly). Stillwater groups artists two ways:

- **Shared MBID** -- every member of the group has the same MusicBrainz artist ID. High confidence.
- **Name collision** -- the members' directory names normalize to the same value (ignoring punctuation style, hyphen vs. underscore, and a leading "The"). Worth a manual look before merging; not classified as a Shared MBID group.

Only artists with a non-empty filesystem path are considered -- a platform-only artist (no filesystem path) can't be merged.

Each group's member table includes a **Disambiguation** column (the artist's MusicBrainz disambiguation text, or "None" when unset). If two or more members carry different, non-empty disambiguation values, the group carries an amber **Disambiguation Conflict** badge and the conflicting values are highlighted in the table -- a member with no disambiguation set is never flagged, since an unset value doesn't contradict anything. This is a hint, not a block: differing disambiguation often means the members are genuinely different artists who happen to share a name or MBID grouping, so it's worth a second look before merging, but the group is still offered for merge. See [Read the preview before confirming](#read-the-preview-before-confirming) for what this means once you open the merge modal.

If a group isn't actually a duplicate, click **Ignore** to dismiss it. Ignored groups move to **Manage ignored** (`/reports/duplicates/ignored`), where you can **Restore** one if you change your mind.

## Merge a group

Click **Merge...** on a group to open the merge modal.

### Pick the survivor

The survivor is the artist record (and on-disk directory) that remains after the merge; every other member of the group is a "loser" that gets folded into it. Stillwater pre-selects a recommended survivor, in this order:

1. The directory whose name already matches the MusicBrainz canonical name -- this is the safest pick, since Lidarr (if you use it) is least likely to re-fork an artist whose folder already matches what it expects.
2. If no directory matches the canonical name, whichever directory holds the most album subdirectories.
3. If neither rule applies, the artist with the lowest ID (a deterministic fallback).

The recommended candidate carries a **Recommended** badge with a tooltip explaining which of the three reasons applied. You can pick a different survivor; Stillwater shows a warning that the merge will still run, but the surviving directory may not match the MusicBrainz canonical name.

Each candidate also has an **Include in merge** checkbox, so you can exclude a member from the group entirely rather than merging it.

### Read the preview before confirming

As soon as you pick a survivor, Stillwater runs a dry-run automatically -- no files or database rows are touched. The **Preview** section lists every album subdirectory that would move into the survivor's folder, plus any warnings. The **Confirm merge** button stays disabled until a clean dry-run completes, so you always see the plan before committing.

If the dry-run finds an **album collision** -- the same album subdirectory name exists under both the survivor and a loser -- the merge is blocked entirely ("Cannot merge: album collisions detected"). Rename or remove one copy of the colliding album on disk, then refresh the page and try again.

### Disambiguation conflicts

If the group has a Disambiguation Conflict (see [Find suspected duplicates](#find-suspected-duplicates)), the merge modal shows an amber warning -- "These Artists May Not Be Duplicates" -- listing the conflicting values, and the **Confirm merge** button stays disabled until you tick "I have reviewed these values and want to merge anyway." This is deliberately a soft gate, not a hard refusal: a differing MusicBrainz ID means MusicBrainz itself asserts the members are different entities, which Stillwater refuses outright and does not offer here. A differing disambiguation is only the operator's own tag, and it may simply be a mistake in one of the source records -- so Stillwater warns loudly and asks for an explicit, deliberate acknowledgement rather than blocking the merge. Ticking the override does not skip the dry-run preview; both checks must pass before Confirm merge is enabled.

## What happens on merge

Once you click **Confirm merge**:

- Every album subdirectory in a loser is moved into the survivor's directory.
- Loose files (images, NFO files) at the top level of a loser's directory are handled per-file: if the survivor already has a same-named file, the survivor's copy wins and the loser's copy is deleted; otherwise the loser's file is moved into the survivor's directory. The `extrafanart/` and `extrathumbs/` folders are the one exception -- both sides' images are kept and merged together rather than treated as a collision.
- If the survivor has no MusicBrainz ID but a loser does, the survivor inherits it before the loser's row is deleted.
- The loser's artist record is deleted, along with its images, aliases, band members, provider IDs, library links, and history.
- If the survivor's directory name wasn't already the MusicBrainz canonical name, Stillwater renames it to match.
- Stillwater tells every connected server where the survivor's directory now lives. This happens on every merge, whether or not the survivor was renamed. The path is first translated into each server's own filesystem namespace using that connection's path mapping; if the translated path lands outside the server's root folders, Stillwater refuses to send it and reports the connection as failed rather than storing a location the server cannot resolve.
- Stillwater deletes each merged-away (loser) artist's record from every connected server, so no stale entry is left pointing at a directory that no longer exists. This removes only the server's library record, never any files.
- Stillwater then refreshes every connected server that had indexed the survivor or any loser, so they pick up the new location. If a server happens to be offline during the merge, trigger a library refresh there yourself once it's back.

**This is a one-way operation.** There's no undo once a merge commits -- the dry-run preview is the safety net, so review it before confirming. Path mapping applies to every connection type (Lidarr, Emby, and Jellyfin alike); see [Connect Lidarr](../getting-started/connect-lidarr.md) for how path mappings and MusicBrainz-ID matching keep a merged artist linked to a server afterward -- the same mechanics apply regardless of which platform the connection is.

If a merge reports a connection as failed with a path-mapping message, set that connection's **Path mapping** (Settings > Connections) so Stillwater can translate its own view of the library into the path that server expects, then re-run the merge or rename. Without a mapping, a server whose library is mounted under a different prefix would be handed a path that means nothing on its side -- and would report success anyway.

## Safety checks that can block a merge

- **A locked artist can't be merged.** If any member of the group is locked, unlock it first.
- **Only one merge runs at a time.** A second merge attempted while one is already in progress is rejected; wait for the running merge to finish.
- **Stale groups are re-validated at merge time.** If the group's membership changed between opening the modal and confirming (someone else edited an artist, for example), the merge is rejected and asks you to refresh the page.
- Merging is an admin-only action.

## See also

- [Run scans](run-scans.md#possible-duplicate-artists) -- how duplicate detection ties into a filesystem scan.
- [Connect Lidarr](../getting-started/connect-lidarr.md) -- path mapping and MBID-based self-heal, which keep a connection's link to an artist intact across merges and renames (applies to Emby and Jellyfin connections too, not just Lidarr).
