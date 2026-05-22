---
description: How the conflict gate detects when a connected media server is writing files that would collide with Stillwater's writes, and how it pauses Stillwater's output to prevent a tug-of-war.
---

<!-- code: internal/conflict/ledger.go (Ledger, ConnectionState, RoundTrip, BannerState, Axis, AnyImageConflict, AnyNFOConflict), internal/conflict/detector.go (Detector, Refresh, Current, detectRoundTrips, checkOne, DisableFileWriteBack auto-re-disable), internal/conflict/gate.go (Gate, AllowImageWrite, AllowNFOWrite, BlockedError), internal/publish/publisher.go (gate checks before writes), internal/connection/ (connection model and feature toggles) -->

# Conflict gate

The **conflict gate** is Stillwater's protection against a write-collision loop with a connected media server. When the gate fires, Stillwater pauses its own image and NFO writes until the collision source is resolved.

## The round-trip problem

When Stillwater shares a library directory with a connected platform (Emby, Jellyfin, Lidarr), writes from either side are visible to the other through the shared filesystem. The loop works like this:

1. Stillwater writes `backdrop.jpg` and `artist.nfo` into the artist directory.
2. Emby's metadata refresh job detects the changed files and re-reads them.
3. Emby then writes its own copy of the same metadata back to disk under its preferred filenames -- possibly overwriting Stillwater's files or creating duplicates alongside them.
4. On the next scan, Stillwater sees the files it did not write and either ignores them (if it wrote the same content) or detects a conflict.

The specific risk depends on how the peer is configured. An Emby server with "Save local metadata" on will rewrite `artist.nfo`. One with "Save artwork locally" on will write image files. If both savers are on, both axes collide.

## What the gate detects

The gate does not watch filesystem events. Instead, it queries each enabled connected platform's API to check whether that platform is configured to write files to disk:

- **NFO writeback** -- the peer's metadata saver is on. Any NFO file Stillwater writes may be overwritten on the next refresh cycle.
- **Image writeback** -- the peer's image/artwork saver is on. Any image Stillwater writes may be duplicated or overwritten.
- **Round-trip overlap** -- two or more enabled connections point to library directories that overlap on disk. Even if only one peer has a saver enabled, the other peer sees every write from either side through the shared path.

The detection result is stored as a **ledger** -- a snapshot of every enabled connection's write-back state plus any round-trip pairs found among their library paths. The ledger is cached for five minutes; callers that need an immediate re-check (for example, after toggling a setting) can force a refresh.

## Banner states

The ledger drives the banner that appears at the top of the Stillwater UI:

- **Clean (emerald)** -- no conflicts detected.
- **Image writes paused (amber)** -- one or more connections have image writeback on, or Stillwater could not determine their state (fail-closed).
- **NFO writes paused (amber)** -- one or more connections have NFO writeback on, or state unknown.
- **Both paused (amber)** -- image and NFO writeback both detected.
- **Round-trip (red)** -- two enabled connections share a filesystem path. This is the most severe state: any Stillwater write reaches both peers, regardless of which saver is enabled. The banner names the overlapping path.
- **Foreign files (slate/blue)** -- no configuration conflict, but files in the library were written by a media server without a Stillwater provenance stamp. This is a warning, not a gate: writes are not paused, but the files are surfaced for review.

Severity order is highest-first: round-trip overrides image/NFO overrides foreign files overrides clean. Two banners are never shown at the same time; the worse state wins.

## How writes are gated

Before Stillwater writes an image or an NFO file, it consults the gate:

- For image writes: `AllowImageWrite` returns an error if any enabled, unmanaged connection has image writeback on, or if any round-trip pair exists, or if any connection's state could not be determined.
- For NFO writes: `AllowNFOWrite` does the same for NFO writeback.

When either check returns a blocked error, the write is rejected and the banner reflects the current ledger state. Rule fixers, manual saves, and bulk operations all go through this check. The gate is not bypassable from the UI.

