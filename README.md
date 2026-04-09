# Stillwater

[![CI](https://github.com/sydlexius/stillwater/actions/workflows/ci.yml/badge.svg)](https://github.com/sydlexius/stillwater/actions/workflows/ci.yml)
[![Release](https://github.com/sydlexius/stillwater/actions/workflows/release.yml/badge.svg)](https://github.com/sydlexius/stillwater/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/sydlexius/stillwater)](https://goreportcard.com/report/github.com/sydlexius/stillwater)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod-go-version/sydlexius/stillwater)](go.mod)

Stillwater is a self-hosted web application for managing artist and composer metadata and images for Emby, Jellyfin, and Kodi media servers. It reads and writes NFO files and handles artist artwork, letting you keep your music library metadata clean and consistent from a single interface.

## Features

- Browse and edit artist and composer NFO metadata files
- Fetch, crop, and organize artist images
- Pull metadata from MusicBrainz and Fanart.tv
- Works with Emby, Jellyfin, and Kodi
- Web UI for browsing and managing your library
- REST API at `/api/v1/` for automation and integration
- Session-based authentication
- Webhook notifications for external integrations

## Requirements

Docker is required. Docker Compose is optional but recommended.

## Installation

### Docker Run

Pull and run the image with a single command:

```bash
docker run -d \
  --name stillwater \
  -p 1973:1973 \
  -e PUID=99 \
  -e PGID=100 \
  -v stillwater-data:/data \
  -v /path/to/your/music:/music:rw \
  --restart unless-stopped \
  ghcr.io/sydlexius/stillwater:latest
```

The image is published to the GitHub Container Registry at `ghcr.io/sydlexius/stillwater`.

### Docker Compose

Create a `docker-compose.yml` file with the following content, adjusting the volume path for your music library:

```yaml
services:
  stillwater:
    image: ghcr.io/sydlexius/stillwater:latest
    container_name: stillwater
    ports:
      - "1973:1973"
    environment:
      - PUID=99
      - PGID=100
      - SW_LOG_LEVEL=info
      - SW_LOG_FORMAT=json
      # SW_ENCRYPTION_KEY will be auto-generated on first run if not set
      # SW_BASE_PATH=/stillwater  # Uncomment for subfolder reverse proxy
    volumes:
      - stillwater-data:/data
      - /path/to/your/music:/music:rw
    restart: unless-stopped

volumes:
  stillwater-data:
```

Start the container:

```bash
docker compose up -d
```

Once running, open your browser and go to `http://your-server-ip:1973`.

### Environment Variables

| Variable | Description | Default |
|---|---|---|
| `PUID` | User ID for file permission mapping | `99` |
| `PGID` | Group ID for file permission mapping | `100` |
| `SW_LOG_LEVEL` | Log verbosity: `info`, `debug`, `warn`, `error` | `info` |
| `SW_LOG_FORMAT` | Log format: `json` or `text` | `json` |
| `SW_ENCRYPTION_KEY` | Key used to encrypt stored secrets. Auto-generated on first run if not set. Set this explicitly to ensure the same key persists across container recreations. | auto-generated |
| `SW_BASE_PATH` | Base path prefix for subfolder reverse proxy deployments, for example `/stillwater` | (none) |
| `SW_PORT` | Port the HTTP server listens on | `1973` |
| `SW_MUSIC_PATH` | Path to the music library inside the container | `/music` |

## Reverse Proxy

### SWAG

Ready-to-use SWAG reverse proxy configuration samples are included in the repository under `build/swag/`:

- `stillwater.subdomain.conf.sample` - For subdomain deployments, for example `https://stillwater.yourdomain.com`
- `stillwater.subfolder.conf.sample` - For subfolder deployments, for example `https://yourdomain.com/stillwater`

For subfolder deployments, set the `SW_BASE_PATH` environment variable to match the path prefix, for example:

```bash
SW_BASE_PATH=/stillwater
```

## Unraid

### Installing via Community Applications

1. Open the **Apps** tab in the Unraid web UI (requires the Community Applications plugin).
2. Search for **Stillwater** and click **Install**.
3. Review the template fields described below and adjust values for your setup.
4. Click **Apply** to pull the image and start the container.

If the app does not appear in search results yet, you can install it manually:

1. Go to **Docker** > **Add Container** > **Template repositories**.
2. Add `https://github.com/sydlexius/unraid-templates`.
3. Click **Save** and then click the **Stillwater** template.

### Template fields

| Field | Description | Default |
|---|---|---|
| **Web UI Port** | Host port mapped to the Stillwater web interface. | `1973` |
| **Config/Database** | Host path for persistent application data (database, backups, config). | (auto-detected) |
| **Music Library** | Host path to your music library. This maps to `/music` inside the container and must be readable and writable by Stillwater. | (required) |
| **PUID** | User ID Stillwater runs as. Use the ID of the user that owns your music files to avoid permission issues. | `99` (nobody) |
| **PGID** | Group ID Stillwater runs as. | `100` (users) |
| **Encryption Key** | AES-256 key used to encrypt API secrets stored in the database. Auto-generated on first run when left blank. | (auto-generated) |
| **Log Level** | Log verbosity: `info`, `debug`, `warn`, or `error`. | `info` |
| **Log Format** | Log output format: `json` or `text`. | `json` |
| **Base Path** | URL prefix for subfolder reverse proxy deployments, for example `/stillwater`. Leave blank for root deployments. | (none) |

### Persisting the encryption key

When `SW_ENCRYPTION_KEY` is left blank, Stillwater generates a random key on first start and stores it in `/data/encryption.key` inside the container (which maps to your **Config/Database** path on the host). The key is reloaded automatically on restart as long as that path is preserved.

If you ever recreate the container with a new appdata path or delete the appdata folder, set `SW_ENCRYPTION_KEY` explicitly in the template to a fixed value so your stored API keys remain decryptable. You can find the auto-generated value in the existing `encryption.key` file before making changes.

## License

[GPL-3.0](LICENSE)
