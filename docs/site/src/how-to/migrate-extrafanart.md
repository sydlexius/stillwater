---
description: Preview moving the images in each artist's extrafanart folder up into the artist folder. Running the migration is not available yet.
---

# Migrate extrafanart Files

The migration moves the images in each artist's `extrafanart/` folder up into the artist folder, under Stillwater's numbered fanart names, so they become ordinary fanart that Stillwater counts, protects, and pushes. It runs only when an administrator starts it, never during a scan or sync.

**The migration will be one-way.** There is no undo, so the preview is your safety net. Nothing is ever deleted: a file byte-identical to an image already in the artist folder is reported and left in place, and the only thing removed is the `extrafanart/` folder itself, once it is empty.

**Running the migration is not available yet.** In this version you can only preview it: send a `POST` to `/api/v1/reports/extrafanart-migration` with `{"dry_run": true}` (the default), using a token from **Settings > API Tokens** that an administrator created, because a token acts with the role of its owner. The response lists each file as planned, skipped (identical copy), or blocked, and changes nothing. A preview can list files as planned and also report problems, so read the `problems` count.

A `dry_run` value that is not true or false gets a 400, a second preview while one runs gets a 409, and `"dry_run": false` gets a 501 with nothing read or changed.
