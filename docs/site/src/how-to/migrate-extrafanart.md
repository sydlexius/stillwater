---
description: Preview, then run, the move of the images in each artist's extrafanart folder up into the artist folder.
---

# Migrate extrafanart Files

The migration moves the images in each artist's `extrafanart/` folder up into the artist folder, under Stillwater's numbered fanart names, so they become ordinary fanart that Stillwater counts, protects, and pushes. It runs only when an administrator starts it, never during a scan or sync.

**The migration is one-way.** There is no undo, so the preview is your safety net. Nothing is ever deleted: a file byte-identical to an image already in the artist folder is reported and left in place, and the only thing removed is the `extrafanart/` folder itself, once it is empty.

**Preview first.** Send a `POST` to `/api/v1/reports/extrafanart-migration` with `{"dry_run": true}` (the default), using a token from **Settings > API Tokens** that an administrator created, because a token acts with the role of its owner. The response lists each file as planned, skipped (identical copy), or blocked, and changes nothing. A blocked file cannot be moved safely (for example a link rather than a real file) and an artist whose files could not be planned is reported with `plan_failed`; the reasons are fixed codes and the full detail is in Stillwater's log. A preview can list files as planned and also report problems, so read the `problems` count.

An artist whose folder does not exist (for example an unmounted share) is skipped, is not counted as a problem, and is reported in `artists_skipped_missing`. If that number is not zero, mount the share and preview again: nothing under a skipped artist was checked. When nothing needed doing but some artists were skipped, the status is `nothing_checked` rather than `nothing_to_do`: check the mount, or remove the stale artists, before trusting the result.

## Run it

When the preview looks right, send the same request with `{"dry_run": false}`. The files move into the artist folder and the response reports `moved`, `failed` and `skipped_identical` counts, plus an outcome for every file. The run keeps going if your client disconnects or a proxy times out, so you can check the result afterwards by previewing again: files already moved no longer appear. A server shutdown or the 10 minute limit stops it; move files not reached are reported as `run_stopped` (identical and blocked files keep their own outcome), and running again is safe. Artists the run had not reached yet are not listed at all, so run again to process them.

The `status` tells you how it went:

- `migrated`: every file moved.
- `partial`: some files moved and some did not. Each artist that had a problem carries an `error` code, and each file that did not move carries a `reason`. `folder_unavailable` means the artist's folder was there when the run started and gone when it reached that artist, for example a share that dropped; files not yet moved were not moved (earlier files of that artist may already have moved). `folder_unreadable` means the folder could not be checked (for example a permissions problem or a stalled share), so Stillwater cannot tell whether it is gone; fix access and run again. `directory_not_removed` means the files moved but the now-empty `extrafanart/` folder could not be removed; remove it by hand or run again. Fix the cause, preview, and run again: the run is safe to repeat and only touches what is left. A file whose destination name was taken by another file is left where it is and reported as `blocked` with the reason `not_moved_safely`, never overwritten. `index_refresh_failed` means the files moved but Stillwater could not clear its stored image hashes in time; that also makes the run `partial`. A file reported as `source_gone` was no longer at its old location; this is expected on a repeat run, and it is also what you would see if something else removed it. It is not counted as moved.
- `failed`: nothing moved, or the run stopped early (for example a server shutdown or an unreadable setting).
- `nothing_to_do` or `nothing_checked`: as in the preview.

A `partial` or `failed` run still answers 200 when it finished, so read `status`, not just the HTTP code.

A `dry_run` value that is not true or false gets a 400, and a second run while one is in progress gets a 409.
