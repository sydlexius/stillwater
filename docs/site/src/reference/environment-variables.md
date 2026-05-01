---
description: Every SW_ environment variable Stillwater honors at startup, with defaults and YAML field equivalents.
---

<!-- code: internal/config/config.go (Default, loadFromEnv, validate, Config struct hierarchy), cmd/stillwater/main.go (SW_CONFIG_PATH usage). Verified against main at the time of drafting. -->

# Environment variables

Stillwater is configured by a small YAML file plus environment-variable overrides. **Environment variables always win** over YAML values.

| Variable | YAML field | Default | Notes |
|---|---|---|---|
| `SW_CONFIG_PATH` | -- | unset | Path to a YAML config file. When unset, Stillwater starts with defaults plus environment overrides only. |
| `SW_PORT` | `server.port` | `1973` | TCP port. Must be 1-65535; an invalid value is rejected at startup. |
| `SW_BASE_PATH` | `server.base_path` | `/` | URL prefix for subfolder reverse-proxy deployments (e.g. `/stillwater`). When set from the environment, the Settings UI marks the field read-only with a "managed by environment" badge so a config-as-code deployment can't be silently overridden through the UI. |
| `SW_DB_PATH` | `database.path` | `/config/stillwater.db` | SQLite file path. Required. |
| `SW_SESSION_SECRET` | `auth.session_secret` | unset | Long random string used to sign session cookies. When unset, Stillwater generates one on first run and persists it in the config directory. Set this only when you need sessions to survive across fresh container deployments with no persistent volume. |
| `SW_ENCRYPTION_KEY` | `encryption.key` | unset | Key used to encrypt provider API keys at rest. When unset, Stillwater generates one on first run and persists it in the config directory. Set this only when restoring from a backup that was encrypted with a known key. |
| `SW_MUSIC_PATH` | `music.library_path` | `/music` | Default library path used when no library is configured yet. Once you've added libraries through the UI, this is informational. |
| `SW_SCANNER_EXCLUSIONS` | `scanner.exclusions` | `Various Artists, Various, VA, Soundtrack, OST` | Comma-separated artist directory names the scanner skips. Whitespace around each token is trimmed. |
| `SW_BACKUP_PATH` | `backup.path` | empty (defaults inside Stillwater to a `backups/` subfolder of the config directory) | Override the directory where automated database backups are written. |
| `SW_BACKUP_RETENTION` | `backup.retention_count` | `7` | Number of recent backups to keep. Older backups are pruned after each successful new backup. Must be a positive number; non-positive values are silently ignored. |
| `SW_BACKUP_INTERVAL` | `backup.interval_hours` | `24` | Hours between automated backups. Must be a positive number; non-positive values are silently ignored. |
| `SW_BACKUP_ENABLED` | `backup.enabled` | `true` | Set to `true` or `1` to enable automated backups; any other value disables them. |
| `SW_LOG_LEVEL` | `logging.level` | `info` | One of `debug`, `info`, `warn`, `error`. The runtime can also raise/lower the live level via the Logs settings tab without restart. |
| `SW_LOG_FORMAT` | `logging.format` | `json` | `json` for log aggregators; `text` for friendlier console output. |

## YAML configuration file

When `SW_CONFIG_PATH` points at a YAML file, Stillwater reads it on startup before applying environment-variable overrides. A complete example:

```yaml
server:
  port: 1973
  base_path: ""              # set to e.g. /stillwater for subfolder reverse-proxy deploys

database:
  path: /config/stillwater.db

auth:
  session_secret: ""          # generate with: openssl rand -base64 64

encryption:
  key: ""                     # generate with: openssl rand -base64 32

music:
  library_path: /music

scanner:
  exclusions:
    - "Various Artists"
    - "Various"
    - "VA"
    - "Soundtrack"
    - "OST"

backup:
  path: ""                    # defaults to <config-dir>/backups when empty
  retention_count: 7
  interval_hours: 24
  enabled: true

logging:
  level: info
  format: json
```

You can ship the file with secrets blank and inject `SW_SESSION_SECRET` and `SW_ENCRYPTION_KEY` at runtime from your secrets manager.

## Behavior notes

### Base path normalization

`/`, `/stillwater/`, and `/stillwater` all normalize at startup. Either no prefix or `/stillwater` -- not both. The "managed by environment" UI marker keeps the value source visible.

### Backup numeric parsing

`SW_BACKUP_RETENTION` and `SW_BACKUP_INTERVAL` require a positive integer. **Invalid values silently fall through to the defaults** (no error logged). Set carefully; verify under Settings > Backups after restart.

### Backup-enabled accepts only `true` or `1`

Anything else disables backups -- `True`, `TRUE`, and `yes` all disable. Pin to `true` or `1`.

### Secrets generated on first run

If you don't set `SW_SESSION_SECRET` and `SW_ENCRYPTION_KEY`, Stillwater writes generated values into the config directory on first run. **Back these files up.** Losing the encryption key means losing the ability to decrypt stored provider API keys (you'd have to re-enter them); losing the session secret signs everyone out.

In container deployments, mount a Docker volume at `/config` so these files survive container recreation. The [install with Docker Compose](../getting-started/install-docker-compose.md) page covers this.

## What's *not* an environment variable

Several runtime behaviors look like they might be env-driven but live entirely in the database (set through the Settings UI):

- Provider API keys -- encrypted in the database.
- Library paths -- `SW_MUSIC_PATH` is just the default starting point; the live source of truth is the libraries you've added through the UI.
- Connection credentials (Emby / Jellyfin / Lidarr URLs and tokens) -- encrypted at rest.
- Per-rule enable / automation toggles.
- Backup schedule fine-tuning beyond the four backup environment variables.

If you've configured something through the UI that you want to bake into the deployment, the answer is usually a settings export/import (see [export and import settings](../how-to/export-import-settings.md)), not a new environment variable.
