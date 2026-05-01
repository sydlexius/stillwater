---
description: Diagnose Emby, Jellyfin, and Lidarr connection failures -- 401/403 errors, image fetcher conflicts, refresh issues.
---

<!-- code: internal/connection/emby/client.go (StatusUnauthorized handling), internal/connection/jellyfin/client.go (StatusUnauthorized handling), internal/connection/state.go (ImageFetcherWarning RiskLevel "warn"/"critical"), internal/conflict/gate.go (gate reasons: shared library paths, server-side image saving, server-side NFO saving), internal/api/handlers_shared_filesystem.go (ImageFetcherWarning collection). -->

# Platform authentication

Most "Stillwater can't talk to my media server" issues are auth issues. This page walks through what each error mode looks like, where to verify, and how to fix.

## Where to look first

Settings > Connections shows a card per platform connection (Emby, Jellyfin, Lidarr). Each card has a live status indicator:

- **Green** -- Stillwater connected on its last attempt; calls are working.
- **Amber** -- the connection is configured but not currently authenticating, or has a non-fatal warning attached.
- **Red** -- authentication is failing or the URL is unreachable.

Click into the card to see the underlying error message.

<!-- SCREENSHOT: Settings > Connections | state: one green Emby + one red Jellyfin (auth failed) + one amber Lidarr (warning) | annotation: status indicators + click-to-expand -->

## "Authentication failed" / 401 Unauthorized

Stillwater got to the platform but the API key was rejected.

**Check:**

- Is the API key still valid on the platform? Platform admins can revoke keys; if your Stillwater admin user was removed, the key probably went with them.
- Did you copy the right key? Some platforms expose multiple kinds of tokens (admin API key vs user access token). Stillwater needs an admin-equivalent key for full functionality.
- Is the key for the right server? When you have multiple instances of the same platform, key + URL must match.

**Fix:**

1. Generate a new API key on the platform (the [connect Emby](../getting-started/connect-emby.md) and [connect Jellyfin](../getting-started/connect-jellyfin.md) pages cover where).
2. Settings > Connections > the failing card > **Edit**.
3. Paste the new key, save.
4. The status indicator should flip green within a few seconds.

## "Connection refused" / "DNS lookup failed" / 502 / 504

Stillwater couldn't even reach the platform.

**Check:**

- Is the URL correct? Common gotchas: missing port (`http://emby:8096` not `http://emby`), `https://` when the platform serves `http://`, an extra path component.
- Is the platform up? Open the URL in a browser from the same machine Stillwater runs on.
- Are Stillwater and the platform on the same network? If Stillwater is in Docker, `localhost` from Stillwater's perspective is the *container*, not the host. Use the host's network name or `host.docker.internal` (Docker Desktop) or the bridge IP.

**Fix:** correct the URL or fix the network path. Restart not required; the next request retries automatically.

## "Forbidden" / 403

Stillwater authenticated but the platform refused the action.

**Check:**

- Does the API key have admin scope? Reads might work with a user-scope token but writes (image upload, library refresh) need admin.
- Is the user the API key belongs to disabled on the platform?
- Is the platform in a maintenance mode that blocks API writes?

**Fix:** generate a new key from an admin user and re-save. If the platform admin can't grant admin scope, accept the read-only mode -- some Stillwater features will be unavailable but most still work.

## "Image fetcher conflict" warning

The Connections card shows an amber warning saying one or more enabled connections have server-side image fetching turned on. This means the platform is configured to download artwork on its own, which would duplicate or conflict with Stillwater's writes.

The risk level is one of:

- **Warn** -- the platform might fetch images that overwrite Stillwater's, but the impact is recoverable.
- **Critical** -- the platform is actively rewriting; expect Stillwater's image writes to be blocked by the conflict gate.

**Fix one of two ways:**

1. **Let Stillwater manage the artwork.** On the platform, turn off the image fetcher for the affected libraries. Stillwater becomes the sole writer.
2. **Let the platform manage.** In Stillwater, turn off the connection's "image write" feature so Stillwater leaves the artwork to the platform.

Pick whichever you prefer; the conflict goes away once one of the two stops writing.

## "Library paths overlap" / 409 from refresh

Two connections (or a connection and a manual library) point at the same files on disk. Stillwater detects this on startup and again whenever connections change.

The conflict gate's reason in the UI:

> library paths overlap between N connection pair(s); any image write reaches multiple servers on shared disk

**Fix:**

- **If both servers should see the artwork:** keep them pointed at the same path; let one (Stillwater) manage writes, set the other to read-only by disabling its server-side fetcher.
- **If they should be independent:** point each at a different directory. Stillwater's library configuration is independent of the platform's; they don't have to overlap.

## "NFO writes paused" banner

The site-wide banner reading "NFO file writes paused" means the conflict gate has detected an active platform that's writing NFO files for the same artists. Until you resolve the underlying overlap, Stillwater will not write NFOs (rule fixers will defer; manual saves will refuse).

**Fix:** Same approach as the image-fetcher conflict above. Either Stillwater or the platform writes NFOs; not both.

The banner clears within seconds of resolving the conflict.

## Refresh from platform returns nothing new

You connected the platform, you clicked "Refresh from platform", but no new artists appeared.

**Check:**

- **Library mapping.** The connection has to map to a specific library on the platform. Settings > Connections > the card > verify the platform-side library is the one you expect.
- **Artist visibility.** Some platforms hide artists behind a "show all" toggle that only admins see. Confirm the platform itself shows the artists you expect.
- **Rate limit.** Large libraries get fetched in pages; the first refresh of a 50,000-artist library can take several minutes. Check the event banner for ongoing progress.

**Fix:** correct the mapping or wait for the refresh to finish. If the refresh actually errored, the event banner shows the cause.

## "Restart the platform's library service after revoking" hint

When you change credentials on the platform side, the platform sometimes caches the old token. If Stillwater shows green but the actions still fail with 401, restart the platform's library service (Emby Server, Jellyfin) to flush. This is a platform behavior, not a Stillwater bug.

## See also

- [Connect Emby](../getting-started/connect-emby.md)
- [Connect Jellyfin](../getting-started/connect-jellyfin.md)
- [Field locks](../core-concepts/field-locks.md#what-about-platforms-pushing-back) -- the bigger picture on conflict-gated writes.
