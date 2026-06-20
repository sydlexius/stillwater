---
description: Convert a deprecated YAML config file to the supported TOML format, section by section.
---

# Convert YAML config to TOML

TOML is the supported configuration format for Stillwater. YAML config is **deprecated**: it still parses today, but support will be removed in a future release, and Stillwater logs a startup warning whenever it loads a YAML file:

```
YAML config format is deprecated; convert to TOML. See https://sydlexius.github.io/stillwater/how-to/convert-yaml-to-toml/
```

This guide walks through converting an existing `config.yaml` to `config.toml` one section at a time. Nothing else changes: the keys, defaults, and `SW_*` environment-variable overrides are identical across both formats. Only the file syntax differs.

## Before you start { #convert-yaml-toml-before }

A few things to know:

- **YAML still works for now.** You can convert at your own pace. The deprecation warning is the only consequence of staying on YAML in this release.
- **Environment variables are unaffected.** `SW_*` variables override the file in both formats and keep the same precedence: built-in default < config file < `SW_*` environment variable.
- **Point Stillwater at the new file.** Stillwater chooses the parser from the file extension: a `.toml` file is parsed as TOML, a `.yaml`/`.yml` file as YAML. If you set `SW_CONFIG_PATH`, update it to the `.toml` path. The container default is `/config/config.toml`.
- **Generate a starting template.** On first run with no config file present, Stillwater writes a fully commented `config.toml` scaffold to `SW_CONFIG_PATH`. You can copy that scaffold and uncomment the lines you need instead of writing the file by hand.

## Syntax differences at a glance { #convert-yaml-toml-syntax }

The mechanical translation from YAML to TOML:

| Concept | YAML | TOML |
| --- | --- | --- |
| Section / nested map | `server:` then indented keys | `[server]` table header, keys below |
| Nested subsection | indentation under the parent | dotted header, e.g. `[server.tls]` |
| Key/value | `port: 1973` | `port = 1973` |
| String | `base_path: /app` (quotes optional) | `base_path = "/app"` (quotes required) |
| Boolean | `enabled: true` | `enabled = true` |
| List | `- Various` items on their own lines | `["Various", "VA"]` inline array |
| Comment | `# comment` | `# comment` |

Key rules for TOML:

- Strings must be quoted: `base_path = "/app"`, not `base_path = /app`.
- Indentation is not significant. Nesting is expressed by the table header (`[server.tls]`), not by leading spaces.
- A table header applies to every key below it until the next header.

## Convert section by section { #convert-yaml-toml-sections }

Each subsection below shows the YAML on the left and the equivalent TOML on the right. Convert only the sections you actually set; anything you omit keeps its built-in default.

### Server { #convert-yaml-toml-server }

```yaml
server:
  port: 1973
  base_path: /
  ux: stable
```

```toml
[server]
port = 1973
base_path = "/"
ux = "stable"
```

TLS, the HTTP redirect listener, and HTTP/3 are nested under `server` and become dotted tables:

```yaml
server:
  tls:
    cert_file: /config/tls/fullchain.pem
    key_file: /config/tls/privkey.pem
    port: 0
  http_redirect:
    port: 80
  http3:
    enabled: false
    port: 0
```

```toml
[server.tls]
cert_file = "/config/tls/fullchain.pem"
key_file = "/config/tls/privkey.pem"
port = 0

[server.http_redirect]
port = 80

[server.http3]
enabled = false
port = 0
```

### ACME { #convert-yaml-toml-acme }

```yaml
acme:
  domain: stillwater.example.com
  email: admin@example.com
  ca: https://acme-v02.api.letsencrypt.org/directory
  cache_dir: /config/acme-cache
```

```toml
[acme]
domain = "stillwater.example.com"
email = "admin@example.com"
ca = "https://acme-v02.api.letsencrypt.org/directory"
cache_dir = "/config/acme-cache"
```

### Database { #convert-yaml-toml-database }

```yaml
database:
  path: /config/stillwater.db
```

```toml
[database]
path = "/config/stillwater.db"
```

### Auth { #convert-yaml-toml-auth }

```yaml
auth:
  session_secret: ""
```

```toml
[auth]
session_secret = ""
```

`session_secret` is generated automatically on first run when left empty; you usually do not set it by hand.

### Encryption { #convert-yaml-toml-encryption }

```yaml
encryption:
  key: ""
```

```toml
[encryption]
key = ""
```

The encryption key is generated automatically on first run when left empty.

### Music { #convert-yaml-toml-music }

```yaml
music:
  library_path: /music
```

```toml
[music]
library_path = "/music"
```

### Scanner { #convert-yaml-toml-scanner }

Note the list translation: YAML block-sequence items become a TOML inline array.

```yaml
scanner:
  depth: 1
  exclusions:
    - Various Artists
    - Various
    - VA
    - Soundtrack
    - OST
  mtime_fast_path: true
```

```toml
[scanner]
depth = 1
exclusions = ["Various Artists", "Various", "VA", "Soundtrack", "OST"]
mtime_fast_path = true
```

### Backup { #convert-yaml-toml-backup }

```yaml
backup:
  path: ""
  retention_count: 7
  interval_hours: 24
  enabled: true
```

```toml
[backup]
path = ""
retention_count = 7
interval_hours = 24
enabled = true
```

### Logging { #convert-yaml-toml-logging }

```yaml
logging:
  level: info
  format: json
```

```toml
[logging]
level = "info"
format = "json"
```

### Rule engine { #convert-yaml-toml-rule-engine }

```yaml
rule_engine:
  artist_workers: 2
```

```toml
[rule_engine]
artist_workers = 2
```

## Switch over and verify { #convert-yaml-toml-verify }

1. Write the TOML file alongside the YAML one (for example `config.toml` next to `config.yaml`).
2. Point Stillwater at it: rename the new file to the path in `SW_CONFIG_PATH`, or update `SW_CONFIG_PATH` to the `.toml` path. The container default is `/config/config.toml`.
3. Restart Stillwater and check the startup logs. The YAML deprecation warning should be **gone**. If it is still present, Stillwater is still reading the YAML file: confirm the extension is `.toml` and that `SW_CONFIG_PATH` points at the new file.
4. Spot-check a setting you customized (port, library path, log level) on the Settings page or in the startup log lines to confirm the values carried over.
5. Once you have confirmed the TOML file loads correctly, delete or archive the old `config.yaml`.

If a value does not take effect, remember that an `SW_*` environment variable for the same setting overrides the file in both formats. Check your environment before assuming the file is wrong.
