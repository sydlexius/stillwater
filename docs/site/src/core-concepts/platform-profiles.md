---
description: How platform profiles bundle image filename conventions and NFO field-map settings so Stillwater writes files in the shape each media server expects.
---

<!-- code: internal/database/migrations/001_initial_schema.sql (platform_profiles INSERT), internal/platform/model.go (Profile, ImageNaming, NFOFormat), internal/platform/service.go (GetActive, SetActive), internal/nfo/fieldmap.go (NFOFieldMap, ApplyFieldMap, DefaultFieldMap), internal/nfo/writeback.go (WriteBackArtistNFOWithFieldMap), internal/publish/publisher.go (getActiveNamingConfig, getActiveFanartPrimary), web/templates/settings.templ (General tab platform profile picker) -->

# Platform profiles

A **platform profile** is a named configuration bundle that tells Stillwater how to write files for a specific media server. It packs two things together: the filenames to use for each image type, and the field-map rules that control how genres, styles, and moods are written into NFO elements. Changing the active profile changes the shape of every file Stillwater writes from that point forward.

## What a profile controls

### Image filenames

Each platform has expectations about what image files are named. Kodi looks for `fanart.jpg` in the artist directory; Emby and Jellyfin look for `backdrop.jpg`. Both look for `folder.jpg` as the thumb. If Stillwater writes the wrong filename, the platform either ignores the file or falls back to a lower-quality asset.

A profile's image naming table covers four slots -- thumb, fanart, logo, banner -- and each slot can list more than one filename. When Stillwater writes images it uses the first name in the list as the primary file; the rest are written as additional copies. That multi-name support lets a single profile satisfy two platforms that share a library directory.

The built-in profiles ship with the conventions each platform actually uses:

--8<-- "docs/_generated/platform-profiles.md"

Emby and Jellyfin use identical conventions in the built-in profiles. Kodi differs on the fanart filename; Plex differs on the thumb filename. The Custom profile is a copy of the Kodi defaults that you can edit freely.

### NFO field map

Different platforms interpret the `<genre>`, `<style>`, and `<mood>` XML elements differently. Kodi surfaces each in its own category; Emby and Jellyfin surface styles as Tags but largely ignore mood elements. The field map controls how Stillwater fills those elements when it writes an NFO.

Three tiers, evaluated in order:

1. **Default behavior** -- each category writes to its native element. Genres go into `<genre>`, styles into `<style>`, moods into `<mood>`. This is the Kodi-compatible default and what all built-in profiles use.
2. **Moods-as-styles** -- moods are additionally written as `<style>` elements. Emby and Jellyfin surface styles as Tags, so this makes moods visible without losing the original `<mood>` elements that Kodi reads.
3. **Advanced remap** -- a full matrix mapping any source category (genres, styles, moods) to any NFO element. Use when your platform ignores certain elements entirely and you want to consolidate.

The field map is configured via Settings, not by switching profiles directly. The active profile's image naming and NFO enabled flag are per-profile, but the field map settings are global. See [NFO files](nfo-files.md) for the full description of how the field map affects what gets written.

<!-- SCREENSHOT: Settings > General > Platform profile picker | state: active profile set to Emby | annotation: where to switch the active profile -->

## One active profile at a time

Exactly one profile is active at any moment. Switching to a different profile is instantaneous -- it changes a single database flag -- but it only affects writes that happen after the switch. Files already on disk keep whatever names they were written with; Stillwater does not rename existing files when you change profiles.

That means switching from Kodi to Emby mid-library leaves some artists with `fanart.jpg` and others with `backdrop.jpg`, until a re-write cycle touches each artist. If that matters for your setup, run a library rescan and a rules pass after switching so every artist gets the new filenames.

## Built-in vs custom profiles

The five built-in profiles (Emby, Jellyfin, Kodi, Plex, Custom) ship pre-seeded. The four platform-named ones are read-only -- you cannot change their image naming or other settings. The Custom profile is built-in but editable: it starts with Kodi-style defaults and you can reshape it freely.

You can also create your own profiles. A user-created profile is fully editable and can be deleted. The only constraint: filenames must use `.jpg`, `.jpeg`, or `.png` extensions; logos must use `.png`; no path separators in filename values.

## The Plex profile and NFO writes

The Plex profile ships with NFO writes disabled (`nfo_enabled = false`). Plex does not read Kodi-style `artist.nfo` files; writing them serves no purpose and creates clutter. When Plex is active, Stillwater writes images but skips NFO output. Switch to a different profile to re-enable NFO writes.

## How to switch profiles

Settings > General > Platform profile. The picker shows all profiles with the active one highlighted. Selecting a different profile and saving activates it immediately. See [images](images.md) for how image filenames interact with the active profile, and [NFO files](nfo-files.md) for how the field map affects NFO output.
