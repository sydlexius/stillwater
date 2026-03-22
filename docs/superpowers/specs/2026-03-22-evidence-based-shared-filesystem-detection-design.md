# Evidence-Based Shared Filesystem Detection

**Date:** 2026-03-22
**Status:** Draft
**Related issues:** #584 (shared-filesystem conflicts), #495 (shared-FS guard), #496 (EXIF provenance)

## Problem

The current shared-filesystem detection compares library path strings to determine
whether Stillwater and a platform (Emby/Jellyfin/Lidarr) share the same filesystem.
This approach is unreliable: different container mount points, symlinks, NFS/SMB
mounts, and empty path fields all cause false negatives. The real consequence is a
write loop where Stillwater and the platform both write images, triggering dedup rules,
which trigger missing-image rules, ad infinitum.

## Design Principle

Stillwater does not coexist with platform metadata writers. It replaces them. If a
platform is configured to write images or NFO files, the correct resolution is to
disable the platform's metadata savers, not to have Stillwater work around them.

## Terminology

The image hashing algorithm used throughout this project is **dHash** (difference
hash), a 64-bit perceptual hash. The database column is named `phash` (short for
"perceptual hash") for generality, but stores dHash values encoded as 16-character
lowercase hex strings. This spec uses "dhash" when referring to the algorithm and
"phash" when referring to the DB column. They hold the same value.

## Solution Overview

Replace path-based detection with a three-tier evidence strategy:

1. **Tier 1 -- fsnotify (real-time):** The existing filesystem watcher detects
   write events Stillwater didn't cause. Cheapest signal -- no file I/O, just
   "I didn't do that." Triggers a targeted recheck of tracked files.
2. **Tier 2 -- mtime comparison (periodic):** On library sync, `stat()` each
   tracked image and NFO, compare against the last-write timestamp Stillwater
   recorded in the DB. Any mtime drift on a Stillwater-managed file means an
   external writer touched it. One `stat()` syscall per file -- works over
   NFS/SMB with no parsing.
3. **Tier 3 -- EXIF/NFO provenance (on-demand):** When the system needs to know
   *who* wrote a file and *why* (dedup decisions, diagnostics, audit trail), parse
   the file and read the `stillwater:v1` provenance tag. Expensive but
   definitive.

Additionally:

4. **Rule blocking:** When external writes are detected and the platform's
   metadata savers are active, block file-mutating rules from being enabled with
   a diagnostic message linking to the platform's settings page.
5. **Resolution:** User disables platform savers, clicks Recheck, rules become
   toggleable again.

## Section 1: Evidence Collection Layer

### On image save (hot path)

`image.Save()` already computes dhash and injects EXIF with `stillwater:v1`
provenance. No changes to the save path itself.

After each save, the caller must persist dhash, source, and the file's mtime in
`artist_images`. The current code path is:

1. `image.Save()` computes dhash internally but does not return it to the caller
2. `setArtistImageFlag()` probes dimensions and updates the `Artist` struct fields
   (e.g., `ThumbExists`) via `artistService.Update()`
3. `artist_images` rows are written separately via `ArtistImage.Upsert()` in
   `internal/artist/sqlite_image.go`

**Changes required:**

- **Option A (preferred):** After `image.Save()` writes the file, call
  `image.ReadProvenance(filePath)` to read back the dhash and source from EXIF,
  then `stat()` the file for mtime, and pass them to `ArtistImage.Upsert()`.
  This avoids changing `Save()`'s return signature and reuses the existing
  `ReadProvenance()` function.
- **Option B:** Modify `image.Save()` to return `*ExifMeta` (or at minimum the
  dhash string) so callers can pass it through without a second file read.

Option A adds one file read per save (cheap, since the file is hot in OS cache).
Option B is more efficient but requires changing every `Save()` call site.

