---
description: Every SW_ environment variable Stillwater honors at startup, with types, defaults, and descriptions.
---

<!-- code: internal/config/config.go (Default, loadFromEnv, validate, Config struct hierarchy, detectFormat), cmd/stillwater/main.go (SW_CONFIG_PATH usage). Verified against main at the time of drafting. -->

# Environment variables

Stillwater is configured by a small TOML file plus environment-variable overrides. **Environment variables always win** over file values. YAML configuration files (`.yaml` or `.yml`) remain accepted for backward compatibility.

The table below is generated from the configuration definition; do not edit it by hand. Run `make generate-docs` after changing a configuration field.

<!-- BEGIN GENERATED: env-reference -->
| Variable | Type | Default | Description |
|---|---|---|---|
| `SW_ACME_CA` | string | unset | ACME directory URL. Defaults to Let's Encrypt production. Set to https://acme-staging-v02.api.letsencrypt.org/directory for testing without burning rate-limit quota. |
| `SW_ACME_CACHE_DIR` | string | unset | Directory where ACME account keys and issued certificates are cached. Defaults to the directory containing SW_DB_PATH plus /acme-cache. Persist this across restarts to avoid hitting CA rate limits. |
| `SW_ACME_DOMAIN` | string | unset | DNS name to request certificates for via ACME (Let's Encrypt by default). Setting this turns on autocert; the domain MUST resolve to this server and port 80 MUST be reachable from the public internet. |
| `SW_ACME_EAB_KEY_ID` | string | unset | Reserved for future use; not yet active. External Account Binding key identifier for ACME CAs that require it (for example ZeroSSL). |
| `SW_ACME_EAB_MAC_KEY` | string | unset | Reserved for future use; not yet active. External Account Binding HMAC key paired with SW_ACME_EAB_KEY_ID. Treat as a secret; will be persisted only after AES-256-GCM encryption when the ACME path lands. |
| `SW_ACME_EMAIL` | string | unset | Contact email registered with the ACME CA. Used for expiry notifications and account recovery; recommended but not required. |
| `SW_ACME_IP` | string | unset | Reserved for future use; not yet active. Public IP address for IP-SAN certificate orders (ZeroSSL). Must not be an RFC1918, loopback, or link-local address. |
| `SW_BACKUP_ENABLED` | boolean | `true` | Set to true or 1 to enable automated backups. Any other value disables them. |
| `SW_BACKUP_INTERVAL` | integer | `24` | Hours between automated backups. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_BACKUP_PATH` | path | (none) | Override the directory where automated database backups are written. When empty Stillwater writes to a backups/ subfolder of the config directory. |
| `SW_BACKUP_RETENTION` | integer | `7` | Number of recent backups to keep. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_BASE_PATH` | path | `/` | URL prefix for subfolder reverse-proxy deployments (for example /stillwater). When set from the environment the Settings UI marks the field read-only. |
| `SW_DB_PATH` | path | `/config/stillwater.db` | Filesystem path to the SQLite database file. |
| `SW_ENCRYPTION_KEY` | string | unset | Key used to encrypt provider API keys at rest. When unset Stillwater generates one on first run and persists it in the config directory. |
| `SW_HTTP3_ENABLED` | boolean | `false` | Set to true or 1 to enable an HTTP/3 (QUIC) listener over UDP. Requires direct TLS to be configured (SW_TLS_CERT_FILE and SW_TLS_KEY_FILE). The Alt-Svc header is added to HTTPS responses so HTTP/3-capable clients upgrade automatically; clients with UDP blocked fall back to HTTP/1.1+HTTP/2 over TCP. |
| `SW_HTTP3_PORT` | integer | unset | Optional dedicated UDP port for HTTP/3. When unset HTTP/3 reuses the effective HTTPS port (SW_TLS_PORT or SW_PORT). Numeric values outside 1-65535 are rejected at startup. |
| `SW_HTTP_REDIRECT_PORT` | integer | unset | Optional plain-HTTP listener port that 301-redirects to the HTTPS listener. Requires TLS to be configured (SW_TLS_CERT_FILE + SW_TLS_KEY_FILE). Typical value 80; must differ from SW_TLS_PORT (or SW_PORT in collapse mode). Numeric values outside 1-65535 are rejected at startup. |
| `SW_LOG_FORMAT` | string | `json` | Log output format. Use json for log aggregators or text for friendlier console output. |
| `SW_LOG_LEVEL` | string | `info` | Log level at startup. One of trace, debug, info, warn, error. The runtime can also adjust the live level from the Logs settings tab. |
| `SW_MUSIC_PATH` | path | `/music` | Default music library path used as a starting point when no library has been added through the UI. |
| `SW_PORT` | integer | `1973` | TCP port the HTTP server listens on. Numeric values outside 1-65535 are rejected at startup. |
| `SW_RULE_ENGINE_ARTIST_WORKERS` | integer | `2` | Number of artists the rule engine processes concurrently during a Run Rules pass. Default 2. Set to 1 for the original strictly-sequential walk; higher values overlap more per-artist provider fetches. The shared per-provider rate limiter still caps total request throughput. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_SCANNER_EXCLUSIONS` | list (comma-separated) | `Various Artists, Various, VA, Soundtrack, OST` | Comma-separated artist directory names the scanner skips. Whitespace around each token is trimmed. |
| `SW_SCANNER_MTIME_FAST_PATH` | boolean | `true` | When true the scanner reuses cached image flags for artist directories whose mtime has not advanced since the previous scan, eliminating the per-file stat + dimension probe loop. Set to false on filesystems with unreliable mtimes (some network shares, FUSE mounts, backup-restored trees) so every scan re-probes. |
| `SW_SESSION_SECRET` | string | unset | Long random string used to sign session cookies. When unset Stillwater generates one on first run and persists it in the config directory. |
| `SW_TLS_CERT_FILE` | string | unset | Path to a PEM-encoded TLS certificate. When set together with SW_TLS_KEY_FILE Stillwater serves HTTPS directly instead of plain HTTP. |
| `SW_TLS_KEY_FILE` | string | unset | Path to the PEM-encoded private key for SW_TLS_CERT_FILE. Both files must be readable by the Stillwater process. |
| `SW_TLS_PORT` | integer | unset | Optional dedicated HTTPS port. When unset Stillwater serves HTTPS on SW_PORT (collapse semantics, single listener). Numeric values outside 1-65535 are rejected at startup. |
| `SW_UX` | string | `stable` | Web UI channel: stable (the current UI), next (the in-development preview UI), or dual (both served; defaults to stable, users opt into the preview via the sw_ux cookie or /next/ paths). Default stable means no behavior change. |
<!-- END GENERATED: env-reference -->

`SW_CONFIG_PATH` is also honored at startup but lives outside the configuration struct: set it to point at a TOML or YAML config file. When unset, Stillwater attempts to read `/config/config.toml`; if that file does not exist, it starts with defaults plus environment overrides only. YAML files are still accepted for existing installs; the loader picks the parser from the file extension (`.toml` -> TOML; `.yaml` or `.yml` -> YAML).

## TOML configuration file

When `SW_CONFIG_PATH` points at a config file, Stillwater reads it on startup before applying environment-variable overrides. A complete TOML example:

```toml
[server]
port = 1973
base_path = ""              # set to e.g. /stillwater for subfolder reverse-proxy deploys

[database]
path = "/config/stillwater.db"

[auth]
session_secret = ""          # generate with: openssl rand -base64 64

[encryption]
key = ""                     # generate with: openssl rand -base64 32

[music]
library_path = "/music"

[scanner]
exclusions = ["Various Artists", "Various", "VA", "Soundtrack", "OST"]

[backup]
path = ""                    # defaults to <config-dir>/backups when empty
retention_count = 7
interval_hours = 24
enabled = true

[logging]
level = "info"
format = "json"
```

You can ship the file with secrets blank and inject `SW_SESSION_SECRET` and `SW_ENCRYPTION_KEY` at runtime from your secrets manager.

The same configuration in YAML (still supported for existing deployments) uses key: value syntax with two-space indentation under each top-level section, e.g. `server:` followed by an indented `port: 1973` line. Environment variables and field names are identical across both formats.

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

### UI channel (`SW_UX`)

`SW_UX` selects which web UI channel Stillwater serves. It is a presentation flag, not an API version: every channel uses the same handlers and `/api/v1/*` endpoints.

- `stable` (default) renders the current UI. No behavior change.
- `next` makes the preview UI the default serving channel for **every** route, not just `/next/*`. `/next/*` is additionally a forced-next lane, and any screen not yet rebuilt falls back to its stable page so navigation never breaks.
- `dual` makes both channels reachable. The stable UI is served by default and users opt into the preview explicitly (via the `sw_ux` cookie or a `/next/*` path); there is no chooser screen.

Per-user opt-in uses the `sw_ux` cookie (`stable` or `next`), which overrides the environment default. Every response carries an `X-Stillwater-UX: stable|next` header indicating the channel that served it, and each request log line includes a `ux=` field.

## What's *not* an environment variable

Several runtime behaviors look like they might be env-driven but live entirely in the database (set through the Settings UI):

- Provider API keys -- encrypted in the database.
- Library paths -- `SW_MUSIC_PATH` is just the default starting point; the live source of truth is the libraries you've added through the UI.
- Connection credentials (Emby / Jellyfin / Lidarr URLs and tokens) -- encrypted at rest.
- Per-rule enable / automation toggles.
- Backup schedule fine-tuning beyond the four backup environment variables.

If you've configured something through the UI that you want to bake into the deployment, the answer is usually a settings export/import (see [export and import settings](../how-to/export-import-settings.md)), not a new environment variable.
