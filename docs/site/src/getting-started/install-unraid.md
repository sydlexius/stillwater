---
description: Install Stillwater on Unraid via Community Applications. The easiest path if you're running Unraid.
---

# Install on Unraid

About 3 minutes if Community Applications is already installed.

## Before you start

You'll need:

- An Unraid server with the **Community Applications** plugin installed. Most Unraid installs already have it.
- A **share** containing your music library, accessible to the Unraid container subsystem. Typically something like `/mnt/user/Music` or wherever you keep your library.

## Install

1. In the Unraid web UI, open the **Apps** tab.
2. Search for **Stillwater** in the Community Applications search box.
3. Click **Install** on the Stillwater template.

Community Applications opens the template configuration page. Most fields are pre-filled with sensible defaults; a few need your input.

## Configure the template

Required fields:

- **Music Library Path.** The host path to your music library. For most setups this is something like `/mnt/user/Music`. Stillwater needs read/write access here for NFO writeback to work.
- **Config Path.** Where Stillwater stores its database, encryption key, and configuration. Unraid auto-fills this based on your appdata location -- typically something like `/mnt/user/appdata/stillwater`. You can change it if you'd rather store the data elsewhere.
- **WebUI Port.** The port to expose Stillwater on. Defaults to `1973`. Change it only if `1973` is already in use on your server.

Defaults you can usually leave alone:

- **PUID / PGID.** Unraid's default `99/100` (the `nobody:users` pair) matches Stillwater's container defaults. Files Stillwater writes to your music library will be owned by this pair, which Unraid is happy with. Stillwater never re-owns your library recursively, so it is safe to point it at a share you also mount into other containers (Lidarr, Plex, and so on) -- it needs write access, not ownership.
- **Network Type.** `Bridge` is the default and is correct for almost all setups.

Optional knobs:

- **Log level / format.** `info` and `json` are the defaults. Switch the format to `text` during initial setup if you want easier-to-read logs in the Docker tab.
- **`SW_BASE_PATH`.** Set this only if you plan to reach Stillwater through a reverse proxy at a subpath like `https://example.com/stillwater`. Leave blank for the typical "Stillwater on its own port or subdomain" setup.

## Resource Limits

The Community Applications template is maintained outside the Stillwater source tree and does not set resource limits, so the values shipped in the Docker Compose file do **not** reach an Unraid install. You have to enter them yourself; nothing applies them on your behalf.

If Stillwater shares your server with other containers, switch the template to **Advanced View** and add the following to **Extra Parameters**:

```text
--cpus=2.0 --pids-limit=512 --ulimit nofile=8192:8192 --memory=3g --memory-reservation=512m
```

Then add two variables alongside your other environment variables:

- **`GOMAXPROCS`** = `2`
- **`GOMEMLIMIT`** = `2400MiB`

Unraid's template also exposes a **Memory Limit** field. If you prefer to use it, set it to `3g` and drop `--memory=3g` from Extra Parameters rather than setting both.

Why these numbers:

- **CPU (`--cpus=2.0` plus `GOMAXPROCS=2`).** Two cores matches `SW_RULE_ENGINE_ARTIST_WORKERS`, which defaults to 2 and is the widest deliberate concurrency in Stillwater. Reaching the limit throttles rather than fails: rules passes and scans take longer, nothing errors. `GOMAXPROCS` must match, because the CPU limit constrains the container through the kernel scheduler without informing the Go runtime, which would otherwise run more work in parallel than the quota can absorb. Raise both together if sweeps feel slow.

- **Processes (`--pids-limit=512`).** A backstop against a runaway, not a working ceiling. Stillwater's normal thread count is far below this, and like the memory limit, but unlike the CPU and file-descriptor limits, exhausting it is fatal to the container rather than degrading, so leave it high.

- **File descriptors (`--ulimit nofile=8192:8192`).** Set well above any healthy peak on purpose. A meaningful share of Stillwater's descriptors are sockets to Emby, Jellyfin, and Lidarr, and how many are open at once depends partly on how those services behave rather than only on what Stillwater is doing. Exhaustion degrades: file opens are logged and skipped, outbound connections surface as a request error, and the filesystem watcher falls back to polling. Do not go below `2048`.

- **Memory (`--memory=3g` plus `GOMEMLIMIT=2400MiB`).** A pair, and both halves matter. `GOMEMLIMIT` is a soft ceiling the Go garbage collector honors, so approaching it shows up as slower passes. `--memory` is the hard ceiling, enforced by the kernel's OOM killer, which terminates the process outright with no chance to flush state or shut down cleanly. Setting `GOMEMLIMIT` at about 80% of the hard limit gives the soft control room to act first; the remaining 20% covers what the Go heap does not account for. Change one and you must change the other.

    `3g` is about three times the worst case Stillwater reaches. Image reads are capped at 25 MB but transiently hold roughly twice that while the buffer grows, and a decoded image is capped at 100 megapixels (about 400 MB worst case) as a separate allocation. Both are bounded by `SW_RULE_ENGINE_ARTIST_WORKERS`, which defaults to 2, keeping the transient peak under about 1 GB. These are per-image bounds, so the figure does not grow with the size of your library -- only raising `SW_RULE_ENGINE_ARTIST_WORKERS` warrants raising it.

    If the hard limit is reached, files already on disk are safe: every NFO and image write is staged in a temporary file and installed with a single rename, so nothing is left half-written. Work in flight is lost, so a rules pass in progress has to be re-run. Because Unraid restarts the container afterwards, the limit is set well above the real peak so that reaching it signals a genuine leak rather than ordinary work.

- **Memory reservation (`--memory-reservation=512m`).** A soft floor used only when the host decides how to pack containers. It reserves nothing and kills nothing, and reflects the steady-state working set rather than the transient peak.

## Apply and first run

Click **Apply** at the bottom of the template page. Unraid pulls the image and starts the container. After a few seconds:

1. Open the **Docker** tab in the Unraid web UI.
2. Click the Stillwater icon and choose **WebUI**.

Stillwater opens in a new tab and presents the first-time setup wizard.

[Continue to first-time setup](first-run-oobe.md){ .md-button .md-button--primary }

## Updating

When a new version is available, Unraid surfaces an update notification on the Docker tab. Click **Apply Update** on the Stillwater container. Your data in the Config Path you set is preserved across updates.

To pin a specific version instead of `latest`, edit the template and change the **Repository** field's tag (for example, from `ghcr.io/sydlexius/stillwater:latest` to `ghcr.io/sydlexius/stillwater:v1.0.0`).

## Backups

Your Config Path directory (typically under appdata, e.g., `/mnt/user/appdata/stillwater`) contains everything Stillwater needs to restore. The standard Unraid backup tools that handle the rest of your appdata will cover Stillwater automatically.

Stillwater also has an in-app scheduled backup feature. Enable it from the web UI under **Settings** > **Backups**, or by adding these environment variables to the template:

- `SW_BACKUP_ENABLED=true`
- `SW_BACKUP_INTERVAL=24`
- `SW_BACKUP_RETENTION=7`

Backups land inside the config volume by default.

## Troubleshooting

See [Installation > Unraid](../troubleshooting/index.md#unraid) in the troubleshooting docs.

## What about Docker Compose?

If you'd rather skip Community Applications and run Stillwater through Unraid's compose plugin or a different host entirely, see [Install with Docker Compose](install-docker-compose.md). The underlying image is identical; CA just wraps it in a GUI form.
