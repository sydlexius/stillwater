---
description: Diagnose common Stillwater issues -- installation problems, first-run wizard hiccups, platform authentication, conflict-gated writes.
---

# Troubleshooting

Targeted diagnostics for the issues that come up most often. For deeper background, follow the cross-links into [core concepts](../core-concepts/index.md).

<div class="grid cards" markdown>

- __Platform authentication__

    ---

    Emby/Jellyfin/Lidarr connection failures, 401/403 errors, refresh issues, image fetcher conflicts.

    [Read more](platform-auth.md)

</div>

## Quick checks

A few things to try before deeper diagnostics:

1. __Read the event banner.__ Most operations surface their result there. A scan that "completed" with zero new artists is a different problem from a scan that errored.
2. __Check the Logs tab.__ Recent errors are visible without leaving the UI. Filter by level (warn / error) to skip the chatter.
3. __Look at conflict-gated chips.__ Amber chips in Settings > Rules indicate the conflict gate is blocking writes for that category. Resolve the underlying conflict first.
4. __Verify the connection cards.__ Settings > Connections shows live status for Emby / Jellyfin / Lidarr. A red status means the connection itself is failing -- fix that before suspecting Stillwater of misbehaving downstream.

## Installation

Install-time issues by platform.

### Native binary

- __`failed to open database: unable to open database file`.__ Stillwater is trying to write to a directory that doesn't exist or isn't writable. Check `SW_DB_PATH` points at a directory you can write to.
- __Web UI loads but writes to NFO files fail.__ The user running Stillwater (your shell user, or the systemd `User=`) doesn't have write access to the music directory. Confirm with `ls -ln <music dir>` and `sudo -u stillwater touch <music dir>/.write-test`.
- __Port 1973 already in use.__ Set `SW_PORT=<something else>` in the environment.

### Docker / Compose

- __Container exits immediately.__ Check `docker compose logs stillwater` for the cause. The most common ones are a music-volume bind mount that points at a non-existent host path, or a database migration error if you're rolling back to an older image.
- __Web UI loads but writes to NFO files fail.__ PUID/PGID don't match the host user owning the music directory. Run `ls -ln /path/to/your/music` and confirm the numeric owner matches the `PUID`/`PGID` in your compose file.
- __Port 1973 is already in use.__ Another service is bound to it. Change the host-side mapping (left of the colon in `ports:`) to a free port.

### Unraid

- __Can't find Stillwater in Community Applications.__ Make sure the Community Applications plugin is up to date. Stillwater appears under search; if it doesn't, refresh the CA database from the __Apps__ tab settings.
- __WebUI shows but writes to NFO files fail.__ The container can't write to your music share. Confirm the music library bind mount in the template points at a real path and that PUID/PGID `99/100` (or whatever you set) has write access to that share.
- __Container starts then immediately stops.__ Check the container's logs from the Docker tab. The most common cause is a misconfigured volume or port conflict.

## First-run wizard

- __Setup screen redirects me to login instead.__ Stillwater already has at least one user. The setup screen only appears when the user table is empty. If you're trying to recover a forgotten admin account, run `stillwater reset-credentials` (binary install) or `docker compose exec stillwater stillwater reset-credentials` (Docker).
- __"Path does not exist or is not writable" on a library.__ The directory either doesn't exist at the path you typed (typo, wrong host vs container path) or the user Stillwater runs as can't write to it. For Docker installs, check that the bind-mount in your compose file maps the host directory correctly. For binary installs, check `id` of the running user against `ls -ln <path>`.
- __The conflict pre-flight step keeps failing.__ Stillwater can't reach your media server. Verify the connection URL works from a `curl` on the Stillwater host, and that the API key is still valid.

## Connections

For auth failures, conflict-gated writes, and most connection-side issues, see the dedicated [Platform authentication](platform-auth.md) page. The follow-ups below cover items not on that page.

- __Connection saves but library import comes back empty.__ The administrator user Stillwater resolved doesn't have access to the music library. In Emby or Jellyfin, open Library access for the admin user and confirm the music library is included.
- __Stillwater wrote my NFO and the platform overwrote it on next refresh.__ Enable the __Let Stillwater manage artwork and NFO files on this server__ toggle on the connection. Without it, Stillwater and the platform's writer compete; with it, Stillwater is sole writer.
- __A rule is enabled in Settings but not running.__ Two common causes: (1) the conflict gate is closed (see the Connections-page banner). Image and NFO auto-fixes are skipped while the gate is closed. (2) The rule is filesystem-dependent and there's no local library configured; Stillwater auto-disables the "NFO exists" rule in that case. Re-add a local library or clear the conflict to resume.
- __Connection shows "Connected" but `Last checked` is stale.__ The timestamp updates when you click __Test__ on a connection (or during initial setup); Stillwater does not run a background re-check loop. To refresh it, open __Settings__ > __Connections__ and trigger __Test__ on the row.

## Reporting a bug

When something is genuinely broken, file an issue on the [GitHub issue tracker](https://github.com/sydlexius/stillwater/issues/new/choose) with:

- Stillwater version (Settings > Updates).
- How you installed (Docker Compose, binary, Unraid).
- Relevant log excerpts (Settings > Logs > Download).
- Steps to reproduce.
