---
description: Review, allowlist, or delete unmatched files Stillwater found on disk, and manage the allowlist that suppresses them from future detection.
---

<!-- code: web/templates/foreign_files.templ, internal/api/handlers_foreign_files.go. -->

# Foreign files

The **Foreign Files** report (`/reports/foreign-files`, linked from the sidebar's Reports group as **Foreign Files**) lists unmatched files: images that match media-server naming patterns (fanart, thumb, logo, and so on) but that Stillwater didn't write and can't attribute to a provider. They typically arrive from another tool, a manual copy, or a naming pattern Stillwater doesn't recognize.

## Review a detected file

Each row shows the artist it was found under, the file name (hover for the full path), its size, and when it was detected. A file linked from more than one artist record or path spelling shows a "linked from multiple artists" badge -- only one row is shown per distinct file even when it has several ledger entries.

Two actions are available per row:

- **Allowlist** -- marks the file as intentionally kept; it drops out of this list and won't be flagged again.
- **Delete** -- removes the file from disk. Asks for confirmation first; this is destructive.

## Dismiss everything at once

When there's at least one detected file, a **Dismiss** button in the header allowlists every currently detected file in one action (also confirmed first). This is a bulk allowlist, not a delete -- no files are removed from disk.

## Manage the allowlist

Click **Manage Allowlist** in the header to open `/reports/foreign-files/allowlist`, a paginated list of every file you've allowlisted (individually or via Dismiss). Each entry shows what it applies to and when it was added. Click **Remove** on an entry to let that file be detected again; use **Back to Detected Files** to return to the main list.

## Keyboard shortcuts

Both pages support roving navigation over the table rows:

| Key | Action |
| --- | --- |
| `j` / `k` | Move focus to the next / previous row |
| `Enter` | Allowlist (detected-files page) or remove (allowlist page) the focused row |
| `h` / `l` | Previous / next page (allowlist page only, when there's more than one page) |

## See also

- [View reports](view-reports.md#duplicate-and-foreign-file-reports) for how this report page fits alongside the two-pane Reports workspace and Duplicates.
