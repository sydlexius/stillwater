---
description: Rebuild image registry rows from the files already on disk, and restore existence flags for artwork that is present but recorded as missing.
---

<!-- code: internal/api/handlers_registry_repair.go, internal/maintenance/image_registry_repair.go, internal/maintenance/maintenance.go. -->

# Repair the Image Registry

Stillwater keeps a registry of every artwork slot it knows about for each artist. If that registry
loses rows, or marks artwork as missing when the file is still on disk, artwork appears to vanish
from the interface even though nothing was deleted from your library.

The repair operation rebuilds that registry from the files already on disk. It never downloads
artwork, never writes to your media server, and never deletes an image file -- it only corrects
Stillwater's own records to match what it finds.

This is an exceptional recovery operation, not routine maintenance. Run it when artwork you can see
on disk is missing from Stillwater, not on a schedule.

!!! note "API only in this release"

    The repair currently has no interface control. Invoke it through the API as shown below.

## Preview First, Then Apply

The operation takes a `commit` flag and always runs in two steps.

**Preview** reports what would change and writes nothing at all:

```bash
curl -X POST https://<your-stillwater>/api/v1/reports/registry-repair/remediate \
  -H 'Content-Type: application/json' \
  -d '{"commit": false}'
```

**Apply** performs the same work and writes the corrections:

```bash
curl -X POST https://<your-stillwater>/api/v1/reports/registry-repair/remediate \
  -H 'Content-Type: application/json' \
  -d '{"commit": true}'
```

Both forms require authentication, and the request is subject to the same protections as every other
state-changing call. Omitting `commit` previews.

The operation is safe to repeat. Running it twice in a row leaves the second run with nothing to do.

### Repair a Single Artist

Pass `artist_id` to limit the work to one artist instead of the whole library:

```bash
curl -X POST https://<your-stillwater>/api/v1/reports/registry-repair/remediate \
  -H 'Content-Type: application/json' \
  -d '{"commit": false, "artist_id": "<artist-id>"}'
```

## Reading the Report

The response summarizes the run, then repeats each pass's own detail underneath.

| Field | Meaning |
|---|---|
| `scanned` | Artists whose image directory was read successfully |
| `rebuilt` | Registry rows created for artwork found on disk |
| `restored` | Existence flags corrected for artwork that is present |
| `absent` | Artists whose directory is definitively not there |
| `unreadable` | Artists whose directory exists but could not be read |
| `skipped` | Artists with no resolvable image directory to examine |
| `unverifiable_rows` | Rows that could not be checked, because their artist's directory could not be read |
| `write_failures` | Corrections that were attempted but did not persist |
| `dry_run` | Whether this was a preview |
| `op_id` | Identifier for correlating the run with the logs |

Three distinctions are worth understanding, because they are deliberately kept apart rather than
merged into a single "problem" count:

- **`absent` is not `unreadable`.** A directory that is genuinely gone is a different situation from
  one that exists but cannot be opened, and they call for different responses. The first usually
  means the artist's files really did move; the second usually means permissions.
- **`skipped` is neither.** These artists have no location recorded to look at, so nothing was
  observed to be absent and no read was attempted. This is a gap in Stillwater's records, not a
  finding about your disk.
- **`unreadable` counts artists; `unverifiable_rows` counts rows.** One unreadable directory holding
  several artwork rows is one unreadable artist and several unverifiable rows.

Every artist considered lands in exactly one of `scanned`, `absent`, `unreadable`, or `skipped`, so
those four always account for the whole library.

A non-zero `write_failures` means the run did **not** fully complete, even though the request
succeeded. Re-run the operation; it is safe to repeat.

## What It Cannot Fix

Repair works from what is on disk, so it can only help artists it can locate. An artist with no
recorded path is reported under `skipped` and is left alone -- there is nowhere to look. Artwork that
was deleted from disk is genuinely gone and is not recreated; the operation corrects records, it does
not restore files.

## Responses

| Status | Meaning |
|---|---|
| 200 | The run completed. Check `write_failures` -- a 200 with failures means incomplete, not clean |
| 409 | A repair is already running. Wait for it to finish |
| 503 | The library is not reachable. Usually a mount that is down; nothing was changed |
| 500 | The run failed. Nothing partial is left behind that a re-run will not correct |

A repair that is interrupted -- for example by disconnecting mid-run -- is reported as a failure
rather than being silently recorded as a large number of individual write errors.

## Confirming the Result

Compare the report against the files themselves rather than against the counts alone. After a commit
run, the artwork that was present on disk should now appear in Stillwater, and a second preview
should report nothing left to do.
