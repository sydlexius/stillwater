---
description: Trigger filesystem and platform scans, schedule recurring runs, monitor progress.
---

<!-- code: internal/scanner/scanner.go (Run, runScan, processDirectory, detectRemoved), internal/api/handlers_scan*.go (POST /api/v1/scans, GET /api/v1/scans/current), internal/watcher/watcher.go (filesystem watch + poll triggering scans), web/templates/library.templ (Scan button, schedule UI). -->

# Run scans

A **scan** is how Stillwater finds new artists, picks up changes, and removes vanished ones. There are three flavors:

- **Filesystem scan** -- walks library folders on disk.
- **Platform scan** -- pulls the artist list from a connected Emby, Jellyfin, or Lidarr instance.
- **Watch-triggered scan** -- happens automatically when filesystem watching is on.

This page covers triggering each one manually plus setting up recurrences.

## Run a manual filesystem scan

1. Go to **Libraries** in the sidebar.
2. Find the library you want to scan in the list.
3. Click **Scan** on its row.

The scan starts in the background. The library row shows a spinner; an event banner appears with progress. When it finishes, the row updates with new artist counts.

<!-- SCREENSHOT: Libraries list during scan | state: one library with spinner + progress banner | annotation: scan trigger button + progress feedback -->

The scan is **incremental** -- only changed directories are processed. A library with 4,000 artists where nothing has moved finishes quickly because Stillwater compares directory modification times against its last scan.

### "Scan all"

To kick off scans for every library at once, use the **Scan all** button at the top of the Libraries tab. This is just a loop over the per-library scans; each library still runs its own scan independently.

## Schedule recurring scans

For libraries on auto-pilot, recurring scans keep the catalog fresh without you clicking anything.

1. Go to **Libraries**.
2. On the library row, open the watch settings.
3. Pick a watch mode:
   - **Off** -- no automatic scans. You trigger manually.
   - **Watch** -- the operating system tells Stillwater the moment a directory appears or disappears. Best on local filesystems.
   - **Poll** -- Stillwater snapshots the directory listing every few minutes and diffs. Required for many network mounts.
   - **Both** -- watch + poll. Watch fires fast, poll catches anything the watcher misses.
4. If you picked Poll or Both, set the interval (1, 5, 15, or 30 minutes).

<!-- SCREENSHOT: Library row > watch settings | state: poll mode selected with 5-minute interval | annotation: where the watch toggle lives -->

Stillwater probes each path on startup to decide whether watch mode is supported. The UI shows the result so you don't pick a mode that won't fire.

## Pull from a connected platform

When you've connected Emby, Jellyfin, or Lidarr, you can pull the platform's artist list instead of (or in addition to) walking the disk.

1. Go to **Libraries** and find a library imported from a platform connection.
2. Click **Refresh from platform**.

Stillwater queries the platform's library and reconciles with what it has stored. New platform-side artists appear in Stillwater (pathless if they don't have a directory on disk yet); deleted ones are removed.

This is also how you bootstrap a Stillwater instance against an existing media server: connect, import the library, and let the platform scan populate the catalog.

## Monitor an in-progress scan

The current scan's status is visible in three places:

- **The library row** -- shows a spinner while running.
- **The site-wide event banner** -- progress and completion notifications appear here.
- **The scan history** under each library -- the last several scan results with started/completed timestamps and counts.

If a scan goes wrong, the banner surfaces an error and the library row reverts. The full error appears in the scan history detail.

## What scans do (and don't do)

A scan **discovers structure**: which directories exist, which `artist.nfo` files are present, what's missing. It populates Stillwater's database with what it finds.

A scan **does not**:

- Refresh metadata from providers. That's a separate action -- see [refresh metadata](refresh-metadata.md).
- Run rules. Rule evaluation is its own pass; you can enable a recurring rule run under **Settings > Rules**.
- Write NFO files. Stillwater writes NFOs only when you save changes or when a fixer runs.

So the typical workflow on a new library is: scan to discover artists -> refresh metadata to populate fields -> run rules to surface what still needs work.

## Concurrent-scan safety

Stillwater allows one scan per library at a time. A second click on the same library while a scan is running is rejected with a brief message; the running scan keeps going. Different libraries can scan in parallel without conflict.

## When the watcher fires

When watch mode is on, Stillwater triggers a scan automatically:

- **A new subdirectory appears** in the library root -> the watcher debounces briefly (so a quick rename doesn't churn) and triggers a scan.
- **A subdirectory disappears** -> after the debounce, the corresponding artist is removed.
- **An artist directory's contents change** -> the watcher does *not* re-scan the artist's metadata; that requires a refresh. The watcher is structure-aware, not content-aware.
