# Milestone 26 -- UX & Maintenance

## Goal

Fix a case-sensitivity bug in image file detection, research MusicBrainz mirror
support, add LQIP placeholders for the artist grid, and implement two new metadata
providers (Spotify and Genius).

## Acceptance Criteria

- [ ] `existingImageFileNames` works correctly on case-sensitive filesystems
- [ ] MusicBrainz mirror research findings documented with actionable recommendations
- [ ] Artist grid shows blurred placeholders when images are unavailable
- [ ] Genius provider fetches artist biographies via Genius API
- [ ] Spotify provider fetches artist images, genres, and popularity via Spotify Web API

## Dependency Map

```
#319 (case-sensitive os.Stat bug) -- standalone bug fix
#315 (MusicBrainz mirror research) -- standalone research
#311 (LQIP placeholders) -- standalone (scanner + images + templates)
#305 (Genius provider) -- standalone new provider
#304 (Spotify provider) -- standalone new provider
```

No blocking relationships among open issues. All five can be worked in parallel.

## Checklist

### Issue #319 -- existingImageFileNames uses case-sensitive os.Stat on canonical paths
- [ ] Case-insensitive file existence check in `internal/rule/fixers.go`
- [ ] Tests for mixed-case filenames on case-sensitive filesystems
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #315 -- Research: MusicBrainz mirror support
- [ ] Survey available community mirrors and self-hosting options
- [ ] Document rate limit implications for mirrors
- [ ] Assess WS/2 API compatibility across mirrors
- [ ] Document configuration UX (base URL field already supported)
- [ ] Findings posted to issue
- [ ] Issue closed

### Issue #311 -- LQIP placeholders for Artist grid view
- [ ] Migration: add placeholder TEXT columns to `artists` table
- [ ] `GeneratePlaceholder()` in `internal/image/processor.go` (16x16, low quality)
- [ ] Generate placeholders at scan time when images are discovered
- [ ] Update artist grid template to use placeholder data URIs as fallback
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #305 -- Add Genius as a metadata provider
- [ ] New package `internal/provider/genius/`
- [ ] Implement `Provider` interface (SearchArtist, GetArtist; GetImages returns empty)
- [ ] Bearer token authentication
- [ ] Map Genius artist description to `ArtistMetadata.Biography`
- [ ] Settings UI: Genius card with API token field
- [ ] Register in provider registry
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged
- [ ] Docs updated (Architecture wiki: new provider)

### Issue #304 -- Add Spotify as a metadata and image provider
- [ ] New package `internal/provider/spotify/`
- [ ] Implement `Provider` interface (SearchArtist, GetArtist, GetImages)
- [ ] OAuth 2.0 Client Credentials flow (client ID + secret)
- [ ] Map Spotify genres, popularity, images to Stillwater types
- [ ] Settings UI: Spotify card with client ID and secret fields
- [ ] Register in provider registry
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged
- [ ] Docs updated (Architecture wiki: new provider)

## UAT / Merge Order

1. PR for #319 (base: main) -- small bug fix, merge first
2. PR for #315 (base: main) -- research only, close issue with findings
3. PR for #311 (base: main) -- LQIP placeholders
4. PR for #305 (base: main) -- Genius provider
5. PR for #304 (base: main) -- Spotify provider

PRs 1-5 are independent and can land in any order.

## Notes

- #319: `[mode: direct] [model: haiku]` -- scope: small bug fix
- #315: research/question -- no code changes expected, just findings
- #311: scope: medium -- touches scanner, image processing, templates, and a migration
- #305 and #304: new provider packages -- follow existing provider patterns in `internal/provider/`
- #304 requires a Spotify Developer application with client ID and secret for Web API access (noted in issue)
