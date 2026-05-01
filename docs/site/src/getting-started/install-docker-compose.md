---
description: Install Stillwater using Docker Compose. The recommended self-hosted setup.
---

# Install with Docker Compose

About 5 minutes from zero to running.

## Before you start

You'll need:

- **Docker** and **Docker Compose** installed. On Linux, `docker --version` and `docker compose version` should both work. macOS and Windows users can install Docker Desktop.
- A directory containing your music library. Stillwater needs to read it, and (for NFO writeback) write `artist.nfo` and image files into it. The directory can be local or a network share that's mounted on the Docker host.
- The host user ID and group ID that own that music directory. Find them with:

    ```bash
    id
    ```

    Note the `uid=` and `gid=` numbers. You'll plug them into the compose file in a moment.

## What gets deployed

One container running the Stillwater binary. Two storage paths:

- `/config` (a Docker named volume): SQLite database, generated encryption key, configuration YAML, and any backups Stillwater writes itself.
- `/music` (a bind mount to your host's music directory): the library Stillwater reads from and writes NFO files into.

Stillwater listens on port `1973` inside the container. The compose file publishes it to `1973` on the host, so you'll reach the web UI at `http://localhost:1973` once it's up.

## The compose file

Save this to `docker-compose.yml` in a new directory (anywhere; the directory just needs to be writable and somewhere you can run `docker compose` from).

```yaml
services:
  stillwater:
    image: ghcr.io/sydlexius/stillwater:latest
    container_name: stillwater
    ports:
      - "1973:1973"
    environment:
      - PUID=1000
      - PGID=1000
      - SW_LOG_LEVEL=info
      - SW_LOG_FORMAT=json
      # SW_ENCRYPTION_KEY is auto-generated on first run if not set.
      # SW_BASE_PATH=/stillwater  # Uncomment for subfolder reverse proxy.
    volumes:
      - stillwater-data:/config
      - /path/to/your/music:/music:rw
    restart: unless-stopped

volumes:
  stillwater-data:
```

## Customize the compose file

Three required edits before you bring it up:

1. **Replace `/path/to/your/music`** with the absolute path to your music library on the host. For example, `/srv/media/music` or `/Users/you/Music`.

2. **Set `PUID` and `PGID`** to the values from `id` you noted earlier. The container drops privileges to this user so files Stillwater writes to your music directory are owned correctly. Mismatch here is the most common source of permission errors.

3. **(Optional) Pin a version tag** instead of `:latest`. For production setups:

    ```yaml
    image: ghcr.io/sydlexius/stillwater:v1.0.0
    ```

    Available tags are listed on the [GitHub releases page](https://github.com/sydlexius/stillwater/releases).

Other knobs you may not need to touch:

- **Port.** If `1973` is taken on your host, change the left side of the port mapping (for example, `"3000:1973"` to expose Stillwater on port 3000).
- **`SW_LOG_FORMAT`.** `json` is right for log aggregators; switch to `text` for friendlier console output during setup.
- **`SW_ENCRYPTION_KEY`.** Stillwater encrypts third-party API keys at rest. On first run it generates a key into `/config/encryption.key` and uses it from then on. You only need to set this env var if you're restoring from a backup that was encrypted with a known key.

## Bring it up

From the directory containing `docker-compose.yml`:

```bash
docker compose up -d
docker compose logs -f stillwater
```

You should see Stillwater start, run any pending database migrations, and report `listening on :1973`. Open `http://localhost:1973` in a browser. The first-time setup wizard greets you.

[Continue to first-time setup](first-run-oobe.md){ .md-button .md-button--primary }

## Day-to-day operations

```bash
# Tail logs
docker compose logs -f stillwater

# Restart the container (no data loss)
docker compose restart stillwater

# Stop the container
docker compose stop

# Open a shell inside the container
docker compose exec stillwater sh
```

## Upgrading

```bash
docker compose pull
docker compose up -d
```

If you pinned a version tag, edit the `image:` line first, then run the same two commands.

## Backups

The `stillwater-data` named volume holds everything Stillwater needs to restore: database, encryption key, config. Back it up with a one-shot tar container. Compose prefixes named volumes with the project name (the project directory's basename, by default), so the example below discovers the actual volume name dynamically:

```bash
VOL=$(docker volume ls --format '{{.Name}}' | grep '_stillwater-data$' | head -n1)
docker run --rm \
  -v "${VOL}:/data" \
  -v "$PWD":/backup \
  alpine tar czf "/backup/stillwater-$(date +%F).tar.gz" -C /data .
```

Stillwater also has a built-in scheduled backup feature. Enable it via the web UI or these env vars:

```yaml
- SW_BACKUP_ENABLED=true
- SW_BACKUP_INTERVAL=24
- SW_BACKUP_RETENTION=7
```

Backups land in `/config/backups` inside the volume by default.

## Just want `docker run`?

For one-off testing or environments where Compose isn't available:

```bash
docker run -d \
  --name stillwater \
  -p 1973:1973 \
  -v stillwater-data:/config \
  -v /path/to/your/music:/music:rw \
  -e PUID=1000 \
  -e PGID=1000 \
  --restart unless-stopped \
  ghcr.io/sydlexius/stillwater:latest
```

The Compose form above is recommended for anything beyond a quick try; backups, log rotation, and reverse-proxy setups all assume a Compose-managed stack.

## Troubleshooting

See [Installation > Docker / Compose](../troubleshooting/index.md#docker-compose) in the troubleshooting docs.
