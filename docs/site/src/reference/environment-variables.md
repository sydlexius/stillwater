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
| `SW_ACME_CA` | string | unset | Reserved for future use; not yet active. Will accept an ACME directory URL or shorthand (letsencrypt, letsencrypt-staging, buypass, zerossl) when the ACME path lands. |
| `SW_ACME_CACHE_DIR` | string | unset | Reserved for future use; not yet active. Directory where ACME account keys and issued certificates will be cached when the ACME path lands. |
| `SW_ACME_DOMAIN` | string | unset | Reserved for future use; not yet active. DNS name that the future ACME path will request certificates for. |
| `SW_ACME_EAB_KEY_ID` | string | unset | Reserved for future use; not yet active. External Account Binding key identifier for ACME CAs that require it (for example ZeroSSL). |
| `SW_ACME_EAB_MAC_KEY` | string | unset | Reserved for future use; not yet active. External Account Binding HMAC key paired with SW_ACME_EAB_KEY_ID. Treat as a secret; will be persisted only after AES-256-GCM encryption when the ACME path lands. |
| `SW_ACME_EMAIL` | string | unset | Reserved for future use; not yet active. Contact email that will be registered with the ACME CA when the ACME path lands. |
| `SW_ACME_IP` | string | unset | Reserved for future use; not yet active. Public IP address for IP-SAN certificate orders (ZeroSSL). Must not be an RFC1918, loopback, or link-local address. |
| `SW_BACKUP_ENABLED` | boolean | `true` | Set to true or 1 to enable automated backups. Any other value disables them. |
| `SW_BACKUP_INTERVAL` | integer | `24` | Hours between automated backups. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_BACKUP_PATH` | path | (none) | Override the directory where automated database backups are written. When empty Stillwater writes to a backups/ subfolder of the config directory. |
| `SW_BACKUP_RETENTION` | integer | `7` | Number of recent backups to keep. Must be a positive integer; non-positive or non-numeric values are silently ignored. |
| `SW_BASE_PATH` | path | `/` | URL prefix for subfolder reverse-proxy deployments (for example /stillwater). When set from the environment the Settings UI marks the field read-only. |
| `SW_DB_PATH` | path | `/config/stillwater.db` | Filesystem path to the SQLite database file. |
| `SW_ENCRYPTION_KEY` | string | unset | Key used to encrypt provider API keys at rest. When unset Stillwater generates one on first run and persists it in the config directory. |
| `SW_HTTP3_ENABLED` | boolean | `false` | Reserved for future use; not yet active. HTTP/3 (QUIC) listener wiring lands in a follow-up PR. |
| `SW_HTTP_REDIRECT_PORT` | integer | unset | Reserved for future use; not yet active. Plain-HTTP redirect listener wiring lands in a follow-up PR. Numeric values outside 1-65535 are rejected at startup. |
| `SW_LOG_FORMAT` | string | `json` | Log output format. Use json for log aggregators or text for friendlier console output. |
| `SW_LOG_LEVEL` | string | `info` | Log level at startup. One of trace, debug, info, warn, error. The runtime can also adjust the live level from the Logs settings tab. |
| `SW_MUSIC_PATH` | path | `/music` | Default music library path used as a starting point when no library has been added through the UI. |
| `SW_PORT` | integer | `1973` | TCP port the HTTP server listens on. Numeric values outside 1-65535 are rejected at startup. |
| `SW_SCANNER_EXCLUSIONS` | list (comma-separated) | `Various Artists, Various, VA, Soundtrack, OST` | Comma-separated artist directory names the scanner skips. Whitespace around each token is trimmed. |
| `SW_SESSION_SECRET` | string | unset | Long random string used to sign session cookies. When unset Stillwater generates one on first run and persists it in the config directory. |
| `SW_TLS_CERT_FILE` | string | unset | Path to a PEM-encoded TLS certificate. When set together with SW_TLS_KEY_FILE Stillwater serves HTTPS directly instead of plain HTTP. |
| `SW_TLS_KEY_FILE` | string | unset | Path to the PEM-encoded private key for SW_TLS_CERT_FILE. Both files must be readable by the Stillwater process. |
| `SW_TLS_PORT` | integer | unset | Optional dedicated HTTPS port. When unset Stillwater serves HTTPS on SW_PORT (collapse semantics, single listener). Numeric values outside 1-65535 are rejected at startup. |
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

## What's *not* an environment variable

Several runtime behaviors look like they might be env-driven but live entirely in the database (set through the Settings UI):

- Provider API keys -- encrypted in the database.
- Library paths -- `SW_MUSIC_PATH` is just the default starting point; the live source of truth is the libraries you've added through the UI.
- Connection credentials (Emby / Jellyfin / Lidarr URLs and tokens) -- encrypted at rest.
- Per-rule enable / automation toggles.
- Backup schedule fine-tuning beyond the four backup environment variables.

If you've configured something through the UI that you want to bake into the deployment, the answer is usually a settings export/import (see [export and import settings](../how-to/export-import-settings.md)), not a new environment variable.
