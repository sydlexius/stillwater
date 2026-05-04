---
description: Every SW_ environment variable Stillwater honors at startup, with types, defaults, and descriptions.
---

<!-- code: internal/config/config.go (Default, loadFromEnv, validate, Config struct hierarchy), cmd/stillwater/main.go (SW_CONFIG_PATH usage). Verified against main at the time of drafting. -->

# Environment variables

Stillwater is configured by a small YAML file plus environment-variable overrides. **Environment variables always win** over YAML values.

The table below is generated from the configuration definition; do not edit it by hand. Run `make generate-docs` after changing a configuration field.

<!-- BEGIN GENERATED: env-reference -->
| Variable | Type | Default | Description |
|---|---|---|---|
| `SW_BACKUP_ENABLED` | boolean | `true` | Set to true or 1 to enable automated backups. Any other value disables them. |
| `SW_BACKUP_INTERVAL` | integer | `24` | Hours between automated backups. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_BACKUP_PATH` | path | (none) | Override the directory where automated database backups are written. When empty Stillwater writes to a backups/ subfolder of the config directory. |
| `SW_BACKUP_RETENTION` | integer | `7` | Number of recent backups to keep. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_BASE_PATH` | path | `/` | URL prefix for subfolder reverse-proxy deployments (for example /stillwater). When set from the environment the Settings UI marks the field read-only. |
| `SW_DB_PATH` | path | `/config/stillwater.db` | Filesystem path to the SQLite database file. |
| `SW_ENCRYPTION_KEY` | string | unset | Key used to encrypt provider API keys at rest. When unset Stillwater generates one on first run and persists it in the config directory. |
| `SW_LOG_FORMAT` | string | `json` | Log output format. Use json for log aggregators or text for friendlier console output. |
| `SW_LOG_LEVEL` | string | `info` | Log level at startup. One of trace, debug, info, warn, error. The runtime can also adjust the live level from the Logs settings tab. |
| `SW_MUSIC_PATH` | path | `/music` | Default music library path used as a starting point when no library has been added through the UI. |
| `SW_PORT` | integer | `1973` | TCP port the HTTP server listens on. Numeric values outside 1-65535 are rejected at startup. |
| `SW_SCANNER_EXCLUSIONS` | list (comma-separated) | `Various Artists, Various, VA, Soundtrack, OST` | Comma-separated artist directory names the scanner skips. Whitespace around each token is trimmed. |
| `SW_SESSION_SECRET` | string | unset | Long random string used to sign session cookies. When unset Stillwater generates one on first run and persists it in the config directory. |
<!-- END GENERATED: env-reference -->

`SW_CONFIG_PATH` is also honored at startup but lives outside the configuration struct: set it to point at a YAML config file. When unset, Stillwater attempts to read `/config/config.yaml`; if that file does not exist, it starts with defaults plus environment overrides only.

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
