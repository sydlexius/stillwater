# Stillwater

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

- Docker and Docker Compose

No other software needs to be installed on your host. Stillwater runs entirely inside a container.

## Installation

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

A Community Applications template is available in the repository under `build/unraid/stillwater.xml`. You can use this to add Stillwater to your Unraid server through the Community Applications plugin.

## License

[GPL-3.0](LICENSE)