The `setArtistImageFlag()` / `updateArtistImageFlag()` functions in
`internal/api/handlers_image.go` must be extended to call `ArtistImage.Upsert()`
with the `PHash`, `Source`, and `LastWrittenAt` fields populated. Currently these
functions only update the top-level `Artist` struct via `artistService.Update()`.

### On NFO save

`nfo.Write()` produces the XML content. `WriteBackArtistNFO()` handles the
filesystem write via atomic write pattern.

**Changes required:**

- Add a `<stillwater>` provenance element to the NFO writer output, e.g.:
  `<stillwater version="1" written="2026-03-22T00:49:00Z" />`
- The parser's `ExtraElements` mechanism already preserves unrecognized XML
  elements during round-trips. However, a dedicated `Stillwater` field on
  `ArtistNFO` is cleaner than relying on ExtraElements, since Stillwater should
  always read its own provenance marker explicitly.
- After writing the NFO, record the file's mtime in the DB (new column on
  `nfo_snapshots` or `artists` table) for Tier 2 mtime comparison.

**Spike dependency:** The platform saver investigation (Section 4) must determine
whether Emby/Jellyfin/Lidarr preserve or strip the `<stillwater>` element when
they rewrite an NFO. If platforms strip it, the NFO provenance marker is useful
only for Stillwater's own round-trips. Mtime comparison (Tier 2) remains the
primary detection mechanism regardless.

### On library onboarding (cold path)

New background job: `ScanLibraryImages(libraryID)`.

- Walks every artist directory in the library path
- For each image file: read EXIF (if present, extract dhash + source), compute
  dhash if EXIF is missing, probe dimensions, record mtime
- For each NFO file: record mtime, check for `<stillwater>` provenance
- Upsert into `artist_images` with phash, source, file_format, last_written_at
- Runs async during OOBE, shows progress ("Indexing images: 340/1200 artists")
- Also available on-demand via API: `POST /api/v1/libraries/{id}/scan-images`
- Short-circuits shared-FS detection: if mtime drift is found on any
  Stillwater-managed file, flag immediately

**Concurrency model:** Follows the existing fix-all pattern -- in-memory progress
tracker with mutex protection. Only one `ScanLibraryImages` job can run at a time
per library (409 Conflict on concurrent starts). Progress is exposed via
`GET /api/v1/libraries/{id}/scan-images/status` returning `{status, progress,
total, library_id}`.

### On library sync (Emby/Jellyfin populate)

During the existing `populateFromEmbyCtx` / `populateFromJellyfinCtx` flow, after
processing each artist:

- If shared FS is already detected for this library pair, skip checks
- Otherwise, `stat()` tracked images and NFOs for that artist -- compare mtime
  against DB. Any drift flags shared FS on both libraries.
- Stop checking further artists after first detection (one canary is enough)

## Section 2: Shared Filesystem Detection

Operates at the **library level**, not per-artist. One canary file proves the
entire filesystem is shared.

### Three-tier detection strategy

**Tier 1 -- fsnotify (real-time, cheapest):**

The existing filesystem watcher (`internal/watcher/`) receives inotify events for
file changes in library directories. Current behavior: triggers a rescan.

New behavior: when a write event fires, check whether Stillwater initiated the
write (maintain an in-memory set of "expected writes" that gets populated before
`image.Save()` / `nfo.WriteBack()` and cleared after). If the event was
unexpected, trigger a targeted recheck of tracked files (images + NFOs) in the
affected artist directory. This is not proof of conflict on its own -- legitimate
non-metadata changes (file reorganization, backups) cause events too. The recheck
determines whether metadata/image files specifically were modified.

Limitation: Docker inotify works on bind-mounted volumes but not on named volumes
backed by overlay2. NFS/SMB may not propagate inotify events. Tier 2 covers these
gaps.

**Tier 2 -- mtime comparison (periodic, cheap):**

On each library sync, for each artist directory, `stat()` tracked images and NFOs.
Compare the file's mtime against the `last_written_at` timestamp Stillwater
recorded in the DB when it last wrote that file. If the mtime is newer than what
Stillwater recorded, an external writer modified the file.

