---
description: How Stillwater reads and writes artist.nfo files -- the XML metadata format every supported platform consumes.
---

<!-- code: internal/nfo/parser.go (Parse, htmlEntityReplacer), internal/nfo/writeback.go (WriteBackArtistNFO), internal/nfo/conflict.go (CheckFileConflict), internal/nfo/model.go (ArtistNFO, StillwaterMeta), internal/nfo/fieldmap.go (NFOFieldMap), internal/filesystem/atomic.go (WriteFileAtomic), internal/library/model.go NFOLockData (#1264) -->

# NFO files

An **NFO file** is the small XML document that media platforms (Kodi, Emby, Jellyfin) read to learn an artist's metadata. Stillwater reads them when it scans, and writes them when you save changes. This page describes the round-trip and the things that affect it.

## What lives in an NFO

Stillwater writes a Kodi-compatible `artist.nfo`. The structured fields cover everything a typical music platform expects:

- **Identity:** name, sort name, type, gender, disambiguation
- **External IDs:** MusicBrainz, AudioDB, Discogs, Wikidata, Deezer, Spotify
- **Descriptive:** genres, styles, moods, years active, born/died, formed/disbanded, biography
- **Images:** thumb references and one or more fanart references
- **Discography:** album entries with title, year, and MusicBrainz release-group ID
- **Lock:** an optional `<lockdata>true</lockdata>` element (see "Lockdata" below)
- **Provenance:** a small Stillwater stamp with version and write timestamp

Anything Stillwater doesn't recognize is preserved verbatim on round-trip. If a third-party tool added a tag Stillwater doesn't model, your edits won't strip it.

## Reading: parse and populate

When the scanner walks a library and finds an `artist.nfo`, it parses the file. The parser is permissive on input:

- **UTF-8 BOM** is stripped automatically.
- **HTML entities** (`&nbsp;`, `&mdash;`, `&eacute;`, etc.) are rewritten to their numeric XML equivalents so the file parses even if the previous writer was sloppy.
- **Unknown elements** are kept and reappear unchanged when Stillwater writes the file back.
- **Album entries** are extracted into the artist's discography view -- but they're not stored in Stillwater's database; albums on disk own that data.

The parsed values are merged into the artist record. Anything already in Stillwater wins where there's a conflict, unless the artist is new.

## Writing: atomic, conflict-aware, optionally locked

A save uses the same atomic-file pattern Stillwater uses for everything else on disk:

1. Write the new content to a temporary file alongside the target.
2. If the target already exists, rename it to a backup name.
3. Rename the temporary file to the target.
4. Remove the backup.

A crash between steps 2 and 3 leaves the previous file recoverable; a crash between steps 3 and 4 leaves only a stale backup to clean up. The on-disk file always exists in a complete state.

### Conflict detection

If the NFO file's modification time is newer than Stillwater's record of when it last wrote the file, the save is aborted. Something else -- usually Emby or Jellyfin's metadata refresh -- has touched the file. Stillwater refuses to blindly overwrite third-party edits.

This ties into the larger conflict-gating mechanism that pauses writes when a connected platform appears to be actively rewriting files. That pause surfaces in the UI as "image / NFO writes paused" until you resolve it. See [field locks](field-locks.md) for the related per-field protection.

### Lockdata

Kodi, Emby, and Jellyfin honor a `<lockdata>true</lockdata>` element as a request to *not* overwrite the file when their own metadata refresh runs. Stillwater's behavior:

- **On read,** if the file has lockdata set, the artist is marked as locked.
- **On write,** Stillwater includes lockdata only when the artist is locked OR when the artist's library has the per-library lockdata switch turned on. The intent: the per-library switch means "every NFO this library writes asks platforms to leave it alone," whereas per-artist locks express the same wish on a single record without library-wide opt-in.

A locked NFO is the strongest defence against an external platform overwriting your edits. It does not stop *you* -- Stillwater still writes the file when you save. It signals "ignore me" to the other tool.

## Per-platform variations

Different platforms read the same NFO format slightly differently. Stillwater handles this with a per-platform **field map**:

- **Kodi default** -- genres go into `<genre>`, styles into `<style>`, moods into `<mood>`. Each category maps to its native element.
- **Emby/Jellyfin tag-friendly** -- moods are *additionally* written as styles so the platforms surface them as Tags. Original mood elements are kept for Kodi compatibility.
- **Custom remap** -- a full matrix mapping any of genres/styles/moods to any of `<genre>`/`<style>`/`<mood>`. Lets you, for example, fold styles into the genre element if your platform of choice ignores `<style>`.

The platform profile chosen for a library decides which field map is in effect when Stillwater writes that library's NFOs.

<!-- SCREENSHOT: Settings > Platform profiles | state: built-in profiles + one custom with advanced remap | annotation: where the field map UI lives -->

## Provenance

Every NFO Stillwater writes carries a small Stillwater stamp near the end -- a schema version and a timestamp. Two purposes. First, **conflict detection** uses it to recognize files Stillwater itself wrote vs. files written by something else. Second, **third-party tools** can read it to skip files Stillwater is managing. Most platforms ignore unknown elements, so the stamp is invisible to them in practice -- but the information is there if you need it.

## What you don't need to think about

- **Encoding.** Stillwater always writes UTF-8 with a leading XML declaration; you don't need to set anything.
- **File location.** The NFO goes next to the other files in the artist directory, named `artist.nfo`. Pathless libraries skip the write entirely.
- **Atomicity.** The temp/backup/rename dance is automatic and fast; you'll never see the intermediate files unless something crashes mid-write.
- **Album discovery.** Stillwater reads album entries into the artist's discography view but doesn't write them back.

What you *do* think about: which platform profile a library uses (the field map), whether to lock individual artists or whole libraries, and whether you trust the conflict gate's verdict when it pauses writes. The rest takes care of itself.
