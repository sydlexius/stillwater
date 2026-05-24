# Architecture Decisions

Key decisions from the risk review that affect implementation across milestones.

## ID-first matching

When MBIDs are available (from Lidarr, NFO, embedded tags), use them directly. Skip name-based matching. Configurable priority: "Prefer ID match" (default), "Prefer name match", "Always prompt". Minimum confidence floor even in YOLO mode.

## Atomic filesystem writes

All file writes (NFO, images) use a shared utility in `internal/filesystem/`: write to .tmp, rename existing to .bak, rename .tmp to target, delete .bak. Fall back to copy+delete with fsync for cross-mount/network shares.

## Singleton rate limiters

One per metadata provider, created at application startup, shared across all handlers and background jobs. MusicBrainz: 1 req/sec globally.

## Adaptive batched transactions

Small batches (< 100): single transaction. Medium (100-1000): transactions of 50. Large (1000+): transactions of 25 with short sleep. User actions get priority over background jobs.

## Image format policy

JPG and PNG only. Logos always PNG (preserve alpha). When saving a new image, delete existing files of the same type in other formats.

## Targeted platform refreshes

Prefer per-artist refresh (Emby/Jellyfin/Lidarr) over full library scan. Full scan only for large bulk operations (500+ artists).

## NFO conflict detection

Check last-modified timestamp before writing. If changed externally, warn instead of overwriting. Also check Lidarr/Emby/Jellyfin metadata saver settings via API.

## Scanner exclusions

Default skip list: "Various Artists", "Various", "VA", "Soundtrack", "OST". Excluded directories appear greyed out and unfetchable.

## Portable settings contract

The encrypted settings export/import bundle (`internal/settingsio`) is the portability contract for cross-instance restore. Two rules govern what it carries:

1. **Settings exported via the blanket KV dump must have a guaranteed DB row.** Code that reads a value via `getStringSetting(..., fallback)` and silently falls back to a hard-coded default when the row is absent will not export that value. The render path for a settings page is therefore responsible for seeding canonical defaults on first view (idempotent `INSERT OR IGNORE`) so an "I never touched this" instance still round-trips faithfully. The auth-provider Settings page does this for every `auth.providers.*` key it reads.

2. **Cross-instance ownership-bearing rows carry their own owners.** As of envelope version 1.3, the payload includes a `users` block. On import, users that already exist on the target are left untouched (the operator's setup wins); users absent on the target are recreated so downstream rows (api_tokens, user_preferences) can attribute back to them via the username -> user_id remap. An opt-in `admin_fallback_tokens` flag exists for environments that prefer to attribute orphan tokens to the importing admin instead of recreating users; the reassignment count surfaces in the import result so it cannot be silent.

   Security constraints on the import path: recreated users land with `is_protected=0` (the bootstrap-admin protection bit cannot be smuggled across instances) and any role outside `administrator | operator | admin` coerces to `operator` (least privilege; an unknown future role must not silently grant elevated access). The `admin_fallback_tokens` opt is a trust-boundary tradeoff: reassigning an orphan token to the importing admin can effectively raise its privileges if the original owner had a lower role on the source, so the flag is opt-in per import and only appropriate for migrations between instances under the same operator's control.

Envelope versions:

- `1.0`: original (settings, connections, platform profiles, webhooks, provider keys, priorities)
- `1.1`: adds rules, scraper configs, user preferences, plaintext summary
- `1.2`: adds libraries (connection refs remapped by type+url) and api_tokens (token_hash + metadata, never plaintext)
- `1.3`: adds users so cross-instance restore can recreate absent owners before the api_tokens / user_preferences username remap

Older envelopes remain importable. The `password_hash` inside the users block is a bcrypt digest -- never plaintext -- and only crosses the wire inside the passphrase-encrypted payload.
