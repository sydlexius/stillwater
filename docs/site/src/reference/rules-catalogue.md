---
description: Every built-in rule in Stillwater -- what it checks, what the fix does, what's configurable, and the default state.
---

<!-- code: internal/rule/service.go (defaultRules, RuleNFO/Thumb/Fanart/Logo/Banner/etc constants, filesystemRules), internal/rule/fixers.go (NFOFixer, MetadataFixer, ImageFixer, ExtraneousImagesFixer, LogoPaddingFixer, DirectoryRenameFixer, BackdropSequencingFixer; CanFix mappings), internal/rule/fixers_language.go (NameLanguageFixer), internal/database/migrations/001_initial_schema.sql (automation_mode DEFAULT 'auto'), internal/rule/service.go SeedDefaults (empty AutomationMode -> auto). 22 rules verified. -->

# Rules catalogue

Stillwater ships with 22 built-in rules across three categories: NFO, image, and metadata. Each section below covers one rule -- what it checks, what the fix does (if it's fixable), what's configurable, and how it ships.

For the *concept* behind enabled/disabled and manual/auto, see [rules](../core-concepts/rules.md). This page is the enumeration.

<!-- BEGIN GENERATED: rules-catalogue -->
## At a glance

| Rule | Category | Default | Fixable |
|---|---|---|---|
| [NFO file exists](#nfo-file-exists) | NFO | Enabled, auto | Yes |
| [NFO has MusicBrainz ID](#nfo-has-musicbrainz-id) | NFO | Enabled, auto | Yes |
| [Biography exists](#biography-exists) | Metadata | Enabled, auto | Yes |
| [Artist/ID mismatch](#artistid-mismatch) | Metadata | Disabled, manual | Detection-only |
| [Directory name matches artist](#directory-name-matches-artist) | Metadata | Enabled, manual | Sometimes |
| [Metadata quality](#metadata-quality) | Metadata | Enabled, manual | Yes |
| [Artist name matches preferred language](#artist-name-matches-preferred-language) | Metadata | Disabled, manual | Sometimes |
| [Thumbnail image exists](#thumbnail-image-exists) | Image | Enabled, auto | Yes |
| [Thumbnail is square](#thumbnail-is-square) | Image | Enabled, auto | Yes |
| [Thumbnail minimum resolution](#thumbnail-minimum-resolution) | Image | Enabled, auto | Yes |
| [Fanart image exists](#fanart-image-exists) | Image | Enabled, auto | Yes |
| [Logo image exists](#logo-image-exists) | Image | Enabled, auto | Yes |
| [Fanart minimum resolution](#fanart-minimum-resolution) | Image | Disabled, auto | Yes |
| [Fanart aspect ratio](#fanart-aspect-ratio) | Image | Disabled, auto | Yes |
| [Logo minimum width](#logo-minimum-width) | Image | Disabled, auto | Yes |
| [Banner image exists](#banner-image-exists) | Image | Disabled, auto | Yes |
| [Banner minimum resolution](#banner-minimum-resolution) | Image | Disabled, auto | Yes |
| [Extraneous image files](#extraneous-image-files) | Image | Enabled, manual | Sometimes |
| [No duplicate images](#no-duplicate-images) | Image | Disabled, auto | Detection-only |
| [Backdrop/fanart sequencing](#backdropfanart-sequencing) | Image | Disabled, manual | Yes |
| [Minimum backdrop count](#minimum-backdrop-count) | Image | Disabled, manual | Detection-only |
| [Logo excessive padding](#logo-excessive-padding) | Image | Disabled, manual | Yes |

A rule marked **Detection-only** has no automated fix; you resolve the violations manually (or by adding artwork that satisfies the check).

---

## NFO file exists

**Category:** NFO &middot; **Default:** Enabled, auto &middot; **Severity:** error &middot; **Filesystem-dependent:** Yes

Artist directory must contain an artist.nfo file

**Fix:** Generates an NFO from the artist's stored metadata and writes it to disk.

**Configurable:** Severity only.

---

## NFO has MusicBrainz ID

**Category:** NFO &middot; **Default:** Enabled, auto &middot; **Severity:** error

The artist.nfo file must contain a MusicBrainz artist ID

**Fix:** Asks providers (MusicBrainz first, then any provider whose response carries an MBID reference) for the artist's MBID and writes it to the NFO.

**Configurable:** Severity only.

---

## Biography exists

**Category:** Metadata &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Artist must have a biography populated

**Fix:** Fetches a biography from providers (Last.fm, Wikipedia, TheAudioDB, etc., per your priority order) and saves it to the artist record.

**Configurable:**

- Minimum biography length (default 10 characters)
- Severity (default: warning)

---

## Artist/ID mismatch

**Category:** Metadata &middot; **Default:** Disabled, manual &middot; **Severity:** warning

Detects when an artist's filesystem folder name differs from their stored metadata name. Uses fuzzy matching to allow minor variations while flagging significant divergences.

**Fix:** No automated fix.

**Configurable:**

- Tolerance (default 0.80)
- Severity (default: warning)

---

## Directory name matches artist

**Category:** Metadata &middot; **Default:** Enabled, manual &middot; **Severity:** warning

Artist directory name should match the canonical artist name

**Fix:** Renames the directory on disk to match the canonical artist name.

**Configurable:**

- Article handling (default: prefix)
- Severity (default: warning)

**Caveats:**

- Requires a local library path; skipped for pathless artists.
- Rename is skipped if the target directory already exists to avoid clobbering another artist's folder.
- Rename is skipped on shared-filesystem libraries to avoid collisions with the platform's own filesystem operations.

---

## Metadata quality

**Category:** Metadata &middot; **Default:** Enabled, manual &middot; **Severity:** warning

Detects placeholder or junk metadata values (e.g. biography of just '?' or 'N/A'). Violations are fixed by clearing the junk value and re-fetching from providers.

**Fix:** Clears the junk value and re-fetches from providers.

**Configurable:** Severity only.

---

## Artist name matches preferred language

**Category:** Metadata &middot; **Default:** Disabled, manual &middot; **Severity:** warning

Flags artists whose stored Name or SortName does not match the user's preferred metadata languages. When MusicBrainz provides a preferred-locale alias, the violation is fixable and Fix/auto mode can promote it; otherwise the violation is informational and can be edited manually or dismissed.

**Fix:** Updates the artist's stored name to the preferred-language form when one is available from MusicBrainz.

**Configurable:** Severity only.

**Caveats:**

- Only resolves when MusicBrainz returns a locale-specific alias; no change is made if no alias matches.

---

## Thumbnail image exists

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** error

Artist directory must contain a thumbnail image (folder.jpg/png)

**Fix:** Downloads a thumbnail image from configured providers (in priority order) and writes it to the artist directory.

**Configurable:** Severity only.

---

## Thumbnail is square

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Thumbnail must be approximately square (1:1 ratio). Violations are fixed by fetching a square replacement from providers; the existing image is not cropped.

**Fix:** Fetches a square replacement thumbnail from providers; the existing non-square image is replaced, not cropped.

**Configurable:**

- Aspect ratio (default 1, tolerance &plusmn;10%)
- Severity (default: warning)

---

## Thumbnail minimum resolution

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Thumbnail must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.

**Fix:** Fetches a higher-resolution thumbnail from providers and replaces the undersized image.

**Configurable:**

- Minimum resolution (default 500 &times; 500 px)
- Severity (default: warning)

---

## Fanart image exists

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Artist directory must contain a fanart/backdrop image

**Fix:** Downloads a fanart/backdrop image from configured providers and writes it to the artist directory.

**Configurable:** Severity only.

---

## Logo image exists

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** info

Artist directory must contain a logo image (logo.png)

**Fix:** Downloads a logo image from configured providers and writes it to the artist directory.

**Configurable:** Severity only.

---

## Fanart minimum resolution

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** warning

Fanart/backdrop must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.

**Fix:** Fetches a higher-resolution fanart image from providers and replaces the undersized file.

**Configurable:**

- Minimum resolution (default 1920 &times; 1080 px)
- Severity (default: warning)

---

## Fanart aspect ratio

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Fanart/backdrop should match the target aspect ratio. Violations are fixed by fetching a correctly-proportioned replacement from providers; the existing image is not cropped.

**Fix:** Fetches a replacement fanart image with the correct aspect ratio from providers.

**Configurable:**

- Aspect ratio (default 1.778, tolerance &plusmn;10%)
- Severity (default: info)

---

## Logo minimum width

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Logo should meet the minimum width for legibility. Violations are fixed by fetching a higher-resolution logo from providers.

**Fix:** Fetches a higher-resolution logo from providers and replaces the undersized file.

**Configurable:**

- Minimum width (default 400 px)
- Severity (default: info)

---

## Banner image exists

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Artist directory should contain a banner image

**Fix:** Downloads a banner image from configured providers and writes it to the artist directory.

**Configurable:** Severity only.

---

## Banner minimum resolution

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Banner must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.

**Fix:** Fetches a higher-resolution banner from providers and replaces the undersized file.

**Configurable:**

- Minimum resolution (default 1000 &times; 185 px)
- Severity (default: info)

---

## Extraneous image files

**Category:** Image &middot; **Default:** Enabled, manual &middot; **Severity:** warning

Flags image files that do not match filenames configured in the active platform profile. Extra files can cause duplicate or incorrect artwork on media servers. Auto-fix deletes them; manual mode lets you review changes first.

**Fix:** Deletes image files in the artist directory that are not in the active platform profile's expected filenames; on shared-filesystem libraries the union of all configured profiles' filenames is used so files owned by another platform are preserved.

**Configurable:** Severity only.

**Caveats:**

- Runs in manual mode only; never auto-deletes files.
- Requires a configured platform profile; on shared-filesystem libraries the fix is skipped if no platform service is available.

---

## No duplicate images

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** warning

Different image slots should not contain visually similar images (default threshold: 90%)

**Fix:** No automated fix.

**Configurable:**

- Tolerance (default 0.90)
- Severity (default: warning)

---

## Backdrop/fanart sequencing

**Category:** Image &middot; **Default:** Disabled, manual &middot; **Severity:** warning

Detects gaps in backdrop/fanart image sequences and incorrect numbering. Violations are fixed by renaming files to fill gaps.

**Fix:** Renames backdrop/fanart files to the canonical sequencing pattern (fanart.jpg, fanart1.jpg, fanart2.jpg ...).

**Configurable:** Severity only.

---

## Minimum backdrop count

**Category:** Image &middot; **Default:** Disabled, manual &middot; **Severity:** warning

Flags artists with fewer backdrops than the configured minimum. This rule is detection-only; resolving violations requires manual upload or multiple evaluation passes.

**Fix:** No automated fix.

**Configurable:**

- Minimum backdrop count (default 1)
- Severity (default: warning)

---

## Logo excessive padding

**Category:** Image &middot; **Default:** Disabled, manual &middot; **Severity:** info

Detects logo images where excessive transparent (PNG) or whitespace (JPG) padding surrounds the content. If the padding area exceeds the configured threshold (default 15%) of the total image area, a violation is raised. Auto-fix trims to content bounds with a configurable margin. Replaces the former logo_trimmable rule.

**Fix:** Crops the excess transparent or whitespace border from the logo image and writes the trimmed version in place.

**Configurable:**

- Padding threshold (default 15% of image area)
- Trim margin (default 2 px)
- Severity (default: info)
<!-- END GENERATED: rules-catalogue -->
