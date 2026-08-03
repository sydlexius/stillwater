# Architecture Decisions

Key decisions from the risk review that affect implementation across milestones.

## ID-first matching

When MBIDs are available (from Lidarr, NFO, embedded tags), use them directly. Skip name-based matching. Configurable priority: "Prefer ID match" (default), "Prefer name match", "Always prompt". Minimum confidence floor even in YOLO mode.

*Where it lives: [Providers and coalesce](architecture/providers-and-coalesce.md).*

## Atomic filesystem writes

All file writes (NFO, images) use a shared utility in `internal/filesystem/`: write to a temp file in the target's own directory, flush it to stable storage, then promote it onto the target with a single rename. POSIX rename is an atomic replace, so the target is never observed missing mid-write, and a failed rename leaves the original content untouched. Fall back to copy+delete with fsync for cross-mount/network shares.

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

## Container resource limits (decision 13)

The shipped compose file bounds CPU, process count, file descriptors, and memory. The first three landed together; the memory limit was deliberately held back and added later, once the two preconditions below were met.

**Why CPU, pids, and nofile were safe immediately:** All three degrade when reached. The CPU quota throttles, so work takes longer and nothing errors. The file-descriptor cap is set an order of magnitude above any healthy peak, and the paths that can hit `EMFILE` already handle it: file opens log and skip, dials surface a request error, and `fsnotify` watcher creation falls back to poll-only mode.

**Why the memory limit came later, and what unblocked it:** A memory limit does not degrade. It is enforced by the cgroup OOM killer, which `SIGKILL`s the process with no deferred cleanup and no flush, and paired with `restart: unless-stopped` any input that reliably exceeds the cap restarts into the same condition and loops. It was therefore held back behind two preconditions, both now met:

- Every path that reads an image file into memory is bounded. The background hashing path and the request-reachable logo-trim handler both cap the read at `MaxDecodeBytes` and return a distinguishable "too large" error rather than allocating without limit. While an unbounded read existed, capping memory would have converted slow degradation into a reproducible crash loop.
- A hard kill can no longer destroy a file. `WriteFileAtomic` stages content in a uniquely-named temp file and installs it with a single rename, never moving the original aside, so there is no window in which a `SIGKILL` leaves the target missing or half-written. The earlier tmp/bak design did have such a window, and that was the second reason to defer.

**How the values were derived:** `mem_limit: 3g` is roughly 3x the worst-case transient. The dominant term is decode, not read: an image read is bounded at 25 MB but `io.ReadAll` grows by `append`, which keeps the old backing array alive while copying, so the realized peak is about 2x the bound (50-60 MB); a decoded image is a separate, larger allocation bounded by the 100-megapixel cap, about 400 MB worst case. `SW_RULE_ENGINE_ARTIST_WORKERS` (default 2) bounds how many run concurrently, putting the peak under about 1 GB. These are per-image bounds, so the budget does not scale with library size -- only raising `ArtistWorkers` widens it.

**Why `GOMEMLIMIT` is set below `mem_limit`:** `mem_limit` alone leaves the runtime blind: the process allocates until the cgroup kills it. `GOMEMLIMIT` at roughly 80% makes the collector respond to pressure before the hard ceiling, converting an OOM kill into backpressure. The 20% margin covers non-heap process memory. The ordering when changing either is `GOMEMLIMIT` first, then `mem_limit`.

**Why `mem_reservation` is set at the working set, not the peak:** It is a scheduling hint that reserves and kills nothing. Setting it at the transient peak would overstate the steady-state cost to a host packing containers.

**Why `pids_limit` is set far above the working value:** Unlike the CPU and file-descriptor caps, exceeding it is fatal (`fatal error: newosproc`). At 512 against a steady-state thread count well under 50, it is only reachable during a genuine thread-explosion bug, where failing fast is the correct outcome. Set near the working value it would be a liability rather than a backstop.

**Why `GOMAXPROCS` is pinned alongside `cpus`:** `cpus` constrains the container through the kernel CFS quota without informing the Go runtime, which then schedules more parallel work and GC workers than the quota can run, adding scheduling latency on top of the throttling. The two values must move together. `2` matches `SW_RULE_ENGINE_ARTIST_WORKERS`, the widest deliberate fan-out in the application.

**Why service-level compose keys, not `deploy.resources`:** The `deploy.resources.limits` form only takes effect under Swarm and is silently ignored by plain `docker compose up`, which is how the documented install path runs. A limit that is silently ignored is indistinguishable from a limit that works, so the keys are placed where the documented runtime honors them.

**Unraid:** The Community Applications template is maintained outside this repository, so none of these compose values reach an Unraid install. The gap is closed in prose rather than in code: the Unraid guide states plainly that the template applies no limits and documents the equivalent **Extra Parameters** (`--cpus`, `--pids-limit`, `--ulimit nofile`, `--memory`, `--memory-reservation`) plus the `GOMAXPROCS` and `GOMEMLIMIT` variables.
