---
description: Install Stillwater as a single static binary on Linux, macOS, or Windows. No Docker required.
---

# Install the binary

About 5 minutes from zero to running.

Stillwater ships as a single statically-linked binary for Linux, macOS, and Windows. No runtime dependencies; no container.

## Before you start

You'll need:

- A directory to keep Stillwater's state (database, encryption key, configuration). The page assumes `~/.stillwater/` for personal installs and `/var/lib/stillwater/` for system-wide installs; pick whichever fits.
- A music library directory the binary can read, and (for NFO writeback) write into. The same directory your media server reads from.

No other prerequisites. The binary is self-contained.

## Download

Grab the archive for your platform from the [latest release](https://github.com/sydlexius/stillwater/releases/latest).

=== "Linux (amd64)"

    ```bash
    curl -LO https://github.com/sydlexius/stillwater/releases/latest/download/stillwater_<VERSION>_linux_amd64.tar.gz
    tar xzf stillwater_<VERSION>_linux_amd64.tar.gz
    sudo install -m 0755 stillwater /usr/local/bin/stillwater
    ```

=== "Linux (arm64)"

    ```bash
    curl -LO https://github.com/sydlexius/stillwater/releases/latest/download/stillwater_<VERSION>_linux_arm64.tar.gz
    tar xzf stillwater_<VERSION>_linux_arm64.tar.gz
    sudo install -m 0755 stillwater /usr/local/bin/stillwater
    ```

=== "macOS (Apple Silicon)"

    ```bash
    curl -LO https://github.com/sydlexius/stillwater/releases/latest/download/stillwater_<VERSION>_darwin_arm64.tar.gz
    tar xzf stillwater_<VERSION>_darwin_arm64.tar.gz
    sudo install -m 0755 stillwater /usr/local/bin/stillwater
    ```

=== "macOS (Intel)"

    ```bash
    curl -LO https://github.com/sydlexius/stillwater/releases/latest/download/stillwater_<VERSION>_darwin_amd64.tar.gz
    tar xzf stillwater_<VERSION>_darwin_amd64.tar.gz
    sudo install -m 0755 stillwater /usr/local/bin/stillwater
    ```

=== "Windows (amd64)"

    Download `stillwater_<VERSION>_windows_amd64.zip` from the [releases page](https://github.com/sydlexius/stillwater/releases/latest), extract `stillwater.exe`, and place it somewhere on your `PATH`.

Replace `<VERSION>` in the URLs above with the current release tag from the [releases page](https://github.com/sydlexius/stillwater/releases/latest) (e.g., `1.0.0`).

!!! tip "Or install with Go"
    If you have a Go 1.26+ toolchain, you can build from source directly:

    ```bash
    go install github.com/sydlexius/stillwater/cmd/stillwater@latest
    ```

    The resulting binary lives in `$GOBIN` (typically `~/go/bin`). Versions built this way report a development fallback string rather than a real release version.

## Verify the download

Each release ships with Sigstore-signed checksums and SLSA build provenance. Verifying is optional but recommended for production installs.

```bash
# Fetch the checksum file and verify the archive against it
curl -LO https://github.com/sydlexius/stillwater/releases/latest/download/stillwater_<VERSION>_checksums.txt
sha256sum -c stillwater_<VERSION>_checksums.txt --ignore-missing
```

Verifying the Sigstore signature on the checksum file itself requires the [`cosign`](https://docs.sigstore.dev/cosign/) CLI; see the [release notes](https://github.com/sydlexius/stillwater/releases/latest) for the exact verification command.

## Choose where Stillwater stores its data

The binary's built-in defaults assume a containerized layout (`/config`, `/music`). For a native install you need to point Stillwater at directories that actually exist on your host. Three knobs cover the common case:

| Env var | What it controls | Example |
|---|---|---|
| `SW_CONFIG_PATH` | Path to `config.toml` (file). Stillwater starts with built-in defaults if the file is missing or unset. YAML files are also accepted for backward compatibility. | `~/.stillwater/config.toml` |
| `SW_DB_PATH` | Path to the SQLite database (file). | `~/.stillwater/stillwater.db` |
| `SW_MUSIC_PATH` | Music library directory. Stillwater needs read/write here for NFO writeback. | `/srv/music` |

A first-time install only needs `SW_DB_PATH` and `SW_MUSIC_PATH`; you don't need a `config.toml` until you want to override defaults beyond what env vars cover.

## Run it

Create the data directory and start Stillwater:

```bash
mkdir -p ~/.stillwater
SW_DB_PATH=~/.stillwater/stillwater.db \
SW_MUSIC_PATH=/srv/music \
stillwater
```

Open `http://localhost:1973` in a browser. The first-time setup wizard greets you.

[Continue to first-time setup](first-run-oobe.md){ .md-button .md-button--primary }

To stop the foreground process, press `Ctrl+C`. For a long-running install, set up a service (next section).

## Run as a service

A foreground binary stops when you log out. For a real install you want Stillwater supervised by your OS init system.

=== "systemd (Linux)"

    Create `/etc/systemd/system/stillwater.service`:

    ```ini
    [Unit]
    Description=Stillwater music metadata manager
    After=network-online.target
    Wants=network-online.target

    [Service]
    Type=simple
    User=stillwater
    Group=stillwater
    ExecStart=/usr/local/bin/stillwater
    Restart=on-failure
    RestartSec=5

    Environment=SW_DB_PATH=/var/lib/stillwater/stillwater.db
    Environment=SW_MUSIC_PATH=/srv/music
    Environment=SW_LOG_LEVEL=info
    Environment=SW_LOG_FORMAT=json

    # Hardening
    ProtectSystem=strict
    ProtectHome=true
    PrivateTmp=true
    NoNewPrivileges=true
    ReadWritePaths=/var/lib/stillwater /srv/music

    [Install]
    WantedBy=multi-user.target
    ```

    Then:

    ```bash
    sudo useradd --system --no-create-home --shell /usr/sbin/nologin stillwater
    sudo mkdir -p /var/lib/stillwater
    sudo chown stillwater:stillwater /var/lib/stillwater
    sudo systemctl daemon-reload
    sudo systemctl enable --now stillwater
    sudo journalctl -u stillwater -f
    ```

    Adjust `ReadWritePaths` to include any music or backup directories Stillwater needs to write to. The service user (`stillwater`) must have read and write access to your music library; verify with `sudo -u stillwater touch /srv/music/.write-test`.

=== "launchd (macOS)"

    Create `~/Library/LaunchAgents/io.stillwater.app.plist`:

    ```xml
    <?xml version="1.0" encoding="UTF-8"?>
    <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
      "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
    <plist version="1.0">
    <dict>
        <key>Label</key>
        <string>io.stillwater.app</string>
        <key>ProgramArguments</key>
        <array>
            <string>/usr/local/bin/stillwater</string>
        </array>
        <key>EnvironmentVariables</key>
        <dict>
            <key>SW_DB_PATH</key>
            <string>/Users/YOU/.stillwater/stillwater.db</string>
            <key>SW_MUSIC_PATH</key>
            <string>/Users/YOU/Music</string>
            <key>SW_LOG_FORMAT</key>
            <string>text</string>
        </dict>
        <key>RunAtLoad</key>
        <true/>
        <key>KeepAlive</key>
        <true/>
        <key>StandardOutPath</key>
        <string>/Users/YOU/.stillwater/stillwater.log</string>
        <key>StandardErrorPath</key>
        <string>/Users/YOU/.stillwater/stillwater.log</string>
    </dict>
    </plist>
    ```

    Replace `YOU` with your username. Then load the agent:

    ```bash
    launchctl load ~/Library/LaunchAgents/io.stillwater.app.plist
    ```

    Logs land at `~/.stillwater/stillwater.log`. Reload after editing the plist with `launchctl unload` followed by `launchctl load`.

=== "Windows"

    Windows has no first-class supervisor for arbitrary binaries. Two common options:

    - **NSSM** (the Non-Sucking Service Manager). Install NSSM, then `nssm install Stillwater "C:\Program Files\stillwater\stillwater.exe"` and configure the service environment variables through the NSSM GUI.
    - **Task Scheduler** with "At log on" trigger. Simpler, but not a true service; Stillwater runs only while the configured user is logged in.

## Configure with a `config.toml` (optional)

Env vars cover most needs, but you can also supply a `config.toml` for static configuration:

```toml
[server]
port = 1973
base_path = "/"

[database]
path = "/var/lib/stillwater/stillwater.db"

[music]
library_path = "/srv/music"

[backup]
enabled = true
interval_hours = 24
retention_count = 7

[logging]
level = "info"
format = "json"
```

Point Stillwater at it with `SW_CONFIG_PATH=/etc/stillwater/config.toml`. Env vars override values from the file, so a config file plus a few environment overrides is a good production pattern.

YAML configuration files (`.yaml` or `.yml`) remain supported for backward compatibility; the loader picks the parser by file extension.

## Upgrading

Replace the binary with the next release's archive. With systemd:

```bash
curl -LO https://github.com/sydlexius/stillwater/releases/latest/download/stillwater_<NEW_VERSION>_linux_amd64.tar.gz
tar xzf stillwater_<NEW_VERSION>_linux_amd64.tar.gz
sudo systemctl stop stillwater
sudo install -m 0755 stillwater /usr/local/bin/stillwater
sudo systemctl start stillwater
```

Stillwater runs schema migrations automatically on startup; you don't need to migrate the database manually.

## Backups

Back up the SQLite database file (`SW_DB_PATH`), the encryption key (in the same directory by default), and your `config.toml` if you have one. The simplest path is to back up the entire data directory:

```bash
tar czf "stillwater-$(date +%F).tar.gz" -C ~/.stillwater .
```

For scheduled in-app backups, set:

```bash
SW_BACKUP_ENABLED=true
SW_BACKUP_INTERVAL=24
SW_BACKUP_RETENTION=7
SW_BACKUP_PATH=~/.stillwater/backups
```

## Troubleshooting

See [Installation > Native binary](../troubleshooting/index.md#native-binary) in the troubleshooting docs.
