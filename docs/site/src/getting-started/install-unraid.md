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