The fail-closed behavior is intentional: if Stillwater cannot confirm a connection's state (network error, API timeout), it treats that connection as conflicted rather than silently proceeding. A false positive -- a brief network hiccup blocking writes -- is preferable to a false negative that triggers a collision loop.

## Coalesce and the detection cache

Multiple concurrent callers could all observe a stale cache at the same moment and each trigger a separate network sweep of every peer. The detector prevents that with a **coalesce** pattern: when the cache is stale and multiple goroutines request a refresh at the same instant, exactly one performs the network fan-out. The others wait, then find the cache populated by the first and return immediately without additional requests.

The cache TTL is five minutes. This balances "you see your remediation reflected quickly" against "we don't hammer every peer on every write."

## "Let Stillwater manage" and auto-re-disable

Each connection has a "Let Stillwater manage" toggle. When on, Stillwater calls the platform's API to turn off its local savers and then marks that connection as managed. A managed connection is excluded from the gate: its savers are assumed off.

The toggle also makes a standing promise: if someone re-enables a saver on the platform's own admin UI while the toggle is on, Stillwater detects the drift on the next conflict refresh and calls the disable API again automatically. If that automatic re-disable fails (peer unreachable), the in-memory managed flag is cleared and the gate re-closes for that connection until the next successful refresh.

## Feature toggles on connections

Each connection has several feature toggles; three of them interact with how Stillwater writes to the shared library:

- **image_write** -- whether Stillwater writes image files for artists sourced from this connection. See [`settings-connections-connections-feature-image-write`](../reference/settings-by-tab.md#settings-connections-connections-feature-image-write).
- **nfo_write** -- whether Stillwater writes NFO files for artists sourced from this connection. See [`settings-connections-connections-feature-nfo-write`](../reference/settings-by-tab.md#settings-connections-connections-feature-nfo-write).
- **library_import** -- whether Stillwater imports the library listing from this connection during scans. See [`settings-connections-connections-feature-library-import`](../reference/settings-by-tab.md#settings-connections-connections-feature-library-import).

Disabling `image_write` or `nfo_write` on a connection stops Stillwater from writing those files for that connection's artists, but it does not change what the peer itself writes. The conflict gate operates independently: it watches what the *peer* is configured to write, not what Stillwater is configured to write.

## Relationship to field locks and rules

The conflict gate and [field locks](field-locks.md) protect against different things:

- **Locks** say "this field should never change automatically." They are set ahead of time and express intent.
- **The conflict gate** says "right now, external activity is detected -- pause disk writes until it stops." It is reactive and transient.

Both apply regardless of the other. A locked artist still benefits from the gate pausing writes when a platform is meddling. An unlocked artist still gets the gate's protection even though locks aren't set.

For [rules](rules.md), the conflict gate is consulted before any fix that produces an image or NFO write. If the gate is active when a rule fixer runs, the write is deferred and the violation stays open rather than silently failing.

## Foreign files

When a connected media server writes an image file to an artist directory without a Stillwater provenance tag, Stillwater records it in the **foreign-files ledger**. Each entry appears in the Settings page under Foreign Files so you can review, allowlist, or delete it.

### Deduplicated display

The list collapses entries that share the same byte content into a single row. When the same physical file is linked to more than one artist record, one representative row appears with a **linked from N artists** badge. Allowlisting or deleting that row acts on the representative artist only; the sibling rows for the other artists remain in the ledger until you clear them with the **Dismiss (allowlist all)** button at the top of the page, which removes every entry globally in one pass.

Entries recorded before a library rescan may not yet have a computed content fingerprint. Those entries are never collapsed and each appear as a separate row until the next scan computes their fingerprints.

### Dismiss (allowlist all)

The **Dismiss (allowlist all)** button at the top of the Foreign Files page adds every currently detected file to the global allowlist in a single operation, then clears the entire ledger. The allowlist suppresses future re-detection of those files on all subsequent scans. Individual allow or delete actions are available per row for more targeted control.
