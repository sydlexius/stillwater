---
description: Install Stillwater, run the first-time setup wizard, and connect your media server.
---

# Getting Started

Three steps from zero to running: install, run the first-time setup, connect a media server.

## 1. Install

Pick the option that matches your environment. All three install the same Stillwater build.

<div class="grid cards" markdown>

- __Docker Compose__ &middot; *Recommended*

    ---

    The canonical self-hosted setup. Auto-restart, easy upgrades, plays nicely with reverse proxies and existing media-server stacks.

    [Install with Docker Compose](install-docker-compose.md)

- __Binary__

    ---

    Smallest footprint, fewest dependencies. Single self-contained binary for macOS, Linux, and Windows.

    [Install the binary](install-binary.md)

- __Unraid__

    ---

    Search "Stillwater" in Community Applications, fill the form, Apply. The easiest path on Unraid; no compose file required.

    [Install on Unraid](install-unraid.md)

</div>

## 2. Run the first-time setup

When Stillwater starts for the first time it presents a setup wizard. Open `http://localhost:1973` in a browser and walk through it: admin user, music library locations, and platform connections.

[First-time setup walkthrough](first-run-oobe.md)

## 3. Pick a delivery mode

Stillwater can deliver curated metadata two ways. Pick the one that matches your setup -- you can change later under Settings.

<div class="grid cards" markdown>

- __NFO writeback__ &middot; *Preferred*

    ---

    Stillwater writes `artist.nfo` files (and image files) directly into your music library on disk. Highest fidelity -- captures fields like Discography that platform APIs don't expose. Requires Stillwater to have read/write access to the library directory your media server reads from.

    Set up by adding the library during [first-time setup](first-run-oobe.md) (the Music Libraries step) or later under __Settings > Libraries__.

- __API push to Emby or Jellyfin__

    ---

    Stillwater connects to Emby or Jellyfin via their HTTP APIs and pushes edits directly. No filesystem access required. Limited to fields each platform's API exposes.

    [Connect Emby](connect-emby.md) or [Connect Jellyfin](connect-jellyfin.md).

</div>

You can run both at once -- Stillwater supports it -- but it's not generally recommended. Pick the mode that matches your setup; add the other later if needed.

!!! info "Running Kodi?"
    Kodi reads NFO files directly from your library. No separate connector is needed; if Stillwater can write to the library, Kodi will pick the changes up on its next refresh.

---

Stuck? Common installation and connection issues live in [Troubleshooting](../troubleshooting/index.md).
