---
description: Every built-in rule in Stillwater -- what it checks, what the fix does, what's configurable, and the default state.
---

<!-- code: internal/rule/service.go (defaultRules, RuleNFO/Thumb/Fanart/Logo/Banner/etc constants, filesystemRules), internal/rule/fixers.go (NFOFixer, MetadataFixer, ImageFixer, ExtraneousImagesFixer, LogoPaddingFixer, DirectoryRenameFixer, BackdropSequencingFixer; CanFix mappings), internal/rule/fixers_language.go (NameLanguageFixer), internal/database/migrations/001_initial_schema.sql (automation_mode DEFAULT 'auto'), internal/rule/service.go SeedDefaults (empty AutomationMode -> auto). 22 rules verified. -->

# Rules catalogue

Stillwater ships with 22 built-in rules across three categories: NFO, image, and metadata. Each section below covers one rule -- what it checks, what the fix does (if it's fixable), what's configurable, and how it ships.

For the *concept* behind enabled/disabled and manual/auto, see [rules](../core-concepts/rules.md). This page is the enumeration.

## At a glance

| Rule | Category | Default | Fixable |
|---|---|---|---|
| [NFO file exists](#nfo-file-exists) | NFO | Enabled, auto | Yes |
| [NFO has MusicBrainz ID](#nfo-has-musicbrainz-id) | NFO | Enabled, auto | Yes |
| [Biography exists](#biography-exists) | Metadata | Enabled, auto | Yes |
| [Metadata quality](#metadata-quality) | Metadata | Enabled, manual | Yes |
| [Directory name matches artist](#directory-name-matches-artist) | Metadata | Enabled, manual | Yes |
| [Artist/ID mismatch](#artistid-mismatch) | Metadata | Disabled, manual | Detection-only |
| [Artist name matches preferred language](#artist-name-matches-preferred-language) | Metadata | Disabled, manual | Sometimes |
| [Thumbnail image exists](#thumbnail-image-exists) | Image | Enabled, auto | Yes |
| [Thumbnail is square](#thumbnail-is-square) | Image | Enabled, auto | Yes |
| [Thumbnail minimum resolution](#thumbnail-minimum-resolution) | Image | Enabled, auto | Yes |
| [Fanart image exists](#fanart-image-exists) | Image | Enabled, auto | Yes |
| [Fanart minimum resolution](#fanart-minimum-resolution) | Image | Disabled, auto | Yes |
| [Fanart aspect ratio](#fanart-aspect-ratio) | Image | Disabled, auto | Yes |
| [Logo image exists](#logo-image-exists) | Image | Enabled, auto | Yes |
| [Logo minimum width](#logo-minimum-width) | Image | Disabled, auto | Yes |
| [Logo excessive padding](#logo-excessive-padding) | Image | Disabled, manual | Yes |
| [Banner image exists](#banner-image-exists) | Image | Disabled, auto | Yes |
| [Banner minimum resolution](#banner-minimum-resolution) | Image | Disabled, auto | Yes |
| [Backdrop/fanart sequencing](#backdropfanart-sequencing) | Image | Disabled, manual | Yes |
| [Minimum backdrop count](#minimum-backdrop-count) | Image | Disabled, manual | Detection-only |
| [Extraneous image files](#extraneous-image-files) | Image | Enabled, manual | Yes |
| [No duplicate images](#no-duplicate-images) | Image | Disabled, auto | Detection-only |

A rule marked **Detection-only** has no automated fix; you resolve the violations manually (or by adding artwork that satisfies the check).

---

## NFO file exists

**Category:** NFO &middot; **Default:** Enabled, auto &middot; **Severity:** error &middot; **Filesystem-dependent:** Yes

Checks that an artist directory contains an `artist.nfo` file. This is the only rule that fundamentally cannot work without local file access -- it's automatically skipped for pathless artists.

**Fix:** Generates an NFO from the artist's stored metadata and writes it to disk.

**Configurable:** Severity only.

## NFO has MusicBrainz ID

**Category:** NFO &middot; **Default:** Enabled, auto &middot; **Severity:** error

Checks that the artist's NFO contains a MusicBrainz artist ID. The MBID is the linchpin for cross-provider correlation -- many providers can be queried by MBID directly, and IDs from other providers are often discovered via MBID-keyed responses.

**Fix:** Asks providers (MusicBrainz first, then any provider whose response carries an MBID reference) for the artist's MBID and writes it to the NFO.

**Configurable:** Severity only.

## Biography exists

**Category:** Metadata &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Checks that the artist has a biography of at least the minimum length.

**Fix:** Fetches a biography from providers (Last.fm, Wikipedia, AudioDB, etc., per your priority) and saves it.

**Configurable:**

- **Minimum length** (default 10 characters). Tune up if you'd like to enforce more substantive bios.

## Metadata quality

**Category:** Metadata &middot; **Default:** Enabled, manual &middot; **Severity:** warning

Detects placeholder or junk metadata values -- a biography of just `?` or `N/A`, a sort name that's identical to a clearly-low-effort autofill, etc.

**Fix:** Clears the junk value and re-fetches from providers.

**Configurable:** Severity only.

## Directory name matches artist

**Category:** Metadata &middot; **Default:** Enabled, manual &middot; **Severity:** warning

Checks that the artist's directory name on disk matches the canonical artist name. Useful when a manual rename in the file browser desynchronizes the directory from Stillwater's record.

**Fix:** Renames the directory to match the canonical name.

**Configurable:**

- **Article handling** -- how to treat leading articles ("The", "A") when comparing names. Choices: **Prefix** (default; "The Beatles" stays "The Beatles"), **Suffix** ("Beatles, The"), **Strip** ("Beatles"). Set to match how your media platform sorts.

## Artist/ID mismatch

**Category:** Metadata &middot; **Default:** Disabled, manual &middot; **Severity:** warning &middot; **Detection-only**

Detects when an artist's filesystem folder name differs significantly from their stored metadata name. Uses fuzzy matching to allow minor variations while flagging real divergences.

**No automated fix.** Resolution is manual: rename the directory, edit the artist's name, or dismiss the violation.

**Configurable:**

- **Tolerance** (default 0.8). Higher values (closer to 1.0) only flag near-perfect mismatches; lower values flag more aggressively.

## Artist name matches preferred language

**Category:** Metadata &middot; **Default:** Disabled, manual &middot; **Severity:** warning

Flags artists whose stored Name or Sort Name does not match your preferred metadata languages (configured under Settings > Metadata).

**Fix:** When MusicBrainz provides a preferred-locale alias for the artist, the violation is fixable -- the fixer promotes that alias to primary. Otherwise the violation is informational and you handle it manually (edit the name) or dismiss it.

**Configurable:** Severity only. The preferred-language list lives in the Metadata settings, not the rule itself.

## Thumbnail image exists

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** error

Checks that the artist has a thumbnail image (`folder.jpg`, `artist.jpg`, `poster.jpg`, etc.).

**Fix:** Fetches a thumbnail from providers in priority order and saves it to disk.

**Configurable:** Severity only.

## Thumbnail is square

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Checks that the thumbnail is approximately square (1:1 aspect ratio). Note: this rule does *not* crop the existing image -- it fetches a square replacement from providers.

**Fix:** Searches providers for a thumbnail that meets the aspect-ratio requirement.

**Configurable:**

- **Aspect ratio** (default 1.0)
- **Tolerance** (default 0.1, meaning the actual ratio must be within +/- 10% of square)

## Thumbnail minimum resolution

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Checks that the thumbnail meets the minimum resolution.

**Fix:** Fetches a higher-resolution replacement from providers.

**Configurable:**

- **Minimum width** (default 500)
- **Minimum height** (default 500)

## Fanart image exists

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** warning

Checks that the artist has at least one fanart/backdrop image.

**Fix:** Fetches a fanart from providers (Fanart.tv has the strongest catalogue) and saves it.

**Configurable:** Severity only.

## Fanart minimum resolution

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** warning

Checks that the fanart meets the minimum resolution. Disabled by default because the threshold is high (1080p) -- enable when you've confirmed your providers can deliver at this quality.

**Fix:** Fetches a higher-resolution replacement; existing image is not upscaled.

**Configurable:**

- **Minimum width** (default 1920)
- **Minimum height** (default 1080)

## Fanart aspect ratio

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Flags fanart whose aspect ratio diverges from 16:9. Like the thumbnail-square rule, it does not crop the existing image -- it fetches a correctly-proportioned replacement.

**Fix:** Searches providers for a fanart matching the target aspect.

**Configurable:**

- **Aspect ratio** (default 16:9)
- **Tolerance** (default 0.1)

## Logo image exists

**Category:** Image &middot; **Default:** Enabled, auto &middot; **Severity:** info

Checks that the artist has a `logo.png`.

**Fix:** Fetches a logo from providers.

**Configurable:** Severity only.

## Logo minimum width

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Checks that the logo meets a minimum width for legibility at the sizes platforms render it.

**Fix:** Fetches a higher-resolution logo from providers.

**Configurable:**

- **Minimum width** (default 400)

## Logo excessive padding

**Category:** Image &middot; **Default:** Disabled, manual &middot; **Severity:** info

Detects logo images where excessive transparent (PNG) or whitespace (JPG) padding surrounds the artwork. When the padding area exceeds the configured percentage of the total image area, a violation is raised. Replaces the older `logo_trimmable` rule.

**Fix:** Trims the logo to its content bounds, leaving a configurable margin.

**Configurable:**

- **Padding threshold** (default 15% of image area)
- **Trim margin** (default 2 pixels of padding to keep after trimming)

## Banner image exists

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Checks that the artist has a banner image. Disabled by default because banners are an optional asset on most platforms.

**Fix:** Fetches a banner from providers.

**Configurable:** Severity only.

## Banner minimum resolution

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** info

Checks that the banner meets the minimum resolution.

**Fix:** Fetches a higher-resolution replacement.

**Configurable:**

- **Minimum width** (default 1000)
- **Minimum height** (default 185)

## Backdrop/fanart sequencing

**Category:** Image &middot; **Default:** Disabled, manual &middot; **Severity:** warning

Detects gaps and incorrect numbering in multi-fanart sequences (e.g. `fanart.jpg`, `fanart3.jpg`, with no `fanart2.jpg`).

**Fix:** Renames files to fill gaps and use the correct numbering for the platform profile.

**Configurable:** Severity only.

## Minimum backdrop count

**Category:** Image &middot; **Default:** Disabled, manual &middot; **Severity:** warning &middot; **Detection-only**

Flags artists with fewer fanart variants than you'd like.

**No single-click fix.** Resolution requires uploading more fanart (manually or via repeated provider fetches) until the count is satisfied.

**Configurable:**

- **Minimum count** (default 1; raise to require more variants)

## Extraneous image files

**Category:** Image &middot; **Default:** Enabled, manual &middot; **Severity:** warning

Flags image files in artist directories that don't match the canonical filenames configured in the active platform profile. Extras can cause duplicate or incorrect artwork on media servers.

**Fix:** Deletes the extraneous files. Default is **manual** mode -- you review each one before deletion. Switch to auto only when you're confident the platform profile matches your library.

**Configurable:** Severity only.

## No duplicate images

**Category:** Image &middot; **Default:** Disabled, auto &middot; **Severity:** warning &middot; **Detection-only**

Detects when different image slots (thumb vs fanart vs logo vs banner) contain visually similar images -- e.g. someone uploaded the same square portrait as both thumb and logo.

**No automated fix.** Resolution: replace one of the slots with a different image, or accept the duplication and dismiss.

**Configurable:**

- **Similarity tolerance** (default 0.90 = 90% similar)
