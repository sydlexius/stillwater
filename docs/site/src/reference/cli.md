---
description: Every command-line flag and subcommand the stillwater binary accepts, with defaults and descriptions.
---

# CLI reference

Stillwater is primarily configured through environment variables and a TOML config file. Command-line flags exist for a small set of administrative one-shot operations that must run offline (before or without starting the server).

**When to use flags vs environment variables:** use environment variables (or the config file) for everything that applies to a running server instance. Use CLI flags when you need to perform a one-time administrative action -- like resetting a password -- without starting the full server.

## Subcommands

Subcommands are passed as the first positional argument before any flags:

```
stillwater reset-credentials
```

The `reset-credentials` subcommand wipes all stored credentials and forces a fresh setup on next start. It clears provider API keys, connection credentials, user accounts, and sessions. Use this when the encryption key is lost or credentials need to be re-entered from scratch.

## Usage

```
stillwater [flags]
stillwater <subcommand>
```

The table below is generated from `internal/cli.Flags` and the `Subcommands` slice in the same package. Do not edit it by hand -- run `make generate-docs` after changing a CLI flag or subcommand.

<!-- BEGIN GENERATED: cli-reference -->
## Flags

| Flag | Type | Default | Description |
|---|---|---|---|
| `--reset-password` | boolean | `false` | Reset the admin user password and exit. Prompts interactively unless --new-password is also set. |
| `--username` | string | (none) | Username for --reset-password. When omitted, defaults to the sole admin user in the database. |
| `--new-password` | string | (none) | New password for --reset-password (INSECURE: visible in process listings; prefer the interactive prompt instead). |

## Subcommands

Subcommands are passed as the first positional argument before any flags (e.g. `stillwater reset-credentials`).

| Subcommand | Summary |
|---|---|
| `reset-credentials` | Wipe all stored credentials and force a fresh setup on next start. |

### `reset-credentials`

Clears all provider API keys, connection credentials, user accounts, and active sessions from the database. Use this when the encryption key is lost or credentials need to be re-entered from scratch. The application will prompt for initial setup on the next start. Requires database access (SW_DB_PATH or SW_CONFIG_PATH must resolve to the live database).
<!-- END GENERATED: cli-reference -->