This is a single `stat()` syscall per tracked file -- works over NFS/SMB, no file
parsing required. It is the primary detection mechanism for most deployments.

**Tier 3 -- EXIF/NFO provenance (on-demand, rich):**

Used when the system needs to know *who* wrote a file (dedup decisions, rule
fix logic, diagnostics). Not used for detection. Parse the image EXIF or NFO XML
and read the provenance marker. Answers: "was this file placed by Stillwater
(source=fanarttv, mode=auto), by the user (source=user), or by an unknown writer
(no provenance)?"

### Data model

Extended columns on the `libraries` table:

| Column | Type | Description |
|--------|------|-------------|
| `shared_fs_status` | TEXT NOT NULL DEFAULT 'unknown' | `unknown`, `detected` |
| `shared_fs_evidence` | TEXT NOT NULL DEFAULT '' | JSON: `{"file", "artist", "expected_mtime", "actual_mtime", "detected_at"}` |
| `shared_fs_peer_library_ids` | TEXT NOT NULL DEFAULT '' | JSON array of peer library IDs (supports multiple platforms sharing the same FS) |

New column on `artist_images`:

| Column | Type | Description |
|--------|------|-------------|
| `last_written_at` | TEXT NOT NULL DEFAULT '' | ISO 8601 timestamp of when Stillwater last wrote this file |

The existing `shared_filesystem INTEGER` boolean column is replaced by
`shared_fs_status TEXT`.

**Schema approach:** All columns are added directly to `001_initial_schema.sql`
per the dev-only convention. The single known deployed instance (UAT) has been
manually migrated via `ALTER TABLE`. No goose migration file is needed.

**Struct changes required:**

- `library.Library` (`internal/library/model.go`): Replace `SharedFilesystem bool`
  with `SharedFSStatus string`, `SharedFSEvidence string`,
  `SharedFSPeerLibraryIDs string`
- `library.Service.scanLibrary()`: Update to scan the new columns
- `library.Service.SetSharedFilesystem()`: Rename to `SetSharedFSStatus()`, accept
  status string + evidence JSON + peer IDs instead of a bool
- `rule.SharedFSCheck.IsShared()` (`internal/rule/shared_fs.go`): Change from
  `lib.SharedFilesystem` (bool) to `lib.SharedFSStatus == "detected"`
- `artist.ArtistImage`: Add `LastWrittenAt string` field

### Detection triggers

1. **fsnotify (Tier 1)** -- unexpected write event triggers targeted recheck
2. **Library sync (Tier 2)** -- mtime comparison on tracked files; first drift
   flags both libraries
3. **Image onboarding scan (Tier 2)** -- mtime comparison during initial indexing
4. **Probe file fallback** -- on initial library connection only (fresh install,
   no tracked files yet). Writes `.stillwater-probe-{uuid}` to the Stillwater
   library path, queries the platform API for visibility, deletes the file. Used
   once, not on every startup. **Dependency:** requires knowledge of the
   platform's file browsing API endpoint, determined by the Section 4 spike.

### What this replaces

- `DetectOverlaps()` path comparison logic -- removed
- `RecheckOverlaps()` startup call -- removed
- `pathsOverlap()`, `cleanPath()`, `isPlatformSource()` helpers -- removed

### What stays

- `SharedFSCheck` runtime guard in the rule engine (reads new status field)
- Notification bar UI (richer message with specific evidence)

## Section 3: Rule Blocking Mechanism

When external writes are detected AND the platform's metadata savers are confirmed
active, file-mutating rules cannot be enabled.

### Affected rules

Any rule whose fixer mutates the filesystem:

- NFO writes (create/overwrite artist.nfo)
- Image saves (download and save images)
- Image deletes (extraneous images)
- Directory renames
- Logo trimming/padding
- Backdrop sequencing

Metadata-only rules (DB updates, health score recalculation) are unaffected.

### Implementation changes required

