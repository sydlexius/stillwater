---
description: Preview, then run, the move of the images in each artist's extrafanart folder up into the artist folder.
---

# Migrate extrafanart Files

The migration moves the images in each artist's `extrafanart/` folder up into the artist folder, under Stillwater's numbered fanart names, so they become ordinary fanart that Stillwater counts, protects, and pushes. It runs only when an administrator starts it, never during a scan or sync.

**The migration is one-way.** There is no undo, so the preview is your safety net. Nothing is ever deleted: a file byte-identical to an image already in the artist folder is reported and left in place, and the only thing removed is the `extrafanart/` folder itself, once it is empty.

**Preview first.** Send a `POST` to `/api/v1/reports/extrafanart-migration` with `{"dry_run": true}` (the default), using a token from **Settings > API Tokens** that an administrator created, because a token acts with the role of its owner. The response lists each file as planned, skipped (identical copy), or blocked, and changes nothing. A blocked file cannot be moved safely (for example a link rather than a real file) and an artist whose files could not be planned is reported with `plan_failed`; the reasons are fixed codes and the full detail is in Stillwater's log. A preview can list files as planned and also report problems, so read the `problems` count.

An artist whose folder does not exist (for example an unmounted share) is skipped, is not counted as a problem, and is reported in `artists_skipped_missing`. If that number is not zero, mount the share and preview again: nothing under a skipped artist was checked. When nothing needed doing but some artists were skipped, the status is `nothing_checked` rather than `nothing_to_do`: check the mount, or remove the stale artists, before trusting the result.

## Open the page

An administrator can also preview the migration in the browser at `/reports/extrafanart-migration` (under your base path, if you set one). The page changes nothing: each time it loads it reads every artist's `extrafanart/` folder and lists what a run would do, so reloading always shows the current state.

- A summary shows the status, how many artists have files, how many files would move, and how many identical copies would be left in place.
- The table lists one row per file with its artist, the file, the name it would get in the artist folder, and its outcome. A row with a problem says why in plain words.
- A note above the table repeats that the migration is one-way and that nothing is deleted.
- If some artist folders were not found, a notice above the summary says how many were skipped and that nothing under them was checked.
- If the preview stops before it finishes, an error notice appears above whatever rows it reached (or on its own if it reached none). Any rows shown are not a complete plan; reload to try again.
- If a migration is running, or another preview is still loading, a notice says so and no table is shown.

The preview reads every artist's folder, so on a large library it can take a while. It never blocks a run: a run you start while a preview is loading goes ahead. While a run is in progress the page shows a notice instead of a preview, and a second preview opened at the same time shows the same notice. The table shows at most the first 500 rows and says how many it left out; the summary still counts every file.

The page has no run button yet: to move the files, use the API as described below.

## Run it

When the preview looks right, send the same request with `{"dry_run": false}`. The files move into the artist folder and the response reports `moved`, `failed` and `skipped_identical` counts, plus an outcome for every file. The run keeps going if your client disconnects or a proxy times out, so you can check the result afterwards by previewing again: files already moved no longer appear. A server shutdown or the 10 minute limit stops it; move files not reached are reported as `run_stopped` (identical and blocked files keep their own outcome), and running again is safe. Artists the run had not reached yet are not listed at all, so run again to process them.

The `status` tells you how it went:

- `migrated`: every file moved.
- `partial`: some files moved and some did not. Each artist that had a problem carries an `error` code, and each file that did not move carries a `reason`. `folder_unavailable` means the artist's folder was there when the run started and gone when it reached that artist, for example a share that dropped; files not yet moved were not moved (earlier files of that artist may already have moved). `folder_unreadable` means the folder could not be checked (for example a permissions problem or a stalled share), so Stillwater cannot tell whether it is gone; fix access and run again. `directory_not_removed` means the files moved but the now-empty `extrafanart/` folder could not be removed; remove it by hand or run again. Fix the cause, preview, and run again: the run is safe to repeat and only touches what is left. A file whose destination name was taken by another file is left where it is and reported as `blocked` with the reason `not_moved_safely`, never overwritten. `index_refresh_failed` means the files moved but Stillwater could not clear its stored image hashes in time; that also makes the run `partial`. A file reported as `source_gone` was no longer at its old location; this is expected on a repeat run, and it is also what you would see if something else removed it. It is not counted as moved.
- `failed`: nothing moved, or the run stopped early (for example a server shutdown or an unreadable setting).
- `nothing_to_do` or `nothing_checked`: as in the preview.

The HTTP code summarizes the outcome, and the body has the same shape for every code:

| Code | Meaning |
| --- | --- |
| 200 | A preview finished, or a live run moved every file that was due (or had nothing to do). A live `nothing_checked` run also answers 200: check `artists_skipped_missing`. |
| 207 | A live run finished with a non-clean outcome (`partial` or `failed`): a file did not move, or every file moved but a follow-up step failed (`index_refresh_failed`, `directory_not_removed`). Read the per-artist and per-file outcomes. |
| 500 | The run stopped early. Files already moved stay moved; run again. |
| 409 | Another run is already in progress. |

`status` stays the authoritative detail: a `failed` run can answer 500 (it stopped early) or 207 (it finished and nothing moved).

A `dry_run` value that is not true or false gets a 400, and a second run while one is in progress gets a 409.
