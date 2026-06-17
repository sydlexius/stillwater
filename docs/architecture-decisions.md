# Architecture Decisions

Key decisions from the risk review that affect implementation across milestones.

## ID-first matching

When MBIDs are available (from Lidarr, NFO, embedded tags), use them directly. Skip name-based matching. Configurable priority: "Prefer ID match" (default), "Prefer name match", "Always prompt". Minimum confidence floor even in YOLO mode.

*Where it lives: [Providers and coalesce](architecture/providers-and-coalesce.md).*

## Atomic filesystem writes

All file writes (NFO, images) use a shared utility in `internal/filesystem/`: write to .tmp, rename existing to .bak, rename .tmp to target, delete .bak. Fall back to copy+delete with fsync for cross-mount/network shares.

*Where it lives: [Scanner pipeline](architecture/scanner-pipeline.md).*

## Singleton rate limiters

One per metadata provider, created at application startup, shared across all handlers and background jobs. MusicBrainz: 1 req/sec globally.

The reactive complement to the limiter is a shared, context-aware retry helper (`DoWithRetry` in `internal/provider/retry.go`) that wraps each provider HTTP round-trip. It honors Retry-After (delta-seconds and HTTP-date) with a jittered, bounded exponential fallback, and applies distinct policies for 429 (more attempts) and 503 (fewer, for a possibly-unhealthy server).

These singleton limiters are also what make the rule engine's artist-level parallelism safe (`SW_RULE_ENGINE_ARTIST_WORKERS`, default 2; set to 1 for the original sequential walk). The bounded worker pool in `walkScopedArtists` overlaps the per-artist provider-fetch latency of independent artists, but because every request still passes through the same FIFO-fair limiter, more workers cannot exceed any provider's request budget; they only hide latency.

*Where it lives: [Providers and coalesce](architecture/providers-and-coalesce.md).*

## Adaptive batched transactions

Small batches (< 100): single transaction. Medium (100-1000): transactions of 50. Large (1000+): transactions of 25 with short sleep. User actions get priority over background jobs.

## Image format policy

JPG and PNG only. Logos always PNG (preserve alpha). When saving a new image, delete existing files of the same type in other formats.

## Targeted platform refreshes

Prefer per-artist refresh (Emby/Jellyfin/Lidarr) over full library scan. Full scan only for large bulk operations (500+ artists).

## NFO conflict detection

Check last-modified timestamp before writing. If changed externally, warn instead of overwriting. Also check Lidarr/Emby/Jellyfin metadata saver settings via API.

*Where it lives: [Conflict gate](architecture/conflict-gate.md).*

## Scanner exclusions

Default skip list: "Various Artists", "Various", "VA", "Soundtrack", "OST". Excluded directories appear greyed out and unfetchable.

*Where it lives: [Scanner pipeline](architecture/scanner-pipeline.md).*

## Portable settings contract

The encrypted settings export/import bundle (`internal/settingsio`) is the portability contract for cross-instance restore. Two rules govern what it carries:

1. **Settings exported via the blanket KV dump must have a guaranteed DB row.** Code that reads a value via `getStringSetting(..., fallback)` and silently falls back to a hard-coded default when the row is absent will not export that value. The render path for a settings page is therefore responsible for seeding canonical defaults on first view (idempotent `INSERT OR IGNORE`) so an "I never touched this" instance still round-trips faithfully. The auth-provider Settings page does this for every `auth.providers.*` key it reads.

2. **Cross-instance ownership-bearing rows carry their own owners.** Envelope version 1.3 introduced a `users` block in the payload so cross-instance restore can recreate absent owners before downstream rows (api_tokens, user_preferences) are remapped. On import, users that already exist on the target are left untouched (the operator's setup wins); users absent on the target are recreated. In pre-1.4 envelopes, downstream rows were attributed back to their owner via a username -> user_id remap; v1.4+ envelopes carry stable UUIDs and match by id, falling back to username only for legacy envelopes (see Envelope versions below). An opt-in `admin_fallback_tokens` flag exists for environments that prefer to attribute orphan tokens to the importing admin instead of recreating users; the reassignment count surfaces in the import result so it cannot be silent.

   Security constraints on the import path: recreated users land with `is_protected=0` (the bootstrap-admin protection bit cannot be smuggled across instances) and any role outside `administrator | operator | admin` coerces to `operator` (least privilege; an unknown future role must not silently grant elevated access). The `admin_fallback_tokens` opt is a trust-boundary tradeoff: reassigning an orphan token to the importing admin can effectively raise its privileges if the original owner had a lower role on the source, so the flag is opt-in per import and only appropriate for migrations between instances under the same operator's control.

Envelope versions:

--8<-- "docs/_generated/envelope-versions.md"

Older envelopes remain importable. The `password_hash` inside the users block is a bcrypt digest -- never plaintext -- and only crosses the wire inside the passphrase-encrypted payload.

*Where it lives: [Settings import/export](architecture/settings-import-export.md).*

## Next-lane routing policy (decision 12)

The `/next/*` URL namespace is hard-gated by `middleware.UX`: when `SW_UX=stable` (the default), any request whose path matches `/next/` or `/next/*` receives an immediate 404 before any handler runs. The lane is simply not there.

**Why 404 and not redirect:** A redirect from `/next/X` to `/X` requires a maintained path map and re-introduces cross-channel coupling. 404 is honest -- the path does not exist when the lane is off -- and aligns with the #1929 principle that reachable-but-disabled routes are a security surface.

**Why middleware, not handler registration:** All `/next/*` routes are registered unconditionally in `router.go`. The gate lives in `middleware.UX` where `laneEnabled` is already computed, so no route-table churn is needed to toggle the feature.

**Handler-level guards (defense in depth):** Every `handleNext*` handler calls `checkNextChannel` as its first guard, which checks `UXChannelFromContext != UXNext` and returns 404 uniformly. In stable mode this is dead code (the middleware gate fires first). In next/dual mode it guards the edge case where an explicit `X-Stillwater-UX: stable` header opts a sub-request back to the stable channel -- those requests reach the handler but the explicit `/next/` path does not serve stable content, so 404 is the honest response.

The policy is: an explicit `/next/` path with the stable opt-out header always returns 404 across all next/ handlers, regardless of whether a stable equivalent exists. Five handlers previously delegated to the stable equivalent instead (`handleNextDashboardPage`, `handleNextArtistsPage`, `handleNextArtistDetailPage`, `handleNextForeignFilesPage`, `handleNextForeignAllowlistPage`); that inconsistency was corrected in #1933 to match the documented policy.

**Promotion path:** Set `SW_UX=next` to make the next/ lane the default (a `sw_ux=stable` cookie opts a user back). The `middleware.UX` gate is lifted automatically; no code change is required (#1757).