- Add `BlockedReason string` field to `rule.Rule` struct
  (`internal/rule/model.go`). Populated at query time, not stored in DB.
- Rule list/get API handlers: populate `BlockedReason` by checking the library's
  `shared_fs_status` and the platform's saver settings
- Rule enable handler: reject enable requests with 409 if `BlockedReason` is set
- Rules UI template: render grayed-out toggle with inline message when
  `BlockedReason` is non-empty
- OpenAPI spec: add `blocked_reason` field to the rule response schema

### Blocking behavior

- Rules remain visible in the UI but the enable toggle is grayed out
- Inline explanation: "Cannot enable: Emby is configured to save artwork to
  media folders. [Disable this in Emby settings](http://emby:8096/web/...)"
- API returns `blocked_reason` on the rule object
- If a rule was already enabled when shared FS is detected, the fixer skips
  execution (existing `SharedFSCheck` safety net) with the diagnostic message

### Unblocking flow

1. User disables metadata savers in the platform UI (following the link)
2. User clicks "Recheck" in Stillwater
3. Stillwater queries platform API for saver settings
4. If savers are off, clear the block, rules become toggleable
5. Optional: run probe file check to verify platform actually stopped writing

## Section 4: Platform Saver Settings Investigation (Spike)

**This spike must run before implementing Section 3.** It determines what settings
each platform exposes, which ones cause conflicts, and what instructions to show.
**The probe file fallback (Section 2) also depends on this spike** to identify the
correct platform API endpoints for checking file visibility.

### Questions to answer per platform

For Emby, Jellyfin, and Lidarr:

- Which metadata saver settings exist (artwork to folders, NFO files, etc.)
- For each setting: write only when missing, overwrite unconditionally, or
  merge/amend?
- Does the platform preserve or strip the `<stillwater>` NFO element when it
  rewrites an NFO?
- Does the platform strip EXIF data when it re-encodes or copies images?
- Are the settings queryable via API? Can we read the current state?
- Are the settings writable via API? (Future: manage from Stillwater)
- What file naming conventions does the platform use when writing?
- Which API endpoint can browse/list files in a library directory? (needed for
  probe file detection)

### Approach

- **Emby**: Test against running instance (localhost:8096). Metadata/image
  provider plugins are installed but libraries not yet configured to use them.
  Enable various saver settings, trigger library scan, observe file behavior.
- **Jellyfin**: Test against running instance (localhost:8097). Stock providers.
  Same approach.
- **Lidarr**: Spin up a test container mapped to the same music paths. Run OOBE,
  configure metadata agent, import an artist, observe what gets written.

### Deliverables

- Findings document per platform (similar to `docs/architecture/emby-artist-metadata.md`)
- Decision matrix: per setting, "coexist" vs "block" vs "warn"
- API endpoints and setting keys for querying saver state
- Platform file browsing API endpoints (for probe file detection)
- UI URLs for "fix this" links in blocking messages

### Future enhancement

Once the investigation determines which settings are API-writable, a future
milestone could let Stillwater manage those settings directly from its own UI,
offering a one-click "disable Emby's metadata savers" action.

## Section 5: Data Flow Summary

### Install/OOBE

1. User adds a library (manual or via platform connection)
2. Background job (`ScanLibraryImages`) scans all artist directories, computes
   dhash, reads EXIF, records mtime, populates `artist_images` with phash,
   source, last_written_at
3. If any tracked file has unexpected mtime drift, shared FS detected
4. If no tracked files yet (fresh install), probe file fallback runs once
   (post-spike)

### Steady state -- Stillwater saves an image or NFO

1. Add file path to the watcher's "expected writes" set
2. `image.Save()` computes dhash, injects EXIF (already happens)
3. Caller reads provenance back via `ReadProvenance()`, stats the file for mtime,
   upserts `artist_images` with phash, source, last_written_at populated
4. For NFOs: `WriteBackArtistNFO()` injects `<stillwater>` element, records mtime
5. Clear the file path from "expected writes" set

### Steady state -- fsnotify event (Tier 1)

1. Filesystem watcher receives a write event
2. Check "expected writes" set -- if Stillwater caused it, ignore
3. If unexpected, trigger targeted recheck: stat tracked images and NFOs in the
   affected artist directory
4. If mtime drift detected on a tracked file, flag shared FS

### Steady state -- library sync (Tier 2)

1. For each artist directory, if shared FS not yet detected, stat tracked files
2. Compare mtime against last_written_at in DB
3. First mtime drift flags both libraries, stops scanning further
4. If detected, query platform API for saver settings
5. If savers active, block file-mutating rules with diagnostic message + link

### User resolves conflict

1. User disables platform savers via the link
2. Clicks "Recheck" in Stillwater
3. Stillwater re-queries platform API, confirms savers off
4. Clears shared FS block, rules become toggleable

### Edge case -- platform independently writes an image

- Platform-written images lack `stillwater:v1` EXIF
- Dedup rule distinguishes by provenance (Tier 3): warn user rather than delete
- If platform is still writing after user claimed to disable savers, the next
  sync or fsnotify event detects it and re-flags

### Edge case -- legitimate non-metadata filesystem changes

- fsnotify fires for file reorganization, backups, non-metadata content
- The targeted recheck only examines tracked files (images, NFOs) -- not all
  files in the directory
- If tracked files are unchanged, the event is ignored
- No false alarms from unrelated filesystem activity

## Migration

### Schema changes

New columns added directly to `001_initial_schema.sql`:

- `libraries.shared_fs_status TEXT NOT NULL DEFAULT 'unknown'`
- `libraries.shared_fs_evidence TEXT NOT NULL DEFAULT ''`
- `libraries.shared_fs_peer_library_ids TEXT NOT NULL DEFAULT ''`
- `artist_images.last_written_at TEXT NOT NULL DEFAULT ''`

The old `shared_filesystem INTEGER` column is left in place (SQLite does not
support DROP COLUMN in all versions). Code ignores it; the `Library` struct no
longer maps it. The UAT database has been manually migrated via ALTER TABLE.

### Code migration

- `library.Library` struct: replace `SharedFilesystem bool` with
  `SharedFSStatus string`, `SharedFSEvidence string`,
  `SharedFSPeerLibraryIDs string`
- `library.Service.scanLibrary()`: scan new columns
- `library.Service.SetSharedFilesystem()`: rename to `SetSharedFSStatus()`
- `rule.SharedFSCheck.IsShared()`: compare `SharedFSStatus == "detected"`
- `artist.ArtistImage`: add `LastWrittenAt string` field, update Upsert
- Remove `library.DetectOverlaps()`, `RecheckOverlaps()`, and path helpers
- Remove `RecheckOverlaps()` call in `cmd/stillwater/main.go`
- Update `001_initial_schema.sql` to include new columns for fresh installs
- Add `<stillwater>` element to NFO writer, add explicit field to `ArtistNFO`

### Backfill

- One-time background job on upgrade: scan all library images, populate
  `artist_images.phash`, `source`, and `last_written_at` from EXIF and stat()
  where available, compute dhash where missing
- Same job as the OOBE onboarding scan, just triggered on first startup after
  upgrade instead of during OOBE

## Dependencies

- EXIF provenance (#496) -- merged, provides `stillwater:v1` tags
- Image dhash (#496) -- merged, computed during save
- `artist_images.phash` column -- exists, unpopulated
- `artist_images.source` column -- exists, inconsistently populated
- Filesystem watcher (`internal/watcher/`) -- exists, needs "expected writes" set
- Platform saver settings spike -- must complete before Section 3 implementation
- Platform file browsing API -- must be identified in spike before probe file
  fallback can be implemented

## Out of Scope

- Managing platform saver settings from within Stillwater (future enhancement)
- Per-artist shared-FS tracking (library level is sufficient)
- Lidarr-specific behavior (deferred until test instance is available)
