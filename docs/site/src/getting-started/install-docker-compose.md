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

- `/config` (a Docker named volume): SQLite database, generated encryption key, optional `config.toml`, and any backups Stillwater writes itself.
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
      # Keep this equal to the `cpus` value below.
      - GOMAXPROCS=2
      # Keep this at about 80% of `mem_limit` below.
      - GOMEMLIMIT=2400MiB
      # SW_ENCRYPTION_KEY is auto-generated on first run if not set.
      # SW_BASE_PATH=/stillwater  # Uncomment for subfolder reverse proxy.
    volumes:
      - stillwater-data:/config
      - /path/to/your/music:/music:rw
    restart: unless-stopped
    cpus: "2.0"
    pids_limit: 512
    mem_limit: 3g
    mem_reservation: 512m
    ulimits:
      nofile:
        soft: 8192
        hard: 8192

volumes:
  stillwater-data:
```

## Customize the compose file

Two required edits before you start the stack, plus one recommended:

1. **Replace `/path/to/your/music`** with the absolute path to your music library on the host. For example, `/srv/media/music` or `/Users/you/Music`.

2. **Set `PUID` and `PGID`** to the values from `id` you noted earlier. The container drops privileges to this user so files Stillwater writes to your music directory are owned correctly. Mismatch here is the most common source of permission errors.

    Stillwater needs **write access** to `/music`, not ownership: it never recursively changes ownership of your library. That is deliberate, because a music share is often mounted into several containers at once, and re-owning it would lock the others out. Make sure the mount is writable by `PUID:PGID` (group-writable is the common arrangement) and new files Stillwater creates will be owned by that pair.

3. **(Optional) Pin a version tag** instead of `:latest`. For production setups:

    ```yaml
    image: ghcr.io/sydlexius/stillwater:v1.0.0
    ```

    Available tags are listed on the [GitHub releases page](https://github.com/sydlexius/stillwater/releases).

Other knobs you may not need to touch:

- **Port.** If `1973` is taken on your host, change the left side of the port mapping (for example, `"3000:1973"` to expose Stillwater on port 3000).
- **`SW_LOG_FORMAT`.** `json` is right for log aggregators; switch to `text` for friendlier console output during setup.
- **`SW_ENCRYPTION_KEY`.** Stillwater encrypts third-party API keys at rest. On first run it generates a key into `/config/encryption.key` and uses it from then on. You only need to set this env var if you're restoring from a backup that was encrypted with a known key.

## Resource Limits

The compose file bounds what the container can consume. These are all plain Compose Spec service keys, so `docker compose up` applies them; the `deploy.resources` form you may have seen elsewhere only takes effect under Swarm, where a limit that is silently ignored looks exactly like one that works.

| Key | Value | What it bounds |
|---|---|---|
| `cpus` + `GOMAXPROCS` | `2.0` / `2` | CPU time |
| `pids_limit` | `512` | Processes and OS threads |
| `ulimits.nofile` | `8192` | Open file descriptors |
| `mem_limit` + `GOMEMLIMIT` | `3g` / `2400MiB` | Memory |
| `mem_reservation` | `512m` | Memory, for scheduling only |

- **CPU.** Two cores matches `SW_RULE_ENGINE_ARTIST_WORKERS`, which defaults to 2 and is the widest deliberate concurrency in Stillwater. Reaching the limit throttles rather than fails: a rules pass or a scan takes longer, and nothing errors. Set `GOMAXPROCS` to the same number, because `cpus` on its own constrains the container through the kernel scheduler without informing the Go runtime, which then runs more work in parallel than the quota can absorb. If sweeps feel slow on a machine with cores to spare, raise both together (`4.0` and `4`).

- **Processes.** `512` is a backstop against a runaway, not a working ceiling. Stillwater's steady-state thread count is far below it, so you should never approach this number in normal operation. Leave it high: like the memory limit, and unlike the CPU and file-descriptor limits, exhausting the process limit is fatal to the container rather than degrading.

- **File descriptors.** `8192` sits well above any healthy peak. It is set generously on purpose, because a meaningful share of Stillwater's descriptors are sockets to Emby, Jellyfin, and Lidarr, and how many of those are open at once depends partly on how those services behave rather than only on what Stillwater is doing. Running out degrades: file opens are logged and skipped, outbound connections surface as a request error, and the filesystem watcher falls back to polling. If you tune it, do not go below `2048`.

- **Memory.** Two settings that work as a pair, and the order matters. `GOMEMLIMIT` is a soft ceiling the Go garbage collector honors: as the heap approaches it the collector works harder, so memory pressure shows up as slower passes. `mem_limit` is the hard ceiling, enforced by the kernel's OOM killer, which terminates the process outright with no chance to flush state or shut down cleanly. `GOMEMLIMIT` is set to about 80% of `mem_limit` so the soft control has room to act before the hard one fires; the remaining 20% covers the parts of the process the Go heap does not account for. If you change one, change both.

    `3g` is roughly three times the worst case Stillwater can actually reach, and the figure is derived rather than picked. Every path that reads an image file caps the read at 25 MB, but the buffer briefly holds about twice that while it grows, so budget 50-60 MB per read. Decoding is the larger cost: a decoded image is capped at 100 megapixels, which is about 400 MB in the worst case, and that is a separate allocation from the bytes it was decoded from. Both are limited by `SW_RULE_ENGINE_ARTIST_WORKERS`, which defaults to 2, so the transient peak stays under about 1 GB.

    These are fixed per-image bounds, so the number does not grow with your library. A larger library makes a sweep take longer, not consume more memory. Raise `mem_limit` (and `GOMEMLIMIT` with it) only if you raise `SW_RULE_ENGINE_ARTIST_WORKERS`, since that is what widens the concurrent-decode term.

    If the hard limit is ever reached, files already on disk are safe: Stillwater stages every NFO and image write in a temporary file and installs it with a single rename, so no file is left half-written. What is lost is the work in flight, meaning a rules pass in progress has to be re-run. Because `restart: unless-stopped` would restart into the same condition, the limit is set far enough above the real peak that reaching it indicates a genuine leak rather than ordinary work.

- **Memory reservation.** `mem_reservation` reserves nothing and kills nothing. It is a soft floor a host uses when deciding how to pack containers, so a busy server does not treat Stillwater as free. `512m` reflects the steady-state working set rather than the transient peak above.

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
VOL=$(docker volume ls --format '{{.Name}}' | grep '_stillwater-data$')
if [ -z "${VOL}" ]; then
  echo "No volume matching '*_stillwater-data' found." >&2
  echo "Run 'docker volume ls' and pick the right name, or set COMPOSE_PROJECT_NAME / use 'docker compose -p' to disambiguate." >&2
  exit 1
fi
if [ "$(printf '%s\n' "${VOL}" | wc -l | tr -d ' ')" -gt 1 ]; then
  printf 'Multiple matching volumes:\n%s\n' "${VOL}" >&2
  echo "Set VOL=<exact_name> manually before running this command." >&2
  exit 1
fi
docker run --rm \
  -v "${VOL}:/data:ro" \
  -v "$PWD":/backup \
  alpine tar czf "/backup/stillwater-$(date +%F).tar.gz" -C /data .
```

Scheduled backups are on by default. Tune them via the web UI or these env vars:

```yaml
- SW_BACKUP_ENABLED=true
- SW_BACKUP_INTERVAL=24   # hours between scheduled backups
- SW_BACKUP_RETENTION=7   # number of recent backups to keep
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
